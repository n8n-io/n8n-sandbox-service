import type { Readable } from "node:stream";
import { InvalidStreamEventError } from "./errors";
import type {
  ExecEvent,
  ExecStartedEvent,
  ExecStdoutEvent,
  ExecStderrEvent,
  ExecExitEvent,
  ExecErrorEvent,
} from "./types";

type JsonObject = Record<string, unknown>;

function isExecEvent(json: JsonObject): json is ExecEvent {
  return (
    typeof json.seq === "number" &&
    typeof json.type === "string" &&
    ["started", "stdout", "stderr", "exit", "error"].includes(json.type)
  );
}

function isStartedEvent(json: JsonObject): json is ExecStartedEvent {
  return isExecEvent(json) && json.type === "started" && typeof json.exec_id === "string";
}

function isStdoutEvent(json: JsonObject): json is ExecStdoutEvent {
  return isExecEvent(json) && json.type === "stdout" && typeof json.data === "string";
}

function isStderrEvent(json: JsonObject): json is ExecStderrEvent {
  return isExecEvent(json) && json.type === "stderr" && typeof json.data === "string";
}

function isExitEvent(json: JsonObject): json is ExecExitEvent {
  return (
    isExecEvent(json) &&
    json.type === "exit" &&
    typeof json.exit_code === "number" &&
    typeof json.success === "boolean" &&
    typeof json.execution_time_ms === "number" &&
    typeof json.timed_out === "boolean" &&
    typeof json.killed === "boolean" &&
    typeof json.seq === "number"
  );
}

function isErrorEvent(json: JsonObject): json is ExecErrorEvent {
  return isExecEvent(json) && json.type === "error" && typeof json.error === "string";
}

/** Yields parsed exec events from an NDJSON stream, one per line. */
export async function* readNdjsonStream(stream: Readable): AsyncGenerator<ExecEvent> {
  let pending = "";
  const decoder = new TextDecoder("utf-8");

  for await (const chunk of stream) {
    pending += decodeChunk(decoder, chunk, { stream: true });

    let newlineIndex = pending.indexOf("\n");
    while (newlineIndex !== -1) {
      const line = pending.slice(0, newlineIndex).trim();
      pending = pending.slice(newlineIndex + 1);
      if (line.length > 0) {
        yield parseExecEvent(line);
      }
      newlineIndex = pending.indexOf("\n");
    }
  }

  pending += decoder.decode();

  const last = pending.trim();
  if (last.length > 0) {
    yield parseExecEvent(last);
  }
}

function decodeChunk(decoder: TextDecoder, chunk: unknown, options?: TextDecodeOptions): string {
  if (typeof chunk === "string") return decoder.decode(Buffer.from(chunk, "utf-8"), options);
  if (chunk instanceof ArrayBuffer) return decoder.decode(chunk, options);
  if (ArrayBuffer.isView(chunk)) return decoder.decode(chunk as ArrayBufferView, options);

  return decoder.decode(Buffer.from(String(chunk), "utf-8"), options);
}

/** Parses a single NDJSON line into a typed exec event. Returns an error event on invalid input. */
export function parseExecEvent(line: string): ExecEvent {
  try {
    const json = JSON.parse(line) as JsonObject;

    if (isStartedEvent(json)) return json;
    if (isStdoutEvent(json)) return json;
    if (isStderrEvent(json)) return json;
    if (isExitEvent(json)) return json;
    if (isErrorEvent(json)) return { ...json, type: "error" as const };

    return { type: "error", error: `Invalid exec event payload: ${line}` };
  } catch (error) {
    throw new InvalidStreamEventError(line, error instanceof Error ? error : undefined);
  }
}

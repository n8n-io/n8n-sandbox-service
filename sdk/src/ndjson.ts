import type { Readable } from "node:stream";
import { StringDecoder } from "node:string_decoder";
import type {
  ExecEvent,
  ExecSessionEvent,
  ExecStdoutEvent,
  ExecStderrEvent,
  ExecExitEvent,
  ExecErrorEvent,
} from "./types";

type JsonObject = Record<string, unknown>;

function isSessionEvent(json: JsonObject): json is ExecSessionEvent {
  return (
    json.type === "session" && typeof json.exec_id === "string" && typeof json.seq === "number"
  );
}

function isStdoutEvent(json: JsonObject): json is ExecStdoutEvent {
  return json.type === "stdout" && typeof json.data === "string" && typeof json.seq === "number";
}

function isStderrEvent(json: JsonObject): json is ExecStderrEvent {
  return json.type === "stderr" && typeof json.data === "string" && typeof json.seq === "number";
}

function isExitEvent(json: JsonObject): json is ExecExitEvent {
  return (
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
  if (typeof json.error !== "string") return false;
  return json.type === "error" || json.type === undefined;
}

/** Yields parsed exec events from an NDJSON stream, one per line. */
export async function* readNdjsonStream(stream: Readable): AsyncGenerator<ExecEvent> {
  let pending = "";
  const decoder = new StringDecoder("utf-8");

  for await (const chunk of stream) {
    pending += decoder.write(Buffer.isBuffer(chunk) ? chunk : Buffer.from(String(chunk), "utf-8"));

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

  pending += decoder.end();

  const last = pending.trim();
  if (last.length > 0) {
    yield parseExecEvent(last);
  }
}

/** Parses a single NDJSON line into a typed exec event. Returns an error event on invalid input. */
export function parseExecEvent(line: string): ExecEvent {
  try {
    const json = JSON.parse(line) as JsonObject;

    if (isSessionEvent(json)) return json;
    if (isStdoutEvent(json)) return json;
    if (isStderrEvent(json)) return json;
    if (isExitEvent(json)) return json;
    if (isErrorEvent(json)) return { ...json, type: "error" as const };

    return { type: "error", error: `Invalid exec event payload: ${line}` };
  } catch (error) {
    return {
      type: "error",
      error: `Invalid exec event payload: ${error instanceof Error ? error.message : String(error)}`,
    };
  }
}

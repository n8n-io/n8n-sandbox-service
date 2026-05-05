import type { Readable } from "node:stream";
import { StringDecoder } from "node:string_decoder";
import type { ExecEvent } from "./types";

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
    const json = JSON.parse(line) as Record<string, unknown>;
    const type = json.type;

    if (type === "stdout" && typeof json.data === "string") {
      return { type: "stdout", data: json.data };
    }
    if (type === "stderr" && typeof json.data === "string") {
      return { type: "stderr", data: json.data };
    }
    if (
      type === "exit" &&
      typeof json.exit_code === "number" &&
      typeof json.success === "boolean" &&
      typeof json.execution_time_ms === "number" &&
      typeof json.timed_out === "boolean" &&
      typeof json.killed === "boolean"
    ) {
      return {
        type: "exit",
        exit_code: json.exit_code,
        success: json.success,
        execution_time_ms: json.execution_time_ms,
        timed_out: json.timed_out,
        killed: json.killed,
      };
    }
    if (type === "error" && typeof json.error === "string") {
      return { type: "error", error: json.error };
    }

    return { type: "error", error: `Invalid exec event payload: ${line}` };
  } catch (error) {
    return { type: "error", error: `Invalid exec event payload: ${error instanceof Error ? error.message : String(error)}` };
  }
}

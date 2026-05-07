import type { Readable } from "node:stream";
import { SandboxServiceError } from "./errors";
import { readNdjsonStream } from "./ndjson";
import type { ExecResult } from "./types";

type ExitMeta = {
  exitCode: number;
  executionTimeMs: number;
  timedOut: boolean;
  killed: boolean;
  success: boolean;
};

/**
 * Accumulates NDJSON exec events from one or more streams into a single
 * ExecResult. Tracks the latest sequence number so callers can resume
 * from the correct position after a disconnect.
 *
 * Usage:
 *   const consumer = new ExecStreamConsumer(onStdout, onStderr);
 *   await consumer.consume(stream);   // may be called multiple times
 *   return consumer.result();          // throws if no exit event received
 */
export class ExecStreamConsumer {
  stdout = "";
  stderr = "";
  /** Last sequence number received, or -1 if no events have been consumed. */
  lastSeq = -1;
  exitMeta: ExitMeta | null = null;
  execError: string | undefined;

  constructor(
    private readonly onStdout?: (data: string) => void,
    private readonly onStderr?: (data: string) => void,
  ) {}

  /** Whether a terminal event (exit or error) has been received. */
  get isDone() {
    return this.exitMeta !== null || this.execError !== undefined;
  }

  /** Reads all events from the stream, updating internal state. */
  async consume(stream: Readable) {
    for await (const event of readNdjsonStream(stream)) {
      if ("seq" in event && typeof event.seq === "number") {
        this.lastSeq = event.seq;
      }
      switch (event.type) {
        case "session":
          break;
        case "stdout":
          this.stdout += event.data;
          this.onStdout?.(event.data);
          break;
        case "stderr":
          this.stderr += event.data;
          this.onStderr?.(event.data);
          break;
        case "exit":
          this.exitMeta = {
            exitCode: event.exit_code,
            executionTimeMs: event.execution_time_ms,
            timedOut: event.timed_out,
            killed: event.killed,
            success: event.success,
          };
          break;
        case "error":
          this.execError = event.error;
          return;
      }
    }
  }

  /** Returns the aggregated result, or throws if the stream ended abnormally. */
  result(): ExecResult {
    if (this.execError) {
      throw new SandboxServiceError(this.execError, 0);
    }
    if (!this.exitMeta) {
      throw new SandboxServiceError("Sandbox exec stream ended without an exit event", 0);
    }
    return {
      exitCode: this.exitMeta.exitCode,
      stdout: this.stdout,
      stderr: this.stderr,
      executionTimeMs: this.exitMeta.executionTimeMs,
      timedOut: this.exitMeta.timedOut,
      killed: this.exitMeta.killed,
      success: this.exitMeta.success,
    };
  }
}

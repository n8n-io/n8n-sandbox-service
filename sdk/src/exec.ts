import type { HttpClient } from "./http";
import { readNdjsonStream } from "./ndjson";
import type { ExecRequest, ExecResult } from "./types";

export async function exec(
  http: HttpClient,
  id: string,
  request: ExecRequest,
): Promise<ExecResult> {
  const { stream } = await http.requestStream("POST", `/sandboxes/${id}/exec`, {
    data: {
      command: request.command,
      env: request.env,
      workdir: request.workdir,
      timeout_ms: request.timeoutMs,
    },
    signal: request.abortSignal,
  });

  let stdout = "";
  let stderr = "";
  let exitMeta: {
    exitCode: number;
    executionTimeMs: number;
    timedOut: boolean;
    killed: boolean;
    success: boolean;
  } | null = null;

  for await (const event of readNdjsonStream(stream)) {
    switch (event.type) {
      case "stdout":
        stdout += event.data;
        request.onStdout?.(event.data);
        break;
      case "stderr":
        stderr += event.data;
        request.onStderr?.(event.data);
        break;
      case "error":
        throw new Error(event.error);
      case "exit":
        exitMeta = {
          exitCode: event.exit_code,
          executionTimeMs: event.execution_time_ms,
          timedOut: event.timed_out,
          killed: event.killed,
          success: event.success,
        };
        break;
    }
  }

  if (!exitMeta) {
    throw new Error("Sandbox exec stream ended without an exit event");
  }

  return {
    exitCode: exitMeta.exitCode,
    stdout,
    stderr,
    executionTimeMs: exitMeta.executionTimeMs,
    timedOut: exitMeta.timedOut,
    killed: exitMeta.killed,
    success: exitMeta.success,
  };
}

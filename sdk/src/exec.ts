import { randomUUID } from "node:crypto";
import { SandboxServiceError } from "./errors";
import { ExecStreamConsumer } from "./exec-stream-consumer";
import type { HttpClient } from "./http";
import type { ExecRequest, ExecResult } from "./types";

const MAX_RESUME_RETRIES = 10;
const RESUME_DELAY_MS = 250;
const TRANSIENT_ERROR_CODES = new Set([
  "ECONNRESET",
  "ECONNREFUSED",
  "EPIPE",
  "ETIMEDOUT",
  "ECONNABORTED",
  "ERR_STREAM_PREMATURE_CLOSE",
]);

export async function exec(
  http: HttpClient,
  id: string,
  request: ExecRequest,
): Promise<ExecResult> {
  const execId = randomUUID();
  const consumer = new ExecStreamConsumer(request.onStdout, request.onStderr);
  let retries = 0;

  const onError = async (error: unknown) => {
    if (request.abortSignal?.aborted) {
      await cancelExecSession(http, id, execId).catch(() => {});
      throw error;
    }
    if (consumer.isDone) return;
    if (!isTransientError(error)) throw error;
    if (++retries > MAX_RESUME_RETRIES) throw error;
    await delay(RESUME_DELAY_MS);
  };

  // Phase 1: Start command via POST (idempotent via exec_id)
  while (!consumer.isDone) {
    try {
      const { stream } = await http.requestStream(
        "POST",
        `/sandboxes/${id}/exec`,
        {
          data: {
            command: request.command,
            env: request.env,
            workdir: request.workdir,
            timeout_ms: request.timeoutMs,
            exec_id: execId,
          },
          signal: request.abortSignal,
        },
      );
      await consumer.consume(stream);
      break;
    } catch (error) {
      await onError(error);
      if (consumer.lastSeq >= 0) break; // Received events, switch to resume
    }
  }

  // Phase 2: Resume via GET (exec_id always known)
  while (!consumer.isDone) {
    try {
      const params: Record<string, string> = { follow: "true" };
      if (consumer.lastSeq >= 0) params.after = String(consumer.lastSeq);
      const { stream } = await http.requestStream(
        "GET",
        `/sandboxes/${id}/exec/${execId}`,
        { params, signal: request.abortSignal },
      );
      await consumer.consume(stream);
      break;
    } catch (error) {
      await onError(error);
    }
  }

  return consumer.result();
}

export async function resumeExecSession(
  http: HttpClient,
  sandboxId: string,
  execId: string,
  afterSeq?: number,
): Promise<ExecResult> {
  const params: Record<string, string> = {};
  if (afterSeq !== undefined) {
    params.after = String(afterSeq);
  }

  const { stream } = await http.requestStream(
    "GET",
    `/sandboxes/${sandboxId}/exec/${execId}`,
    { params },
  );

  const consumer = new ExecStreamConsumer();
  await consumer.consume(stream);
  return consumer.result();
}

export async function cancelExecSession(
  http: HttpClient,
  sandboxId: string,
  execId: string,
): Promise<void> {
  await http.requestVoid(
    "DELETE",
    `/sandboxes/${sandboxId}/exec/${execId}`,
  );
}

function isTransientError(error: unknown): boolean {
  if (error instanceof SandboxServiceError) {
    return error.status === 0 || error.status === 503;
  }
  if (!(error instanceof Error)) return false;

  const code = (error as NodeJS.ErrnoException).code;
  if (code) {
    return TRANSIENT_ERROR_CODES.has(code);
  }

  return false;
}

function delay(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

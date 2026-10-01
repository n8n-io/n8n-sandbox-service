import { randomUUID } from "node:crypto";
import { SandboxServiceError, InvalidStreamEventError } from "./errors";
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
  let limitedRetries = 0;
  // Whether a POST may have reached the sandbox: one returned a stream, or failed without
  // showing how far it got. A 429 only proves that the refused request started nothing.
  let mayHaveStarted = false;

  const onError = async (error: unknown) => {
    if (request.abortSignal?.aborted) {
      await deleteExecution(http, id, execId).catch(() => {});
      throw error;
    }
    if (consumer.isDone) return;
    if (!isTransientError(error)) throw error;
    if (++retries > MAX_RESUME_RETRIES) throw error;
    await delay(RESUME_DELAY_MS);
  };

  // For when retries on a 429 run out: if the command may be running, cancel it in the
  // background rather than leave it running with nobody following it. Execution DELETEs
  // have a limit of their own, so the full limit that refused us does not block this.
  const cancelIfStarted = () => {
    if (mayHaveStarted) void deleteExecution(http, id, execId).catch(() => {});
  };

  // Phase 1: Start command via POST (idempotent via exec_id)
  while (!consumer.isDone) {
    try {
      const { stream } = await http.requestStream("POST", `/sandboxes/${id}/executions`, {
        data: {
          command: request.command,
          env: request.env,
          workdir: request.workdir,
          timeout_ms: request.timeoutMs,
          exec_id: execId,
        },
        signal: request.abortSignal,
      });
      mayHaveStarted = true;
      await consumer.consume(stream);
      if (consumer.isDone) break;
      // Stream ended without a terminal event (e.g. load-balancer timeout).
      // If we received events, switch to resume via GET; otherwise retry POST.
      if (consumer.lastSeq >= 0) break;
      if (++retries > MAX_RESUME_RETRIES) break;
      await delay(RESUME_DELAY_MS);
    } catch (error) {
      if (isLimited(error)) {
        // Posting the same exec_id again starts the command if nothing has yet, and
        // follows it if an earlier POST did. The retry policy decides how often and how
        // long to wait.
        const wait = http.retryDelayFor(error, limitedRetries++);
        if (wait === undefined) {
          cancelIfStarted();
          throw error;
        }
        await delay(wait, request.abortSignal);
        if (request.abortSignal?.aborted)
          await onError(new SandboxServiceError("Request aborted", 0));
        continue;
      }
      await onError(error);
      mayHaveStarted = true;
      if (consumer.lastSeq >= 0) break; // Received events, switch to resume
    }
  }

  // Phase 2: Resume via GET (exec_id always known)
  while (!consumer.isDone) {
    try {
      const params: Record<string, string> = { follow: "true" };
      if (consumer.lastSeq >= 0) params.after = String(consumer.lastSeq);
      const { stream } = await http.requestStream("GET", `/sandboxes/${id}/executions/${execId}`, {
        params,
        signal: request.abortSignal,
      });
      await consumer.consume(stream);
      if (consumer.isDone) break;
      if (++retries > MAX_RESUME_RETRIES) break;
      await delay(RESUME_DELAY_MS);
    } catch (error) {
      // The client has already retried this GET as its policy allows.
      if (isLimited(error) && !request.abortSignal?.aborted) {
        cancelIfStarted();
        throw error;
      }
      await onError(error);
    }
  }

  void deleteExecution(http, id, execId).catch(() => {});
  return consumer.result();
}

export async function resumeExecution(
  http: HttpClient,
  sandboxId: string,
  execId: string,
  afterSeq?: number,
): Promise<ExecResult> {
  const params: Record<string, string> = { follow: "true" };
  if (afterSeq !== undefined) {
    params.after = String(afterSeq);
  }

  const { stream } = await http.requestStream(
    "GET",
    `/sandboxes/${sandboxId}/executions/${execId}`,
    {
      params,
    },
  );

  const consumer = new ExecStreamConsumer();
  await consumer.consume(stream);
  return consumer.result();
}

export async function deleteExecution(
  http: HttpClient,
  sandboxId: string,
  execId: string,
): Promise<void> {
  await http.requestVoid("DELETE", `/sandboxes/${sandboxId}/executions/${execId}`);
}

function isTransientError(error: unknown): boolean {
  if (error instanceof InvalidStreamEventError) {
    return true;
  }
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

function isLimited(error: unknown): error is SandboxServiceError {
  return error instanceof SandboxServiceError && error.status === 429;
}

/** Resolves after `ms`, or as soon as `signal` aborts. */
function delay(ms: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    if (signal?.aborted) {
      resolve();
      return;
    }
    const done = () => {
      clearTimeout(timer);
      signal?.removeEventListener("abort", done);
      resolve();
    };
    const timer = setTimeout(done, ms);
    signal?.addEventListener("abort", done);
  });
}

import { Readable } from "node:stream";
import { InvalidStreamEventError, SandboxServiceError } from "../errors";
import type { HttpClient } from "../http";
import { readNdjsonLines } from "../ndjson";
import { destroyStreams, multipartBody, type MultipartPart } from "./multipart";
import type {
  CodeSandbox,
  CodeTaskEvent,
  CodeTaskEventType,
  CreateCodeSandboxOptions,
  RunCodeTaskOptions,
} from "./types";

/** The input path of RunCodeTaskOptions.taskInput. */
const TASK_INPUT_PATH = "task.json";

const EVENT_TYPES: ReadonlySet<CodeTaskEventType> = new Set<CodeTaskEventType>([
  "accepted",
  "heartbeat",
  "done",
  "error",
]);

/** Returns the current code sandbox of `options.name`, or creates one. */
export async function createCodeSandbox(
  http: HttpClient,
  options: CreateCodeSandboxOptions,
): Promise<CodeSandbox> {
  return http.requestJson<CodeSandbox>("POST", "/code/sandboxes", {
    data: options,
    // Idempotent on the name: a repeated create returns the same sandbox.
    retry: "always",
  });
}

/**
 * Deletes a code sandbox and stops all its tasks. Treats 404 as success so retries
 * after a dropped 204 stay idempotent.
 */
export async function deleteCodeSandbox(http: HttpClient, id: string): Promise<void> {
  await deleteIgnoringNotFound(http, `/code/sandboxes/${encodeURIComponent(id)}`);
}

/**
 * Deletes all code sandboxes of `name`, draining ones too, and stops all their tasks.
 * Treats 404 as success so retries after a dropped 204 stay idempotent.
 */
export async function deleteCodeSandboxByName(http: HttpClient, name: string): Promise<void> {
  await deleteIgnoringNotFound(http, `/code/sandboxes/names/${encodeURIComponent(name)}`);
}

async function deleteIgnoringNotFound(http: HttpClient, path: string): Promise<void> {
  try {
    await http.requestVoid("DELETE", path);
  } catch (err) {
    if (err instanceof SandboxServiceError && err.status === 404) return;
    throw err;
  }
}

/**
 * Runs a task in code sandbox `sandboxId`. Resolves when the response starts,
 * with the task events as they arrive. Ending the iteration early closes the
 * stream.
 */
export async function runCodeTask(
  http: HttpClient,
  sandboxId: string,
  options: RunCodeTaskOptions,
): Promise<AsyncGenerator<CodeTaskEvent, void, undefined>> {
  const inputs = options.inputs ?? [];
  let spec: string;
  try {
    if (inputs.some((input) => input.path === TASK_INPUT_PATH)) {
      throw new Error(`Pass the ${TASK_INPUT_PATH} content as taskInput, not in inputs`);
    }
    spec = JSON.stringify(options.spec);
  } catch (error) {
    destroyStreams([options.taskInput, ...inputs.map((input) => input.content)]);
    throw error;
  }
  const parts: MultipartPart[] = [
    { name: "spec", contentType: "application/json", content: spec },
    {
      name: "input",
      filename: TASK_INPUT_PATH,
      contentType: "application/json",
      content: options.taskInput,
    },
    ...inputs.map((input) => ({
      name: "input",
      filename: input.path,
      contentType: input.contentType ?? "application/octet-stream",
      content: input.content,
    })),
  ];
  const { contentType, body, destroy: destroyUpload } = multipartBody(parts);

  let stream: Readable;
  try {
    ({ stream } = await http.requestStream("POST", `/code/sandboxes/${sandboxId}/tasks`, {
      data: body,
      headers: { "Content-Type": contentType },
      signal: options.signal,
      // The inputs can be streams, which cannot be sent twice. A task can also
      // have run when the outcome is unknown, so the caller decides on a retry.
      retry: "never",
    }));
  } catch (error) {
    destroyUpload();
    throw error;
  }
  const { signal } = options;
  const closed = { value: false };
  const release = () => {
    closed.value = true;
    signal?.removeEventListener("abort", release);
    stream.destroy();
    destroyUpload();
  };
  // The response stream outlives the request, so an abort closes it here.
  if (signal?.aborted) release();
  else signal?.addEventListener("abort", release, { once: true });
  return releasingOnClose(readTaskEvents(stream, release, closed, signal), release);
}

/**
 * Cancels task `taskId` in code sandbox `sandboxId`. Resolves when the cancel
 * is requested. The task stream then ends with an `error` event with code
 * `cancelled`. Fails with status 404 when the task is not running.
 */
export async function cancelCodeTask(
  http: HttpClient,
  sandboxId: string,
  taskId: string,
): Promise<void> {
  await http.requestVoid("DELETE", `/code/sandboxes/${sandboxId}/tasks/${taskId}`);
}

/**
 * Wraps events so that return() and throw() call release, also before the
 * first next(): a generator that has not started skips its finally block.
 * release comes first, because the generator queues return() and throw()
 * behind a next() that waits for an event.
 */
function releasingOnClose(
  events: AsyncGenerator<CodeTaskEvent, void, undefined>,
  release: () => void,
): AsyncGenerator<CodeTaskEvent, void, undefined> {
  const wrapper: AsyncGenerator<CodeTaskEvent, void, undefined> = {
    next: () => events.next(),
    return: (value) => {
      release();
      return events.return(value);
    },
    throw: (error) => {
      release();
      return events.throw(error);
    },
    [Symbol.asyncIterator]: () => wrapper,
  };
  return wrapper;
}

async function* readTaskEvents(
  stream: Readable,
  release: () => void,
  closed: { value: boolean },
  signal?: AbortSignal,
): AsyncGenerator<CodeTaskEvent, void, undefined> {
  try {
    for await (const line of readNdjsonLines(stream)) {
      // One chunk can hold many lines, which are yielded after an abort too.
      if (signal?.aborted) throw new SandboxServiceError("Request aborted", 0);
      yield parseTaskEvent(line);
    }
  } catch (error) {
    if (error instanceof InvalidStreamEventError) throw error;
    // An abort destroys the stream, which fails the read with an error of its own.
    if (signal?.aborted) throw new SandboxServiceError("Request aborted", 0);
    // The caller closed the events while a read was pending.
    if (closed.value) return;
    throw error;
  } finally {
    release();
  }
}

function parseTaskEvent(line: string): CodeTaskEvent {
  let json: unknown;
  try {
    json = JSON.parse(line);
  } catch (error) {
    throw new InvalidStreamEventError(line, error);
  }
  if (
    typeof json !== "object" ||
    json === null ||
    !EVENT_TYPES.has((json as { type?: unknown }).type as CodeTaskEventType)
  ) {
    throw new InvalidStreamEventError(line);
  }
  return json as CodeTaskEvent;
}

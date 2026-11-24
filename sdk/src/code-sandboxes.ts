import { Readable } from "node:stream";
import { InvalidStreamEventError, SandboxServiceError } from "./errors";
import type { HttpClient } from "./http";
import { destroyStreamParts, multipartBody, type MultipartPart } from "./multipart";
import { readNdjsonLines } from "./ndjson";
import type {
  CodeSandbox,
  CodeTaskEvent,
  CreateCodeSandboxOptions,
  RunCodeTaskOptions,
} from "./types";

/** The input path of RunCodeTaskOptions.taskInput. */
const TASK_INPUT_PATH = "task.json";

/** Returns the current code sandbox of `options.name`, or creates one. */
export async function createCodeSandbox(
  http: HttpClient,
  options: CreateCodeSandboxOptions,
): Promise<CodeSandbox> {
  return http.requestJson<CodeSandbox>("POST", "/code/sandboxes", {
    data: {
      name: options.name,
      runtime: options.runtime,
      runtimeVersion: options.runtimeVersion,
      maxConcurrency: options.maxConcurrency,
      memoryMb: options.memoryMb,
      idleTimeoutMs: options.idleTimeoutMs,
      maxAgeMs: options.maxAgeMs,
    },
    // Idempotent on the name: a repeated create returns the same sandbox.
    isSafeToRetry: true,
  });
}

/**
 * Runs a task in code sandbox `id`. Resolves when the response starts, with
 * the task events as they arrive. Ending the iteration early closes the stream.
 */
export async function runCodeTask(
  http: HttpClient,
  id: string,
  options: RunCodeTaskOptions,
): Promise<AsyncGenerator<CodeTaskEvent, void, undefined>> {
  const inputs = options.inputs ?? [];
  const parts: MultipartPart[] = [
    { name: "spec", contentType: "application/json", content: JSON.stringify(options.spec) },
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
  if (inputs.some((input) => input.path === TASK_INPUT_PATH)) {
    destroyStreamParts(parts);
    throw new Error(`Pass the ${TASK_INPUT_PATH} content as taskInput, not in inputs`);
  }
  const { contentType, body, destroy } = multipartBody(parts);

  let stream: Readable;
  try {
    ({ stream } = await http.requestStream("POST", `/code/sandboxes/${id}/tasks`, {
      data: body,
      headers: { "Content-Type": contentType },
      signal: options.signal,
      // The inputs can be streams, which cannot be sent twice. A task can also
      // have run when the outcome is unknown, so the caller decides on a retry.
      retry: false,
    }));
  } catch (error) {
    destroy();
    throw error;
  }
  const { signal } = options;
  const closed = { value: false };
  const release = () => {
    closed.value = true;
    signal?.removeEventListener("abort", release);
    stream.destroy();
    destroy();
  };
  // The response stream outlives the request, so an abort closes it here.
  if (signal?.aborted) release();
  else signal?.addEventListener("abort", release, { once: true });
  return releasingOnClose(readTaskEvents(stream, release, closed, signal), release);
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
    typeof (json as CodeTaskEvent).type !== "string"
  ) {
    throw new InvalidStreamEventError(line);
  }
  return json as CodeTaskEvent;
}

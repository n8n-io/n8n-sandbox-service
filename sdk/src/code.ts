import { randomBytes } from "node:crypto";
import * as http from "node:http";
import * as https from "node:https";
import { Readable } from "node:stream";
import { pipeline } from "node:stream/promises";
import { SandboxServiceError } from "./errors";

/**
 * Client for the Task API (`/v1/code/...`): code sandboxes, tasks, task
 * outputs and RPC results.
 */

/** The largest RPC result body that the service accepts. */
export const RPC_RESULT_MAX_BYTES = 1024 * 1024 * 1024;

export interface CodeSandboxClientOptions {
  baseUrl: string;
  apiKey?: string;
  /** Agent for the HTTP requests. Defaults to a keep-alive agent. */
  agent?: http.Agent;
}

export interface CreateCodeSandboxOptions {
  runtime?: string;
  slots?: number;
  taskMemoryMb?: number;
  spareWorkers?: boolean;
  /** Run all tasks in one shared worker process. The slots limit how many run at the same time. */
  reuseWorkers?: boolean;
  labels?: Record<string, string>;
  signal?: AbortSignal;
}

export interface CodeSandboxTimings {
  create: number;
  runnerCreate: number;
  daemonReady: number;
  spareReady: number;
}

export interface CodeSandbox {
  id: string;
  runtime: string;
  runtimeVersion: string;
  slots: number;
  state: string;
  busySlots?: number;
  /** Worker processes started since the sandbox started (GET only). */
  workersStarted?: number;
  /** CPU time of the sandbox container since its start, in microseconds (GET only). */
  cpuUsageUsec?: number;
  /** Peak and current memory of the sandbox container, in bytes (GET only). */
  memoryPeakBytes?: number;
  memoryCurrentBytes?: number;
  /** Time since the code daemon started, in ms (GET only). */
  uptimeMs?: number;
  timings?: CodeSandboxTimings;
}

export interface CodeTaskSpec {
  taskId: string;
  timeoutMs?: number;
  inlineResultMaxBytes?: number;
}

/** One input file. `path` is relative to the `in/` folder of the task. */
export interface CodeTaskInput {
  path: string;
  content: Buffer | string | Readable | (() => Promise<Readable>);
}

export interface CodeTaskOutput {
  path: string;
  size: number;
}

export type CodeTaskEvent =
  | { type: "accepted"; taskId: string; slot: number }
  | { type: "stdout"; data: string }
  | { type: "stderr"; data: string }
  | CodeTaskRpcEvent
  | CodeTaskDoneEvent
  | CodeTaskErrorEvent;

/**
 * An RPC call of the task. Answer it with `postRpcResult` while the task runs.
 * The task is the scope of the call.
 */
export interface CodeTaskRpcEvent {
  type: "rpc";
  callId: string;
  name: string;
  params: unknown[];
}

export interface CodeTaskDoneEvent {
  type: "done";
  result: unknown;
  outputs?: CodeTaskOutput[];
  timings?: Record<string, unknown>;
}

export type CodeTaskErrorCode =
  | "user_error"
  | "timeout"
  | "cancelled"
  | "oom"
  | "crashed"
  | "invalid";

export interface CodeTaskErrorEvent {
  type: "error";
  code: CodeTaskErrorCode;
  message: string;
  userError?: unknown;
  timings?: Record<string, unknown>;
}

export interface RunCodeTaskOptions {
  signal?: AbortSignal;
  /** Called for each event. Must not block. */
  onEvent?: (event: CodeTaskEvent) => void;
  /**
   * Answers the RPC calls of the task. The client posts each reply with
   * `postRpcResult`. A call runs in the background, so the stream is read
   * while it runs. Without this option, use `onEvent` and `postRpcResult`.
   */
  onRpc?: (call: CodeTaskRpcEvent) => Promise<RpcReply>;
}

/** A transport failure of a task request. `accepted` tells if the worker had the task. */
export class CodeTaskTransportError extends SandboxServiceError {
  constructor(
    message: string,
    status: number,
    readonly accepted: boolean,
  ) {
    super(message, status);
    this.name = "CodeTaskTransportError";
  }
}

export type RpcReply = { ok: true; data: unknown } | { ok: false; error: unknown };

export class CodeSandboxClient {
  private readonly base: URL;
  private readonly agent: http.Agent;

  constructor(private readonly options: CodeSandboxClientOptions) {
    if (!options.baseUrl) throw new Error("Sandbox service URL is not configured");
    this.base = new URL(options.baseUrl.replace(/\/+$/, "") + "/");
    const isHttps = this.base.protocol === "https:";
    this.agent =
      options.agent ??
      (isHttps
        ? new https.Agent({ keepAlive: true, maxSockets: 256 })
        : new http.Agent({ keepAlive: true, maxSockets: 256 }));
  }

  async createSandbox(opts: CreateCodeSandboxOptions = {}): Promise<CodeSandbox> {
    const { signal, ...body } = opts;
    return this.json<CodeSandbox>("POST", "v1/code/sandboxes", body, signal);
  }

  async getSandbox(id: string): Promise<CodeSandbox> {
    return this.json<CodeSandbox>("GET", `v1/code/sandboxes/${id}`);
  }

  /** Deletes the sandbox and kills all its tasks. A 404 counts as success. */
  async deleteSandbox(id: string): Promise<void> {
    try {
      await this.json("DELETE", `v1/code/sandboxes/${id}`);
    } catch (e) {
      if (e instanceof SandboxServiceError && e.status === 404) return;
      throw e;
    }
  }

  /**
   * Runs a task. The inputs are streamed as multipart parts. Resolves with the
   * terminal event. Rejects with CodeTaskTransportError if the stream ends
   * without one.
   */
  async runTask(
    sandboxId: string,
    spec: CodeTaskSpec,
    inputs: CodeTaskInput[],
    opts: RunCodeTaskOptions = {},
  ): Promise<CodeTaskDoneEvent | CodeTaskErrorEvent> {
    const boundary = "----n8ncode" + randomBytes(12).toString("hex");
    const res = await this.request(
      "POST",
      `v1/code/sandboxes/${sandboxId}/tasks`,
      { "Content-Type": `multipart/form-data; boundary=${boundary}` },
      Readable.from(multipartBody(boundary, spec, inputs)),
      opts.signal,
    );
    if (res.statusCode !== 200) {
      throw await errorFromResponse(res);
    }
    let accepted = false;
    let terminal: CodeTaskDoneEvent | CodeTaskErrorEvent | undefined;
    const answer = (call: CodeTaskRpcEvent) => {
      void Promise.resolve()
        .then(async () => await opts.onRpc!(call))
        .catch((e: unknown): RpcReply => ({ ok: false, error: serializeError(e) }))
        .then(async (reply) => await this.postRpcResult(sandboxId, spec.taskId, call.callId, reply))
        // A lost result shows as a task timeout.
        .catch(() => {});
    };
    try {
      for await (const ev of readNdjson<CodeTaskEvent>(res)) {
        if (ev.type === "accepted") accepted = true;
        if (ev.type === "rpc") {
          if (!Array.isArray(ev.params)) ev.params = [];
          if (opts.onRpc) answer(ev);
        }
        opts.onEvent?.(ev);
        if (ev.type === "done" || ev.type === "error") {
          terminal = ev;
        }
      }
    } catch (e) {
      throw new CodeTaskTransportError(`task stream failed: ${(e as Error).message}`, 0, accepted);
    }
    if (!terminal) {
      throw new CodeTaskTransportError("task stream ended without a terminal event", 0, accepted);
    }
    return terminal;
  }

  /** Streams one output file of a finished task. */
  async getOutput(
    sandboxId: string,
    taskId: string,
    path: string,
    signal?: AbortSignal,
  ): Promise<Readable> {
    const enc = path.split("/").map(encodeURIComponent).join("/");
    const res = await this.request(
      "GET",
      `v1/code/sandboxes/${sandboxId}/tasks/${taskId}/outputs/${enc}`,
      {},
      undefined,
      signal,
    );
    if (res.statusCode !== 200) throw await errorFromResponse(res);
    return res;
  }

  /** Cancels a running task, or deletes the outputs of a finished task. */
  async deleteTask(sandboxId: string, taskId: string): Promise<void> {
    await this.json("DELETE", `v1/code/sandboxes/${sandboxId}/tasks/${taskId}`);
  }

  /**
   * Sends the reply to an RPC call of a running task. A reply larger than
   * RPC_RESULT_MAX_BYTES is replaced with an error reply. A 404 (the task or
   * the call is no longer open) counts as success.
   */
  async postRpcResult(
    sandboxId: string,
    taskId: string,
    callId: string,
    reply: RpcReply,
  ): Promise<void> {
    const errorReply = (message: string) =>
      Buffer.from(JSON.stringify({ ok: false, error: { message } }));
    let payload: Buffer;
    try {
      payload = Buffer.from(JSON.stringify(reply));
    } catch (e) {
      // A value that JSON cannot encode (BigInt, a cycle), or a string that
      // is too large for V8.
      payload = errorReply(`RPC result cannot be serialized: ${(e as Error).message}`);
    }
    if (payload.length > RPC_RESULT_MAX_BYTES) payload = errorReply("RPC result too large");
    const res = await this.request(
      "POST",
      `v1/code/sandboxes/${sandboxId}/tasks/${encodeURIComponent(taskId)}/rpc/${encodeURIComponent(callId)}`,
      { "Content-Type": "application/json", "Content-Length": String(payload.length) },
      Readable.from([payload]),
    );
    const text = await readAll(res);
    const status = res.statusCode ?? 0;
    if (status === 404 || (status >= 200 && status < 300)) return;
    throw errorFromText(status, text);
  }

  private async json<T>(
    method: string,
    path: string,
    body?: unknown,
    signal?: AbortSignal,
  ): Promise<T> {
    const payload = body === undefined ? undefined : Buffer.from(JSON.stringify(body));
    const res = await this.request(
      method,
      path,
      payload
        ? { "Content-Type": "application/json", "Content-Length": String(payload.length) }
        : {},
      payload ? Readable.from([payload]) : undefined,
      signal,
    );
    const text = await readAll(res);
    if ((res.statusCode ?? 0) >= 300) throw errorFromText(res.statusCode ?? 0, text);
    return (text ? JSON.parse(text) : undefined) as T;
  }

  private request(
    method: string,
    path: string,
    headers: Record<string, string>,
    body: Readable | undefined,
    signal?: AbortSignal,
  ): Promise<http.IncomingMessage> {
    const url = new URL(path, this.base);
    const mod = url.protocol === "https:" ? https : http;
    const allHeaders: Record<string, string> = { ...headers };
    if (this.options.apiKey) allHeaders["X-Api-Key"] = this.options.apiKey;
    return new Promise((resolve, reject) => {
      const req = mod.request(
        url,
        { method, headers: allHeaders, agent: this.agent, signal },
        resolve,
      );
      req.on("error", reject);
      if (body) {
        pipeline(body, req).catch(reject);
      } else {
        req.end();
      }
    });
  }
}

async function* multipartBody(
  boundary: string,
  spec: CodeTaskSpec,
  inputs: CodeTaskInput[],
): AsyncGenerator<Buffer> {
  yield Buffer.from(
    `--${boundary}\r\nContent-Disposition: form-data; name="spec"\r\nContent-Type: application/json\r\n\r\n` +
      JSON.stringify(spec) +
      "\r\n",
  );
  for (const input of inputs) {
    const name = input.path.replace(/["\\\r\n]/g, "_");
    yield Buffer.from(
      `--${boundary}\r\nContent-Disposition: form-data; name="input"; filename="${name}"\r\nContent-Type: application/octet-stream\r\n\r\n`,
    );
    const content = typeof input.content === "function" ? await input.content() : input.content;
    if (typeof content === "string") {
      yield Buffer.from(content);
    } else if (Buffer.isBuffer(content)) {
      yield content;
    } else {
      for await (const chunk of content) {
        yield Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk);
      }
    }
    yield Buffer.from("\r\n");
  }
  yield Buffer.from(`--${boundary}--\r\n`);
}

/**
 * The longest NDJSON line that readNdjson accepts, in characters. A worker
 * control line is at most 64 MiB, and an "rpc" event carries one.
 */
export const NDJSON_MAX_LINE_CHARS = 128 * 1024 * 1024;

/**
 * Parses an NDJSON stream. The time is linear in the stream size, also for
 * long lines: each chunk is searched once.
 */
export async function* readNdjson<T>(
  stream: AsyncIterable<Buffer | string>,
  maxLineChars = NDJSON_MAX_LINE_CHARS,
): AsyncGenerator<T> {
  const decoder = new TextDecoder("utf-8");
  let parts: string[] = [];
  let partsLength = 0;
  const take = (last: string): string => {
    parts.push(last);
    const line = parts.join("");
    parts = [];
    partsLength = 0;
    return line.trim();
  };
  for await (const chunk of stream) {
    const text = typeof chunk === "string" ? chunk : decoder.decode(chunk, { stream: true });
    let start = 0;
    let nl = text.indexOf("\n");
    while (nl !== -1) {
      if (partsLength + nl - start > maxLineChars) throw new Error("NDJSON line too long");
      const line = take(text.slice(start, nl));
      if (line) yield JSON.parse(line) as T;
      start = nl + 1;
      nl = text.indexOf("\n", start);
    }
    if (start < text.length) {
      partsLength += text.length - start;
      if (partsLength > maxLineChars) throw new Error("NDJSON line too long");
      parts.push(text.slice(start));
    }
  }
  const line = take(decoder.decode());
  if (line) yield JSON.parse(line) as T;
}

async function readAll(stream: AsyncIterable<Buffer | string>): Promise<string> {
  const chunks: Buffer[] = [];
  for await (const c of stream) chunks.push(Buffer.isBuffer(c) ? c : Buffer.from(c));
  return Buffer.concat(chunks).toString("utf-8");
}

async function errorFromResponse(res: http.IncomingMessage): Promise<SandboxServiceError> {
  return errorFromText(res.statusCode ?? 0, await readAll(res));
}

function errorFromText(status: number, text: string): SandboxServiceError {
  let message = text;
  try {
    const parsed = JSON.parse(text) as { error?: string };
    if (parsed.error) message = parsed.error;
  } catch {
    // Not JSON.
  }
  return new SandboxServiceError(message || `HTTP ${status}`, status);
}

function serializeError(e: unknown): unknown {
  if (e instanceof Error) return { message: e.message, name: e.name };
  return e;
}

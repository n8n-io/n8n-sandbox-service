import { randomBytes } from "node:crypto";
import * as http from "node:http";
import * as https from "node:https";
import { Readable } from "node:stream";
import { pipeline } from "node:stream/promises";
import { SandboxServiceError } from "./errors";

/**
 * Client for the Task API (`/v1/code/...`): code sandboxes, tasks, task
 * outputs and the RPC channel.
 */

export interface CodeSandboxClientOptions {
  baseUrl: string;
  apiKey?: string;
  /** Agent for the HTTP requests. Defaults to a keep-alive agent. */
  agent?: http.Agent;
  /**
   * Creates the channel WebSocket. Defaults to the global WebSocket of Node,
   * which accepts a `headers` option.
   */
  webSocketFactory?: (url: string, headers: Record<string, string>) => WebSocketLike;
}

/** The subset of the WebSocket API that the channel uses. */
export interface WebSocketLike {
  send(data: string): void;
  close(code?: number, reason?: string): void;
  addEventListener(type: "open" | "close" | "error" | "message", listener: (ev: any) => void): void;
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
  channelOpen?: boolean;
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
  | CodeTaskDoneEvent
  | CodeTaskErrorEvent;

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
  onEvent?: (event: CodeTaskEvent) => void;
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

export interface RpcCall {
  taskId: string;
  callId: string;
  name: string;
  params: unknown[];
}

export type RpcReply = { ok: true; data: unknown } | { ok: false; error: unknown };

export interface CodeChannel {
  readonly runtimeVersion: string;
  /** Resolves when the channel closes. */
  readonly closed: Promise<void>;
  close(): void;
}

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
    try {
      for await (const ev of readNdjson<CodeTaskEvent>(res)) {
        if (ev.type === "accepted") accepted = true;
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
   * Opens the RPC channel. `handler` answers each rpc:call. Resolves after the
   * channel:ready frame.
   */
  async openChannel(
    sandboxId: string,
    handler: (call: RpcCall) => Promise<RpcReply>,
  ): Promise<CodeChannel> {
    const url = new URL(`v1/code/sandboxes/${sandboxId}/channel`, this.base);
    url.protocol = url.protocol === "https:" ? "wss:" : "ws:";
    const headers: Record<string, string> = {};
    if (this.options.apiKey) headers["X-Api-Key"] = this.options.apiKey;
    const factory =
      this.options.webSocketFactory ??
      ((u: string, h: Record<string, string>) =>
        new (globalThis as any).WebSocket(u, { headers: h }) as WebSocketLike);
    const ws = factory(url.toString(), headers);

    return new Promise<CodeChannel>((resolve, reject) => {
      let ready = false;
      let resolveClosed!: () => void;
      const closed = new Promise<void>((r) => (resolveClosed = r));
      ws.addEventListener("message", (ev: { data: unknown }) => {
        let frame: any;
        try {
          frame = JSON.parse(String(ev.data));
        } catch {
          return;
        }
        if (frame.type === "channel:ready" && !ready) {
          ready = true;
          resolve({
            runtimeVersion: String(frame.runtimeVersion ?? ""),
            closed,
            close: () => ws.close(1000, "client close"),
          });
          return;
        }
        if (frame.type === "rpc:call") {
          const call: RpcCall = {
            taskId: frame.taskId,
            callId: frame.callId,
            name: frame.name,
            params: Array.isArray(frame.params) ? frame.params : [],
          };
          void handler(call)
            .catch((e: unknown): RpcReply => ({ ok: false, error: serializeError(e) }))
            .then((reply) => {
              ws.send(
                JSON.stringify({
                  type: "rpc:result",
                  taskId: call.taskId,
                  callId: call.callId,
                  ...reply,
                }),
              );
            });
        }
      });
      ws.addEventListener("error", (ev: { message?: string }) => {
        if (!ready)
          reject(new SandboxServiceError(`channel error: ${ev?.message ?? "unknown"}`, 0));
      });
      ws.addEventListener("close", () => {
        if (!ready) reject(new SandboxServiceError("channel closed before it was ready", 0));
        resolveClosed();
      });
    });
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

/** Parses an NDJSON stream. Lines have no size limit. */
export async function* readNdjson<T>(stream: AsyncIterable<Buffer | string>): AsyncGenerator<T> {
  const decoder = new TextDecoder("utf-8");
  let pending = "";
  for await (const chunk of stream) {
    pending += typeof chunk === "string" ? chunk : decoder.decode(chunk, { stream: true });
    let nl = pending.indexOf("\n");
    while (nl !== -1) {
      const line = pending.slice(0, nl).trim();
      pending = pending.slice(nl + 1);
      if (line) yield JSON.parse(line) as T;
      nl = pending.indexOf("\n");
    }
  }
  pending += decoder.decode();
  if (pending.trim()) yield JSON.parse(pending.trim()) as T;
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

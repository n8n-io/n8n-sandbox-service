import axios, { type AxiosInstance, type Method, isAxiosError } from "axios";
import type { Readable } from "node:stream";
import { SandboxServiceError, createErrorFromResponse } from "./errors";
import type { RetryOptions } from "./types";

export interface RequestOptions {
  data?: unknown;
  params?: Record<string, string>;
  headers?: Record<string, string>;
  signal?: AbortSignal;
  /** Allow retry on non-idempotent methods (e.g. POST). Default false. */
  isSafeToRetry?: boolean;
}

interface NormalizedRetryOptions {
  attempts: number;
  baseDelayMs: number;
  maxDelayMs: number;
  retryOnStatuses: Set<number>;
  jitter: boolean;
}

export class HttpClient {
  private readonly instance: AxiosInstance;
  private readonly retry: NormalizedRetryOptions;

  constructor(baseUrl: string, apiKey?: string, retry?: RetryOptions) {
    const normalizedBase = baseUrl.replace(/\/+$/, "");
    if (!normalizedBase) {
      throw new Error("Sandbox service URL is not configured");
    }

    this.instance = axios.create({
      baseURL: normalizedBase,
      headers: apiKey ? { "X-Api-Key": apiKey } : {},
    });
    this.retry = {
      attempts: Math.max(0, retry?.attempts ?? 0),
      baseDelayMs: Math.max(0, retry?.baseDelayMs ?? 200),
      maxDelayMs: Math.max(0, retry?.maxDelayMs ?? 10000),
      retryOnStatuses: new Set(retry?.retryOnStatuses ?? [429, 502, 503]),
      jitter: retry?.jitter ?? true,
    };
  }

  async requestJson<T>(method: Method, path: string, options: RequestOptions = {}): Promise<T> {
    return this.withRetry(async () => {
      const response = await this.instance.request<T>({
        method,
        url: path,
        data: options.data,
        params: options.params,
        headers: options.headers,
        signal: options.signal,
      });
      return response.data;
    }, method, options.signal, options.isSafeToRetry === true);
  }

  async requestStream(
    method: Method,
    path: string,
    options: RequestOptions = {},
  ): Promise<{ stream: Readable; status: number }> {
    return this.withRetry(async () => {
      const response = await this.instance.request<Readable>({
        method,
        url: path,
        data: options.data,
        params: options.params,
        headers: options.headers,
        signal: options.signal,
        responseType: "stream",
        validateStatus: () => true,
      });

      if (response.status >= 400) {
        const body = await this.drainStream(response.data);
        throw createErrorFromResponse(response.status, this.tryParseJson(body));
      }

      return { stream: response.data, status: response.status };
    }, method, options.signal, options.isSafeToRetry === true);
  }

  async requestBuffer(
    method: Method,
    path: string,
    options: Omit<RequestOptions, "data"> = {}
  ): Promise<Buffer> {
    return this.withRetry(async () => {
      const response = await this.instance.request<ArrayBuffer>({
        method,
        url: path,
        params: options.params,
        headers: options.headers,
        signal: options.signal,
        responseType: "arraybuffer",
        validateStatus: () => true,
      });

      const body = Buffer.from(response.data);
      if (response.status >= 400) {
        throw createErrorFromResponse(response.status, this.tryParseJson(body.toString("utf-8")));
      }

      return body;
    }, method, options.signal, options.isSafeToRetry === true);
  }

  async requestVoid(method: Method, path: string, options: RequestOptions = {}): Promise<void> {
    return this.withRetry(async () => {
      await this.instance.request({
        method,
        url: path,
        data: options.data,
        params: options.params,
        headers: options.headers,
        signal: options.signal,
      });
    }, method, options.signal, options.isSafeToRetry === true);
  }

  private toServiceError(error: unknown): SandboxServiceError {
    if (error instanceof SandboxServiceError) return error;

    if (isAxiosError(error) && error.response) {
      return createErrorFromResponse(error.response.status, error.response.data);
    }

    const message = error instanceof Error ? error.message : "Unknown sandbox service error";
    return new SandboxServiceError(message, 0);
  }

  private async withRetry<T>(
    operation: () => Promise<T>,
    method: Method,
    signal?: AbortSignal,
    isSafeToRetry: boolean = false,
  ): Promise<T> {
    let attempt = 0;
    while (true) {
      try {
        return await operation();
      } catch (error) {
        const serviceError = this.toServiceError(error);
        if (!this.shouldRetry(serviceError, attempt, method, signal, isSafeToRetry)) {
          throw serviceError;
        }

        const delayMs = this.retryDelayMs(attempt);
        await this.sleep(delayMs, signal);
        attempt += 1;
      }
    }
  }

  private shouldRetry(
    error: SandboxServiceError,
    attempt: number,
    method: Method,
    signal?: AbortSignal,
    isSafeToRetry: boolean = false,
  ): boolean {
    if (signal?.aborted) return false;
    if (attempt >= this.retry.attempts) return false;
    if (!isSafeToRetry && !this.isMethodIdempotent(method)) return false;
    if (error.status === 0) return true;
    return this.retry.retryOnStatuses.has(error.status);
  }

  private isMethodIdempotent(method: Method): boolean {
    const m = String(method).toUpperCase();
    return m === "GET" || m === "HEAD" || m === "OPTIONS" || m === "PUT" || m === "DELETE";
  }

  private retryDelayMs(attempt: number): number {
    const base = this.retry.baseDelayMs * (2 ** attempt);
    const capped = Math.min(base, this.retry.maxDelayMs);
    if (!this.retry.jitter) return capped;
    const factor = 0.5 + Math.random(); // [0.5, 1.5)
    return Math.floor(capped * factor);
  }

  private sleep(ms: number, signal?: AbortSignal): Promise<void> {
    if (ms <= 0) return Promise.resolve();
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => {
        signal?.removeEventListener("abort", onAbort);
        resolve();
      }, ms);
      const onAbort = () => {
        clearTimeout(timer);
        signal?.removeEventListener("abort", onAbort);
        reject(new SandboxServiceError("Request aborted", 0));
      };
      if (signal) {
        signal.addEventListener("abort", onAbort);
      }
    });
  }

  private async drainStream(stream: Readable): Promise<string> {
    const chunks: Buffer[] = [];
    for await (const chunk of stream) {
      chunks.push(Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk));
    }
    return Buffer.concat(chunks).toString("utf-8");
  }

  private tryParseJson(text: string): unknown {
    try {
      return JSON.parse(text);
    } catch {
      return text;
    }
  }
}

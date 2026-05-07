import axios, { type AxiosInstance, type Method, isAxiosError } from "axios";
import type { Readable } from "node:stream";
import { SandboxServiceError, createErrorFromResponse } from "./errors";

export interface RequestOptions {
  data?: unknown;
  params?: Record<string, string>;
  headers?: Record<string, string>;
  signal?: AbortSignal;
}

export class HttpClient {
  private readonly instance: AxiosInstance;

  constructor(baseUrl: string, apiKey?: string) {
    const normalizedBase = baseUrl.replace(/\/+$/, "");
    if (!normalizedBase) {
      throw new Error("Sandbox service URL is not configured");
    }

    this.instance = axios.create({
      baseURL: normalizedBase,
      headers: apiKey ? { "X-Api-Key": apiKey } : {},
    });
  }

  async requestJson<T>(method: Method, path: string, options: RequestOptions = {}): Promise<T> {
    try {
      const response = await this.instance.request<T>({
        method,
        url: path,
        data: options.data,
        params: options.params,
        headers: options.headers,
        signal: options.signal,
      });
      return response.data;
    } catch (error) {
      throw this.toServiceError(error);
    }
  }

  async requestStream(
    method: Method,
    path: string,
    options: RequestOptions = {},
  ): Promise<{ stream: Readable; status: number }> {
    try {
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
    } catch (error) {
      if (error instanceof SandboxServiceError) throw error;
      throw this.toServiceError(error);
    }
  }

  async requestBuffer(
    method: Method,
    path: string,
    options: Omit<RequestOptions, "data"> = {},
  ): Promise<Buffer> {
    try {
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
    } catch (error) {
      throw this.toServiceError(error);
    }
  }

  async requestVoid(method: Method, path: string, options: RequestOptions = {}): Promise<void> {
    try {
      await this.instance.request({
        method,
        url: path,
        data: options.data,
        params: options.params,
        headers: options.headers,
        signal: options.signal,
      });
    } catch (error) {
      throw this.toServiceError(error);
    }
  }

  private toServiceError(error: unknown): SandboxServiceError {
    if (error instanceof SandboxServiceError) return error;

    if (isAxiosError(error) && error.response) {
      return createErrorFromResponse(error.response.status, error.response.data);
    }

    const message = error instanceof Error ? error.message : "Unknown sandbox service error";
    return new SandboxServiceError(message, 0);
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

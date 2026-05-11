import { afterAll, beforeAll, describe, expect, it } from "vitest";
import { SandboxServiceError } from "../src/errors.js";
import { HttpClient } from "../src/http.js";
import { startTestServer, type TestServer } from "./helpers.js";

describe("HttpClient", () => {
  let server: TestServer;
  let baseUrl: string;

  beforeAll(async () => {
    server = await startTestServer((req, res) => {
      if (req.url === "/stream-error") {
        res.writeHead(404, { "Content-Type": "application/json" });
        res.end(JSON.stringify({ error: "sandbox not found", code: 4041 }));
        return;
      }

      if (req.url === "/buffer-error") {
        res.writeHead(404, { "Content-Type": "application/json" });
        res.end(JSON.stringify({ error: "file not found", code: 4042 }));
        return;
      }

      res.writeHead(500, { "Content-Type": "text/plain" });
      res.end("unexpected request");
    });
    baseUrl = server.baseUrl;
  });

  afterAll(async () => {
    await server.close();
  });

  it("preserves JSON error payloads for stream requests", async () => {
    const client = new HttpClient(baseUrl);

    try {
      await client.requestStream("POST", "/stream-error");
      throw new Error("Expected requestStream to throw");
    } catch (error) {
      expect(error).toBeInstanceOf(SandboxServiceError);
      expect(error).toMatchObject({
        message: "sandbox not found",
        status: 404,
        code: 4041,
      });
    }
  });

  it("preserves JSON error payloads for buffer requests", async () => {
    const client = new HttpClient(baseUrl);

    try {
      await client.requestBuffer("GET", "/buffer-error");
      throw new Error("Expected requestBuffer to throw");
    } catch (error) {
      expect(error).toBeInstanceOf(SandboxServiceError);
      expect(error).toMatchObject({
        message: "file not found",
        status: 404,
        code: 4042,
      });
    }
  });

  it("does not retry 502 responses with default retry options", async () => {
    let hits = 0;
    const failServer = await startTestServer((req, res) => {
      if (req.url !== "/always-502") {
        res.writeHead(404);
        res.end();
        return;
      }
      hits += 1;
      res.writeHead(502, { "Content-Type": "application/json" });
      res.end(JSON.stringify({ error: "sandbox not running" }));
    });

    try {
      const client = new HttpClient(failServer.baseUrl);
      await expect(client.requestJson("GET", "/always-502")).rejects.toMatchObject({
        status: 502,
        message: "sandbox not running",
      });
      expect(hits).toBe(1);
    } finally {
      await failServer.close();
    }
  });

  it("retries transient 503 responses and eventually succeeds", async () => {
    let hits = 0;
    const retryServer = await startTestServer((req, res) => {
      if (req.url !== "/retry-503") {
        res.writeHead(404);
        res.end();
        return;
      }
      hits += 1;
      if (hits < 3) {
        res.writeHead(503, { "Content-Type": "application/json" });
        res.end(JSON.stringify({ error: "sandbox unavailable" }));
        return;
      }
      res.writeHead(200, { "Content-Type": "application/json" });
      res.end(JSON.stringify({ ok: true }));
    });

    try {
      const client = new HttpClient(retryServer.baseUrl, undefined, {
        attempts: 3,
        baseDelayMs: 1,
        maxDelayMs: 2,
        jitter: false,
      });
      const out = await client.requestJson<{ ok: boolean }>("GET", "/retry-503");
      expect(out).toEqual({ ok: true });
      expect(hits).toBe(3);
    } finally {
      await retryServer.close();
    }
  });

  it("retries 503 with default retry options when none passed", async () => {
    let hits = 0;
    const retryServer = await startTestServer((req, res) => {
      if (req.url !== "/default-retry") {
        res.writeHead(404);
        res.end();
        return;
      }
      hits += 1;
      if (hits < 4) {
        res.writeHead(503, { "Content-Type": "application/json" });
        res.end(JSON.stringify({ error: "runner unavailable" }));
        return;
      }
      res.writeHead(200, { "Content-Type": "application/json" });
      res.end(JSON.stringify({ ok: true }));
    });

    try {
      const client = new HttpClient(retryServer.baseUrl, undefined, {
        baseDelayMs: 1,
        maxDelayMs: 2,
        jitter: false,
      });
      const out = await client.requestJson<{ ok: boolean }>("GET", "/default-retry");
      expect(out).toEqual({ ok: true });
      expect(hits).toBe(4);
    } finally {
      await retryServer.close();
    }
  });

  it("stops after retry budget on transient failures", async () => {
    let hits = 0;
    const failServer = await startTestServer((req, res) => {
      if (req.url !== "/always-503") {
        res.writeHead(404);
        res.end();
        return;
      }
      hits += 1;
      res.writeHead(503, { "Content-Type": "application/json" });
      res.end(JSON.stringify({ error: "sandbox unavailable" }));
    });

    try {
      const client = new HttpClient(failServer.baseUrl, undefined, {
        attempts: 2,
        baseDelayMs: 1,
        maxDelayMs: 2,
        jitter: false,
      });
      await expect(client.requestJson("GET", "/always-503")).rejects.toMatchObject({
        status: 503,
        message: "sandbox unavailable",
      });
      // initial + 2 retries
      expect(hits).toBe(3);
    } finally {
      await failServer.close();
    }
  });

  it("does not retry non-idempotent POST by default", async () => {
    let hits = 0;
    const postServer = await startTestServer((req, res) => {
      if (req.url !== "/post-503") {
        res.writeHead(404);
        res.end();
        return;
      }
      hits += 1;
      res.writeHead(503, { "Content-Type": "application/json" });
      res.end(JSON.stringify({ error: "sandbox unavailable" }));
    });

    try {
      const client = new HttpClient(postServer.baseUrl, undefined, {
        attempts: 3,
        baseDelayMs: 1,
        maxDelayMs: 2,
        jitter: false,
      });
      await expect(
        client.requestJson("POST", "/post-503", { data: { x: 1 } }),
      ).rejects.toMatchObject({
        status: 503,
      });
      expect(hits).toBe(1);
    } finally {
      await postServer.close();
    }
  });

  it("retries non-idempotent POST only when explicitly opted in", async () => {
    let hits = 0;
    const postServer = await startTestServer((req, res) => {
      if (req.url !== "/post-retry") {
        res.writeHead(404);
        res.end();
        return;
      }
      hits += 1;
      if (hits < 3) {
        res.writeHead(503, { "Content-Type": "application/json" });
        res.end(JSON.stringify({ error: "sandbox unavailable" }));
        return;
      }
      res.writeHead(200, { "Content-Type": "application/json" });
      res.end(JSON.stringify({ ok: true }));
    });

    try {
      const client = new HttpClient(postServer.baseUrl, undefined, {
        attempts: 3,
        baseDelayMs: 1,
        maxDelayMs: 2,
        jitter: false,
      });
      const out = await client.requestJson<{ ok: boolean }>("POST", "/post-retry", {
        data: { x: 1 },
        isSafeToRetry: true,
      });
      expect(out).toEqual({ ok: true });
      expect(hits).toBe(3);
    } finally {
      await postServer.close();
    }
  });
});

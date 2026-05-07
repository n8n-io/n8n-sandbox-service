import { createServer, type Server } from "node:http";
import { afterAll, beforeAll, describe, expect, it } from "vitest";
import { SandboxServiceError } from "../src/errors.js";
import { HttpClient } from "../src/http.js";

describe("HttpClient", () => {
  let server: Server;
  let baseUrl: string;

  beforeAll(async () => {
    server = createServer((req, res) => {
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

    await new Promise<void>((resolve, reject) => {
      server.listen(0, "127.0.0.1", () => resolve());
      server.once("error", reject);
    });

    const address = server.address();
    if (address === null || typeof address === "string") {
      throw new Error("Expected an ephemeral TCP port");
    }

    baseUrl = `http://127.0.0.1:${address.port}`;
  });

  afterAll(async () => {
    await new Promise<void>((resolve, reject) => {
      server.close((error) => (error ? reject(error) : resolve()));
    });
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

  it("retries transient 503 responses and eventually succeeds", async () => {
    let hits = 0;
    const retryServer = createServer((req, res) => {
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

    await new Promise<void>((resolve, reject) => {
      retryServer.listen(0, "127.0.0.1", () => resolve());
      retryServer.once("error", reject);
    });
    const addr = retryServer.address();
    if (addr === null || typeof addr === "string") {
      throw new Error("Expected retry server TCP address");
    }
    const retryBaseUrl = `http://127.0.0.1:${addr.port}`;

    try {
      const client = new HttpClient(retryBaseUrl, undefined, {
        attempts: 3,
        baseDelayMs: 1,
        maxDelayMs: 2,
        jitter: false,
      });
      const out = await client.requestJson<{ ok: boolean }>("GET", "/retry-503");
      expect(out).toEqual({ ok: true });
      expect(hits).toBe(3);
    } finally {
      await new Promise<void>((resolve, reject) => {
        retryServer.close((error) => (error ? reject(error) : resolve()));
      });
    }
  });

  it("stops after retry budget on transient failures", async () => {
    let hits = 0;
    const failServer = createServer((req, res) => {
      if (req.url !== "/always-503") {
        res.writeHead(404);
        res.end();
        return;
      }
      hits += 1;
      res.writeHead(503, { "Content-Type": "application/json" });
      res.end(JSON.stringify({ error: "sandbox unavailable" }));
    });

    await new Promise<void>((resolve, reject) => {
      failServer.listen(0, "127.0.0.1", () => resolve());
      failServer.once("error", reject);
    });
    const addr = failServer.address();
    if (addr === null || typeof addr === "string") {
      throw new Error("Expected fail server TCP address");
    }
    const failBaseUrl = `http://127.0.0.1:${addr.port}`;

    try {
      const client = new HttpClient(failBaseUrl, undefined, {
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
      await new Promise<void>((resolve, reject) => {
        failServer.close((error) => (error ? reject(error) : resolve()));
      });
    }
  });

  it("does not retry non-idempotent POST by default", async () => {
    let hits = 0;
    const postServer = createServer((req, res) => {
      if (req.url !== "/post-503") {
        res.writeHead(404);
        res.end();
        return;
      }
      hits += 1;
      res.writeHead(503, { "Content-Type": "application/json" });
      res.end(JSON.stringify({ error: "sandbox unavailable" }));
    });

    await new Promise<void>((resolve, reject) => {
      postServer.listen(0, "127.0.0.1", () => resolve());
      postServer.once("error", reject);
    });
    const addr = postServer.address();
    if (addr === null || typeof addr === "string") {
      throw new Error("Expected post server TCP address");
    }
    const postBaseUrl = `http://127.0.0.1:${addr.port}`;

    try {
      const client = new HttpClient(postBaseUrl, undefined, {
        attempts: 3,
        baseDelayMs: 1,
        maxDelayMs: 2,
        jitter: false,
      });
      await expect(client.requestJson("POST", "/post-503", { data: { x: 1 } })).rejects.toMatchObject({
        status: 503,
      });
      expect(hits).toBe(1);
    } finally {
      await new Promise<void>((resolve, reject) => {
        postServer.close((error) => (error ? reject(error) : resolve()));
      });
    }
  });

  it("retries non-idempotent POST only when explicitly opted in", async () => {
    let hits = 0;
    const postServer = createServer((req, res) => {
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

    await new Promise<void>((resolve, reject) => {
      postServer.listen(0, "127.0.0.1", () => resolve());
      postServer.once("error", reject);
    });
    const addr = postServer.address();
    if (addr === null || typeof addr === "string") {
      throw new Error("Expected post retry server TCP address");
    }
    const postBaseUrl = `http://127.0.0.1:${addr.port}`;

    try {
      const client = new HttpClient(postBaseUrl, undefined, {
        attempts: 3,
        baseDelayMs: 1,
        maxDelayMs: 2,
        jitter: false,
      });
      const out = await client.requestJson<{ ok: boolean }>("POST", "/post-retry", {
        data: { x: 1 },
        retryUnsafe: true,
      });
      expect(out).toEqual({ ok: true });
      expect(hits).toBe(3);
    } finally {
      await new Promise<void>((resolve, reject) => {
        postServer.close((error) => (error ? reject(error) : resolve()));
      });
    }
  });
});

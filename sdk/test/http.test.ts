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
});

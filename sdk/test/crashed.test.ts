import { describe, expect, it } from "vitest";
import { SandboxCrashedError, SandboxServiceError } from "../src/errors.js";
import { HttpClient } from "../src/http.js";
import { startTestServer } from "./helpers.js";

const RESTARTED_BODY = JSON.stringify({
  error: "sandbox restarted after guest crash; state in memory was lost",
  reason: "sandbox_restarted",
});

describe("sandbox restarted after a guest crash", () => {
  it("raises SandboxCrashedError from a JSON request", async () => {
    const server = await startTestServer((_req, res) => {
      res.writeHead(409, { "Content-Type": "application/json", "X-Sandbox-Restarted": "1" });
      res.end(RESTARTED_BODY);
    });

    try {
      const client = new HttpClient(server.baseUrl);
      await expect(client.requestJson("GET", "/sandboxes/abc/files")).rejects.toBeInstanceOf(
        SandboxCrashedError,
      );
    } finally {
      await server.close();
    }
  });

  it("raises SandboxCrashedError from a stream request, which is where an exec starts", async () => {
    const server = await startTestServer((_req, res) => {
      res.writeHead(409, { "Content-Type": "application/json", "X-Sandbox-Restarted": "1" });
      res.end(RESTARTED_BODY);
    });

    try {
      const client = new HttpClient(server.baseUrl);
      await expect(
        client.requestStream("POST", "/sandboxes/abc/executions"),
      ).rejects.toBeInstanceOf(SandboxCrashedError);
    } finally {
      await server.close();
    }
  });

  // Error bodies are reshaped between the runner and the API, so the reason field has
  // to stand on its own.
  it("recognizes the reason field without the header", async () => {
    const server = await startTestServer((_req, res) => {
      res.writeHead(409, { "Content-Type": "application/json" });
      res.end(RESTARTED_BODY);
    });

    try {
      const client = new HttpClient(server.baseUrl);
      await expect(client.requestJson("GET", "/sandboxes/abc/files")).rejects.toBeInstanceOf(
        SandboxCrashedError,
      );
    } finally {
      await server.close();
    }
  });

  it("leaves an unrelated 409 as a plain service error", async () => {
    const server = await startTestServer((_req, res) => {
      res.writeHead(409, { "Content-Type": "application/json" });
      res.end(JSON.stringify({ error: "sandbox already exists" }));
    });

    try {
      const client = new HttpClient(server.baseUrl);
      const error = await client
        .requestJson("GET", "/sandboxes/abc/files")
        .catch((caught: unknown) => caught);
      expect(error).toBeInstanceOf(SandboxServiceError);
      expect(error).not.toBeInstanceOf(SandboxCrashedError);
    } finally {
      await server.close();
    }
  });

  // Retrying this away is the one thing that would defeat the point of the 409: the
  // client would get a working sandbox and never learn what it lost.
  it("is not retried", async () => {
    let hits = 0;
    const server = await startTestServer((_req, res) => {
      hits += 1;
      res.writeHead(409, { "Content-Type": "application/json", "X-Sandbox-Restarted": "1" });
      res.end(RESTARTED_BODY);
    });

    try {
      const client = new HttpClient(server.baseUrl, undefined, { baseDelayMs: 0 });
      await expect(client.requestJson("GET", "/sandboxes/abc/files")).rejects.toBeInstanceOf(
        SandboxCrashedError,
      );
      expect(hits).toBe(1);
    } finally {
      await server.close();
    }
  });
});

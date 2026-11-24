import { randomUUID } from "node:crypto";
import { createReadStream } from "node:fs";
import type { IncomingMessage, ServerResponse } from "node:http";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { PassThrough, Readable } from "node:stream";
import { afterEach, describe, expect, it } from "vitest";
import {
  createCodeSandbox,
  deleteCodeSandbox,
  deleteCodeSandboxByName,
  runCodeTask,
} from "../../src/code-tasks/code-sandboxes.js";
import { InvalidStreamEventError, SandboxServiceError } from "../../src/errors.js";
import { HttpClient } from "../../src/http.js";
import type { CreateCodeSandboxOptions } from "../../src/code-tasks/types.js";
import { startTestServer, type TestServer } from "../helpers.js";

const options: CreateCodeSandboxOptions = {
  name: "scope-1",
  runtime: "n8n-js",
  runtimeVersion: "2.42.0",
  maxConcurrency: 2,
  memoryMb: 1024,
  idleTimeoutMs: 30000,
};
const sandbox = {
  id: "11111111-1111-4111-8111-111111111111",
  name: "scope-1",
  created: true,
  state: "ready",
};

type Answer = { status: number; body: unknown };

describe("createCodeSandbox", () => {
  let server: TestServer | undefined;
  let requests: { method?: string; url?: string; body: unknown }[] = [];

  afterEach(async () => {
    await server?.close();
    server = undefined;
    requests = [];
  });

  /** Answers the nth request (1-based) with answer(n). */
  async function client(answer: (n: number) => Answer): Promise<HttpClient> {
    server = await startTestServer((req, res) => {
      let raw = "";
      req.on("data", (chunk) => (raw += chunk));
      req.on("end", () => {
        requests.push({ method: req.method, url: req.url, body: JSON.parse(raw) });
        const { status, body } = answer(requests.length);
        res.writeHead(status, { "Content-Type": "application/json" });
        res.end(JSON.stringify(body));
      });
    });
    return new HttpClient(server.baseUrl, "key", { baseDelayMs: 0 });
  }

  it("posts the settings and returns the sandbox", async () => {
    const http = await client(() => ({ status: 201, body: sandbox }));

    await expect(createCodeSandbox(http, options)).resolves.toEqual(sandbox);
    expect(requests).toEqual([{ method: "POST", url: "/code/sandboxes", body: options }]);
  });

  it("repeats the create on a 503, since it is idempotent on the name", async () => {
    const http = await client((n) =>
      n === 1
        ? { status: 503, body: { error: "at capacity", code: 503, reason: "no_capacity" } }
        : { status: 200, body: { ...sandbox, created: false } },
    );

    await expect(createCodeSandbox(http, options)).resolves.toMatchObject({ created: false });
    expect(requests).toHaveLength(2);
  });

  it("throws on other settings for the name", async () => {
    const http = await client(() => ({
      status: 409,
      body: {
        error: "sandbox name exists with other settings",
        code: 409,
        reason: "sandbox_settings_mismatch",
      },
    }));

    await expect(createCodeSandbox(http, options)).rejects.toMatchObject({
      status: 409,
      reason: "sandbox_settings_mismatch",
    });
    expect(requests).toHaveLength(1);
  });
});

describe("deleteCodeSandbox and deleteCodeSandboxByName", () => {
  let server: TestServer | undefined;
  let requests: { method?: string; url?: string }[] = [];

  afterEach(async () => {
    await server?.close();
    server = undefined;
    requests = [];
  });

  /** Answers every request with status. */
  async function client(status: number): Promise<HttpClient> {
    server = await startTestServer((req, res) => {
      requests.push({ method: req.method, url: req.url });
      if (status === 204) {
        res.writeHead(204);
        res.end();
        return;
      }
      res.writeHead(status, { "Content-Type": "application/json" });
      res.end(JSON.stringify({ error: "failed", code: status }));
    });
    return new HttpClient(server.baseUrl, "key", { baseDelayMs: 0 });
  }

  it("deletes by id", async () => {
    const http = await client(204);

    await expect(deleteCodeSandbox(http, sandbox.id)).resolves.toBeUndefined();
    expect(requests).toEqual([{ method: "DELETE", url: `/code/sandboxes/${sandbox.id}` }]);
  });

  it("deletes by name and encodes the name", async () => {
    const http = await client(204);

    await expect(deleteCodeSandboxByName(http, "wf:1")).resolves.toBeUndefined();
    expect(requests).toEqual([{ method: "DELETE", url: "/code/sandboxes/names/wf%3A1" }]);
  });

  it("treats 404 as success", async () => {
    const http = await client(404);

    await expect(deleteCodeSandbox(http, sandbox.id)).resolves.toBeUndefined();
    await expect(deleteCodeSandboxByName(http, "scope-1")).resolves.toBeUndefined();
  });

  it("throws on other errors", async () => {
    const http = await client(502);

    await expect(deleteCodeSandbox(http, sandbox.id)).rejects.toMatchObject({ status: 502 });
    await expect(deleteCodeSandboxByName(http, "scope-1")).rejects.toMatchObject({ status: 502 });
  });
});

describe("runCodeTask", () => {
  const id = "11111111-1111-4111-8111-111111111111";
  let server: TestServer | undefined;
  let requests: {
    method?: string;
    url?: string;
    headers: IncomingMessage["headers"];
    body: Buffer;
  }[] = [];

  afterEach(async () => {
    server?.server.closeAllConnections();
    await server?.close();
    server = undefined;
    requests = [];
  });

  /** Reads each request body, then hands the response to respond. */
  async function client(
    respond: (res: ServerResponse<IncomingMessage>, n: number) => void,
  ): Promise<HttpClient> {
    server = await startTestServer((req, res) => {
      const chunks: Buffer[] = [];
      req.on("data", (chunk: Buffer) => chunks.push(chunk));
      req.on("end", () => {
        requests.push({
          method: req.method,
          url: req.url,
          headers: req.headers,
          body: Buffer.concat(chunks),
        });
        respond(res, requests.length);
      });
    });
    return new HttpClient(server.baseUrl, "key", { baseDelayMs: 0 });
  }

  function events(res: ServerResponse<IncomingMessage>, lines: unknown[]): void {
    res.writeHead(200, { "Content-Type": "application/x-ndjson" });
    res.end(lines.map((line) => JSON.stringify(line) + "\n").join(""));
  }

  async function collect(iterable: AsyncIterable<unknown>): Promise<unknown[]> {
    const out: unknown[] = [];
    for await (const item of iterable) out.push(item);
    return out;
  }

  async function formData(request: (typeof requests)[number]): Promise<FormData> {
    return new Response(new Uint8Array(request.body), {
      headers: { "Content-Type": String(request.headers["content-type"]) },
    }).formData();
  }

  it("posts the spec and inputs as multipart and yields the events", async () => {
    const http = await client((res) =>
      events(res, [
        { type: "accepted", taskId: "t_1" },
        { type: "done", result: { ok: true } },
      ]),
    );

    const stream = await runCodeTask(http, id, {
      spec: { taskId: "t_1", timeoutMs: 1000 },
      taskInput: '{"code":"return 1"}',
      inputs: [
        { path: "bin/0", content: Buffer.from([0, 1, 2]) },
        { path: "bin/1", content: Readable.from([Buffer.from("str"), Buffer.from("eam")]) },
      ],
    });

    await expect(collect(stream)).resolves.toEqual([
      { type: "accepted", taskId: "t_1" },
      { type: "done", result: { ok: true } },
    ]);
    expect(requests).toHaveLength(1);
    expect(requests[0]).toMatchObject({ method: "POST", url: `/code/sandboxes/${id}/tasks` });
    expect(requests[0].headers["x-api-key"]).toBe("key");

    const form = await formData(requests[0]);
    expect(JSON.parse(String(form.get("spec")))).toEqual({ taskId: "t_1", timeoutMs: 1000 });
    const inputs = form.getAll("input") as File[];
    expect(inputs.map((file) => file.name)).toEqual(["task.json", "bin/0", "bin/1"]);
    expect(await inputs[0].text()).toBe('{"code":"return 1"}');
    expect(inputs[0].type).toBe("application/json");
    expect([...new Uint8Array(await inputs[1].arrayBuffer())]).toEqual([0, 1, 2]);
    expect(await inputs[2].text()).toBe("stream");
  });

  it("sends a stream input as it is read", async () => {
    const input = new PassThrough();
    let received = "";
    server = await startTestServer((req, res) => {
      req.on("data", (chunk: Buffer) => {
        received += chunk.toString("utf-8");
        // The second write waits until the server has the first one.
        if (received.includes("first")) input.end("second");
      });
      req.on("end", () => events(res, [{ type: "done" }]));
    });
    const http = new HttpClient(server.baseUrl, "key");
    input.write("first");

    const stream = await runCodeTask(http, id, {
      spec: { taskId: "t_1" },
      taskInput: "{}",
      inputs: [{ path: "bin/0", content: input }],
    });

    await expect(collect(stream)).resolves.toEqual([{ type: "done" }]);
    expect(received).toContain("firstsecond");
  });

  it.each([429, 503])("does not retry a %i", async (status) => {
    const http = await client((res) => {
      res.writeHead(status, { "Content-Type": "application/json" });
      res.end(JSON.stringify({ error: "sandbox is full", code: status, reason: "sandbox_full" }));
    });

    const err = await runCodeTask(http, id, { spec: { taskId: "t_1" }, taskInput: "{}" }).catch(
      (e) => e,
    );

    expect(err).toBeInstanceOf(SandboxServiceError);
    expect(err).toMatchObject({ status, reason: "sandbox_full" });
    expect(requests).toHaveLength(1);
  });

  it("destroys the stream inputs of a refused task", async () => {
    server = await startTestServer((_req, res) => {
      res.writeHead(429, { "Content-Type": "application/json" });
      res.end(JSON.stringify({ error: "sandbox is full", code: 429 }));
    });
    const http = new HttpClient(server.baseUrl, "key");
    // Never ends, as a slow upload would.
    const input = new PassThrough();
    input.write("x".repeat(1000));

    await expect(
      runCodeTask(http, id, {
        spec: { taskId: "t_1" },
        taskInput: "{}",
        inputs: [{ path: "bin/0", content: input }],
      }),
    ).rejects.toMatchObject({ status: 429 });
    expect(input.destroyed).toBe(true);
  });

  it("closes the connection when the iteration ends early", async () => {
    let closed!: () => void;
    const responseClosed = new Promise<void>((resolve) => (closed = resolve));
    const http = await client((res) => {
      res.on("close", () => closed());
      res.writeHead(200, { "Content-Type": "application/x-ndjson" });
      res.write(JSON.stringify({ type: "accepted" }) + "\n");
    });

    const stream = await runCodeTask(http, id, { spec: { taskId: "t_1" }, taskInput: "{}" });
    for await (const event of stream) {
      expect(event).toEqual({ type: "accepted" });
      break;
    }

    await responseClosed;
  });

  it("closes the connection on return() before the first event", async () => {
    let closed!: () => void;
    const responseClosed = new Promise<void>((resolve) => (closed = resolve));
    const http = await client((res) => {
      res.on("close", () => closed());
      res.writeHead(200, { "Content-Type": "application/x-ndjson" });
      res.write(JSON.stringify({ type: "accepted" }) + "\n");
    });

    const stream = await runCodeTask(http, id, { spec: { taskId: "t_1" }, taskInput: "{}" });
    await expect(stream.return()).resolves.toEqual({ done: true, value: undefined });

    await responseClosed;
  });

  it("fails the iteration and closes the connection when the signal aborts", async () => {
    let closed!: () => void;
    const responseClosed = new Promise<void>((resolve) => (closed = resolve));
    const http = await client((res) => {
      res.on("close", () => closed());
      res.writeHead(200, { "Content-Type": "application/x-ndjson" });
      res.write(JSON.stringify({ type: "accepted" }) + "\n");
    });
    const controller = new AbortController();

    const stream = await runCodeTask(http, id, {
      spec: { taskId: "t_1" },
      taskInput: "{}",
      signal: controller.signal,
    });
    await stream.next();
    controller.abort();

    await expect(stream.next()).rejects.toMatchObject({ status: 0, message: "Request aborted" });
    await responseClosed;
  });

  it("yields no buffered event after the signal aborts", async () => {
    const http = await client((res) => {
      res.writeHead(200, { "Content-Type": "application/x-ndjson" });
      res.write('{"type":"accepted"}\n{"type":"heartbeat"}\n');
    });
    const controller = new AbortController();

    const stream = await runCodeTask(http, id, {
      spec: { taskId: "t_1" },
      taskInput: "{}",
      signal: controller.signal,
    });
    await expect(stream.next()).resolves.toEqual({ done: false, value: { type: "accepted" } });
    controller.abort();

    await expect(stream.next()).rejects.toMatchObject({ status: 0, message: "Request aborted" });
  });

  it.each([
    ["return()", (stream: AsyncGenerator<unknown>) => stream.return(undefined)],
    ["an abort", (_: AsyncGenerator<unknown>, controller: AbortController) => controller.abort()],
  ])("stops a pending upload on %s", async (_, close) => {
    // Answers at the first input chunk, while the upload is still open.
    server = await startTestServer((req, res) => {
      req.once("data", () => {
        res.writeHead(200, { "Content-Type": "application/x-ndjson" });
        res.write(JSON.stringify({ type: "accepted" }) + "\n");
      });
    });
    const http = new HttpClient(server.baseUrl, "key");
    const controller = new AbortController();
    // Never ends, as a slow upload would.
    const input = new PassThrough();
    input.write("x");

    const stream = await runCodeTask(http, id, {
      spec: { taskId: "t_1" },
      taskInput: "{}",
      inputs: [{ path: "bin/0", content: input }],
      signal: controller.signal,
    });
    await stream.next();
    await close(stream, controller);

    expect(input.destroyed).toBe(true);
  });

  it("ends a pending next() and closes the connection on return()", async () => {
    let closed!: () => void;
    const responseClosed = new Promise<void>((resolve) => (closed = resolve));
    const http = await client((res) => {
      res.on("close", () => closed());
      res.writeHead(200, { "Content-Type": "application/x-ndjson" });
      res.write(JSON.stringify({ type: "accepted" }) + "\n");
    });

    const stream = await runCodeTask(http, id, { spec: { taskId: "t_1" }, taskInput: "{}" });
    await stream.next();
    const pending = stream.next();

    await expect(stream.return()).resolves.toEqual({ done: true, value: undefined });
    await expect(pending).resolves.toEqual({ done: true, value: undefined });
    await responseClosed;
  });

  it("closes the connection and rethrows on throw()", async () => {
    let closed!: () => void;
    const responseClosed = new Promise<void>((resolve) => (closed = resolve));
    const http = await client((res) => {
      res.on("close", () => closed());
      res.writeHead(200, { "Content-Type": "application/x-ndjson" });
      res.write(JSON.stringify({ type: "accepted" }) + "\n");
    });

    const stream = await runCodeTask(http, id, { spec: { taskId: "t_1" }, taskInput: "{}" });

    await expect(stream.throw(new Error("stop"))).rejects.toThrow("stop");
    await responseClosed;
  });

  it("fails the iteration when the connection drops", async () => {
    const http = await client((res) => {
      res.writeHead(200, { "Content-Type": "application/x-ndjson" });
      res.write(JSON.stringify({ type: "accepted" }) + "\n", () => res.socket?.destroy());
    });

    const stream = await runCodeTask(http, id, { spec: { taskId: "t_1" }, taskInput: "{}" });

    await expect(stream.next()).resolves.toEqual({ done: false, value: { type: "accepted" } });
    await expect(stream.next()).rejects.toThrow();
  });

  it("fails the task when a waiting stream input fails", async () => {
    server = await startTestServer((req, res) => {
      req.resume();
      req.on("end", () => events(res, [{ type: "done" }]));
    });
    const http = new HttpClient(server.baseUrl, "key");
    // The encoder waits on the first input while the second one fails.
    const first = new PassThrough();
    first.write("x");
    const second = new PassThrough();

    const task = runCodeTask(http, id, {
      spec: { taskId: "t_1" },
      taskInput: "{}",
      inputs: [
        { path: "bin/0", content: first },
        { path: "bin/1", content: second },
      ],
    });
    second.destroy(new Error("ENOENT"));

    await expect(task).rejects.toThrow("ENOENT");
    expect(first.destroyed).toBe(true);
  });

  it.each(["{", "[]", '{"taskId":"t_1"}', '{"type":"progress"}', "null"])(
    "throws InvalidStreamEventError for the line %s",
    async (line) => {
      const http = await client((res) => {
        res.writeHead(200, { "Content-Type": "application/x-ndjson" });
        res.end(line + "\n");
      });

      const stream = await runCodeTask(http, id, { spec: { taskId: "t_1" }, taskInput: "{}" });

      await expect(collect(stream)).rejects.toBeInstanceOf(InvalidStreamEventError);
    },
  );

  it("sends the content type of each input", async () => {
    const http = await client((res) => events(res, []));

    const stream = await runCodeTask(http, id, {
      spec: { taskId: "t_1" },
      taskInput: "{}",
      inputs: [
        { path: "bin/0", content: "x", contentType: 'image/svg+xml; charset="utf-8"' },
        { path: "bin/1", content: "x" },
      ],
    });
    await collect(stream);

    const inputs = (await formData(requests[0])).getAll("input") as File[];
    expect(inputs.map((file) => file.type)).toEqual([
      "application/json",
      'image/svg+xml; charset="utf-8"',
      "application/octet-stream",
    ]);
  });

  it("refuses task.json in inputs and destroys the stream inputs", async () => {
    const http = await client((res) => events(res, []));
    const taskInput = new PassThrough();
    // The open fails after the destroy; unhandled, it would fail the run.
    const input = createReadStream(join(tmpdir(), `missing-${randomUUID()}`));

    await expect(
      runCodeTask(http, id, {
        spec: { taskId: "t_1" },
        taskInput,
        inputs: [
          { path: "bin/0", content: input },
          { path: "task.json", content: "{}" },
        ],
      }),
    ).rejects.toThrow(/taskInput/);
    expect(taskInput.destroyed).toBe(true);
    expect(input.destroyed).toBe(true);
    await new Promise((resolve) => input.on("close", resolve));
    expect(requests).toHaveLength(0);
  });

  it("destroys the stream inputs when the spec cannot be serialized", async () => {
    const http = await client((res) => events(res, []));
    const taskInput = new PassThrough();
    const input = new PassThrough();

    await expect(
      runCodeTask(http, id, {
        spec: { taskId: "t_1", limit: 1n },
        taskInput,
        inputs: [{ path: "bin/0", content: input }],
      }),
    ).rejects.toThrow(/BigInt/);
    expect(taskInput.destroyed).toBe(true);
    expect(input.destroyed).toBe(true);
    expect(requests).toHaveLength(0);
  });

  it("destroys the stream inputs when an input path is refused", async () => {
    const http = await client((res) => events(res, []));
    const input = new PassThrough();

    await expect(
      runCodeTask(http, id, {
        spec: { taskId: "t_1" },
        taskInput: "{}",
        inputs: [
          { path: "bin/0", content: input },
          { path: 'a"b', content: "x" },
        ],
      }),
    ).rejects.toThrow(/must not contain/);
    expect(input.destroyed).toBe(true);
    expect(requests).toHaveLength(0);
  });
});

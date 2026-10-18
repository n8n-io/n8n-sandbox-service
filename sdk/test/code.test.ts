import { Readable } from "node:stream";
import { afterEach, describe, expect, it } from "vitest";
import { CodeSandboxClient, CodeTaskTransportError, readNdjson } from "../src";
import { startTestServer, type TestServer } from "./helpers";

let server: TestServer | undefined;
afterEach(async () => {
  await server?.close();
  server = undefined;
});

describe("CodeSandboxClient", () => {
  it("creates a code sandbox", async () => {
    let body = "";
    server = await startTestServer((req, res) => {
      req.on("data", (c) => (body += c));
      req.on("end", () => {
        expect(req.method).toBe("POST");
        expect(req.url).toBe("/v1/code/sandboxes");
        expect(req.headers["x-api-key"]).toBe("k");
        res.writeHead(201, { "Content-Type": "application/json" });
        res.end(
          JSON.stringify({
            id: "s1",
            runtime: "n8n-js",
            runtimeVersion: "1",
            slots: 2,
            state: "ready",
          }),
        );
      });
    });
    const client = new CodeSandboxClient({ baseUrl: server.baseUrl, apiKey: "k" });
    const sb = await client.createSandbox({ slots: 2, spareWorkers: true });
    expect(sb.id).toBe("s1");
    expect(JSON.parse(body)).toEqual({ slots: 2, spareWorkers: true });
  });

  it("streams multipart inputs and reads the event stream", async () => {
    let received = "";
    server = await startTestServer((req, res) => {
      req.setEncoding("latin1");
      req.on("data", (c) => (received += c));
      req.on("end", () => {
        res.writeHead(200, { "Content-Type": "application/x-ndjson" });
        res.write('{"type":"accepted","taskId":"t1","slot":0}\n{"type":"stdout","data":"hi\\n"}\n');
        res.end(
          '{"type":"done","result":{"result":[1]},"outputs":[{"path":"result.json","size":3}]}\n',
        );
      });
    });
    const client = new CodeSandboxClient({ baseUrl: server.baseUrl });
    const events: string[] = [];
    const done = await client.runTask(
      "s1",
      { taskId: "t1", timeoutMs: 1000 },
      [
        { path: "task.json", content: '{"a":1}' },
        { path: "bin/0", content: () => Promise.resolve(Readable.from([Buffer.from("abc")])) },
      ],
      { onEvent: (e) => events.push(e.type) },
    );
    expect(done).toMatchObject({ type: "done", result: { result: [1] } });
    expect(events).toEqual(["accepted", "stdout", "done"]);
    expect(received).toContain('name="spec"');
    expect(received).toContain('{"taskId":"t1","timeoutMs":1000}');
    expect(received).toContain('filename="bin/0"');
    expect(received).toContain("abc");
  });

  it("reports a stream without a terminal event", async () => {
    server = await startTestServer((req, res) => {
      req.resume();
      req.on("end", () => {
        res.writeHead(200);
        res.end('{"type":"accepted","taskId":"t1","slot":0}\n');
      });
    });
    const client = new CodeSandboxClient({ baseUrl: server.baseUrl });
    const err = await client.runTask("s1", { taskId: "t1" }, []).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(CodeTaskTransportError);
    expect((err as CodeTaskTransportError).accepted).toBe(true);
  });

  it("answers rpc events with postRpcResult while the stream is open", async () => {
    const results: { url?: string; body: string }[] = [];
    let release!: () => void;
    const answered = new Promise<void>((r) => (release = r));
    server = await startTestServer((req, res) => {
      let body = "";
      req.on("data", (c) => (body += c));
      req.on("end", () => {
        if (req.url?.includes("/rpc/")) {
          results.push({ url: req.url, body });
          res.writeHead(204);
          res.end();
          release();
          return;
        }
        res.writeHead(200, { "Content-Type": "application/x-ndjson" });
        res.write('{"type":"accepted","taskId":"t1","slot":0}\n');
        res.write(
          '{"type":"rpc","callId":"c1","name":"helpers.httpRequest","params":[{"url":"u"}]}\n',
        );
        // The task ends only after the result arrived.
        void answered.then(() => res.end('{"type":"done","result":null}\n'));
      });
    });
    const client = new CodeSandboxClient({ baseUrl: server.baseUrl, apiKey: "k" });
    const calls: unknown[] = [];
    const done = await client.runTask("s1", { taskId: "t1" }, [], {
      onRpc: async (call) => {
        calls.push(call);
        return { ok: true, data: { status: 200 } };
      },
    });
    expect(done.type).toBe("done");
    expect(calls).toEqual([
      { type: "rpc", callId: "c1", name: "helpers.httpRequest", params: [{ url: "u" }] },
    ]);
    expect(results).toEqual([
      { url: "/v1/code/sandboxes/s1/tasks/t1/rpc/c1", body: '{"ok":true,"data":{"status":200}}' },
    ]);
  });

  it("postRpcResult accepts 204 and 404 and throws on other statuses", async () => {
    let status = 204;
    server = await startTestServer((req, res) => {
      req.resume();
      req.on("end", () => {
        res.writeHead(status, { "Content-Type": "application/json" });
        res.end(status >= 300 ? JSON.stringify({ error: "nope" }) : "");
      });
    });
    const client = new CodeSandboxClient({ baseUrl: server.baseUrl });
    await expect(
      client.postRpcResult("s1", "t1", "c1", { ok: true, data: 1 }),
    ).resolves.toBeUndefined();
    status = 404;
    await expect(
      client.postRpcResult("s1", "t1", "c1", { ok: true, data: 1 }),
    ).resolves.toBeUndefined();
    status = 500;
    await expect(
      client.postRpcResult("s1", "t1", "c1", { ok: true, data: 1 }),
    ).rejects.toMatchObject({
      status: 500,
      message: "nope",
    });
  });

  it("parses a long NDJSON line split over many chunks", async () => {
    const big = "x".repeat(200_000);
    const text = JSON.stringify({ a: big }) + "\n" + JSON.stringify({ b: 1 }) + "\n";
    const chunks: string[] = [];
    for (let i = 0; i < text.length; i += 1000) chunks.push(text.slice(i, i + 1000));
    const out: unknown[] = [];
    for await (const ev of readNdjson(Readable.from(chunks))) out.push(ev);
    expect(out).toEqual([{ a: big }, { b: 1 }]);
  });

  it("refuses an NDJSON line that is too long", async () => {
    const read = async () => {
      for await (const _ of readNdjson(Readable.from(["0123456789", "0123456789\n"]), 15)) {
        // Nothing.
      }
    };
    await expect(read()).rejects.toThrow("NDJSON line too long");
  });

  it("postRpcResult replaces a reply that JSON cannot encode", async () => {
    let body = "";
    server = await startTestServer((req, res) => {
      req.on("data", (c) => (body += c));
      req.on("end", () => {
        res.writeHead(204);
        res.end();
      });
    });
    const client = new CodeSandboxClient({ baseUrl: server.baseUrl });
    await client.postRpcResult("s1", "t1", "c1", { ok: true, data: { n: 1n } });
    expect(JSON.parse(body)).toMatchObject({ ok: false });
    expect(JSON.parse(body).error.message).toContain("cannot be serialized");
  });

  it("parses NDJSON split over chunks", async () => {
    const out: unknown[] = [];
    for await (const ev of readNdjson(Readable.from(['{"a":', '1}\n{"b"', ":2}"]))) out.push(ev);
    expect(out).toEqual([{ a: 1 }, { b: 2 }]);
  });
});

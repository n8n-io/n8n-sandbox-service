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

  it("parses NDJSON split over chunks", async () => {
    const out: unknown[] = [];
    for await (const ev of readNdjson(Readable.from(['{"a":', '1}\n{"b"', ":2}"]))) out.push(ev);
    expect(out).toEqual([{ a: 1 }, { b: 2 }]);
  });
});

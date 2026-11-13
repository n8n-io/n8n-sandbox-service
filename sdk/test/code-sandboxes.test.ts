import { afterEach, describe, expect, it } from "vitest";
import { createCodeSandbox } from "../src/code-sandboxes.js";
import { HttpClient } from "../src/http.js";
import type { CreateCodeSandboxOptions } from "../src/types.js";
import { startTestServer, type TestServer } from "./helpers.js";

const options: CreateCodeSandboxOptions = {
  name: "scope-1",
  runtime: "n8n-js",
  runtimeVersion: "2.42.0",
  slots: 2,
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

import { afterEach, describe, expect, it } from "vitest";
import { HttpClient } from "../src/http.js";
import { createSandbox } from "../src/sandboxes.js";
import type { RetryOptions } from "../src/types.js";
import { startTestServer, type TestServer } from "./helpers.js";

const created = {
  id: "11111111-1111-4111-8111-111111111111",
  status: "running",
  created_at: 1000,
  last_active_at: 1000,
  ephemeral: false,
  egress: "public",
};
const noCapacity = { error: "sandbox runners are at capacity", code: 503, reason: "no_capacity" };

type Answer = { status: number; body: unknown };

describe("createSandbox when no runner has capacity", () => {
  let server: TestServer | undefined;
  let bodies: unknown[] = [];

  afterEach(async () => {
    await server?.close();
    server = undefined;
    bodies = [];
  });

  /** Serves POST /sandboxes, answering the nth request (1-based) with answer(n). */
  async function client(answer: (n: number) => Answer, retry: RetryOptions): Promise<HttpClient> {
    server = await startTestServer((req, res) => {
      let raw = "";
      req.on("data", (chunk) => (raw += chunk));
      req.on("end", () => {
        bodies.push(JSON.parse(raw));
        const { status, body } = answer(bodies.length);
        res.writeHead(status, { "Content-Type": "application/json" });
        res.end(JSON.stringify(body));
      });
    });
    return new HttpClient(server.baseUrl, "key", { baseDelayMs: 0, ...retry });
  }

  it("repeats an anonymous create, since the 503 says nothing was created", async () => {
    const http = await client(
      (n) => (n <= 2 ? { status: 503, body: noCapacity } : { status: 201, body: created }),
      {},
    );

    await expect(createSandbox(http, { egress: "public" })).resolves.toMatchObject({
      id: created.id,
    });
    expect(bodies).toEqual([{ egress: "public" }, { egress: "public" }, { egress: "public" }]);
  });

  it("does not repeat an anonymous create on any other 503", async () => {
    const http = await client(() => ({ status: 503, body: { error: "runner unavailable" } }), {});

    await expect(createSandbox(http, { egress: "public" })).rejects.toMatchObject({
      status: 503,
      reason: undefined,
    });
    expect(bodies).toHaveLength(1);
  });

  it("gives up when the retry policy does", async () => {
    const http = await client(() => ({ status: 503, body: noCapacity }), { attempts: 2 });

    await expect(createSandbox(http, { egress: "public" })).rejects.toMatchObject({
      status: 503,
      reason: "no_capacity",
    });
    expect(bodies).toHaveLength(3);
  });

  it("does not repeat it when the policy leaves out 503", async () => {
    const http = await client(() => ({ status: 503, body: noCapacity }), {
      retryOnStatuses: [429],
    });

    await expect(createSandbox(http, { egress: "public" })).rejects.toMatchObject({ status: 503 });
    expect(bodies).toHaveLength(1);
  });

  // A create with an id is idempotent, so the client's own retry already covers it;
  // a second loop around that one would multiply the attempts.
  it("retries a create with an id under the policy once, not twice over", async () => {
    const http = await client(() => ({ status: 503, body: noCapacity }), { attempts: 2 });

    await expect(createSandbox(http, { id: created.id, egress: "public" })).rejects.toMatchObject({
      status: 503,
      reason: "no_capacity",
    });
    expect(bodies).toHaveLength(3);
  });
});

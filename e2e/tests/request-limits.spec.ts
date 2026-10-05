import { expect, test } from '@playwright/test';
import { BASE_URL, createSandbox, deleteSandbox, execWithTransientRetry, getApiKey } from './helpers';

test.describe.configure({ timeout: 120_000 });

// The runner defaults, which the e2e stacks leave unset:
// SANDBOX_RUNNER_MAX_INFLIGHT_PER_SANDBOX and SANDBOX_RUNNER_MAX_EXEC_TIMEOUT.
const PER_SANDBOX_LIMIT = 16;
const MAX_EXEC_TIMEOUT_MS = 900_000;

async function postExec(id: string, body: object, signal?: AbortSignal): Promise<Response> {
  return fetch(`${BASE_URL}/sandboxes/${id}/executions`, {
    method: 'POST',
    headers: { 'X-Api-Key': await getApiKey(), 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
    signal,
  });
}

// Waits until the sandbox runs a command. The body is read to its end, and the
// API and runner release a request's slot before they end its response, so no
// request of this one holds a slot once it returns.
async function waitUntilRunsCommands(id: string): Promise<void> {
  const deadline = Date.now() + 12_000;
  for (;;) {
    const res = await postExec(id, { command: 'true', timeout_ms: 5_000 });
    const body = await res.text();
    if (res.ok && body.includes('"type":"exit"')) return;
    if (Date.now() > deadline) throw new Error(`sandbox ${id} did not run a command: ${res.status} ${body}`);
    await new Promise((resolve) => setTimeout(resolve, 200));
  }
}

// Raw fetch rather than the SDK, which retries a 429 and would hide it.
test.describe('request limits', () => {
  test('a sandbox refuses requests beyond its limit on requests in progress', async () => {
    const id = await createSandbox();
    const controllers: AbortController[] = [];
    try {
      await waitUntilRunsCommands(id);

      // fetch resolves on the response headers, so each 200 is a stream that is
      // still open, holding its slot, when the others arrive.
      const responses = await Promise.all(
        Array.from({ length: PER_SANDBOX_LIMIT + 1 }, () => {
          const controller = new AbortController();
          controllers.push(controller);
          return postExec(id, { command: 'sleep 20', timeout_ms: 25_000 }, controller.signal);
        }),
      );

      const statuses = responses.map((r) => r.status);
      expect(statuses.filter((s) => s === 200), `statuses: ${statuses.join(',')}`).toHaveLength(PER_SANDBOX_LIMIT);
      expect(statuses.filter((s) => s === 429), `statuses: ${statuses.join(',')}`).toHaveLength(1);
      const refused = responses.find((r) => r.status === 429);
      expect(refused!.headers.get('retry-after')).toBe('1');
      expect(await refused!.json()).toMatchObject({ reason: 'sandbox_request_limit' });
    } finally {
      for (const controller of controllers) controller.abort();
      await deleteSandbox(id);
    }
  });

  test('an exec asking for more than the maximum timeout is refused', async () => {
    const id = await createSandbox();
    try {
      const res = await postExec(id, { command: 'true', timeout_ms: MAX_EXEC_TIMEOUT_MS + 1 });
      expect(res.status).toBe(400);
      expect(await res.text()).toContain(String(MAX_EXEC_TIMEOUT_MS));

      const atMax = await execWithTransientRetry(id, 'true', { timeoutMs: MAX_EXEC_TIMEOUT_MS });
      expect(atMax.exitCode).toBe(0);
    } finally {
      await deleteSandbox(id);
    }
  });
});

import { test, expect } from '@playwright/test';
import { createSandbox, deleteSandbox } from './helpers';
import { parseCounter } from './metrics-helpers';

// e2e/run.sh sets SANDBOX_API_METRICS_ENABLED=true on the API container, so
// /metrics is mounted and bypasses X-Api-Key. These tests exercise the live
// endpoint against the dockerized API.

test.describe('API metrics endpoint', () => {
  test('is served without X-Api-Key and returns expected families', async ({ request }) => {
    const resp = await request.get('/metrics');
    expect(resp.status()).toBe(200);

    const body = await resp.text();
    for (const want of [
      'sandbox_http_requests_total',
      'sandbox_http_request_duration_seconds',
      'sandbox_sandbox_operations_total',
      'sandbox_sandboxes_active',
      'sandbox_runners_registered',
      'go_goroutines',
      'process_start_time_seconds',
      'role="api"',
    ]) {
      expect(body, `missing ${want} in /metrics body`).toContain(want);
    }
  });

  test('records sandbox lifecycle operations', async ({ request }) => {
    const before = parseCounter(
      await scrape(request),
      'sandbox_sandbox_operations_total',
      { role: 'api', operation: 'create', result: 'success' },
    );

    const id = await createSandbox();
    await deleteSandbox(id);

    // Poll briefly — observation runs inside the handler's defer; the increment
    // is committed by the time the response returns, but scrape gathering is
    // independent so we allow a couple of retries for clock/cache jitter.
    let after = before;
    for (let i = 0; i < 5; i++) {
      after = parseCounter(
        await scrape(request),
        'sandbox_sandbox_operations_total',
        { role: 'api', operation: 'create', result: 'success' },
      );
      if (after > before) break;
      await new Promise((r) => setTimeout(r, 200));
    }
    expect(after).toBeGreaterThan(before);

    const deletes = parseCounter(
      await scrape(request),
      'sandbox_sandbox_operations_total',
      { role: 'api', operation: 'delete', result: 'success' },
    );
    expect(deletes).toBeGreaterThan(0);
  });
});

async function scrape(request: import('@playwright/test').APIRequestContext): Promise<string> {
  const resp = await request.get('/metrics');
  expect(resp.status()).toBe(200);
  return resp.text();
}

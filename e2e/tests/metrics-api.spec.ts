import { test, expect } from '@playwright/test';
import { createSandbox, deleteSandbox } from './helpers';
import { parseCounter } from './metrics-helpers';

// e2e/run.sh sets SANDBOX_API_METRICS_ENABLED=true on the API container, so
// /metrics is mounted and bypasses X-Api-Key. These tests exercise the live
// endpoint against the dockerized API.

test.describe('API metrics endpoint', { tag: ['@docker-runner', '@firecracker-runner'] }, () => {
  test('is served without X-Api-Key and returns expected families', async ({ request }) => {
    const resp = await request.get('/metrics');
    expect(resp.status()).toBe(200);

    const body = await resp.text();
    const expected = [
      'sandbox_http_requests_total',
      'sandbox_http_request_duration_seconds',
      'sandbox_sandbox_operations_total',
      'sandbox_sandboxes_active',
      'sandbox_runners_registered',
      'go_goroutines',
      'process_start_time_seconds',
      'role="api"',
    ];
    for (const name of expected) {
      expect(body).toContain(name);
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

    // The recorder's Inc() runs inside the handler's defer, which fires
    // before the response is sent. By the time createSandbox/deleteSandbox
    // resolve on the SDK, the counters have been incremented and the next
    // scrape will reflect them.
    const body = await scrape(request);
    const createsAfter = parseCounter(body, 'sandbox_sandbox_operations_total', {
      role: 'api',
      operation: 'create',
      result: 'success',
    });
    const deletesAfter = parseCounter(body, 'sandbox_sandbox_operations_total', {
      role: 'api',
      operation: 'delete',
      result: 'success',
    });
    expect(createsAfter).toBeGreaterThan(before);
    expect(deletesAfter).toBeGreaterThan(0);
  });
});

async function scrape(request: import('@playwright/test').APIRequestContext): Promise<string> {
  const resp = await request.get('/metrics');
  expect(resp.status()).toBe(200);
  return resp.text();
}

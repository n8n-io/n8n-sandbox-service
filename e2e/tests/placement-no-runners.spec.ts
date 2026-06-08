import { test, expect } from '@playwright/test';
import { apiRequest } from './helpers';

// Run only from e2e/run-no-runner.sh (API has no registered runners).

test('POST /sandboxes returns 503 with understandable error', { tag: '@docker-runner' }, async ({ request }) => {
  const resp = await apiRequest(request, 'POST', '/sandboxes', { data: {} });
  expect(resp.status).toBe(503);
  const body = (await resp.json()) as { error?: string };
  expect(body.error).toBeTruthy();
  expect(body.error).toMatch(/no sandbox runners/i);
});

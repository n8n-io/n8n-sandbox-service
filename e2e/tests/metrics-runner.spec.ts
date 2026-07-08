import { test, expect } from '@playwright/test';
import { createSandbox, deleteSandbox, scrapeRunnerMetrics } from './helpers';
import { parseCounter } from './metrics-helpers';
test.describe('Runner metrics endpoint', () => {
  test('is served without X-Api-Key and returns expected families', async () => {
    const body = scrapeRunnerMetrics();
    const expected = [
      'sandbox_http_requests_total',
      'sandbox_http_request_duration_seconds',
      'sandbox_containers_active',
      'go_goroutines',
      'process_start_time_seconds',
      'role="runner"',
    ];
    for (const name of expected) {
      expect(body).toContain(name);
    }
  });

  test('records container lifecycle operations', async () => {
    test.setTimeout(60_000);
    const containerOps = 'sandbox_container_operations_total';
    const createLabels = { role: 'runner', operation: 'create', result: 'success' };
    const deleteLabels = { role: 'runner', operation: 'delete', result: 'success' };

    const beforeBody = scrapeRunnerMetrics();
    const createsBefore = parseCounter(beforeBody, containerOps, createLabels);
    const deletesBefore = parseCounter(beforeBody, containerOps, deleteLabels);

    const id = await createSandbox();
    await deleteSandbox(id);

    const afterBody = scrapeRunnerMetrics();
    const createsAfter = parseCounter(afterBody, containerOps, createLabels);
    const deletesAfter = parseCounter(afterBody, containerOps, deleteLabels);

    expect(createsAfter).toBeGreaterThan(createsBefore);
    expect(deletesAfter).toBeGreaterThan(deletesBefore);
  });
});

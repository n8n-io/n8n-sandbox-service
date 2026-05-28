import { test, expect } from '@playwright/test';
import { execFileSync } from 'node:child_process';
import { createSandbox, deleteSandbox } from './helpers';
import { parseCounter } from './metrics-helpers';

// e2e/run.sh sets SANDBOX_RUNNER_METRICS_ENABLED=true on the runner container.
// The runner is not host-exposed, so these tests scrape /metrics from inside
// the runner container via `docker exec`.

function scrapeRunnerMetrics(runnerContainer: string): string {
  return execFileSync(
    'docker',
    ['exec', runnerContainer, 'wget', '-q', '-O', '-', 'http://localhost:8080/metrics'],
    { encoding: 'utf8' },
  );
}

test.describe('Runner metrics endpoint', () => {
  test('is served without X-Api-Key and returns expected families', async () => {
    test.skip(!process.env.E2E_RUNNER_CONTAINER_NAME, 'needs E2E_RUNNER_CONTAINER_NAME (from e2e/run.sh)');
    const runnerContainer = process.env.E2E_RUNNER_CONTAINER_NAME!;

    const body = scrapeRunnerMetrics(runnerContainer);
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
    test.skip(!process.env.E2E_RUNNER_CONTAINER_NAME, 'needs E2E_RUNNER_CONTAINER_NAME (from e2e/run.sh)');
    test.setTimeout(60_000);
    const runnerContainer = process.env.E2E_RUNNER_CONTAINER_NAME!;
    const containerOps = 'sandbox_container_operations_total';
    const createLabels = { role: 'runner', operation: 'create', result: 'success' };
    const deleteLabels = { role: 'runner', operation: 'delete', result: 'success' };

    const beforeBody = scrapeRunnerMetrics(runnerContainer);
    const createsBefore = parseCounter(beforeBody, containerOps, createLabels);
    const deletesBefore = parseCounter(beforeBody, containerOps, deleteLabels);

    const id = await createSandbox();
    await deleteSandbox(id);

    const afterBody = scrapeRunnerMetrics(runnerContainer);
    const createsAfter = parseCounter(afterBody, containerOps, createLabels);
    const deletesAfter = parseCounter(afterBody, containerOps, deleteLabels);

    expect(createsAfter).toBeGreaterThan(createsBefore);
    expect(deletesAfter).toBeGreaterThan(deletesBefore);
  });
});

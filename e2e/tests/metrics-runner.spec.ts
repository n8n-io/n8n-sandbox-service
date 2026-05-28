import { test, expect } from '@playwright/test';
import { execFileSync } from 'node:child_process';
import { createSandbox, deleteSandbox } from './helpers';

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
    for (const want of [
      'sandbox_http_requests_total',
      'sandbox_http_request_duration_seconds',
      'sandbox_containers_active',
      'go_goroutines',
      'process_start_time_seconds',
      'role="runner"',
    ]) {
      expect(body, `missing ${want} in /metrics body`).toContain(want);
    }
  });

  test('records container lifecycle operations', async () => {
    test.skip(!process.env.E2E_RUNNER_CONTAINER_NAME, 'needs E2E_RUNNER_CONTAINER_NAME (from e2e/run.sh)');
    test.setTimeout(60_000);
    const runnerContainer = process.env.E2E_RUNNER_CONTAINER_NAME!;

    const before = parseCounter(
      scrapeRunnerMetrics(runnerContainer),
      'sandbox_container_operations_total',
      { role: 'runner', operation: 'create', result: 'success' },
    );

    const id = await createSandbox();
    await deleteSandbox(id);

    // The runner observes the op in the gRPC handler that the API hit; by the
    // time deleteSandbox returns, the observation is committed. Poll briefly
    // to absorb scrape-gather jitter.
    let createsAfter = before;
    for (let i = 0; i < 10; i++) {
      createsAfter = parseCounter(
        scrapeRunnerMetrics(runnerContainer),
        'sandbox_container_operations_total',
        { role: 'runner', operation: 'create', result: 'success' },
      );
      if (createsAfter > before) break;
      await new Promise((r) => setTimeout(r, 300));
    }
    expect(createsAfter).toBeGreaterThan(before);

    const deletes = parseCounter(
      scrapeRunnerMetrics(runnerContainer),
      'sandbox_container_operations_total',
      { role: 'runner', operation: 'delete', result: 'success' },
    );
    expect(deletes).toBeGreaterThan(0);
  });
});

// parseCounter returns the value of a single Prometheus counter sample matching
// every label in `labels`. Returns 0 if no series matches.
function parseCounter(body: string, name: string, labels: Record<string, string>): number {
  for (const raw of body.split('\n')) {
    const line = raw.trim();
    if (!line || line.startsWith('#')) continue;
    if (!line.startsWith(name)) continue;
    const open = line.indexOf('{');
    const close = line.indexOf('}');
    if (open < 0 || close < 0) continue;
    const seriesLabels = parseLabels(line.slice(open + 1, close));
    if (!matches(seriesLabels, labels)) continue;
    const value = Number(line.slice(close + 1).trim().split(/\s+/)[0]);
    if (Number.isFinite(value)) return value;
  }
  return 0;
}

function parseLabels(s: string): Record<string, string> {
  const out: Record<string, string> = {};
  const re = /(\w+)="([^"]*)"/g;
  let m: RegExpExecArray | null;
  while ((m = re.exec(s)) !== null) {
    out[m[1]] = m[2];
  }
  return out;
}

function matches(series: Record<string, string>, want: Record<string, string>): boolean {
  for (const [k, v] of Object.entries(want)) {
    if (series[k] !== v) return false;
  }
  return true;
}

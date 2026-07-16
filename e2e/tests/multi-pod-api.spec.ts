import { test, expect, request as playwrightRequest } from '@playwright/test';
import { execFileSync } from 'node:child_process';
import './matchers';
import {
  execWithTransientRetry,
  deleteSandbox,
  ensureTenantAuth,
  getApiKey,
  sandboxClient,
  waitForSandboxStatus,
} from './helpers';
import { parseGauge } from './metrics-helpers';

const BASE_URL_A = process.env.BASE_URL_A || 'http://localhost:18092';
const BASE_URL_B = process.env.BASE_URL_B || 'http://localhost:18093';
const POD_A = process.env.E2E_API_POD_A_CONTAINER || '';

// Run from e2e/run-postgres-multi-pod.sh (two API pods, one Postgres, Docker runner).

const multiPodHarness = process.env.E2E_MULTI_POD === '1';
const multiPodFailover = process.env.E2E_MULTI_POD_FAILOVER === '1';

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

function docker(args: string[]): void {
  execFileSync('docker', args, { stdio: 'inherit' });
}

async function fetchMetrics(baseUrl: string): Promise<string> {
  const resp = await fetch(`${baseUrl}/metrics`);
  expect(resp.status).toBe(200);
  return resp.text();
}

async function waitForRegisteredRunners(
  baseUrl: string,
  min: number,
  timeoutMs: number,
): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const body = await fetchMetrics(baseUrl);
    const n = parseGauge(body, 'sandbox_runners_registered');
    if (n >= min) return;
    await sleep(500);
  }
  const body = await fetchMetrics(baseUrl);
  throw new Error(
    `expected sandbox_runners_registered >= ${min} on ${baseUrl}, got ${parseGauge(body, 'sandbox_runners_registered')}`,
  );
}

async function waitForApiHttp(baseUrl: string, timeoutMs: number): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    try {
      const resp = await fetch(`${baseUrl}/healthz`);
      if (resp.ok) return;
    } catch {
      /* not listening yet */
    }
    await sleep(500);
  }
  throw new Error(`API at ${baseUrl} did not become healthy within ${timeoutMs}ms`);
}

test.describe('multi-pod API (Postgres)', () => {
  test.beforeEach(() => {
    test.skip(!multiPodHarness, 'requires e2e/run-postgres-multi-pod.sh (E2E_MULTI_POD=1)');
  });

  test('runner heartbeats on pod A; create and exec on pod B', async () => {
    await ensureTenantAuth(BASE_URL_A);
    const key = await getApiKey();
    const clientB = sandboxClient(BASE_URL_B, key);

    const record = await clientB.createSandbox();
    try {
      const result = await execWithTransientRetry(record.id, 'echo multi-pod', undefined, clientB);
      expect(result.stdout).toBe('multi-pod\n');
      expect(result).toHaveSucceeded();
    } finally {
      await deleteSandbox(record.id, clientB);
    }
  });

  test('sandbox created on pod B is visible on pod A', async () => {
    await ensureTenantAuth(BASE_URL_A);
    const key = await getApiKey();
    const clientA = sandboxClient(BASE_URL_A, key);
    const clientB = sandboxClient(BASE_URL_B, key);

    const record = await clientB.createSandbox();
    try {
      const got = await clientA.getSandbox(record.id);
      expect(got.id).toBe(record.id);
      expect(got.status).toBe('running');
    } finally {
      await deleteSandbox(record.id, clientB);
    }
  });
});

test.describe.serial('multi-pod API failover @multi-pod-failover', () => {
  test.beforeEach(() => {
    test.skip(
      !multiPodHarness || !multiPodFailover,
      'requires e2e/run-postgres-multi-pod.sh failover phase (E2E_MULTI_POD_FAILOVER=1)',
    );
    test.skip(!POD_A, 'needs E2E_API_POD_A_CONTAINER');
  });

  test('lead API pod death: runner reconnects and pod B keeps working', async () => {
    test.setTimeout(120_000);

    await waitForRegisteredRunners(BASE_URL_A, 1, 30_000);
    await waitForRegisteredRunners(BASE_URL_B, 1, 30_000);

    docker(['stop', '-t', '10', POD_A]);

    await waitForRegisteredRunners(BASE_URL_B, 1, 45_000);

    // Pod A is down; mint against the surviving pod (shared Postgres).
    await ensureTenantAuth(BASE_URL_B);
    const clientB = sandboxClient(BASE_URL_B, await getApiKey());
    const record = await clientB.createSandbox();
    try {
      const result = await execWithTransientRetry(record.id, 'echo failover', undefined, clientB);
      expect(result.stdout).toBe('failover\n');
      expect(result).toHaveSucceeded();
    } finally {
      await deleteSandbox(record.id, clientB);
    }
  });

  test('lead API pod death: idle sweeper continues on surviving pod', async () => {
    test.setTimeout(120_000);

    await ensureTenantAuth(BASE_URL_B);
    const key = await getApiKey();
    const clientB = sandboxClient(BASE_URL_B, key);
    const record = await clientB.createSandbox();

    const reqB = await playwrightRequest.newContext({
      baseURL: BASE_URL_B,
      extraHTTPHeaders: { 'X-Api-Key': key },
    });
    try {
      await waitForSandboxStatus(reqB, record.id, 'stopped', 90_000);
    } finally {
      await reqB.dispose();
      await deleteSandbox(record.id, clientB).catch(() => {});
    }
  });

  test.afterAll(async () => {
    if (!POD_A) return;
    try {
      docker(['start', POD_A]);
      await waitForApiHttp(BASE_URL_A, 60_000);
    } catch {
      /* best-effort restore for harness cleanup */
    }
  });
});

import { APIRequestContext, expect } from '@playwright/test';
import * as http from 'node:http';
import * as https from 'node:https';
import { SandboxClient, type ExecResult } from '@n8n/sandbox-client';
import { execFileSync } from 'node:child_process';

/** Admin key from env (SANDBOX_API_KEYS). Used to mint tenant keys. */
export const ADMIN_API_KEY = process.env.SANDBOX_API_KEY || 'test';
const BASE_URL = process.env.BASE_URL || process.env.BASE_URL_A || 'http://localhost:8080';

let tenantApiKey: string | null = null;
let tenantMintPromise: Promise<string> | null = null;

async function mintTenantApiKey(mintBaseUrl: string = BASE_URL): Promise<string> {
  const res = await fetch(`${mintBaseUrl}/admin/tenants`, {
    method: 'POST',
    headers: {
      'X-Api-Key': ADMIN_API_KEY,
      'Content-Type': 'application/json',
    },
    body: JSON.stringify({ name: `e2e-${process.pid}-${Date.now()}` }),
  });
  if (!res.ok) {
    throw new Error(`mint tenant API key failed: ${res.status} ${await res.text()}`);
  }
  const body = (await res.json()) as {
    tenant: { id: string };
    key: { api_key: string };
  };
  tenantApiKey = body.key.api_key;
  client = new SandboxClient({ baseUrl: mintBaseUrl, apiKey: tenantApiKey });
  return tenantApiKey;
}

/** Resolves the tenant API key used for sandbox traffic (mints once per process). */
export async function getApiKey(mintBaseUrl?: string): Promise<string> {
  if (tenantApiKey) return tenantApiKey;
  if (!tenantMintPromise) {
    tenantMintPromise = mintTenantApiKey(mintBaseUrl ?? BASE_URL).catch((err) => {
      tenantMintPromise = null;
      throw err;
    });
  }
  return tenantMintPromise;
}

export async function ensureTenantAuth(mintBaseUrl?: string): Promise<void> {
  await getApiKey(mintBaseUrl);
}

/** Tenant-scoped client (initialized after ensureTenantAuth / first helper call). */
export let client = new SandboxClient({ baseUrl: BASE_URL, apiKey: ADMIN_API_KEY });

export function sandboxClient(baseUrl: string = BASE_URL, apiKey?: string): SandboxClient {
  return new SandboxClient({ baseUrl, apiKey: apiKey ?? tenantApiKey ?? ADMIN_API_KEY });
}

export type { ExecResult };

async function headers(extra?: Record<string, string>): Promise<Record<string, string>> {
  return { 'X-Api-Key': await getApiKey(), ...extra };
}

export async function createSandbox(): Promise<string> {
  await ensureTenantAuth();
  const record = await client.createSandbox();
  return record.id;
}

function isTransientCreateError(err: unknown): boolean {
  const status = (err as { status?: number })?.status;
  if (status === 503 || status === 500) {
    return true;
  }
  const msg = String((err as Error)?.message || '').toLowerCase();
  return (
    msg.includes('timeout waiting for daemon') ||
    msg.includes('connect to daemon') ||
    msg.includes('daemon temporarily unavailable') ||
    msg.includes('runner unavailable') ||
    msg.includes('internal server error')
  );
}

export async function createSandboxWithRetry(maxAttempts = 5): Promise<string> {
  let lastErr: unknown;
  for (let i = 0; i < maxAttempts; i++) {
    try {
      return await createSandbox();
    } catch (err) {
      lastErr = err;
      const retry = isTransientCreateError(err) && i < maxAttempts - 1;
      if (!retry) {
        throw err;
      }
      await new Promise((r) => setTimeout(r, 2500));
    }
  }
  throw lastErr instanceof Error ? lastErr : new Error(String(lastErr));
}

export async function deleteSandbox(id: string, c: SandboxClient = client): Promise<void> {
  await ensureTenantAuth();
  await c.deleteSandbox(id);
}

export async function exec(
  id: string,
  command: string,
  opts?: { env?: Record<string, string>; workdir?: string; timeoutMs?: number },
): Promise<ExecResult> {
  await ensureTenantAuth();
  return client.exec(id, {
    command,
    env: opts?.env,
    workdir: opts?.workdir,
    timeoutMs: opts?.timeoutMs,
  });
}

function isTransientExecError(err: unknown): boolean {
  const status = (err as { status?: number })?.status;
  if (status === 503) {
    return true;
  }
  const msg = String((err as Error)?.message || '').toLowerCase();
  return (
    msg.includes('internal server error') ||
    msg.includes('daemon temporarily unavailable') ||
    msg.includes('runner unavailable') ||
    msg.includes('sandbox temporarily unavailable') ||
    msg.includes('sandbox exec stream ended without an exit event')
  );
}

export async function execWithTransientRetry(
  id: string,
  command: string,
  opts?: { env?: Record<string, string>; workdir?: string; timeoutMs?: number; retryWindowMs?: number },
  c?: SandboxClient,
): Promise<ExecResult> {
  await ensureTenantAuth();
  const active = c ?? client;
  const deadlineMs = opts?.retryWindowMs ?? 12_000;
  const deadline = Date.now() + deadlineMs;
  let lastErr: unknown;
  while (Date.now() < deadline) {
    try {
      return await active.exec(id, {
        command,
        env: opts?.env,
        workdir: opts?.workdir,
        timeoutMs: opts?.timeoutMs,
      });
    } catch (err) {
      lastErr = err;
      if (!isTransientExecError(err)) {
        throw err;
      }
      await new Promise((r) => setTimeout(r, 200));
    }
  }
  throw new Error(`exec did not recover within ${deadlineMs}ms: ${String(lastErr)}`);
}

export async function uploadFile(
  id: string,
  path: string,
  content: string | Buffer,
): Promise<void> {
  await ensureTenantAuth();
  const filePath = path.startsWith('/') ? path : `/${path}`;
  await client.writeFile(id, filePath, content);
}

export async function downloadFile(
  id: string,
  path: string,
): Promise<string> {
  await ensureTenantAuth();
  const filePath = path.startsWith('/') ? path : `/${path}`;
  const buf = await client.readFile(id, filePath);
  return buf.toString('utf-8');
}

/**
 * Starts an exec via a streaming HTTP request, reads until the started event
 * arrives, then destroys the response (simulating a client disconnect).
 * Returns the exec_id from the started event.
 */
export async function startAndDisconnect(sandboxId: string, command: string): Promise<string> {
  const apiKey = await getApiKey();
  return new Promise((resolve, reject) => {
    const url = new URL(`${BASE_URL}/sandboxes/${sandboxId}/executions`);
    const reqFn = url.protocol === 'https:' ? https.request : http.request;
    const body = JSON.stringify({ command });
    let resolved = false;

    const req = reqFn(
      url,
      {
        method: 'POST',
        headers: {
          'X-Api-Key': apiKey,
          'Content-Type': 'application/json',
          'Content-Length': Buffer.byteLength(body).toString(),
        },
      },
      (res) => {
        let buffer = '';
        const onData = (chunk: Buffer) => {
          buffer += chunk.toString('utf-8');
          let idx = buffer.indexOf('\n');
          while (idx !== -1) {
            const line = buffer.slice(0, idx).trim();
            buffer = buffer.slice(idx + 1);
            if (line.length > 0) {
              const event = JSON.parse(line);
              if (event.type === 'started' && event.exec_id) {
                resolved = true;
                res.removeListener('data', onData);
                res.destroy();
                resolve(event.exec_id as string);
                return;
              }
            }
            idx = buffer.indexOf('\n');
          }
        };
        res.on('data', onData);
        res.on('end', () => {
          if (!resolved) reject(new Error('stream ended without started event'));
        });
        res.on('error', (err) => {
          if (!resolved) {
            console.error('startAndDisconnect stream error:', err);
            reject(err);
          }
        });
      },
    );

    req.on('error', reject);
    req.write(body);
    req.end();
  });
}

export async function apiRequest(
  request: APIRequestContext,
  method: string,
  path: string,
  opts?: { data?: unknown; rawHeaders?: Record<string, string> },
): Promise<{ status: number; body: string; json: () => Promise<unknown> }> {
  const h = opts?.rawHeaders ?? (await headers({ 'Content-Type': 'application/json' }));
  const resp = await request.fetch(path, {
    method,
    headers: h,
    data: opts?.data,
  });
  const body = await resp.text();
  return {
    status: resp.status(),
    body,
    json: () => Promise.resolve(JSON.parse(body)),
  };
}

/** Poll GET /sandboxes/{id} until status matches (GET does not bump last_active_at). */
export async function waitForSandboxStatus(
  request: APIRequestContext,
  id: string,
  status: string,
  timeoutMs = 90_000,
): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const res = await apiRequest(request, 'GET', `/sandboxes/${id}`);
    if (res.status === 200) {
      const j = (await res.json()) as { status?: string };
      if (j.status === status) return;
    }
    await new Promise((r) => setTimeout(r, 500));
  }
  const last = await apiRequest(request, 'GET', `/sandboxes/${id}`);
  if (last.status === 404) {
    throw new Error(`sandbox ${id} was deleted before reaching status ${status}`);
  }
  const j = (await last.json()) as { status?: string };
  expect(j.status).toBe(status);
}

/** Poll until sandbox row is gone (404). */
export async function waitForSandbox404(
  request: APIRequestContext,
  id: string,
  timeoutMs = 90_000,
): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const res = await apiRequest(request, 'GET', `/sandboxes/${id}`);
    if (res.status === 404) return;
    await new Promise((r) => setTimeout(r, 500));
  }
  const last = await apiRequest(request, 'GET', `/sandboxes/${id}`);
  expect(last.status).toBe(404);
}

export function docker(args: string[]): string {
  return execFileSync('docker', args, {
    stdio: ['pipe', 'pipe', 'pipe'],
    encoding: 'utf8',
  });
}

export function dockerOutput(args: string[]): string {
  try {
    return execFileSync('docker', args, {
      stdio: ['pipe', 'pipe', 'pipe'],
      encoding: 'utf8',
    });
  } catch (err: unknown) {
    const e = err as { stdout?: unknown; stderr?: unknown };
    const stdout = e.stdout ? String(e.stdout) : '';
    const stderr = e.stderr ? String(e.stderr) : '';
    return `${stdout}\n${stderr}`.trim();
  }
}

function e2eProjectDir(): string {
  return process.env.E2E_PROJECT_DIR || `${process.cwd()}/..`;
}

export function stopSandboxViaRunner(sandboxId: string): void {
  if (!process.env.E2E_RUNNER_CONTROL_GRPC_ADDR || !process.env.E2E_RUNNER_API_KEY) {
    throw new Error('E2E_RUNNER_CONTROL_GRPC_ADDR and E2E_RUNNER_API_KEY must be set');
  }
  execFileSync('go', ['run', './cmd/e2e-runnerctl', 'stop', sandboxId], {
    cwd: e2eProjectDir(),
    stdio: ['pipe', 'pipe', 'pipe'],
    encoding: 'utf8',
    env: { ...process.env },
  });
}

export function scrapeRunnerMetricsAt(addr: string): string {
  const host = addr.replace(/^https?:\/\//, '');
  return execFileSync('curl', ['-sf', `http://${host}/metrics`], { encoding: 'utf8' });
}

export function scrapeRunnerMetrics(): string {
  if (process.env.E2E_RUNNER_HTTP_ADDR) {
    return scrapeRunnerMetricsAt(process.env.E2E_RUNNER_HTTP_ADDR);
  }
  const runnerContainer = process.env.E2E_RUNNER_CONTAINER_NAME;
  if (!runnerContainer) {
    throw new Error('need E2E_RUNNER_HTTP_ADDR or E2E_RUNNER_CONTAINER_NAME for runner metrics');
  }
  return execFileSync(
    'docker',
    ['exec', runnerContainer, 'wget', '-q', '-O', '-', 'http://localhost:8080/metrics'],
    { encoding: 'utf8' },
  );
}

export async function waitRunnerHttpReady(addr: string, deadlineMs = 75_000): Promise<void> {
  const url = `http://${addr.replace(/^https?:\/\//, '')}/readyz`;
  const deadline = Date.now() + deadlineMs;
  while (Date.now() < deadline) {
    try {
      execFileSync('curl', ['-sf', url], { stdio: 'pipe' });
      return;
    } catch {
      await new Promise((r) => setTimeout(r, 250));
    }
  }
  throw new Error(`runner not ready at ${url} within ${deadlineMs}ms`);
}

export function restartRunnerForE2E(): void {
  if (process.env.E2E_RUNNER_CONTAINER_NAME) {
    execFileSync('docker', ['restart', process.env.E2E_RUNNER_CONTAINER_NAME], { stdio: 'inherit' });
    return;
  }
  const envFile = process.env.E2E_FIRECRACKER_RUNNER_ENV_FILE;
  if (!envFile) {
    throw new Error('need E2E_RUNNER_CONTAINER_NAME or E2E_FIRECRACKER_RUNNER_ENV_FILE to restart runner');
  }
  const projectDir = e2eProjectDir();
  execFileSync('bash', [`${projectDir}/e2e/lib/restart-firecracker-runner.sh`], {
    stdio: 'inherit',
    env: { ...process.env, E2E_FIRECRACKER_RUNNER_ENV_FILE: envFile },
  });
}

export function stopFirecrackerRunnerPid(pid: string, remote?: { ssh: string; sshOpts?: string }): void {
  if (remote?.ssh) {
    const sshArgs = remote.sshOpts ? remote.sshOpts.split(/\s+/).filter(Boolean) : [];
    execFileSync('ssh', [...sshArgs, remote.ssh, 'sudo', 'kill', '-TERM', pid], { stdio: 'pipe' });
    return;
  }
  execFileSync('sudo', ['kill', '-TERM', pid], { stdio: 'pipe' });
}

export function restartFirecrackerRunnerFromEnvFile(
  envFile: string,
  remote?: { ssh: string; sshOpts?: string },
): void {
  const projectDir = e2eProjectDir();
  if (remote?.ssh) {
    const sshArgs = remote.sshOpts ? remote.sshOpts.split(/\s+/).filter(Boolean) : [];
    execFileSync('ssh', [...sshArgs, remote.ssh, 'bash', `${projectDir}/e2e/lib/restart-firecracker-runner.sh`], {
      stdio: 'inherit',
      env: { ...process.env, E2E_FIRECRACKER_RUNNER_ENV_FILE: envFile },
    });
    return;
  }
  execFileSync('bash', [`${projectDir}/e2e/lib/restart-firecracker-runner.sh`], {
    stdio: 'inherit',
    env: { ...process.env, E2E_FIRECRACKER_RUNNER_ENV_FILE: envFile },
  });
}

export function innerContainerName(sandboxID: string): string {
  return `sandbox-${sandboxID.slice(0, 12)}`;
}

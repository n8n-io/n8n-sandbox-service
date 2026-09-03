import { APIRequestContext, expect } from '@playwright/test';
import * as http from 'node:http';
import * as https from 'node:https';
import {
  SandboxClient,
  SandboxServiceError,
  type CreateSandboxOptions,
  type ExecResult,
} from '@n8n/sandbox-client';
import { execFileSync } from 'node:child_process';

/** Admin key from env (SANDBOX_API_KEYS). Used to mint tenant keys. */
export const ADMIN_API_KEY = process.env.SANDBOX_API_KEY || 'test';
export const BASE_URL = process.env.BASE_URL || process.env.BASE_URL_A || 'http://localhost:8080';

let tenantApiKey: string | null = null;
let tenantMintPromise: Promise<string> | null = null;

/** Admin SandboxClient (SANDBOX_API_KEYS). */
export function adminClient(baseUrl: string = BASE_URL): SandboxClient {
  return new SandboxClient({ baseUrl, apiKey: ADMIN_API_KEY });
}

/** SandboxClient for an explicit base URL and API key. */
export function apiClient(baseUrl: string, apiKey: string): SandboxClient {
  return new SandboxClient({ baseUrl, apiKey });
}

/**
 * Tenant SandboxClient using the minted process key.
 * Call ensureTenantAuth / getApiKey first.
 */
export function tenantClient(baseUrl: string = BASE_URL): SandboxClient {
  if (!tenantApiKey) {
    throw new Error('tenantClient: call ensureTenantAuth()/getApiKey() first');
  }
  return apiClient(baseUrl, tenantApiKey);
}

/**
 * Process-wide tenant client for BASE_URL (or the mint URL). Assigned by
 * ensureTenantAuth / getApiKey; do not use before that.
 */
export let client!: SandboxClient;

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
  client = apiClient(mintBaseUrl, tenantApiKey);
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

export type { ExecResult };

async function headers(extra?: Record<string, string>): Promise<Record<string, string>> {
  return { 'X-Api-Key': await getApiKey(), ...extra };
}

export async function createSandbox(options?: CreateSandboxOptions): Promise<string> {
  await ensureTenantAuth();
  const record = await client.createSandbox(options);
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

export async function deleteSandbox(id: string, c?: SandboxClient): Promise<void> {
  await ensureTenantAuth();
  try {
    await (c ?? client).deleteSandbox(id);
  } catch (err) {
    // Idempotent cleanup: row may already be gone after runner-gone reap or a
    // retried DELETE whose first attempt succeeded.
    if (err instanceof SandboxServiceError && err.status === 404) return;
    throw err;
  }
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

/**
 * Kills a sandbox's guest from the inside, with no host access.
 *
 * `kill -9 1` does not work and fails silently: the kernel drops any signal that
 * init has installed no handler for, and SIGKILL can never be handled, so the
 * signal is discarded and the killer still exits 0. SIGTERM is the opposite —
 * the daemon calls `signal.Notify` for it, so it is delivered, the daemon shuts
 * down and returns from `main`, and a PID 1 that exits panics the kernel. The
 * golden boot args carry `panic=1 reboot=k`, so the panic resets the CPU about a
 * second later and the VM is gone. Signalling PID 1 is permitted because the
 * daemon drops to uid 1000 at startup and runs execs as that same user.
 *
 * On Firecracker the VMM process exits, which is what the runner detects. On
 * Docker the container exits and its restart policy brings it back.
 *
 * Sent as a bare request rather than through `exec`, because the SDK reacts to
 * the dropped stream the way any client would: it resumes the execution over
 * `GET /executions/{exec_id}`. That route wakes a stopped sandbox and reports the
 * recovery as a 409, by design — so going through the SDK recovers the very crash
 * the caller asked for, before the caller can observe it. The stream is abandoned
 * instead of read for the same reason. The delete that follows is harmless here:
 * it never wakes a sandbox, so it cannot take the report.
 */
export async function crashGuest(id: string): Promise<void> {
  // Resolved outside the try: minting the tenant key is not the request whose
  // failure a crash is allowed to look like.
  const requestHeaders = await headers({ 'Content-Type': 'application/json' });

  let res: Response;
  try {
    res = await fetch(`${BASE_URL}/sandboxes/${id}/executions`, {
      method: 'POST',
      headers: requestHeaders,
      body: JSON.stringify({ command: 'kill -TERM 1' }),
    });
  } catch {
    // Shutdown is graceful, so this call normally returns before the guest goes
    // down — but a dropped connection is the requested outcome, not a failure.
    return;
  }

  // The daemon writes the 200 and flushes it before it runs the command, so a
  // delivered kill always carries one. Any other status is a request that never
  // reached the guest, and a caller that went on would wait out its crash timeout
  // on a sandbox that is still alive.
  if (!res.ok) {
    throw new Error(`crash guest ${id} failed: ${res.status} ${await res.text()}`);
  }
  await res.body?.cancel();
}

/**
 * Command that adds a second IPv4 address to eth0 through the legacy ioctl,
 * since iproute2 is absent from the sandbox image. The ':1' label keeps the
 * address secondary, so the sandbox's own address — and with it the daemon
 * connection carrying the exec — survives.
 *
 * It runs as the sandbox user, which has no CAP_NET_ADMIN, so the ioctl is
 * denied. Callers expect that: sandbox-capabilities.spec.ts asserts the denial,
 * and network-isolation.spec.ts skips on it.
 */
export const addAddress = (ip: string) =>
  `python3 -c "import fcntl,socket,struct;` +
  `s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM);` +
  `fcntl.ioctl(s,0x8916,struct.pack('16sH2s4s16s',b'eth0:1',socket.AF_INET,` +
  `b'\\x00\\x00',socket.inet_aton('${ip}'),b'\\x00'*16))"`;

/** An unused address in the same /24 as ip. */
export const siblingOf = (ip: string) => ip.split('.').slice(0, 3).concat('250').join('.');

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
): Promise<{
  status: number;
  body: string;
  headers: Record<string, string>;
  json: () => Promise<unknown>;
}> {
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
    // Playwright lowercases response header names.
    headers: resp.headers(),
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

// The runner serves TLS with a private CA, so -k here and --no-check-certificate
// below: these probes only need to reach the unauthenticated endpoints.
export function scrapeRunnerMetricsAt(addr: string): string {
  const host = addr.replace(/^https?:\/\//, '');
  return execFileSync('curl', ['-skf', `https://${host}/metrics`], { encoding: 'utf8' });
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
    [
      'exec',
      runnerContainer,
      'wget',
      '-q',
      '-O',
      '-',
      '--no-check-certificate',
      'https://localhost:8080/metrics',
    ],
    { encoding: 'utf8' },
  );
}

export async function waitRunnerHttpReady(addr: string, deadlineMs = 75_000): Promise<void> {
  const url = `https://${addr.replace(/^https?:\/\//, '')}/readyz`;
  const deadline = Date.now() + deadlineMs;
  while (Date.now() < deadline) {
    try {
      execFileSync('curl', ['-skf', url], { stdio: 'pipe' });
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

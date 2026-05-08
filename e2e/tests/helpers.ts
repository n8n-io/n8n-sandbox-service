import { APIRequestContext } from '@playwright/test';
import { SandboxClient, type ExecResult } from '@n8n/sandbox-client';
import { execFileSync } from 'node:child_process';

const API_KEY = process.env.SANDBOX_API_KEY || 'test';
const BASE_URL = process.env.BASE_URL || 'http://localhost:8080';

export const client = new SandboxClient({ baseUrl: BASE_URL, apiKey: API_KEY });

export type { ExecResult };

function headers(extra?: Record<string, string>): Record<string, string> {
  return { 'X-Api-Key': API_KEY, ...extra };
}

export async function createSandbox(): Promise<string> {
  const record = await client.createSandbox();
  return record.id;
}

export async function deleteSandbox(id: string): Promise<void> {
  await client.deleteSandbox(id);
}

export async function exec(
  id: string,
  command: string,
  opts?: { env?: Record<string, string>; workdir?: string; timeoutMs?: number },
): Promise<ExecResult> {
  return client.exec(id, {
    command,
    env: opts?.env,
    workdir: opts?.workdir,
    timeoutMs: opts?.timeoutMs,
  });
}

function isTransientExecError(err: unknown): boolean {
  const status = (err as { status?: number })?.status;
  if (status === 502 || status === 503) {
    return true;
  }
  const msg = String((err as Error)?.message || '').toLowerCase();
  return (
    msg.includes('internal server error') ||
    msg.includes('daemon unreachable') ||
    msg.includes('runner unavailable') ||
    msg.includes('sandbox exec stream ended without an exit event')
  );
}

export async function execWithTransientRetry(
  id: string,
  command: string,
  opts?: { env?: Record<string, string>; workdir?: string; timeoutMs?: number; retryWindowMs?: number },
): Promise<ExecResult> {
  const deadlineMs = opts?.retryWindowMs ?? 12_000;
  const deadline = Date.now() + deadlineMs;
  let lastErr: unknown;
  while (Date.now() < deadline) {
    try {
      return await exec(id, command, opts);
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
  const filePath = path.startsWith('/') ? path : `/${path}`;
  await client.writeFile(id, filePath, content);
}

export async function downloadFile(
  id: string,
  path: string,
): Promise<string> {
  const filePath = path.startsWith('/') ? path : `/${path}`;
  const buf = await client.readFile(id, filePath);
  return buf.toString('utf-8');
}

export async function apiRequest(
  request: APIRequestContext,
  method: string,
  path: string,
  opts?: { data?: unknown; rawHeaders?: Record<string, string> },
): Promise<{ status: number; body: string; json: () => Promise<unknown> }> {
  const h = opts?.rawHeaders ?? headers({ 'Content-Type': 'application/json' });
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

export function innerContainerName(sandboxID: string): string {
  return `sandbox-${sandboxID.slice(0, 12)}`;
}

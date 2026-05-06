import { APIRequestContext } from '@playwright/test';
import { SandboxClient, type ExecResult } from '@n8n/sandbox-client';

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

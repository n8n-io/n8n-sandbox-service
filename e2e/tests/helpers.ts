import { APIRequestContext } from '@playwright/test';

const API_KEY = process.env.SANDBOX_API_KEY || 'test';

interface ExecOptions {
  env?: Record<string, string>;
  workdir?: string;
  timeout_ms?: number;
}

interface ExecEvent {
  type: string;
  data?: string;
  exit_code?: number;
  success?: boolean;
  execution_time_ms?: number;
  timed_out?: boolean;
  killed?: boolean;
  error?: string;
}

export interface ExecResult {
  stdout: string;
  stderr: string;
  exit: ExecEvent | null;
  events: ExecEvent[];
}

function headers(extra?: Record<string, string>): Record<string, string> {
  return { 'X-Api-Key': API_KEY, ...extra };
}

export async function createSandbox(request: APIRequestContext, reqBody?: Record<string, unknown>): Promise<string> {
  const resp = await request.post('/sandboxes', {
    headers: headers({ 'Content-Type': 'application/json' }),
    data: reqBody ?? {},
  });
  if (resp.status() !== 201) {
    throw new Error(`create sandbox failed: ${resp.status()} ${await resp.text()}`);
  }
  const body = await resp.json();
  return body.id;
}

export async function deleteSandbox(request: APIRequestContext, id: string): Promise<void> {
  await request.delete(`/sandboxes/${id}`, { headers: headers() });
}

export async function exec(
  request: APIRequestContext,
  id: string,
  command: string,
  opts?: ExecOptions,
): Promise<ExecResult> {
  const body: Record<string, unknown> = { command, ...opts };
  const resp = await request.post(`/sandboxes/${id}/exec`, {
    headers: headers({ 'Content-Type': 'application/json' }),
    data: body,
  });
  if (resp.status() !== 200) {
    throw new Error(`exec failed: ${resp.status()} ${await resp.text()}`);
  }
  const text = await resp.text();
  return parseNdjson(text);
}

function parseNdjson(text: string): ExecResult {
  const lines = text.trim().split('\n').filter(Boolean);
  const events: ExecEvent[] = lines.map((l) => JSON.parse(l));
  let stdout = '';
  let stderr = '';
  let exit: ExecEvent | null = null;
  for (const ev of events) {
    if (ev.type === 'stdout') stdout += ev.data || '';
    if (ev.type === 'stderr') stderr += ev.data || '';
    if (ev.type === 'exit') exit = ev;
  }
  return { stdout, stderr, exit, events };
}

export async function uploadFile(
  request: APIRequestContext,
  id: string,
  path: string,
  content: string | Buffer,
): Promise<void> {
  const filePath = path.startsWith('/') ? path : `/${path}`;
  const resp = await request.put(`/sandboxes/${id}/files?path=${encodeURIComponent(filePath)}`, {
    headers: headers({ 'Content-Type': 'application/octet-stream' }),
    data: content,
  });
  if (resp.status() !== 200) {
    throw new Error(`upload failed: ${resp.status()} ${await resp.text()}`);
  }
}

export async function downloadFile(
  request: APIRequestContext,
  id: string,
  path: string,
): Promise<string> {
  const filePath = path.startsWith('/') ? path : `/${path}`;
  const resp = await request.get(`/sandboxes/${id}/files/content?path=${encodeURIComponent(filePath)}`, {
    headers: headers(),
  });
  if (resp.status() !== 200) {
    throw new Error(`download failed: ${resp.status()} ${await resp.text()}`);
  }
  return resp.text();
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

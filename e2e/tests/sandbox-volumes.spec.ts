import { test, expect, type APIRequestContext } from '@playwright/test';
import './matchers';
import { execFileSync } from 'node:child_process';
import { createHash, randomUUID } from 'node:crypto';
import * as fs from 'node:fs';
import * as os from 'node:os';
import * as path from 'node:path';
import {
  VOLUME_PUBLISHER_API_KEY,
  apiRequest,
  deleteSandbox,
  ensureTenantAuth,
  exec,
  execWithTransientRetry,
  stopSandboxViaRunner,
} from './helpers';

// Shared volumes published through /shared-volumes and attached through
// POST /sandboxes. On the Firecracker lane each publish builds an image.

type SandboxVolume = {
  name?: string;
  digest?: string;
  scope?: string;
  mount_path: string;
  optional: boolean;
  attached: boolean;
  reason?: string;
};
type Sandbox = { id: string; volumes?: SandboxVolume[] };
type Published = { name: string; digest: string; marker: string };

const sha256 = (data: Buffer) => createHash('sha256').update(data).digest('hex');

test.describe('Sandbox volumes', () => {
  test.describe.configure({ timeout: 300_000 });

  let work = '';
  let sandboxID = '';
  const published: Published[] = [];

  // A tar of a readme with a marker new on every run, since the API's data dir
  // outlives a run, and a script.
  async function publish(request: APIRequestContext, name: string): Promise<Published> {
    const marker = `${name} ${randomUUID()}`;
    const tree = path.join(work, name);
    fs.mkdirSync(path.join(tree, 'bin'), { recursive: true });
    fs.writeFileSync(path.join(tree, 'readme.md'), `${marker}\n`);
    fs.writeFileSync(path.join(tree, 'bin/hello'), '#!/bin/sh\necho hello from a volume\n', { mode: 0o755 });
    const tar = path.join(work, `${name}.tar`);
    execFileSync('tar', ['-C', tree, '-cf', tar, '.']);
    const body = fs.readFileSync(tar);
    const digest = sha256(body);
    const res = await request.put(`/shared-volumes/sha256:${digest}?name=${name}`, {
      headers: { 'X-Api-Key': VOLUME_PUBLISHER_API_KEY, 'Content-Type': 'application/x-tar' },
      data: body,
      timeout: 280_000,
    });
    expect(res.status(), await res.text()).toBe(201);
    const volume = { name, digest: `sha256:${digest}`, marker };
    published.push(volume);
    return volume;
  }

  async function read(id: string, file: string): Promise<string> {
    const result = await execWithTransientRetry(id, `cat ${file}`);
    expect(result).toHaveSucceeded();
    return result.stdout.trim();
  }

  test.beforeAll(async () => {
    await ensureTenantAuth();
    work = fs.mkdtempSync(path.join(os.tmpdir(), 'sandbox-volumes-'));
  });

  test.afterAll(async ({ request }) => {
    if (sandboxID) await deleteSandbox(sandboxID);
    // Every delete runs before the check, so one failure leaves no other volume behind.
    const statuses: number[] = [];
    for (const volume of published) {
      const res = await request.delete(`/shared-volumes/${volume.digest}`, { headers: { 'X-Api-Key': VOLUME_PUBLISHER_API_KEY } });
      statuses.push(res.status());
    }
    if (work) fs.rmSync(work, { recursive: true, force: true });
    expect(statuses).toEqual(published.map(() => 204));
  });

  test('a sandbox reads its volumes, runs from them, can not write them, and keeps them across stop and wake', async ({
    request,
  }) => {
    const suffix = randomUUID().slice(0, 8);
    const tools = await publish(request, `e2e-tools-${suffix}`);
    const knowledge = await publish(request, `e2e-kb-${suffix}`);
    const missing = `e2e-missing-${suffix}`;

    const res = await apiRequest(request, 'POST', '/sandboxes', {
      data: {
        volumes: [
          { name: tools.name, mount_path: '/mnt/tools' },
          { digest: knowledge.digest, mount_path: '/mnt/knowledge', optional: true },
          { name: missing, mount_path: '/mnt/missing', optional: true },
        ],
      },
    });
    expect(res.status, res.body).toBe(201);
    const sandbox = (await res.json()) as Sandbox;
    sandboxID = sandbox.id;
    const want: SandboxVolume[] = [
      { name: tools.name, digest: tools.digest, scope: 'shared', mount_path: '/mnt/tools', optional: false, attached: true },
      { name: knowledge.name, digest: knowledge.digest, scope: 'shared', mount_path: '/mnt/knowledge', optional: true, attached: true },
      { name: missing, mount_path: '/mnt/missing', optional: true, attached: false, reason: 'not_found' },
    ];
    expect(sandbox.volumes).toEqual(want);

    expect(await read(sandbox.id, '/mnt/tools/readme.md')).toBe(tools.marker);
    expect(await read(sandbox.id, '/mnt/knowledge/readme.md')).toBe(knowledge.marker);
    const ran = await exec(sandbox.id, '/mnt/tools/bin/hello');
    expect(ran).toHaveSucceeded();
    expect(ran.stdout.trim()).toBe('hello from a volume');
    const wrote = await exec(sandbox.id, 'touch /mnt/tools/new 2>&1');
    expect(wrote.exitCode).not.toBe(0);
    expect(wrote.stdout).toContain('Read-only file system');

    const got = await apiRequest(request, 'GET', `/sandboxes/${sandbox.id}`);
    expect(got.status).toBe(200);
    expect(((await got.json()) as Sandbox).volumes).toEqual(want);

    stopSandboxViaRunner(sandbox.id);
    expect(await read(sandbox.id, '/mnt/tools/readme.md')).toBe(tools.marker);
    expect(await read(sandbox.id, '/mnt/knowledge/readme.md')).toBe(knowledge.marker);
  });
});

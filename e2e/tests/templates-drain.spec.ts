import { test, expect } from '@playwright/test';
import './matchers';
import { execFileSync } from 'node:child_process';
import {
  ADMIN_API_KEY,
  BASE_URL,
  client,
  createSandbox,
  deleteSandbox,
  ensureTenantAuth,
  startAndDisconnect,
} from './helpers';

async function admin(method: string, path: string): Promise<{ status: number; body: any }> {
  const res = await fetch(`${BASE_URL}${path}`, { method, headers: { 'X-Api-Key': ADMIN_API_KEY } });
  const text = await res.text();
  return { status: res.status, body: text ? JSON.parse(text) : undefined };
}

test.describe('Templates', () => {
  test('the catalog lists the default template, served by a runner', async () => {
    await ensureTenantAuth();
    const templates = await client.listTemplates();
    const def = templates.find((t) => t.name === 'default');
    expect(def, JSON.stringify(templates)).toBeDefined();
    expect(def!.runners).toBeGreaterThan(0);
    expect(def!.memoryMb).toBeGreaterThan(0);
  });

  // Each template lands on a runner serving it and boots its own image, with
  // its own limits.
  test('a create with each template boots that template', async () => {
    const runner = process.env.E2E_RUNNER_CONTAINER_NAME;
    test.skip(!runner, 'needs E2E_RUNNER_CONTAINER_NAME to inspect the container');
    await ensureTenantAuth();
    const small = (await client.listTemplates()).find((t) => t.name === 'small');
    test.skip(!small, 'the e2e lane serves no "small" template');
    expect(small!.runners).toBeGreaterThan(0);
    for (const template of ['default', 'small']) {
      const id = await createSandbox({ template });
      try {
        expect((await client.getSandbox(id)).template ?? 'default').toBe(template);
        const container = execFileSync(
          'docker',
          ['exec', runner!, 'docker', 'ps', '--filter', `label=sandbox-service.id=${id}`, '--format', '{{.Image}} {{.Label "sandbox-service.template"}}'],
          { encoding: 'utf8' },
        ).trim();
        const [image, label] = container.split(' ');
        expect(label).toBe(template);
        expect(image).toMatch(template === 'small' ? /:e2e-small-/ : /:e2e-(?!small)/);
        const mem = await client.exec(id, { command: 'cat /sys/fs/cgroup/memory.max' });
        if (template === 'small') expect(mem.stdout.trim()).toBe(String(256 * 1024 * 1024));
      } finally {
        await deleteSandbox(id);
      }
    }
  });

  test('an unknown template is refused', async () => {
    await ensureTenantAuth();
    const err = await client.createSandbox({ egress: 'public', template: 'no-such-template' }).catch((e) => e);
    expect(err.status).toBe(400);
    expect(err.reason).toBe('unknown_template');
  });
});

test.describe('Runner drain', () => {
  test.describe.configure({ mode: 'serial' });

  test('a draining runner takes no creates and is drained once its last run ends', async () => {
    const runners = await admin('GET', '/admin/runners');
    const runner = runners.body.runners.find((r: { healthy: boolean }) => r.healthy);
    expect(runner).toBeDefined();
    const id = await createSandbox();
    try {
      // A run in progress whose stream is gone still holds the drain.
      const execId = await startAndDisconnect(id, 'sleep 20');
      expect(execId).toBeTruthy();
      const state = await admin('POST', `/admin/runners/${runner.id}/drain`);
      expect(state.status).toBe(200);
      expect(state.body.draining).toBe(true);
      expect(state.body.drained).toBe(false);
      expect(state.body.active_executions).toBeGreaterThan(0);

      await expect
        .poll(async () => (await admin('GET', '/admin/runners')).body.runners.find((r: { id: string }) => r.id === runner.id)?.cordoned, {
          timeout: 20_000,
        })
        .toBe(true);

      await expect
        .poll(async () => (await admin('GET', `/admin/runners/${runner.id}/drain`)).body.drained, { timeout: 60_000, intervals: [2_000] })
        .toBe(true);
    } finally {
      await admin('DELETE', `/admin/runners/${runner.id}/drain`);
      await deleteSandbox(id);
    }
  });
});

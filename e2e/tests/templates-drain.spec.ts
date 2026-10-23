import { test, expect } from '@playwright/test';
import './matchers';
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

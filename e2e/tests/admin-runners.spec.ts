import { test, expect } from '@playwright/test';
import { ADMIN_API_KEY, PROVISIONER_API_KEY, SCALER_API_KEY } from './helpers';

test.describe('GET /admin/runners', () => {
  test('lists runner capacity without addresses', async ({ request }) => {
    const resp = await request.get('/admin/runners', { headers: { 'X-Api-Key': ADMIN_API_KEY } });
    expect(resp.status()).toBe(200);
    const body = await resp.json();
    expect(body.runners.length).toBeGreaterThan(0);
    for (const runner of body.runners) {
      expect(Object.keys(runner).sort()).toEqual(
        ['capacity_stopped', 'capacity_total', 'capacity_used', 'cordoned', 'healthy', 'id', 'last_seen'],
      );
    }
  });

  test('a scaler key may list and cordon runners and reach no other admin or sandbox route', async ({ request }) => {
    const scaler = { 'X-Api-Key': SCALER_API_KEY };
    const resp = await request.get('/admin/runners', { headers: scaler });
    expect(resp.status()).toBe(200);
    expect((await resp.json()).runners.length).toBeGreaterThan(0);

    for (const path of ['/admin/scaler', '/admin/tenants', '/sandboxes']) {
      const denied = await request.get(path, { headers: scaler });
      expect(denied.status(), path).toBe(403);
    }
  });

  test('a provisioner key may not list runners', async ({ request }) => {
    const resp = await request.get('/admin/runners', { headers: { 'X-Api-Key': PROVISIONER_API_KEY } });
    expect(resp.status()).toBe(403);
  });
});

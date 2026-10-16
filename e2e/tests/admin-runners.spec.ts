import { test, expect } from '@playwright/test';
import { ADMIN_API_KEY } from './helpers';

test.describe('GET /admin/runners', () => {
  test('lists runner capacity without addresses', async ({ request }) => {
    const resp = await request.get('/admin/runners', { headers: { 'X-Api-Key': ADMIN_API_KEY } });
    expect(resp.status()).toBe(200);
    const body = await resp.json();
    expect(body.runners.length).toBeGreaterThan(0);
    for (const runner of body.runners) {
      expect(Object.keys(runner).sort()).toEqual(
        ['capacity_stopped', 'capacity_total', 'capacity_used', 'healthy', 'id', 'last_seen'],
      );
    }
  });
});

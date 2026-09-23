import { test, expect } from '@playwright/test';
import { ADMIN_API_KEY, PROVISIONER_API_KEY } from './helpers';

// A provisioner key (SANDBOX_API_PROVISIONER_KEYS) may only create tenants and
// delete empty tenants. It must never reach an existing tenant's sandboxes.
test.describe('Provisioner key', () => {
  test('creates a tenant it cannot then read, and deletes it once empty', async ({ request }) => {
    const provisioner = { 'X-Api-Key': PROVISIONER_API_KEY };
    let tenantId: string | undefined;
    let tenantKey: string | undefined;
    let sandboxId: string | undefined;

    try {
      const created = await request.post('/admin/tenants', {
        headers: { ...provisioner, 'Content-Type': 'application/json' },
        data: { name: `prov-${Date.now()}`, external_ref: 'e2e-provisioner' },
      });
      expect(created.status()).toBe(201);
      const body = await created.json();
      tenantId = body.tenant.id as string;
      tenantKey = body.key.api_key as string;
      expect(tenantKey).toMatch(/^sbk_/);

      // The returned tenant key is a working tenant key.
      const tenant = { 'X-Api-Key': tenantKey };
      const create = await request.post('/sandboxes', {
        headers: { ...tenant, 'Content-Type': 'application/json' },
        data: {},
      });
      expect(create.status()).toBe(201);
      sandboxId = (await create.json()).id as string;

      const list = await request.get('/sandboxes', { headers: tenant });
      expect(list.status()).toBe(200);
      expect((await list.json()).map((s: { id: string }) => s.id)).toContain(sandboxId);

      // The provisioner gets 403 on everything but create and delete tenant.
      const denied: Array<{ method: 'GET' | 'POST' | 'DELETE'; path: string }> = [
        { method: 'GET', path: '/admin/tenants' },
        { method: 'GET', path: `/admin/tenants/${tenantId}` },
        { method: 'GET', path: `/admin/tenants/${tenantId}/keys` },
        { method: 'POST', path: `/admin/tenants/${tenantId}/keys` },
        { method: 'GET', path: '/sandboxes' },
        { method: 'POST', path: '/sandboxes' },
        { method: 'GET', path: `/sandboxes/${sandboxId}` },
        { method: 'DELETE', path: `/sandboxes/${sandboxId}` },
        { method: 'POST', path: `/sandboxes/${sandboxId}/executions` },
        { method: 'GET', path: `/sandboxes/${sandboxId}/files?path=/tmp` },
      ];
      for (const route of denied) {
        const resp = await request.fetch(route.path, {
          method: route.method,
          headers: { ...provisioner, 'Content-Type': 'application/json' },
          data: route.method === 'GET' ? undefined : {},
        });
        expect(resp.status(), `${route.method} ${route.path}`).toBe(403);
      }

      // The sandbox is untouched by the denied calls.
      const stillThere = await request.get(`/sandboxes/${sandboxId}`, { headers: tenant });
      expect(stillThere.status()).toBe(200);

      // Delete refuses while the tenant owns a sandbox, then succeeds.
      const busy = await request.delete(`/admin/tenants/${tenantId}`, { headers: provisioner });
      expect(busy.status()).toBe(409);

      const removed = await request.delete(`/sandboxes/${sandboxId}`, { headers: tenant });
      expect(removed.status()).toBe(204);
      sandboxId = undefined;

      const gone = await request.delete(`/admin/tenants/${tenantId}`, { headers: provisioner });
      expect(gone.status()).toBe(204);
      tenantId = undefined;

      const revoked = await request.get('/sandboxes', { headers: tenant });
      expect(revoked.status()).toBe(401);
    } finally {
      if (sandboxId && tenantKey) {
        await request
          .delete(`/sandboxes/${sandboxId}`, { headers: { 'X-Api-Key': tenantKey } })
          .catch(() => undefined);
      }
      if (tenantId) {
        await request
          .delete(`/admin/tenants/${tenantId}`, { headers: { 'X-Api-Key': ADMIN_API_KEY } })
          .catch(() => undefined);
      }
    }
  });

  test('cannot create tenants with unlimited or above-default quota', async ({ request }) => {
    const headers = { 'X-Api-Key': PROVISIONER_API_KEY, 'Content-Type': 'application/json' };
    for (const max_sandboxes of [0, 2147483647]) {
      const resp = await request.post('/admin/tenants', {
        headers,
        data: { name: 'quota', max_sandboxes },
      });
      expect(resp.status(), `max_sandboxes=${max_sandboxes}`).toBe(400);
    }
  });
});

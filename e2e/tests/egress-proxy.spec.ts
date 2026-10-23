import { test, expect } from '@playwright/test';
import './matchers';
import { client, createSandbox, deleteSandbox, ensureTenantAuth, execWithTransientRetry } from './helpers';

// An egress "proxy" sandbox reaches the hosts its spec allows, through the
// runner's proxy, and nothing else: not other hosts, not the internet
// directly, not private ranges, and no DNS.
test.describe('Egress proxy', () => {
  const ALLOWED = 'example.com';
  const spec = {
    allow: [{ name: 'web', hosts: [ALLOWED] }],
    labels: { instance_id: 'e2e', session_id: 's1' },
  };
  const BOUNDED_RESOLVER = `RES_OPTIONS='timeout:1 attempts:2'`;

  const run = (id: string, cmd: string) => execWithTransientRetry(id, cmd, { timeoutMs: 30_000 });

  test('reaches the allowed host through the proxy and nothing else', async () => {
    const id = await createSandbox({ egress: 'proxy', egressProxy: spec });
    try {
      const env = await run(id, 'echo "$HTTPS_PROXY|$SSL_CERT_FILE"');
      expect(env.stdout.trim()).toMatch(/^http:\/\/[0-9.]+:3128\|\/tmp\/sandbox-egress\/ca-bundle\.pem$/);

      const allowed = await run(id, `curl -fsS -o /dev/null -w '%{http_code}' --max-time 15 https://${ALLOWED}/`);
      expect(allowed.exitCode, allowed.stderr).toBe(0);
      expect(allowed.stdout.trim()).toBe('200');

      // Another host is refused at the CONNECT, with the reason.
      const other = await run(id, `curl -sS -o /dev/null --max-time 15 -w '%{http_code}' https://www.iana.org/ 2>&1; true`);
      expect(other.stdout).not.toContain('200');
      const plainOther = await run(
        id,
        `curl -sS -D - -o /dev/null --max-time 15 http://www.iana.org/`,
      );
      expect(plainOther.stdout).toContain('X-Sandbox-Egress-Refusal: host_not_allowed');

      // Around the proxy: a direct connection, a private address, DNS.
      const direct = await run(id, `curl --noproxy '*' -sS -o /dev/null --max-time 5 http://1.1.1.1/`);
      expect(direct.exitCode).not.toBe(0);
      const viaProxyIP = await run(id, `curl -sS -D - -o /dev/null --max-time 10 http://169.254.169.254/`);
      expect(viaProxyIP.stdout).toContain('X-Sandbox-Egress-Refusal: host_not_allowed');
      const dns = await run(id, `${BOUNDED_RESOLVER} getent hosts ${ALLOWED} || true`);
      expect(dns.stdout.trim()).toBe('');
    } finally {
      await deleteSandbox(id);
    }
  });

  test('a replaced spec applies to the next request', async () => {
    await ensureTenantAuth();
    const id = await createSandbox({ egress: 'proxy', egressProxy: spec });
    try {
      await client.updateEgressProxy(id, { allow: [] });
      const after = await run(id, `curl -sS -D - -o /dev/null --max-time 10 http://${ALLOWED}/`);
      expect(after.stdout).toContain('X-Sandbox-Egress-Refusal: host_not_allowed');
      await client.updateEgressProxy(id, spec);
      const restored = await run(id, `curl -fsS -o /dev/null --max-time 15 https://${ALLOWED}/`);
      expect(restored.exitCode, restored.stderr).toBe(0);
    } finally {
      await deleteSandbox(id);
    }
  });

  test('model egress can be blocked and read back', async () => {
    await ensureTenantAuth();
    const id = await createSandbox({ egress: 'proxy', egressProxy: spec });
    try {
      expect(await client.getModelEgress(id)).toBe(false);
      expect(await client.setModelEgress(id, true)).toBe(true);
      expect(await client.getModelEgress(id)).toBe(true);
    } finally {
      await deleteSandbox(id);
    }
  });
});

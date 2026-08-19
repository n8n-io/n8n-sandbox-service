import { test, expect } from '@playwright/test';
import './matchers';
import {
  ADMIN_API_KEY,
  BASE_URL,
  apiClient,
  createSandbox,
  deleteSandbox,
  exec,
  execWithTransientRetry,
} from './helpers';
import type { SandboxClient } from '@n8n/sandbox-client';

const tcpConnect = (ip: string, port: number = 80, timeout: number = 3) =>
  `curl --connect-timeout ${timeout} -s -o /dev/null http://${ip}:${port}/`;

const tcpConnectV6 = (ip: string, port: number = 443, timeout: number = 3) =>
  `curl --connect-timeout ${timeout} -sk -o /dev/null -6 "https://[${ip}]:${port}/"`;

const resolve = (host: string) =>
  `getent ahostsv4 ${host} | head -1 | awk '{print $1}'`;

// Both runtimes put the gateway at .1 of the sandbox's own /24: the TAP address
// on Firecracker, the bridge gateway on Sysbox. Deriving it beats reading it,
// since iproute2 is absent from the image and the Firecracker guest has no
// /proc/net/route either — the daemon runs as PID 1, so nothing mounts /proc.
const gatewayOf = (ip: string) => ip.split('.').slice(0, 3).concat('1').join('.');

test.describe('Network isolation', () => {
  test('sandbox can reach public internet', async () => {
    const id = await createSandbox();
    try {
      const result = await exec(id, `curl -fsSL -o /dev/null -w '%{http_code}' --max-time 15 https://example.com/`, {
        timeoutMs: 30_000,
      });
      expect(result).toHaveSucceeded();
      expect(result.stdout.trim()).toBe('200');
    } finally {
      await deleteSandbox(id);
    }
  });

  test('sandbox cannot reach private IPs', async () => {
    const id = await createSandbox();
    try {
      // One address per blocked range in netpolicy.PrivateRangesV4, except
      // 127.0.0.0/8: the guest routes loopback to its own lo, so it never
      // reaches the FORWARD chain and the assertion would prove nothing.
      const privateIPs = [
        '10.0.0.1',
        '172.16.0.1',
        '192.168.1.1',
        '169.254.169.254', // cloud metadata endpoint
        '100.64.0.1', // carrier-grade NAT
        '198.18.0.1', // benchmarking
        '240.0.0.1', // reserved
      ];

      for (const ip of privateIPs) {
        const result = await execWithTransientRetry(id, tcpConnect(ip, 80, 3), { timeoutMs: 10_000 });
        expect(result.exitCode, `expected private IP ${ip} to be unreachable`).not.toBe(0);
      }
    } finally {
      await deleteSandbox(id);
    }
  });

  test('sandboxes cannot reach each other across tenants', async ({ request }) => {
    const id1 = await createSandbox();
    let otherTenantId: string | undefined;
    let otherClient: SandboxClient | undefined;
    let id2: string | undefined;

    try {
      // Sandbox 2 belongs to a second tenant, so this covers the network
      // boundary and the tenant boundary at the same time.
      const other = await request.post('/admin/tenants', {
        headers: { 'X-Api-Key': ADMIN_API_KEY, 'Content-Type': 'application/json' },
        data: { name: `netiso-${Date.now()}` },
      });
      expect(other.status()).toBe(201);
      const otherBody = await other.json();
      otherTenantId = otherBody.tenant.id as string;
      otherClient = apiClient(BASE_URL, otherBody.key.api_key as string);

      id2 = (await otherClient.createSandbox()).id;

      // Get sandbox 2's IP
      const ipResult = await execWithTransientRetry(id2, 'hostname -I', { timeoutMs: 5_000 }, otherClient);
      expect(ipResult).toHaveSucceeded();
      const sandbox2IP = ipResult.stdout.trim();
      expect(sandbox2IP).toBeTruthy();

      // Start an HTTP listener in sandbox 2 on port 9999
      await execWithTransientRetry(
        id2,
        `node -e "require('http').createServer((q,r)=>r.end('ok')).listen(9999,'0.0.0.0');setTimeout(()=>{},30000)" &`,
        { timeoutMs: 5_000 },
        otherClient,
      );

      // Sandbox 1 should not be able to reach sandbox 2
      const result = await execWithTransientRetry(id1, tcpConnect(sandbox2IP, 9999, 3), { timeoutMs: 10_000 });
      expect(result.exitCode, `expected sandbox 1 to not reach sandbox 2 at ${sandbox2IP}`).not.toBe(0);
    } finally {
      if (id2 && otherClient) {
        await deleteSandbox(id2, otherClient).catch(() => undefined);
      }
      await deleteSandbox(id1).catch(() => undefined);
      if (otherTenantId) {
        await request.delete(`/admin/tenants/${otherTenantId}`, {
          headers: { 'X-Api-Key': ADMIN_API_KEY },
        });
      }
    }
  });

  test('sandbox cannot reach the runner', async () => {
    const id = await createSandbox();
    try {
      const ipResult = await execWithTransientRetry(id, 'hostname -I', { timeoutMs: 5_000 });
      expect(ipResult).toHaveSucceeded();
      const ownIP = ipResult.stdout.trim().split(/\s+/)[0];
      expect(ownIP, 'expected the sandbox to report its own IPv4 address').toMatch(
        /^\d+\.\d+\.\d+\.\d+$/,
      );
      const gateway = gatewayOf(ownIP);

      // On Sysbox the gateway is a host address that the runner listens on, so
      // this is the regression test for the INPUT chain in netrules. On
      // Firecracker the gateway is the TAP inside the sandbox netns and the
      // runner is not there at all, so it only asserts that this stays true;
      // the guest's route to the host is covered by the private-range test.
      for (const port of [8080, 9091]) {
        const result = await execWithTransientRetry(id, tcpConnect(gateway, port, 3), {
          timeoutMs: 10_000,
        });
        expect(
          result.exitCode,
          `expected runner port ${port} on ${gateway} to be unreachable`,
        ).not.toBe(0);
      }
    } finally {
      await deleteSandbox(id);
    }
  });

  test('sandbox cannot reach IPv6 destinations', async () => {
    const id = await createSandbox();
    try {
      // Cloudflare and Google public DNS, IPv6 addresses. With IPv6 disabled
      // in the container netns, the AF_INET6 connect must fail.
      const v6Targets = [
        '2606:4700:4700::1111',
        '2001:4860:4860::8888',
      ];

      for (const ip of v6Targets) {
        const result = await execWithTransientRetry(id, tcpConnectV6(ip, 443, 3), { timeoutMs: 10_000 });
        expect(result.exitCode, `expected IPv6 ${ip} to be unreachable`).not.toBe(0);
      }
    } finally {
      await deleteSandbox(id);
    }
  });

  test('DNS resolution works', async () => {
    const id = await createSandbox();
    try {
      const result = await execWithTransientRetry(id, resolve('example.com'), { timeoutMs: 10_000 });
      expect(result).toHaveSucceeded();
      // Should resolve to an IP address
      expect(result.stdout.trim()).toMatch(/^\d+\.\d+\.\d+\.\d+$/);
    } finally {
      await deleteSandbox(id);
    }
  });
});

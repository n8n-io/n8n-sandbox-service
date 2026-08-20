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

// Keeps the response body, so a test can tell which listener answered rather
// than only whether the connection succeeded.
const httpGet = (ip: string, port: number, timeout: number = 3) =>
  `curl --connect-timeout ${timeout} -s http://${ip}:${port}/`;

const httpGetWithRetry = (ip: string, port: number) =>
  `curl --retry 5 --retry-connrefused --retry-delay 1 --connect-timeout 3 -fsS http://${ip}:${port}/`;

const tcpConnectV6 = (ip: string, port: number = 443, timeout: number = 3) =>
  `curl --connect-timeout ${timeout} -sk -o /dev/null -6 "https://[${ip}]:${port}/"`;

const resolve = (host: string) =>
  `getent ahostsv4 ${host} | head -1 | awk '{print $1}'`;

// Both runtimes put the gateway at .1 of the sandbox's own /24: the TAP address
// on Firecracker, the bridge gateway on Sysbox. Deriving it beats reading it,
// since iproute2 is absent from the image and the Firecracker guest has no
// /proc/net/route either — the daemon runs as PID 1, so nothing mounts /proc.
const gatewayOf = (ip: string) => ip.split('.').slice(0, 3).concat('1').join('.');

const siblingOf = (ip: string) => ip.split('.').slice(0, 3).concat('250').join('.');

// Adds a second IPv4 address to eth0 through the legacy ioctl, since iproute2 is
// absent from the sandbox image. The ':1' label keeps the address secondary, so
// the sandbox's own address — and with it the daemon connection carrying this
// exec — survives. Needs CAP_NET_ADMIN, hence sudo.
const addAddress = (ip: string) =>
  `sudo -n python3 -c "import fcntl,socket,struct;` +
  `s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM);` +
  `fcntl.ioctl(s,0x8916,struct.pack('16sH2s4s16s',b'eth0:1',socket.AF_INET,` +
  `b'\\x00\\x00',socket.inet_aton('${ip}'),b'\\x00'*16))"`;

const tcpConnectFrom = (source: string, ip: string, port: number, timeout: number = 3) =>
  `curl --interface ${source} --connect-timeout ${timeout} -s -o /dev/null http://${ip}:${port}/`;

// Cleanup runs even when the test body has already failed, so a delete failure
// is reported softly: throwing here would replace the real error with this one.
// Returns whether the sandbox is gone, which decides whether its tenant can be
// deleted at all. 404 is already treated as success by deleteSandbox, and the
// SDK retries transient failures, so anything reaching here is a real leak.
const cleanUpSandbox = async (id: string, c?: SandboxClient): Promise<boolean> => {
  try {
    await deleteSandbox(id, c);
    return true;
  } catch (err) {
    expect.soft(err, `expected sandbox ${id} to be deleted`).toBeUndefined();
    return false;
  }
};

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
      // What makes this cross-tenant is the API side: id2 is created and exec'd
      // with otherClient, and sandbox 1's key cannot name it. The runner has no
      // tenant concept, so the network side proves only that one sandbox cannot
      // reach another's listener, by whichever mechanism gets there first. On
      // Sysbox that is the private-range drop, which covers the whole bridge
      // subnet before the per-container chains see the packet. On Firecracker
      // every guest holds the same address, so sandbox 1 ends up dialling
      // itself. Neither can be narrowed from inside a sandbox, which is why the
      // positive control below carries the weight: without it the assertion
      // would also pass if nothing were listening at all.
      const other = await request.post('/admin/tenants', {
        headers: { 'X-Api-Key': ADMIN_API_KEY, 'Content-Type': 'application/json' },
        data: { name: `netiso-${Date.now()}` },
      });
      expect(other.status()).toBe(201);
      const otherBody = await other.json();
      otherTenantId = otherBody.tenant.id as string;
      otherClient = apiClient(BASE_URL, otherBody.key.api_key as string);

      id2 = (await otherClient.createSandbox()).id;

      const ipResult = await execWithTransientRetry(id2, 'hostname -I', { timeoutMs: 5_000 }, otherClient);
      expect(ipResult).toHaveSucceeded();
      const sandbox2IP = ipResult.stdout.trim().split(/\s+/)[0];
      expect(sandbox2IP, 'expected sandbox 2 to report its own IPv4 address').toMatch(
        /^\d+\.\d+\.\d+\.\d+$/,
      );

      // A per-run token, so reaching sandbox 2's listener is distinguishable
      // from reaching anything else that happens to answer on that port.
      const token = `netiso${Date.now()}`;
      await execWithTransientRetry(
        id2,
        `node -e "require('http').createServer((q,r)=>r.end('${token}')).listen(9999,'0.0.0.0');setTimeout(()=>{},30000)" &`,
        { timeoutMs: 5_000 },
        otherClient,
      );

      // Positive control. Sandbox 2 reaches the listener on its own address,
      // which stays inside its netns and so is unaffected by the policy.
      // --retry-connrefused covers the gap between backgrounding node and the
      // port being bound.
      const control = await execWithTransientRetry(
        id2,
        httpGetWithRetry(sandbox2IP, 9999),
        { timeoutMs: 20_000 },
        otherClient,
      );
      expect(
        control,
        `expected sandbox 2 to reach its own listener on ${sandbox2IP}:9999`,
      ).toHaveSucceeded();
      expect(control.stdout.trim(), 'expected the listener to answer with the token').toBe(token);

      const result = await execWithTransientRetry(id1, httpGet(sandbox2IP, 9999), { timeoutMs: 10_000 });
      expect(result.exitCode, `expected sandbox 1 to not reach ${sandbox2IP}:9999`).not.toBe(0);
      expect(result.stdout, "expected sandbox 1 to not receive sandbox 2's response").not.toContain(
        token,
      );
    } finally {
      const sandbox2Gone = id2 && otherClient ? await cleanUpSandbox(id2, otherClient) : true;
      await cleanUpSandbox(id1);

      // The API refuses to delete a tenant that still owns a sandbox, so the
      // tenant delete is conditional: attempting it after a failed sandbox
      // delete would report a conflict rather than the leak behind it.
      if (otherTenantId && sandbox2Gone) {
        const res = await request.delete(`/admin/tenants/${otherTenantId}`, {
          headers: { 'X-Api-Key': ADMIN_API_KEY },
        });
        expect.soft(res.status(), `expected tenant ${otherTenantId} to be deleted`).toBe(204);
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

  // The host and private-range rules are matched on the bridge interface rather
  // than on the address the runner assigned, because a sandbox that reaches
  // CAP_NET_ADMIN can give itself another address and a source-matched rule
  // would no longer apply to it. Passwordless sudo in the image plus Sysbox's
  // full capability bounding set are enough to do that, so this asserts the
  // policy still holds for traffic the runner never handed out an address for.
  test('sandbox cannot escape the policy by adding an address', async () => {
    const id = await createSandbox();
    try {
      const ipResult = await execWithTransientRetry(id, 'hostname -I', { timeoutMs: 5_000 });
      expect(ipResult).toHaveSucceeded();
      const ownIP = ipResult.stdout.trim().split(/\s+/)[0];
      expect(ownIP, 'expected the sandbox to report its own IPv4 address').toMatch(
        /^\d+\.\d+\.\d+\.\d+$/,
      );
      // An unused address in the sandbox's own subnet, so it stays inside the
      // NAT source range and looks like a legitimate neighbour to any rule that
      // matches on addresses.
      const extraIP = siblingOf(ownIP);

      const added = await execWithTransientRetry(id, addAddress(extraIP), { timeoutMs: 10_000 });
      test.skip(
        added.exitCode !== 0,
        `sandbox cannot add an address (${added.stderr.trim() || 'no stderr'}), so this bypass does not apply to this runtime`,
      );

      // Establishes that the added address is usable as a source, so a failure
      // below means the policy blocked the traffic rather than that it never
      // left the sandbox.
      const publicResult = await execWithTransientRetry(
        id,
        `curl --interface ${extraIP} -fsS -o /dev/null --max-time 15 https://example.com/`,
        { timeoutMs: 30_000 },
      );
      expect(publicResult, `expected ${extraIP} to work as a source address`).toHaveSucceeded();

      const blocked: [string, number][] = [
        [gatewayOf(ownIP), 8080],
        [gatewayOf(ownIP), 9091],
        ['169.254.169.254', 80],
      ];
      for (const [ip, port] of blocked) {
        const result = await execWithTransientRetry(id, tcpConnectFrom(extraIP, ip, port), {
          timeoutMs: 10_000,
        });
        expect(
          result.exitCode,
          `expected ${ip}:${port} to be unreachable from the added address ${extraIP}`,
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

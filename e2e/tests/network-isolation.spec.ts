import { test, expect } from '@playwright/test';
import './matchers';
import { createSandbox, deleteSandbox, exec, execWithTransientRetry } from './helpers';
import { BOTH_RUNNERS } from './tags';

const tcpConnect = (ip: string, port: number = 80, timeout: number = 3) =>
  `curl --connect-timeout ${timeout} -s -o /dev/null http://${ip}:${port}/`;

const tcpConnectV6 = (ip: string, port: number = 443, timeout: number = 3) =>
  `curl --connect-timeout ${timeout} -sk -o /dev/null -6 "https://[${ip}]:${port}/"`;

const resolve = (host: string) =>
  `getent ahostsv4 ${host} | head -1 | awk '{print $1}'`;

test.describe('Network isolation', BOTH_RUNNERS, () => {
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
      const privateIPs = [
        '10.0.0.1',
        '172.16.0.1',
        '192.168.1.1',
        '169.254.169.254', // cloud metadata endpoint
      ];

      for (const ip of privateIPs) {
        const result = await execWithTransientRetry(id, tcpConnect(ip, 80, 3), { timeoutMs: 10_000 });
        expect(result.exitCode, `expected private IP ${ip} to be unreachable`).not.toBe(0);
      }
    } finally {
      await deleteSandbox(id);
    }
  });

  test('sandboxes cannot reach each other', async () => {
    const id1 = await createSandbox();
    const id2 = await createSandbox();
    try {
      // Get sandbox 2's IP
      const ipResult = await execWithTransientRetry(id2, 'hostname -I', { timeoutMs: 5_000 });
      expect(ipResult).toHaveSucceeded();
      const sandbox2IP = ipResult.stdout.trim();
      expect(sandbox2IP).toBeTruthy();

      // Start an HTTP listener in sandbox 2 on port 9999
      await exec(id2, `node -e "require('http').createServer((q,r)=>r.end('ok')).listen(9999,'0.0.0.0');setTimeout(()=>{},30000)" &`, { timeoutMs: 5_000 });

      // Sandbox 1 should not be able to reach sandbox 2
      const result = await execWithTransientRetry(id1, tcpConnect(sandbox2IP, 9999, 3), { timeoutMs: 10_000 });
      expect(result.exitCode, `expected sandbox 1 to not reach sandbox 2 at ${sandbox2IP}`).not.toBe(0);
    } finally {
      await deleteSandbox(id1);
      await deleteSandbox(id2);
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

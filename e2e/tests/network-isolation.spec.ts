import { test, expect } from '@playwright/test';
import { createSandbox, deleteSandbox, exec } from './helpers';

// Helper: use python3 for HTTP requests since curl is not in the sandbox rootfs
const pyGet = (url: string, timeout: number = 5) =>
  `python3 -c "import urllib.request; r=urllib.request.urlopen('${url}', timeout=${timeout}); print(r.status)"`;

const pyConnect = (ip: string, port: number = 80, timeout: number = 3) =>
  `python3 -c "import socket; s=socket.socket(); s.settimeout(${timeout}); s.connect(('${ip}', ${port})); s.close(); print('connected')"`;

const pyResolve = (host: string) =>
  `python3 -c "import socket; print(socket.gethostbyname('${host}'))"`;

test.describe('Network isolation', () => {
  test('sandbox can reach public internet', async () => {
    const id = await createSandbox();
    try {
      const result = await exec(id, pyGet('http://httpbin.org/ip', 15), {
        timeoutMs: 30_000,
      });
      expect(result.exitCode).toBe(0);
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
        const result = await exec(id, pyConnect(ip, 80, 3), {
          timeoutMs: 10_000,
        });
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
      const ipResult = await exec(id2, 'hostname -I', { timeoutMs: 5_000 });
      expect(ipResult.exitCode).toBe(0);
      const sandbox2IP = ipResult.stdout.trim();
      expect(sandbox2IP).toBeTruthy();

      // Start a TCP listener in sandbox 2 on port 9999
      await exec(id2, `python3 -c "
import socket, threading
s = socket.socket()
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(('0.0.0.0', 9999))
s.listen(1)
import time; time.sleep(30)
" &`, { timeoutMs: 5_000 });

      // Sandbox 1 should not be able to reach sandbox 2
      const result = await exec(id1, pyConnect(sandbox2IP, 9999, 3), {
        timeoutMs: 10_000,
      });
      expect(result.exitCode, `expected sandbox 1 to not reach sandbox 2 at ${sandbox2IP}`).not.toBe(0);
    } finally {
      await deleteSandbox(id1);
      await deleteSandbox(id2);
    }
  });

  test('DNS resolution works', async () => {
    const id = await createSandbox();
    try {
      const result = await exec(id, pyResolve('example.com'), { timeoutMs: 10_000 });
      expect(result.exitCode).toBe(0);
      // Should resolve to an IP address
      expect(result.stdout.trim()).toMatch(/^\d+\.\d+\.\d+\.\d+$/);
    } finally {
      await deleteSandbox(id);
    }
  });
});

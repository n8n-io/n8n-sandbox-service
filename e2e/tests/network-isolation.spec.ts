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
  test('sandbox can reach public internet', async ({ request }) => {
    const id = await createSandbox(request);
    try {
      const result = await exec(request, id, pyGet('http://httpbin.org/ip', 15), {
        timeout_ms: 30_000,
      });
      expect(result.exit?.exit_code).toBe(0);
      expect(result.stdout.trim()).toBe('200');
    } finally {
      await deleteSandbox(request, id);
    }
  });

  test('sandbox cannot reach private IPs', async ({ request }) => {
    const id = await createSandbox(request);
    try {
      const privateIPs = [
        '10.0.0.1',
        '172.16.0.1',
        '192.168.1.1',
        '169.254.169.254', // cloud metadata endpoint
      ];

      for (const ip of privateIPs) {
        const result = await exec(request, id, pyConnect(ip, 80, 3), {
          timeout_ms: 10_000,
        });
        expect(result.exit?.exit_code, `expected private IP ${ip} to be unreachable`).not.toBe(0);
      }
    } finally {
      await deleteSandbox(request, id);
    }
  });

  test('sandboxes cannot reach each other', async ({ request }) => {
    const id1 = await createSandbox(request);
    const id2 = await createSandbox(request);
    try {
      // Get sandbox 2's IP
      const ipResult = await exec(request, id2, 'hostname -I', { timeout_ms: 5_000 });
      expect(ipResult.exit?.exit_code).toBe(0);
      const sandbox2IP = ipResult.stdout.trim();
      expect(sandbox2IP).toBeTruthy();

      // Start a TCP listener in sandbox 2 on port 9999
      await exec(request, id2, `python3 -c "
import socket, threading
s = socket.socket()
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(('0.0.0.0', 9999))
s.listen(1)
import time; time.sleep(30)
" &`, { timeout_ms: 5_000 });

      // Sandbox 1 should not be able to reach sandbox 2
      const result = await exec(request, id1, pyConnect(sandbox2IP, 9999, 3), {
        timeout_ms: 10_000,
      });
      expect(result.exit?.exit_code, `expected sandbox 1 to not reach sandbox 2 at ${sandbox2IP}`).not.toBe(0);
    } finally {
      await deleteSandbox(request, id1);
      await deleteSandbox(request, id2);
    }
  });

  test('backward compat: empty body still works', async ({ request }) => {
    const id = await createSandbox(request);
    try {
      const result = await exec(request, id, 'echo backward-compat-ok', { timeout_ms: 5_000 });
      expect(result.exit?.exit_code).toBe(0);
      expect(result.stdout).toContain('backward-compat-ok');
    } finally {
      await deleteSandbox(request, id);
    }
  });

  test('DNS resolution works', async ({ request }) => {
    const id = await createSandbox(request);
    try {
      const result = await exec(request, id, pyResolve('n8n.io'), { timeout_ms: 10_000 });
      expect(result.exit?.exit_code).toBe(0);
      // Should resolve to an IP address
      expect(result.stdout.trim()).toMatch(/^\d+\.\d+\.\d+\.\d+$/);
    } finally {
      await deleteSandbox(request, id);
    }
  });
});

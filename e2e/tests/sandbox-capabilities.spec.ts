import { test, expect } from '@playwright/test';
import './matchers';
import { createSandbox, deleteSandbox, exec, execWithTransientRetry } from './helpers';
import { DOCKER_ONLY } from './tags';

const addAddress = (ip: string) =>
  `sudo -n python3 -c "import fcntl,socket,struct;` +
  `s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM);` +
  `fcntl.ioctl(s,0x8916,struct.pack('16sH2s4s16s',b'eth0:1',socket.AF_INET,` +
  `b'\\x00\\x00',socket.inet_aton('${ip}'),b'\\x00'*16))"`;

const siblingOf = (ip: string) => ip.split('.').slice(0, 3).concat('250').join('.');

test.describe('Docker sandbox capability policy', DOCKER_ONLY, () => {
  test('daemon and user processes have only the allowed bounding capabilities', async () => {
    const id = await createSandbox();
    try {
      const result = await exec(
        id,
        `for pid in 1 self; do ` +
          `awk '$1 == "CapEff:" { print $2 }' /proc/$pid/status; ` +
          `awk '$1 == "CapBnd:" { print $2 }' /proc/$pid/status; ` +
          `done`,
      );
      expect(result).toHaveSucceeded();
      expect(result.stdout.trim().split(/\s+/)).toEqual([
        '0000000000000000',
        '00000000200000cb',
        '0000000000000000',
        '00000000200000cb',
      ]);
    } finally {
      await deleteSandbox(id);
    }
  });

  test('passwordless sudo can install a package', async () => {
    test.setTimeout(180_000);
    const id = await createSandbox();
    try {
      const result = await exec(
        id,
        'sudo -n apt-get update && sudo -n env DEBIAN_FRONTEND=noninteractive apt-get install -y jq && jq --version',
        { timeoutMs: 150_000 },
      );
      expect(result).toHaveSucceeded();
      expect(result.stdout).toMatch(/jq-\d+/);
    } finally {
      await deleteSandbox(id);
    }
  });

  test('cannot add a secondary address without CAP_NET_ADMIN', async () => {
    const id = await createSandbox();
    try {
      const ipResult = await execWithTransientRetry(id, 'hostname -I', { timeoutMs: 5_000 });
      expect(ipResult).toHaveSucceeded();
      const ownIP = ipResult.stdout.trim().split(/\s+/)[0];
      expect(ownIP).toMatch(/^\d+\.\d+\.\d+\.\d+$/);
      const extraIP = siblingOf(ownIP);

      const added = await exec(id, addAddress(extraIP));
      expect(added.exitCode, 'expected address addition to be denied').not.toBe(0);

      const addresses = await exec(id, 'hostname -I');
      expect(addresses).toHaveSucceeded();
      expect(addresses.stdout.split(/\s+/)).not.toContain(extraIP);
    } finally {
      await deleteSandbox(id);
    }
  });
});

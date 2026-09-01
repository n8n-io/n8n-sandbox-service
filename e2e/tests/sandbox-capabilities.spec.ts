import { test, expect } from '@playwright/test';
import './matchers';
import {
  addAddress,
  createSandbox,
  deleteSandbox,
  exec,
  execWithTransientRetry,
  siblingOf,
} from './helpers';
import { DOCKER_ONLY } from './tags';

// Every sandbox process starts as uid 1000, so nothing is effective. The
// bounding set is where a restored capability would show up.
const NO_CAPABILITIES = '0000000000000000';

test.describe('Docker sandbox capability policy', DOCKER_ONLY, () => {
  test('daemon and user processes have no capabilities at all', async () => {
    const id = await createSandbox();
    try {
      const result = await execWithTransientRetry(
        id,
        `for pid in 1 self; do ` +
          `awk '$1 == "CapEff:" { print $2 }' /proc/$pid/status; ` +
          `awk '$1 == "CapBnd:" { print $2 }' /proc/$pid/status; ` +
          `done`,
      );
      expect(result).toHaveSucceeded();
      expect(result.stdout.trim().split(/\s+/)).toEqual([
        NO_CAPABILITIES,
        NO_CAPABILITIES,
        NO_CAPABILITIES,
        NO_CAPABILITIES,
      ]);
    } finally {
      await deleteSandbox(id);
    }
  });

  // An empty bounding set alone leaves the path to root open: a setuid-root
  // binary still makes the caller uid 0, and uid 0 owns /usr/local whatever its
  // capabilities. This pins the three things that close it.
  test('has no path to root', async () => {
    const id = await createSandbox();
    try {
      const uid = await execWithTransientRetry(id, 'id -u');
      expect(uid).toHaveSucceeded();
      expect(uid.stdout.trim()).not.toBe('0');

      const escalation = await exec(
        id,
        'command -v sudo; find / -xdev -perm /4000 -type f 2>/dev/null',
        { timeoutMs: 60_000 },
      );
      expect(escalation.stdout.trim(), 'expected no sudo and no setuid binary').toBe('');

      const write = await exec(id, 'touch /usr/local/bin/escalated');
      expect(write.exitCode, 'expected a write to /usr/local/bin to be refused').not.toBe(0);
      expect(write.stderr).toMatch(/Permission denied/);
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

      // Positive control. Without it the assertion below also passes when
      // python3 fcntl is missing, hiding a capability regression.
      const plumbing = await exec(id, 'python3 -c "import fcntl"');
      expect(plumbing, 'expected python3 fcntl to be available').toHaveSucceeded();

      const added = await exec(id, addAddress(extraIP));
      expect(added.exitCode, 'expected address addition to be denied').not.toBe(0);
      // The errno separates the missing capability from other ioctl failures,
      // such as ENODEV when the interface is not named eth0.
      expect(added.stderr, 'expected the denial to come from the missing capability').toMatch(
        /PermissionError|Operation not permitted/,
      );

      const addresses = await exec(id, 'hostname -I');
      expect(addresses).toHaveSucceeded();
      expect(addresses.stdout.split(/\s+/)).not.toContain(extraIP);
    } finally {
      await deleteSandbox(id);
    }
  });
});

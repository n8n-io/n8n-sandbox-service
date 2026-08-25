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

// The bounding set the runner grants: CHOWN, DAC_OVERRIDE, FOWNER, SETGID,
// and SETUID (bits 0, 1, 3, 6, 7).
const ALLOWED_BOUNDING_SET = '00000000000000cb';

// No effective capabilities, because every sandbox process starts as uid 1000.
const NO_CAPABILITIES = '0000000000000000';

test.describe('Docker sandbox capability policy', DOCKER_ONLY, () => {
  test('daemon and user processes have only the allowed bounding capabilities', async () => {
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
        ALLOWED_BOUNDING_SET,
        NO_CAPABILITIES,
        ALLOWED_BOUNDING_SET,
      ]);
    } finally {
      await deleteSandbox(id);
    }
  });

  // iputils-ping asks setcap to give /usr/bin/ping CAP_NET_RAW. The allowlist
  // holds no CAP_SETFCAP, so setcap fails and the package installs a setuid
  // binary instead. This is the install path the capability policy changed, so
  // it is the one the suite pins.
  test('passwordless sudo can install a package that sets file capabilities', async () => {
    test.setTimeout(240_000);
    const id = await createSandbox();
    try {
      const updated = await execWithTransientRetry(id, 'sudo -n apt-get update', {
        timeoutMs: 60_000,
      });
      expect(updated).toHaveSucceeded();

      const installed = await exec(
        id,
        'sudo -n env DEBIAN_FRONTEND=noninteractive apt-get install -y iputils-ping && ls -l /usr/bin/ping',
        { timeoutMs: 150_000 },
      );
      expect(installed).toHaveSucceeded();
      expect(installed.stdout, 'expected the setuid fallback for /usr/bin/ping').toMatch(/^-rws/m);
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
      // passwordless sudo or python3 fcntl is missing, which hides a
      // capability regression instead of reporting one.
      const plumbing = await exec(id, 'sudo -n python3 -c "import fcntl"');
      expect(plumbing, 'expected sudo -n and python3 fcntl to be available').toHaveSucceeded();

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

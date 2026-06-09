import { test, expect } from '@playwright/test';
import { createSandbox, deleteSandbox, exec } from './helpers';
import { RUNNER_TAGS } from './tags';

// The runner is always configured with per-sandbox disk quotas (see
// e2e/run.sh). On hosts whose kernel lacks CONFIG_XFS_QUOTA — notably Docker
// Desktop's linuxkit on macOS — the runner falls back to no enforcement at
// startup and sandboxes still run, just unbounded.
//
// Set E2E_VERIFY_DISK_QUOTA=true to assert that quotas actually enforce on
// this host.
test.describe('Disk quota enforcement', { tag: RUNNER_TAGS.docker }, () => {
  test.skip(
    process.env.E2E_VERIFY_DISK_QUOTA !== 'true',
    'set E2E_VERIFY_DISK_QUOTA=true to run (requires host kernel with CONFIG_XFS_QUOTA)',
  );

  test('writing past per-sandbox quota fails with ENOSPC', async () => {
    // e2e/run.sh sets SANDBOX_RUNNER_DEFAULT_DISK_QUOTA_MB=50, so a 100 MB
    // write must run out of space partway through.
    const id = await createSandbox();
    try {
      const result = await exec(
        id,
        'dd if=/dev/zero of=/home/user/big.bin bs=1M count=100 2>&1 || true',
      );
      expect(result.stdout + result.stderr).toMatch(/no space left on device/i);
    } finally {
      await deleteSandbox(id);
    }
  });
});

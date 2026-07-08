import { test, expect } from '@playwright/test';
import { createSandbox, deleteSandbox, exec } from './helpers';
import { RUNNER_TAGS } from './tags';

// Docker: per-sandbox writable-layer quota via xfs prjquota (see e2e/run.sh).
// On hosts whose kernel lacks CONFIG_XFS_QUOTA — notably Docker Desktop's
// linuxkit on macOS — the runner falls back to no enforcement at startup.
//
// Set E2E_VERIFY_DISK_QUOTA=true to assert xfs quota enforcement on Docker.
test.describe('Docker disk quota enforcement', { tag: RUNNER_TAGS.docker }, () => {
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

// Firecracker: disk is the whole per-sandbox ext4 image (sparse clone of the
// golden template). No runtime SANDBOX_RUNNER_DEFAULT_DISK_QUOTA_MB — capacity
// is fixed at golden-build time (1 GiB in e2e). fallocate fails fast without
// writing a multi-GB file.
test.describe('Firecracker rootfs capacity', { tag: RUNNER_TAGS.firecracker }, () => {
  test('allocating past ext4 image size fails with ENOSPC', async () => {
    const id = await createSandbox();
    try {
      const result = await exec(
        id,
        'fallocate -l 2G /home/user/big.bin 2>&1 || true',
      );
      expect(result.stdout + result.stderr).toMatch(/no space left on device/i);
    } finally {
      await deleteSandbox(id);
    }
  });
});

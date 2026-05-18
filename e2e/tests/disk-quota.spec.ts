import { test, expect } from '@playwright/test';
import { createSandbox, deleteSandbox, exec, dockerOutput } from './helpers';

// Real enforcement depends on the host kernel having CONFIG_XFS_QUOTA, which
// is absent on Docker Desktop's linuxkit kernel. The runner logs whether the
// quota pool mounted; we read that line once and skip if it says DISABLED so
// macOS dev keeps a clean run rather than asserting fallback behavior here.
test.describe('Disk quota enforcement', () => {
  test.beforeAll(() => {
    test.skip(
      !process.env.E2E_RUNNER_CONTAINER_NAME,
      'needs E2E_RUNNER_CONTAINER_NAME (from e2e/run.sh)',
    );
    const logs = dockerOutput(['logs', process.env.E2E_RUNNER_CONTAINER_NAME!]);
    test.skip(
      !logs.includes('disk quota enforcement: ENABLED'),
      'host kernel lacks CONFIG_XFS_QUOTA — quota enforcement unavailable',
    );
  });

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

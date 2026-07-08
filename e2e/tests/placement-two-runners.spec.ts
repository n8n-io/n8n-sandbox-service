import { test, expect } from '@playwright/test';
import { createSandboxWithRetry, deleteSandbox, execWithTransientRetry } from './helpers';
import { BOTH_RUNNERS } from './tags';

// Run from e2e/run-two-runners.sh (Docker) or e2e/run-firecracker-two-runners-azure.sh.

test('sandboxes stay on the correct runner', BOTH_RUNNERS, async () => {
  const id1 = await createSandboxWithRetry();
  const id2 = await createSandboxWithRetry();

  try {
    await execWithTransientRetry(id1, "printf '%s' 'A' > /tmp/placement-marker");
    await execWithTransientRetry(id2, "printf '%s' 'B' > /tmp/placement-marker");

    const r1 = await execWithTransientRetry(id1, 'cat /tmp/placement-marker');
    const r2 = await execWithTransientRetry(id2, 'cat /tmp/placement-marker');

    expect(r1.stdout.trim()).toBe('A');
    expect(r2.stdout.trim()).toBe('B');
  } finally {
    await deleteSandbox(id1);
    await deleteSandbox(id2);
  }
});

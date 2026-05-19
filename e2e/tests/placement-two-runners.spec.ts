import { test, expect } from '@playwright/test';
import { createSandboxWithRetry, deleteSandbox, exec } from './helpers';

// Run only from e2e/run-two-runners.sh (two registered runners).

test('sandboxes stay on the correct runner', async () => {
  const id1 = await createSandboxWithRetry();
  const id2 = await createSandboxWithRetry();

  try {
    await exec(id1, "printf '%s' 'A' > /tmp/placement-marker");
    await exec(id2, "printf '%s' 'B' > /tmp/placement-marker");

    const r1 = await exec(id1, 'cat /tmp/placement-marker');
    const r2 = await exec(id2, 'cat /tmp/placement-marker');

    expect(r1.stdout.trim()).toBe('A');
    expect(r2.stdout.trim()).toBe('B');
  } finally {
    await deleteSandbox(id1);
    await deleteSandbox(id2);
  }
});

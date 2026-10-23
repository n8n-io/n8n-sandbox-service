import { expect, test } from '@playwright/test';
import './matchers';
import {
  createSandbox,
  deleteSandbox,
  execWithTransientRetry,
  stopSandboxViaRunner,
} from './helpers';
test.describe.configure({ timeout: 120_000 });

// The suite runs on the sandbox host (or an NTP-synced peer), so a guest's wall
// clock has to fall inside the window around the exec that reads it. A
// Firecracker guest restored from a snapshot keeps the clock it had when the
// snapshot was taken unless the runner asks Firecracker to advance it and the
// guest keeps time with kvm-clock. Whether the guest kernel would pick tsc
// instead depends on the host's VM family, so run this on the runners' family.
const TOLERANCE_MS = 5_000;
const STOPPED_FOR_MS = 15_000;

// How far the guest clock lies outside [before, after]; 0 when inside.
async function guestClockSkewMs(id: string): Promise<number> {
  const before = Date.now();
  const res = await execWithTransientRetry(id, 'date +%s%3N');
  const after = Date.now();
  expect(res).toHaveSucceeded();
  const guest = Number(res.stdout.trim());
  expect(Number.isFinite(guest), `guest date output: ${res.stdout}`).toBe(true);
  if (guest < before) return guest - before;
  if (guest > after) return guest - after;
  return 0;
}

test.describe('sandbox clock', () => {
  test('a new sandbox has the current time', async () => {
    const id = await createSandbox();
    try {
      const skew = await guestClockSkewMs(id);
      expect(Math.abs(skew), `guest clock off by ${skew} ms`).toBeLessThan(TOLERANCE_MS);
    } finally {
      await deleteSandbox(id);
    }
  });

  test('a woken sandbox has the current time, not the time it was stopped', async () => {
    const id = await createSandbox();
    try {
      await execWithTransientRetry(id, 'true');
      stopSandboxViaRunner(id);
      await new Promise((r) => setTimeout(r, STOPPED_FOR_MS));

      const skew = await guestClockSkewMs(id);
      expect(Math.abs(skew), `guest clock off by ${skew} ms`).toBeLessThan(TOLERANCE_MS);
    } finally {
      await deleteSandbox(id);
    }
  });
});

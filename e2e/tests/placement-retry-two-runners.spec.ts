import { test, expect } from '@playwright/test';
import './matchers';
import { adminClient, execWithTransientRetry } from './helpers';
import {
  activeOn,
  burst,
  createdIds,
  deleteAll,
  placements,
  placementsSince,
  settle,
  statuses,
} from './capacity-helpers';
import { FIRECRACKER_ONLY } from './tags';

// Run from e2e/run-firecracker-two-runners.sh. Firecracker only, because a
// Docker runner never refuses a create.
test.describe('Placement retry across runners', FIRECRACKER_ONLY, () => {
  const addrs = [process.env.E2E_RUNNER1_HTTP_ADDR ?? '', process.env.E2E_RUNNER2_HTTP_ADDR ?? ''];
  const capacities = [
    Number(process.env.E2E_RUNNER1_CAPACITY_TOTAL ?? 0),
    Number(process.env.E2E_RUNNER2_CAPACITY_TOTAL ?? 0),
  ];

  test('a create refused by a full runner is placed on the other one', async () => {
    test.skip(
      addrs.some((a) => !a) || capacities.some((c) => !c),
      'needs the two-runner harness (e2e/run-firecracker-two-runners-azure.sh)',
    );
    test.setTimeout(300_000);

    let retried = false;
    for (let round = 1; round <= 3 && !retried; round++) {
      expect(await settle(addrs), 'runners should start empty').toEqual([0, 0]);

      // Both report the same load, so every pick in the burst goes to the same
      // runner. It takes what it has room for and refuses the rest, which the API
      // places on the other runner.
      const n = Math.max(...capacities) + 1;
      const before = await placements();
      const answers = await burst(n);
      const ids = createdIds(answers);
      try {
        expect(statuses(answers)).toEqual(Array(n).fill(201));
        const active = addrs.map(activeOn);
        // Nothing left behind by the refusals.
        expect(active[0] + active[1]).toBe(n);

        const delta = await placementsSince(before);
        expect(delta.first_choice + delta.retried).toBe(n);
        retried = delta.retried > 0;
        if (!retried) {
          continue;
        }
        expect(
          active.some((a, i) => a === capacities[i]),
          `one runner should be full: active ${active}, capacities ${capacities}`,
        ).toBe(true);
        const admin = adminClient();
        for (const id of ids) {
          expect(await execWithTransientRetry(id, 'echo placed', undefined, admin)).toHaveSucceeded();
        }
      } finally {
        await deleteAll(ids);
      }
    }
    expect(retried, 'every burst was placed without a refusal').toBe(true);
  });
});

import { test, expect } from '@playwright/test';
import { SandboxClient } from '@n8n/sandbox-client';
import { ADMIN_API_KEY, BASE_URL, adminClient, deleteSandbox } from './helpers';
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

// Run from e2e/run-firecracker.sh: one runner with E2E_RUNNER_CAPACITY_TOTAL
// slots. Firecracker only, because a Docker runner never refuses a create.
test.describe('Runner at capacity', FIRECRACKER_ONLY, () => {
  const addr = process.env.E2E_RUNNER_HTTP_ADDR ?? '';
  const capacity = Number(process.env.E2E_RUNNER_CAPACITY_TOTAL ?? 0);

  test.beforeEach(() => {
    test.skip(
      !addr || !capacity,
      'needs E2E_RUNNER_HTTP_ADDR and E2E_RUNNER_CAPACITY_TOTAL (from e2e/run-firecracker.sh)',
    );
  });

  test('creates past capacity get a retryable 503 and leave nothing behind', async () => {
    test.setTimeout(300_000);

    let refusedByRunner = false;
    for (let round = 1; round <= 3 && !refusedByRunner; round++) {
      const [active] = await settle([addr]);
      const free = capacity - active;
      expect(free, `runner has no free slot (${active}/${capacity} in use)`).toBeGreaterThan(0);

      const before = await placements();
      const answers = await burst(free + 2);
      const ids = createdIds(answers);
      try {
        expect(statuses(answers)).toEqual([...Array(free).fill(201), 503, 503]);
        for (const refused of answers.filter((a) => a.status === 503)) {
          expect(refused.retryAfter).toBe('1');
          expect(refused.body.reason).toBe('no_capacity');
        }
        // The refused creates hold no slot.
        expect(activeOn(addr)).toBe(capacity);

        const delta = await placementsSince(before);
        expect(delta.first_choice).toBe(free);
        // exhausted: the runner refused. no_runner: its heartbeat reported it full
        // first, so the API refused without asking it.
        expect(delta.exhausted + delta.no_runner).toBe(2);
        refusedByRunner = delta.exhausted > 0;
      } finally {
        await deleteAll(ids);
      }
    }
    expect(refusedByRunner, 'every burst was refused by the API before reaching the runner').toBe(true);
  });

  test('the SDK repeats an anonymous create until a slot frees up', async () => {
    test.setTimeout(300_000);

    const [active] = await settle([addr]);
    expect(active, 'runner is already full').toBeLessThan(capacity);
    let ids = createdIds(await burst(capacity - active));
    try {
      expect(ids).toHaveLength(capacity - active);

      // Retries span 19s, longer than the heartbeat that tells the API a slot is free.
      const client = new SandboxClient({
        baseUrl: BASE_URL,
        apiKey: ADMIN_API_KEY,
        retry: { attempts: 6, baseDelayMs: 1000, maxDelayMs: 4000, jitter: false },
      });
      const before = await placements();
      const creating = client.createSandbox({ egress: 'public' });
      await new Promise((r) => setTimeout(r, 500));
      const [freed, ...rest] = ids;
      ids = rest;
      await deleteSandbox(freed, adminClient());

      const record = await creating;
      ids.push(record.id);
      const delta = await placementsSince(before);
      expect(delta.exhausted + delta.no_runner, 'the create was never refused').toBeGreaterThan(0);
    } finally {
      await deleteAll(ids);
    }
  });
});

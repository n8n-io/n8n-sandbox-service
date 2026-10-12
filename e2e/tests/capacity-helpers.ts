// Helpers for the specs that fill runners to capacity.
//
// A runner reports its slot count to the API every 10s, and the API only stops
// picking a full runner once that report arrives. Before then, the runner itself
// refuses the create, which is the path the API retries on another runner. So:
// - settle() waits until counts have been still for longer than one report, so
//   the API's view is current when a test starts;
// - burst() sends creates all at once, so every pick happens before the next
//   report can say the runner filled up.
// A report can still land mid-burst, rarely. The specs assert what holds either
// way and repeat the burst until the placement metrics show the runner refused.
import { ADMIN_API_KEY, BASE_URL, adminClient, deleteSandbox, scrapeRunnerMetricsAt } from './helpers';
import { parseCounter, parseGauge } from './metrics-helpers';

const HEARTBEAT_SETTLE_MS = 11_000;

export type CreateAnswer = {
  status: number;
  retryAfter: string | null;
  body: { id?: string; error?: string; reason?: string };
};

/** POST /sandboxes with the admin key and no id. Plain fetch, so no SDK retry hides the status. */
export async function postCreate(): Promise<CreateAnswer> {
  const res = await fetch(`${BASE_URL}/sandboxes`, {
    method: 'POST',
    headers: { 'X-Api-Key': ADMIN_API_KEY, 'Content-Type': 'application/json' },
    body: JSON.stringify({ egress: 'public' }),
  });
  return { status: res.status, retryAfter: res.headers.get('retry-after'), body: await res.json() };
}

/** Sends n creates at once. */
export function burst(n: number): Promise<CreateAnswer[]> {
  return Promise.all(Array.from({ length: n }, () => postCreate()));
}

export function createdIds(answers: CreateAnswer[]): string[] {
  return answers.flatMap((a) => (a.status === 201 && a.body.id ? [a.body.id] : []));
}

export function statuses(answers: CreateAnswer[]): number[] {
  return answers.map((a) => a.status).sort((a, b) => a - b);
}

export async function deleteAll(ids: string[]): Promise<void> {
  const admin = adminClient();
  await Promise.all(ids.map((id) => deleteSandbox(id, admin)));
}

/** Slot-blocking sandboxes on the runner whose HTTPS listener is at addr. */
export function activeOn(addr: string): number {
  return parseGauge(scrapeRunnerMetricsAt(addr), 'sandbox_containers_active');
}

/**
 * Waits until the slot counts of the runners at addrs have not changed for longer
 * than a heartbeat, and returns them.
 */
export async function settle(addrs: string[], timeoutMs = 90_000): Promise<number[]> {
  const deadline = Date.now() + timeoutMs;
  let counts = addrs.map(activeOn);
  let stillSince = Date.now();
  while (Date.now() - stillSince < HEARTBEAT_SETTLE_MS) {
    if (Date.now() > deadline) {
      throw new Error(`runner slot counts did not settle within ${timeoutMs}ms: ${counts}`);
    }
    await new Promise((r) => setTimeout(r, 1000));
    const next = addrs.map(activeOn);
    if (next.some((n, i) => n !== counts[i])) {
      counts = next;
      stillSince = Date.now();
    }
  }
  return counts;
}

/** sandbox_create_placements_total by result. */
export type Placements = { first_choice: number; retried: number; exhausted: number; no_runner: number };

export async function placements(): Promise<Placements> {
  const res = await fetch(`${BASE_URL}/metrics`);
  if (!res.ok) {
    throw new Error(`GET /metrics: ${res.status}`);
  }
  const body = await res.text();
  const count = (result: string) =>
    parseCounter(body, 'sandbox_create_placements_total', { role: 'api', result });
  return {
    first_choice: count('first_choice'),
    retried: count('retried'),
    exhausted: count('exhausted'),
    no_runner: count('no_runner'),
  };
}

/** Placements recorded since before was taken. */
export async function placementsSince(before: Placements): Promise<Placements> {
  const now = await placements();
  return {
    first_choice: now.first_choice - before.first_choice,
    retried: now.retried - before.retried,
    exhausted: now.exhausted - before.exhausted,
    no_runner: now.no_runner - before.no_runner,
  };
}

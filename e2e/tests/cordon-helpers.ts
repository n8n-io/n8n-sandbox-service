import { expect } from '@playwright/test';
import { BASE_URL, SCALER_API_KEY } from './helpers';

type AdminRunner = { id: string; cordoned: boolean; last_seen: number };

/** Runners that heartbeated recently, as GET /admin/runners reports them. */
export async function liveRunners(): Promise<AdminRunner[]> {
  const res = await fetch(`${BASE_URL}/admin/runners`, { headers: { 'X-Api-Key': SCALER_API_KEY } });
  expect(res.status).toBe(200);
  const body = (await res.json()) as { runners: AdminRunner[] };
  const now = Date.now() / 1000;
  return body.runners.filter((runner) => now - runner.last_seen < 45);
}

/** Cordons or uncordons a runner through the API with the scaler key. */
export async function setCordon(id: string, cordoned: boolean): Promise<boolean> {
  const res = await fetch(`${BASE_URL}/admin/runners/${encodeURIComponent(id)}/cordon`, {
    method: cordoned ? 'PUT' : 'DELETE',
    headers: { 'X-Api-Key': SCALER_API_KEY },
  });
  const body = (await res.json()) as { id: string; cordoned: boolean };
  expect(res.status, JSON.stringify(body)).toBe(200);
  expect(body.id).toBe(id);
  return body.cordoned;
}

export async function waitForAdminCordoned(want: boolean, timeoutMs: number): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    const runners = await liveRunners();
    if (runners.length > 0 && runners.every((runner) => runner.cordoned === want)) return;
    if (Date.now() > deadline) {
      throw new Error(`/admin/runners did not report cordoned=${want} within ${timeoutMs}ms: ${JSON.stringify(runners)}`);
    }
    await new Promise((resolve) => setTimeout(resolve, 200));
  }
}

import { delay } from "./delay";
import type { HttpClient } from "./http";
import { EgressMismatchError, SandboxServiceError } from "./errors";
import type { CreateSandboxOptions, SandboxRecord, SandboxWireResponse } from "./types";

/** Reason of a 503 for a create no runner had capacity for. Nothing was created. */
const NO_CAPACITY_REASON = "no_capacity";

export async function createSandbox(
  http: HttpClient,
  options: CreateSandboxOptions,
): Promise<SandboxRecord> {
  const post = () =>
    http.requestJson<SandboxWireResponse>("POST", "/sandboxes", {
      data: { id: options.id, ephemeral: options.ephemeral, egress: options.egress },
      // Only a caller-supplied id makes a repeated POST idempotent; retrying an
      // anonymous create would provision a second sandbox.
      isSafeToRetry: options.id !== undefined,
    });
  // The client already retries a create with an id on any 503.
  const response = options.id === undefined ? await retryWhileNoCapacity(http, post) : await post();
  const record = mapSandboxRecord(response);
  // An API from before egress modes ignores the field and reports none, read as
  // public; a current one refuses a reconnect in another mode itself (409).
  if (record.egress !== options.egress) {
    throw new EgressMismatchError(record.id, options.egress, record.egress);
  }
  return record;
}

export async function getSandbox(http: HttpClient, id: string): Promise<SandboxRecord> {
  const response = await http.requestJson<SandboxWireResponse>("GET", `/sandboxes/${id}`);
  return mapSandboxRecord(response);
}

/** Deletes a sandbox. Treats 404 as success so retries after a dropped 204 stay idempotent. */
export async function deleteSandbox(http: HttpClient, id: string): Promise<void> {
  try {
    await http.requestVoid("DELETE", `/sandboxes/${id}`);
  } catch (err) {
    if (err instanceof SandboxServiceError && err.status === 404) return;
    throw err;
  }
}

/**
 * Repeats an anonymous create while the service answers that no runner had capacity.
 * That 503 guarantees nothing was created, so repeating the create cannot provision
 * a second sandbox. The retry policy decides how often and how long to wait.
 */
async function retryWhileNoCapacity<T>(http: HttpClient, post: () => Promise<T>): Promise<T> {
  for (let attempt = 0; ; attempt++) {
    try {
      return await post();
    } catch (error) {
      if (!isNoCapacity(error)) throw error;
      const wait = http.retryDelayFor(error, attempt);
      if (wait === undefined) throw error;
      await delay(wait);
    }
  }
}

function isNoCapacity(error: unknown): error is SandboxServiceError {
  return (
    error instanceof SandboxServiceError &&
    error.status === 503 &&
    error.reason === NO_CAPACITY_REASON
  );
}

function mapSandboxRecord(wire: SandboxWireResponse): SandboxRecord {
  const record: SandboxRecord = {
    id: wire.id,
    status: wire.status,
    createdAt: wire.created_at,
    lastActiveAt: wire.last_active_at,
    ephemeral: wire.ephemeral === true,
    egress: wire.egress ?? "public",
  };
  // Left out rather than set to undefined so `toEqual` comparisons and JSON
  // output of records from older services stay unchanged.
  if (wire.tenant_id !== undefined) record.tenantId = wire.tenant_id;
  return record;
}

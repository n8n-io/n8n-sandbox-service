import type { HttpClient } from "./http";
import { EgressMismatchError, SandboxServiceError } from "./errors";
import type { CreateSandboxOptions, SandboxRecord, SandboxWireResponse } from "./types";

export async function createSandbox(
  http: HttpClient,
  options: CreateSandboxOptions,
): Promise<SandboxRecord> {
  const response = await http.requestJson<SandboxWireResponse>("POST", "/sandboxes", {
    data: { id: options.id, ephemeral: options.ephemeral, egress: options.egress },
    // Only a caller-supplied id makes a repeated POST idempotent; retrying an
    // anonymous create would provision a second sandbox.
    isSafeToRetry: options.id !== undefined,
  });
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

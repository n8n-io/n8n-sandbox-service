import type { HttpClient } from "./http";
import { SandboxServiceError } from "./errors";
import type { SandboxRecord, SandboxWireResponse } from "./types";

export async function createSandbox(http: HttpClient): Promise<SandboxRecord> {
  const response = await http.requestJson<SandboxWireResponse>("POST", "/sandboxes");
  return mapSandboxRecord(response);
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
  return {
    id: wire.id,
    status: wire.status,
    createdAt: wire.created_at,
    lastActiveAt: wire.last_active_at,
  };
}

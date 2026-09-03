import type { HttpClient } from "./http";
import { SandboxServiceError } from "./errors";
import type { CreateSandboxOptions, SandboxRecord, SandboxWireResponse } from "./types";

export async function createSandbox(
  http: HttpClient,
  options?: CreateSandboxOptions,
): Promise<SandboxRecord> {
  const data: Record<string, unknown> = {};
  if (options?.id !== undefined) data.id = options.id;
  if (options?.ephemeral !== undefined) data.ephemeral = options.ephemeral;

  const response =
    Object.keys(data).length > 0
      ? await http.requestJson<SandboxWireResponse>("POST", "/sandboxes", {
          data,
          // Only a caller-supplied id makes a repeated POST idempotent; retrying
          // an anonymous create would provision a second sandbox.
          isSafeToRetry: options?.id !== undefined,
        })
      : await http.requestJson<SandboxWireResponse>("POST", "/sandboxes");
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
    ephemeral: wire.ephemeral === true,
  };
}

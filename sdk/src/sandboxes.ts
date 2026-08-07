import type { HttpClient } from "./http";
import type { CreateSandboxOptions, SandboxRecord, SandboxWireResponse } from "./types";

export async function createSandbox(
  http: HttpClient,
  options?: CreateSandboxOptions,
): Promise<SandboxRecord> {
  const response =
    options?.id !== undefined
      ? await http.requestJson<SandboxWireResponse>("POST", "/sandboxes", {
          data: { id: options.id },
          isSafeToRetry: true,
        })
      : await http.requestJson<SandboxWireResponse>("POST", "/sandboxes");
  return mapSandboxRecord(response);
}

export async function getSandbox(http: HttpClient, id: string): Promise<SandboxRecord> {
  const response = await http.requestJson<SandboxWireResponse>("GET", `/sandboxes/${id}`);
  return mapSandboxRecord(response);
}

export async function deleteSandbox(http: HttpClient, id: string): Promise<void> {
  await http.requestVoid("DELETE", `/sandboxes/${id}`);
}

function mapSandboxRecord(wire: SandboxWireResponse): SandboxRecord {
  return {
    id: wire.id,
    status: wire.status,
    createdAt: wire.created_at,
    lastActiveAt: wire.last_active_at,
  };
}

import type { HttpClient } from "./http";
import type { CreateSandboxOptions, SandboxRecord, SandboxWireResponse } from "./types";

export async function createSandbox(
  http: HttpClient,
  options: CreateSandboxOptions = {},
): Promise<SandboxRecord> {
  const body: Record<string, unknown> = {};

  const steps = options.dockerfile?.build();
  if (steps?.length) {
    body.dockerfile_steps = steps;
  }

  if (options.networkPolicy) {
    const np: Record<string, string[]> = {};
    if (options.networkPolicy.allowedIps) np.allowed_ips = options.networkPolicy.allowedIps;
    if (options.networkPolicy.deniedIps) np.denied_ips = options.networkPolicy.deniedIps;
    body.network_policy = np;
  }

  if (options.resourceLimits) {
    const rl: Record<string, number> = {};
    if (options.resourceLimits.memoryMb !== undefined)
      rl.memory_mb = options.resourceLimits.memoryMb;
    if (options.resourceLimits.cpuPercent !== undefined)
      rl.cpu_percent = options.resourceLimits.cpuPercent;
    if (options.resourceLimits.pidsMax !== undefined) rl.pids_max = options.resourceLimits.pidsMax;
    body.resource_limits = rl;
  }

  const response = await http.requestJson<SandboxWireResponse>("POST", "/sandboxes", {
    data: Object.keys(body).length > 0 ? body : undefined,
  });

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
    provider: wire.provider,
    imageId: wire.image_id ?? "",
    createdAt: wire.created_at,
    lastActiveAt: wire.last_active_at,
  };
}

import { delay } from "./delay";
import type { HttpClient } from "./http";
import { CreateOptionMismatchError, EgressMismatchError, SandboxServiceError } from "./errors";
import type {
  CreateSandboxOptions,
  EgressProxySpec,
  IdleWindowsUpdate,
  SandboxRecord,
  SandboxWireResponse,
  TemplateInfo,
} from "./types";

/** Reason of a 503 for a create no runner had capacity for. Nothing was created. */
const NO_CAPACITY_REASON = "no_capacity";

export async function createSandbox(
  http: HttpClient,
  options: CreateSandboxOptions,
): Promise<SandboxRecord> {
  const post = () =>
    http.requestJson<SandboxWireResponse>("POST", "/sandboxes", {
      data: {
        id: options.id,
        ephemeral: options.ephemeral,
        egress: options.egress,
        egress_proxy: options.egressProxy,
        template: options.template,
        idle_stop_after: options.idleStopAfter,
        idle_delete_after: options.idleDeleteAfter,
      },
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
  // A current API refuses a mismatch itself (409); an older one ignores the
  // option and reports nothing for it.
  checkOption(record.id, "template", options.template || undefined, record.template ?? "default");
  checkOption(record.id, "idle_stop_after", options.idleStopAfter, record.idleStopAfter);
  checkOption(record.id, "idle_delete_after", options.idleDeleteAfter, record.idleDeleteAfter);
  return record;
}

function checkOption(
  sandboxId: string,
  option: string,
  requested: string | number | undefined,
  actual: string | number | undefined,
): void {
  if (requested !== undefined && requested !== actual) {
    throw new CreateOptionMismatchError(sandboxId, option, String(requested), String(actual));
  }
}

export async function getSandbox(http: HttpClient, id: string): Promise<SandboxRecord> {
  const response = await http.requestJson<SandboxWireResponse>("GET", `/sandboxes/${id}`);
  return mapSandboxRecord(response);
}

/** Changes a sandbox's idle windows; the same body twice leaves the same result, so it is retried. */
export async function updateIdleWindows(
  http: HttpClient,
  id: string,
  update: IdleWindowsUpdate,
): Promise<SandboxRecord> {
  const response = await http.requestJson<SandboxWireResponse>("PATCH", `/sandboxes/${id}`, {
    data: { idle_stop_after: update.idleStopAfter, idle_delete_after: update.idleDeleteAfter },
    isSafeToRetry: true,
  });
  return mapSandboxRecord(response);
}

/** Replaces a proxy sandbox's egress spec, for requests made afterwards. */
export async function updateEgressProxy(
  http: HttpClient,
  id: string,
  spec: EgressProxySpec,
): Promise<void> {
  await http.requestVoid("PUT", `/sandboxes/${id}/egress-proxy`, {
    data: spec,
    isSafeToRetry: true,
  });
}

/** Reports whether a proxy sandbox's model requests are blocked. */
export async function getModelEgress(http: HttpClient, id: string): Promise<boolean> {
  const response = await http.requestJson<{ blocked: boolean }>(
    "GET",
    `/sandboxes/${id}/model-egress`,
  );
  return response.blocked;
}

/** Blocks or unblocks a proxy sandbox's model requests (budget cut-off). */
export async function setModelEgress(
  http: HttpClient,
  id: string,
  blocked: boolean,
): Promise<boolean> {
  const response = await http.requestJson<{ blocked: boolean }>(
    "PUT",
    `/sandboxes/${id}/model-egress`,
    {
      data: { blocked },
      isSafeToRetry: true,
    },
  );
  return response.blocked;
}

/** Lists the template catalog. */
export async function listTemplates(http: HttpClient): Promise<TemplateInfo[]> {
  const response = await http.requestJson<{
    templates: {
      name: string;
      vcpus: number;
      memory_mb: number;
      disk_mb: number;
      runners: number;
    }[];
  }>("GET", "/templates", { isSafeToRetry: true });
  return response.templates.map((t) => ({
    name: t.name,
    vcpus: t.vcpus,
    memoryMb: t.memory_mb,
    diskMb: t.disk_mb,
    runners: t.runners,
  }));
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
  if (wire.template !== undefined) record.template = wire.template;
  if (wire.idle_stop_after !== undefined) record.idleStopAfter = wire.idle_stop_after;
  if (wire.idle_delete_after !== undefined) record.idleDeleteAfter = wire.idle_delete_after;
  if (wire.tenant_id !== undefined) record.tenantId = wire.tenant_id;
  return record;
}

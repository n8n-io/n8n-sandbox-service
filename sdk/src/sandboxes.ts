import { delay } from "./delay";
import type { HttpClient } from "./http";
import { EgressMismatchError, SandboxServiceError } from "./errors";
import type {
  AttachedSandboxVolume,
  CreateSandboxOptions,
  LeftOutSandboxVolume,
  SandboxRecord,
  SandboxVolume,
  SandboxVolumeWire,
  SandboxWireResponse,
} from "./types";

const NO_CAPACITY_REASON = "no_capacity";
const VOLUME_STORE_UNAVAILABLE_REASON = "volume_store_unavailable";

export async function createSandbox(
  http: HttpClient,
  options: CreateSandboxOptions,
): Promise<SandboxRecord> {
  const hasVolumes = (options.volumes?.length ?? 0) > 0;
  const post = () =>
    http.requestJson<SandboxWireResponse>("POST", "/sandboxes", {
      data: {
        id: options.id,
        ephemeral: options.ephemeral,
        egress: options.egress,
        volumes: options.volumes?.map((volume) => ({
          name: volume.name,
          digest: volume.digest,
          mount_path: volume.mountPath,
          optional: volume.optional,
        })),
      },
      // Only a caller-supplied id makes a repeated POST idempotent; retrying an
      // anonymous create would provision a second sandbox.
      retry: options.id !== undefined ? "always" : "idempotent",
    });
  // The client already retries a create with an id on any 503.
  const response =
    options.id === undefined ? await retryAnonymousCreate(http, post, hasVolumes) : await post();
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
 * Repeats an anonymous create while the service answers with a 503 whose reason
 * guarantees nothing was created, so repeating cannot provision a second sandbox.
 * The retry policy decides how often and how long to wait.
 */
async function retryAnonymousCreate<T>(
  http: HttpClient,
  post: () => Promise<T>,
  hasVolumes: boolean,
): Promise<T> {
  for (let attempt = 0; ; attempt++) {
    try {
      return await post();
    } catch (error) {
      if (!isRetryableCreateError(error, hasVolumes)) throw error;
      const wait = http.retryDelayFor(error, attempt);
      if (wait === undefined) throw error;
      await delay(wait);
    }
  }
}

function isRetryableCreateError(error: unknown, hasVolumes: boolean): error is SandboxServiceError {
  if (!(error instanceof SandboxServiceError) || error.status !== 503) return false;
  return (
    error.reason === NO_CAPACITY_REASON ||
    (hasVolumes && error.reason === VOLUME_STORE_UNAVAILABLE_REASON)
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
  if (Array.isArray(wire.volumes)) record.volumes = wire.volumes.map(mapSandboxVolume);
  return record;
}

function mapSandboxVolume(wire: SandboxVolumeWire): SandboxVolume {
  if (wire.attached === true) {
    const volume: AttachedSandboxVolume = {
      digest: wire.digest ?? "",
      scope: wire.scope ?? "",
      mountPath: wire.mount_path,
      optional: wire.optional === true,
      attached: true,
    };
    if (wire.name !== undefined) volume.name = wire.name;
    return volume;
  }
  const volume: LeftOutSandboxVolume = {
    mountPath: wire.mount_path,
    optional: wire.optional === true,
    attached: false,
    reason: wire.reason ?? "error",
  };
  if (wire.name !== undefined) volume.name = wire.name;
  if (wire.digest !== undefined) volume.digest = wire.digest;
  if (wire.scope !== undefined) volume.scope = wire.scope;
  return volume;
}

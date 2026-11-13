import type { HttpClient } from "./http";
import type { CodeSandbox, CreateCodeSandboxOptions } from "./types";

/** Returns the current code sandbox of `options.name`, or creates one. */
export async function createCodeSandbox(
  http: HttpClient,
  options: CreateCodeSandboxOptions,
): Promise<CodeSandbox> {
  return http.requestJson<CodeSandbox>("POST", "/code/sandboxes", {
    data: {
      name: options.name,
      runtime: options.runtime,
      runtimeVersion: options.runtimeVersion,
      slots: options.slots,
      memoryMb: options.memoryMb,
      idleTimeoutMs: options.idleTimeoutMs,
      maxAgeMs: options.maxAgeMs,
    },
    // Idempotent on the name: a repeated create returns the same sandbox.
    isSafeToRetry: true,
  });
}

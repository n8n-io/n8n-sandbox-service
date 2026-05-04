export class SandboxServiceError extends Error {
  constructor(
    message: string,
    readonly status: number,
    readonly code?: number,
  ) {
    super(message);
    this.name = "SandboxServiceError";
  }
}

export function createErrorFromResponse(status: number, data: unknown): SandboxServiceError {
  if (typeof data === "object" && data !== null && "error" in data) {
    const payload = data as { error: string; code?: number };
    return new SandboxServiceError(payload.error, status, payload.code);
  }

  const message =
    typeof data === "string" && data.length > 0
      ? data
      : `Sandbox service request failed with status ${status}`;

  return new SandboxServiceError(message, status);
}

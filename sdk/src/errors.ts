/**
 * Error thrown when the sandbox service returns a failed response.
 */
export class SandboxServiceError extends Error {
  /**
   * Creates a sandbox service error with HTTP status and optional API error code.
   */
  constructor(
    message: string,
    readonly status: number,
    readonly code?: number,
  ) {
    super(message);
    this.name = "SandboxServiceError";
  }
}

/**
 * Normalizes a sandbox service error response into a typed error instance.
 */
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

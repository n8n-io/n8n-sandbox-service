import { describe, expect, it } from "vitest";
import {
  createErrorFromResponse,
  SandboxCrashedError,
  SandboxServiceError,
} from "../src/errors.js";

describe("createErrorFromResponse", () => {
  it("takes the message and code from a well-formed error body", () => {
    const error = createErrorFromResponse(404, { error: "sandbox not found", code: 4041 });

    expect(error).toBeInstanceOf(SandboxServiceError);
    expect(error).toMatchObject({ message: "sandbox not found", status: 404, code: 4041 });
  });

  it("uses a plain string body as the message", () => {
    expect(createErrorFromResponse(500, "upstream exploded").message).toBe("upstream exploded");
  });

  // Nothing upstream guarantees the shape of an error body, and the fields are read
  // straight into a typed error. Taking them on trust is what would hand a caller a
  // `message` that is not a string, typed as one.
  it("falls back to the generic message when the error field is not a string", () => {
    const error = createErrorFromResponse(502, { error: 42 });

    expect(error.message).toBe("Sandbox service request failed with status 502");
    expect(error.code).toBeUndefined();
  });

  it("drops a code that is not a number", () => {
    const error = createErrorFromResponse(400, { error: "bad request", code: "4001" });

    expect(error.message).toBe("bad request");
    expect(error.code).toBeUndefined();
  });

  it("takes the reason from the body and drops one that is not a string", () => {
    const body = { error: "sandbox runners are at capacity", code: 503, reason: "no_capacity" };
    expect(createErrorFromResponse(503, body).reason).toBe("no_capacity");
    expect(createErrorFromResponse(503, { ...body, reason: 1 }).reason).toBeUndefined();
    expect(createErrorFromResponse(503, { error: "unavailable" }).reason).toBeUndefined();
  });

  it("keeps the reason on a restarted-sandbox error", () => {
    const error = createErrorFromResponse(409, {
      error: "sandbox restarted after guest crash; state in memory was lost",
      reason: "sandbox_restarted",
    });

    expect(error).toBeInstanceOf(SandboxCrashedError);
    expect(error.reason).toBe("sandbox_restarted");
  });

  it("falls back to the generic message for a body with no error field", () => {
    expect(createErrorFromResponse(503, {}).message).toBe(
      "Sandbox service request failed with status 503",
    );
  });
});

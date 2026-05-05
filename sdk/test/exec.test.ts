import { Readable } from "node:stream";
import { describe, expect, it, vi } from "vitest";
import { SandboxServiceError } from "../src/errors.js";
import { exec } from "../src/exec.js";
import type { HttpClient } from "../src/http.js";

function createMockHttp(ndjsonLines: string[]): HttpClient {
  return {
    requestStream: vi.fn().mockResolvedValue({
      stream: Readable.from([Buffer.from(ndjsonLines.join("\n") + "\n")]),
      status: 200,
    }),
  } as unknown as HttpClient;
}

describe("exec", () => {
  it("aggregates stdout and returns exit result", async () => {
    const http = createMockHttp([
      '{"type":"stdout","data":"hello\\n"}',
      '{"type":"stdout","data":"world\\n"}',
      '{"type":"exit","exit_code":0,"success":true,"execution_time_ms":100,"timed_out":false,"killed":false}',
    ]);

    const result = await exec(http, "sandbox-1", { command: "echo hello" });

    expect(result).toEqual({
      exitCode: 0,
      stdout: "hello\nworld\n",
      stderr: "",
      executionTimeMs: 100,
      timedOut: false,
      killed: false,
      success: true,
    });
  });

  it("aggregates stderr separately", async () => {
    const http = createMockHttp([
      '{"type":"stdout","data":"out"}',
      '{"type":"stderr","data":"err"}',
      '{"type":"exit","exit_code":1,"success":false,"execution_time_ms":50,"timed_out":false,"killed":false}',
    ]);

    const result = await exec(http, "sandbox-1", { command: "failing" });

    expect(result.stdout).toBe("out");
    expect(result.stderr).toBe("err");
    expect(result.exitCode).toBe(1);
    expect(result.success).toBe(false);
  });

  it("invokes onStdout and onStderr callbacks", async () => {
    const http = createMockHttp([
      '{"type":"stdout","data":"a"}',
      '{"type":"stderr","data":"b"}',
      '{"type":"exit","exit_code":0,"success":true,"execution_time_ms":10,"timed_out":false,"killed":false}',
    ]);

    const stdoutChunks: string[] = [];
    const stderrChunks: string[] = [];

    await exec(http, "sandbox-1", {
      command: "test",
      onStdout: (data) => stdoutChunks.push(data),
      onStderr: (data) => stderrChunks.push(data),
    });

    expect(stdoutChunks).toEqual(["a"]);
    expect(stderrChunks).toEqual(["b"]);
  });

  it("throws SandboxServiceError on error event", async () => {
    const http = createMockHttp(['{"type":"error","error":"command not found"}']);

    const err = await exec(http, "sandbox-1", { command: "bad" }).catch((e) => e);
    expect(err).toBeInstanceOf(SandboxServiceError);
    expect(err.message).toBe("command not found");
  });

  it("throws SandboxServiceError if stream ends without exit event", async () => {
    const http = createMockHttp(['{"type":"stdout","data":"partial"}']);

    const err = await exec(http, "sandbox-1", { command: "incomplete" }).catch((e) => e);
    expect(err).toBeInstanceOf(SandboxServiceError);
    expect(err.message).toBe("Sandbox exec stream ended without an exit event");
  });

  it("passes correct request body to http layer", async () => {
    const http = createMockHttp([
      '{"type":"exit","exit_code":0,"success":true,"execution_time_ms":1,"timed_out":false,"killed":false}',
    ]);

    await exec(http, "sandbox-1", {
      command: "ls",
      env: { FOO: "bar" },
      workdir: "/tmp",
      timeoutMs: 5000,
    });

    expect(http.requestStream).toHaveBeenCalledWith("POST", "/sandboxes/sandbox-1/exec", {
      data: {
        command: "ls",
        env: { FOO: "bar" },
        workdir: "/tmp",
        timeout_ms: 5000,
      },
      signal: undefined,
    });
  });
});

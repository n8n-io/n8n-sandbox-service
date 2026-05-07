import { Readable } from "node:stream";
import { describe, expect, it, vi } from "vitest";
import { SandboxServiceError } from "../src/errors.js";
import { exec, resumeExecSession } from "../src/exec.js";
import type { HttpClient } from "../src/http.js";

function createMockHttp(ndjsonLines: string[]): HttpClient {
  return {
    requestStream: vi.fn().mockResolvedValue({
      stream: Readable.from([Buffer.from(ndjsonLines.join("\n") + "\n")]),
      status: 200,
    }),
    requestVoid: vi.fn().mockResolvedValue(undefined),
  } as unknown as HttpClient;
}

describe("exec", () => {
  it("aggregates stdout and returns exit result", async () => {
    const http = createMockHttp([
      '{"seq":0,"type":"session","exec_id":"sess-1"}',
      '{"seq":1,"type":"stdout","data":"hello\\n"}',
      '{"seq":2,"type":"stdout","data":"world\\n"}',
      '{"seq":3,"type":"exit","exit_code":0,"success":true,"execution_time_ms":100,"timed_out":false,"killed":false}',
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
      '{"seq":0,"type":"session","exec_id":"sess-1"}',
      '{"seq":1,"type":"stdout","data":"out"}',
      '{"seq":2,"type":"stderr","data":"err"}',
      '{"seq":3,"type":"exit","exit_code":1,"success":false,"execution_time_ms":50,"timed_out":false,"killed":false}',
    ]);

    const result = await exec(http, "sandbox-1", { command: "failing" });

    expect(result.stdout).toBe("out");
    expect(result.stderr).toBe("err");
    expect(result.exitCode).toBe(1);
    expect(result.success).toBe(false);
  });

  it("invokes onStdout and onStderr callbacks", async () => {
    const http = createMockHttp([
      '{"seq":0,"type":"session","exec_id":"sess-1"}',
      '{"seq":1,"type":"stdout","data":"a"}',
      '{"seq":2,"type":"stderr","data":"b"}',
      '{"seq":3,"type":"exit","exit_code":0,"success":true,"execution_time_ms":10,"timed_out":false,"killed":false}',
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
    const http = createMockHttp([
      '{"seq":0,"type":"session","exec_id":"sess-1"}',
      '{"seq":1,"type":"error","error":"command not found"}',
    ]);

    const err = await exec(http, "sandbox-1", { command: "bad" }).catch((e) => e);
    expect(err).toBeInstanceOf(SandboxServiceError);
    expect(err.message).toBe("command not found");
  });

  it("throws SandboxServiceError if stream ends without exit event", async () => {
    const http = createMockHttp([
      '{"seq":0,"type":"session","exec_id":"sess-1"}',
      '{"seq":1,"type":"stdout","data":"partial"}',
    ]);

    // Stream ends without exit -> enters resume loop -> GET also ends without exit
    (http.requestStream as ReturnType<typeof vi.fn>).mockResolvedValueOnce({
      stream: Readable.from([Buffer.from("")]),
      status: 200,
    });

    const err = await exec(http, "sandbox-1", { command: "incomplete" }).catch((e) => e);
    expect(err).toBeInstanceOf(SandboxServiceError);
    expect(err.message).toBe("Sandbox exec stream ended without an exit event");
  });

  it("passes exec_id in request body", async () => {
    const http = createMockHttp([
      '{"seq":0,"type":"session","exec_id":"sess-1"}',
      '{"seq":1,"type":"exit","exit_code":0,"success":true,"execution_time_ms":1,"timed_out":false,"killed":false}',
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
        exec_id: expect.any(String),
      },
      signal: undefined,
    });
  });

  it("resumes after stream ends without exit event", async () => {
    const mockHttp = {
      requestStream: vi
        .fn()
        .mockResolvedValueOnce({
          stream: Readable.from([
            Buffer.from(
              '{"seq":0,"type":"session","exec_id":"sess-resume"}\n' +
                '{"seq":1,"type":"stdout","data":"part1"}\n',
            ),
          ]),
          status: 200,
        })
        .mockResolvedValueOnce({
          stream: Readable.from([
            Buffer.from(
              '{"seq":2,"type":"stdout","data":"part2"}\n' +
                '{"seq":3,"type":"exit","exit_code":0,"success":true,"execution_time_ms":100,"timed_out":false,"killed":false}\n',
            ),
          ]),
          status: 200,
        }),
      requestVoid: vi.fn().mockResolvedValue(undefined),
    } as unknown as HttpClient;

    const result = await exec(mockHttp, "sandbox-1", { command: "test" });

    expect(result.stdout).toBe("part1part2");
    expect(result.exitCode).toBe(0);
    expect(mockHttp.requestStream).toHaveBeenCalledTimes(2);
    // Second call is GET resume with after=1
    const lastCall = (mockHttp.requestStream as ReturnType<typeof vi.fn>).mock.calls[1];
    expect(lastCall[0]).toBe("GET");
    expect(lastCall[2]).toEqual(
      expect.objectContaining({
        params: { after: "1", follow: "true" },
      }),
    );
  });

  it("retries POST with same exec_id on transient error", async () => {
    const mockHttp = {
      requestStream: vi
        .fn()
        .mockRejectedValueOnce(new SandboxServiceError("network error", 0))
        .mockResolvedValueOnce({
          stream: Readable.from([
            Buffer.from(
              '{"seq":0,"type":"session","exec_id":"sess-retry"}\n' +
                '{"seq":1,"type":"exit","exit_code":0,"success":true,"execution_time_ms":50,"timed_out":false,"killed":false}\n',
            ),
          ]),
          status: 200,
        }),
      requestVoid: vi.fn().mockResolvedValue(undefined),
    } as unknown as HttpClient;

    const result = await exec(mockHttp, "sandbox-1", { command: "test" });

    expect(result.exitCode).toBe(0);
    expect(mockHttp.requestStream).toHaveBeenCalledTimes(2);

    // Both POST calls should use the same exec_id
    const firstCall = (mockHttp.requestStream as ReturnType<typeof vi.fn>).mock.calls[0];
    const secondCall = (mockHttp.requestStream as ReturnType<typeof vi.fn>).mock.calls[1];
    expect(firstCall[2].data.exec_id).toBe(secondCall[2].data.exec_id);
  });

  it("cancels session on abort signal", async () => {
    const controller = new AbortController();

    const stream = new Readable({
      read() {
        this.push(Buffer.from('{"seq":0,"type":"session","exec_id":"sess-abort"}\n'));
        this.push(null);
      },
    });

    const mockHttp = {
      requestStream: vi.fn().mockResolvedValueOnce({ stream, status: 200 }),
      requestVoid: vi.fn().mockResolvedValue(undefined),
    } as unknown as HttpClient;

    // Pre-abort the signal so the resume GET will fail immediately
    controller.abort();

    await exec(mockHttp, "sandbox-1", {
      command: "sleep 100",
      abortSignal: controller.signal,
    }).catch(() => {});

    // Should cancel using the client-defined exec_id, not the one from the session event
    expect(mockHttp.requestVoid).toHaveBeenCalledWith(
      "DELETE",
      expect.stringMatching(/^\/sandboxes\/sandbox-1\/exec\/.+$/),
    );
  });
});

describe("resumeExecSession", () => {
  it("requests follow mode so running sessions can complete", async () => {
    const http = createMockHttp([
      '{"seq":2,"type":"stdout","data":"part2"}',
      '{"seq":3,"type":"exit","exit_code":0,"success":true,"execution_time_ms":100,"timed_out":false,"killed":false}',
    ]);

    const result = await resumeExecSession(http, "sandbox-1", "exec-1", 1);

    expect(result.stdout).toBe("part2");
    expect(result.exitCode).toBe(0);
    expect(http.requestStream).toHaveBeenCalledWith("GET", "/sandboxes/sandbox-1/exec/exec-1", {
      params: { after: "1", follow: "true" },
    });
  });
});

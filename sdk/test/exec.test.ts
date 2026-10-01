import { Readable } from "node:stream";
import { describe, expect, it, vi } from "vitest";
import { SandboxServiceError } from "../src/errors.js";
import { exec, resumeExecution } from "../src/exec.js";
import type { HttpClient } from "../src/http.js";

function createMockHttp(ndjsonLines: string[]): HttpClient {
  return {
    requestStream: vi.fn().mockImplementation(() =>
      Promise.resolve({
        stream: Readable.from([Buffer.from(ndjsonLines.join("\n") + "\n")]),
        status: 200,
      }),
    ),
    requestVoid: vi.fn().mockResolvedValue(undefined),
  } as unknown as HttpClient;
}

describe("exec", () => {
  it("aggregates stdout and returns exit result", async () => {
    const http = createMockHttp([
      '{"seq":0,"type":"started","exec_id":"sess-1"}',
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
      '{"seq":0,"type":"started","exec_id":"sess-1"}',
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

  it("keeps exit result when a proxy error arrives after exit", async () => {
    const http = createMockHttp([
      '{"seq":0,"type":"started","exec_id":"sess-1"}',
      '{"seq":1,"type":"stdout","data":"ok"}',
      '{"seq":2,"type":"exit","exit_code":0,"success":true,"execution_time_ms":25,"timed_out":false,"killed":false}',
      '{"error":"internal server error"}',
    ]);

    const result = await exec(http, "sandbox-1", { command: "cat /tmp/file" });

    expect(result).toEqual({
      exitCode: 0,
      stdout: "ok",
      stderr: "",
      executionTimeMs: 25,
      timedOut: false,
      killed: false,
      success: true,
    });
  });

  it("invokes onStdout and onStderr callbacks", async () => {
    const http = createMockHttp([
      '{"seq":0,"type":"started","exec_id":"sess-1"}',
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
      '{"seq":0,"type":"started","exec_id":"sess-1"}',
      '{"seq":1,"type":"error","error":"command not found"}',
    ]);

    const err = await exec(http, "sandbox-1", { command: "bad" }).catch((e) => e);
    expect(err).toBeInstanceOf(SandboxServiceError);
    expect(err.message).toBe("command not found");
  });

  it("throws SandboxServiceError if stream ends without exit event", async () => {
    const http = {
      requestStream: vi.fn().mockImplementation(() =>
        Promise.resolve({
          stream: Readable.from([Buffer.from("")]),
          status: 200,
        }),
      ),
      requestVoid: vi.fn().mockResolvedValue(undefined),
    } as unknown as HttpClient;

    const err = await exec(http, "sandbox-1", { command: "incomplete" }).catch((e) => e);
    expect(err).toBeInstanceOf(SandboxServiceError);
    expect(err.message).toBe("Sandbox exec stream ended without an exit event");
  });

  it("passes exec_id in request body", async () => {
    const http = createMockHttp([
      '{"seq":0,"type":"started","exec_id":"sess-1"}',
      '{"seq":1,"type":"exit","exit_code":0,"success":true,"execution_time_ms":1,"timed_out":false,"killed":false}',
    ]);

    await exec(http, "sandbox-1", {
      command: "ls",
      env: { FOO: "bar" },
      workdir: "/tmp",
      timeoutMs: 5000,
    });

    expect(http.requestStream).toHaveBeenCalledWith("POST", "/sandboxes/sandbox-1/executions", {
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
              '{"seq":0,"type":"started","exec_id":"sess-resume"}\n' +
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

  it("resumes after stream ends with a truncated event", async () => {
    const mockHttp = {
      requestStream: vi
        .fn()
        .mockResolvedValueOnce({
          stream: Readable.from([
            Buffer.from(
              '{"seq":0,"type":"started","exec_id":"sess-resume"}\n' +
                '{"seq":1,"type":"stdout","data":"part1"}\n' +
                '{"seq":2,"type":"stdout","data":"truncated',
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

    expect(result).toEqual({
      exitCode: 0,
      stdout: "part1part2",
      stderr: "",
      executionTimeMs: 100,
      timedOut: false,
      killed: false,
      success: true,
    });
    expect(mockHttp.requestStream).toHaveBeenCalledTimes(2);
    const resumeCall = (mockHttp.requestStream as ReturnType<typeof vi.fn>).mock.calls[1];
    expect(resumeCall[0]).toBe("GET");
    expect(resumeCall[1]).toEqual(
      expect.stringMatching(/^\/sandboxes\/sandbox-1\/executions\/.+$/),
    );
    expect(resumeCall[2]).toEqual(
      expect.objectContaining({
        params: { after: "1", follow: "true" },
      }),
    );
  });

  it("retries POST with same exec_id when stream truncates before any valid event", async () => {
    const mockHttp = {
      requestStream: vi
        .fn()
        .mockResolvedValueOnce({
          stream: Readable.from([
            Buffer.from('{"seq":0,"type":"started","exec_id":"sess-truncated'),
          ]),
          status: 200,
        })
        .mockResolvedValueOnce({
          stream: Readable.from([
            Buffer.from(
              '{"seq":0,"type":"started","exec_id":"sess-retry"}\n' +
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

    const firstCall = (mockHttp.requestStream as ReturnType<typeof vi.fn>).mock.calls[0];
    const secondCall = (mockHttp.requestStream as ReturnType<typeof vi.fn>).mock.calls[1];
    expect(firstCall[0]).toBe("POST");
    expect(secondCall[0]).toBe("POST");
    expect(firstCall[2].data.exec_id).toBe(secondCall[2].data.exec_id);
  });

  it("retries POST with same exec_id on transient error", async () => {
    const mockHttp = {
      requestStream: vi
        .fn()
        .mockRejectedValueOnce(new SandboxServiceError("network error", 0))
        .mockResolvedValueOnce({
          stream: Readable.from([
            Buffer.from(
              '{"seq":0,"type":"started","exec_id":"sess-retry"}\n' +
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

  it("retries POST with same exec_id after a 429", async () => {
    const limited = new SandboxServiceError("too many requests", 429);
    const mockHttp = {
      requestStream: vi
        .fn()
        .mockRejectedValueOnce(limited)
        .mockRejectedValueOnce(limited)
        .mockResolvedValueOnce({
          stream: Readable.from([
            Buffer.from(
              '{"seq":0,"type":"started","exec_id":"sess-limited"}\n' +
                '{"seq":1,"type":"exit","exit_code":0,"success":true,"execution_time_ms":5,"timed_out":false,"killed":false}\n',
            ),
          ]),
          status: 200,
        }),
      requestVoid: vi.fn().mockResolvedValue(undefined),
      retryDelayFor: vi.fn().mockReturnValue(0),
    } as unknown as HttpClient;

    const result = await exec(mockHttp, "sandbox-1", { command: "test" });

    expect(result.exitCode).toBe(0);
    const calls = (mockHttp.requestStream as ReturnType<typeof vi.fn>).mock.calls;
    expect(calls).toHaveLength(3);
    expect(calls.every((call) => call[0] === "POST")).toBe(true);
    expect(new Set(calls.map((call) => call[2].data.exec_id)).size).toBe(1);
    expect(mockHttp.retryDelayFor).toHaveBeenNthCalledWith(1, limited, 0);
    expect(mockHttp.retryDelayFor).toHaveBeenNthCalledWith(2, limited, 1);
  });

  it("waits the retry policy's delay before re-posting after a 429", async () => {
    vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout"] });
    try {
      const mockHttp = {
        requestStream: vi
          .fn()
          .mockRejectedValueOnce(new SandboxServiceError("too many requests", 429))
          .mockResolvedValueOnce({
            stream: Readable.from([
              Buffer.from(
                '{"seq":0,"type":"started","exec_id":"sess-wait"}\n' +
                  '{"seq":1,"type":"exit","exit_code":0,"success":true,"execution_time_ms":5,"timed_out":false,"killed":false}\n',
              ),
            ]),
            status: 200,
          }),
        requestVoid: vi.fn().mockResolvedValue(undefined),
        retryDelayFor: vi.fn().mockReturnValue(1000),
      } as unknown as HttpClient;

      const running = exec(mockHttp, "sandbox-1", { command: "test" });

      await vi.advanceTimersByTimeAsync(999);
      expect(mockHttp.requestStream).toHaveBeenCalledTimes(1);
      await vi.advanceTimersByTimeAsync(1);
      expect((await running).exitCode).toBe(0);
      expect(mockHttp.requestStream).toHaveBeenCalledTimes(2);
    } finally {
      vi.useRealTimers();
    }
  });

  it("throws the 429 once the retry policy gives up", async () => {
    const limited = new SandboxServiceError("too many requests", 429);
    const mockHttp = {
      requestStream: vi.fn().mockRejectedValue(limited),
      requestVoid: vi.fn().mockResolvedValue(undefined),
      retryDelayFor: vi.fn().mockReturnValueOnce(0).mockReturnValue(undefined),
    } as unknown as HttpClient;

    await expect(exec(mockHttp, "sandbox-1", { command: "test" })).rejects.toBe(limited);
    expect(mockHttp.requestStream).toHaveBeenCalledTimes(2);
    // Every POST was refused, so nothing started and there is nothing to cancel.
    expect(mockHttp.requestVoid).not.toHaveBeenCalled();
  });

  // A POST that lost its stream before any event may still have started the command, so a
  // later 429 does not prove it never ran.
  it("cancels the execution when it gives up on a 429 after a POST may have started it", async () => {
    const limited = new SandboxServiceError("too many requests", 429);
    const mockHttp = {
      requestStream: vi
        .fn()
        .mockResolvedValueOnce({ stream: Readable.from([]), status: 200 })
        .mockRejectedValue(limited),
      requestVoid: vi.fn().mockResolvedValue(undefined),
      retryDelayFor: vi.fn().mockReturnValue(undefined),
    } as unknown as HttpClient;

    await expect(exec(mockHttp, "sandbox-1", { command: "test" })).rejects.toBe(limited);
    const calls = (mockHttp.requestStream as ReturnType<typeof vi.fn>).mock.calls;
    expect(mockHttp.requestVoid).toHaveBeenCalledWith(
      "DELETE",
      `/sandboxes/sandbox-1/executions/${calls[0][2].data.exec_id}`,
    );
  });

  it("cancels the execution when resuming it keeps getting 429", async () => {
    const limited = new SandboxServiceError("too many requests", 429);
    const mockHttp = {
      requestStream: vi
        .fn()
        .mockResolvedValueOnce({
          stream: Readable.from([Buffer.from('{"seq":0,"type":"started","exec_id":"sess-r"}\n')]),
          status: 200,
        })
        .mockRejectedValue(limited),
      requestVoid: vi.fn().mockResolvedValue(undefined),
      retryDelayFor: vi.fn().mockReturnValue(undefined),
    } as unknown as HttpClient;

    await expect(exec(mockHttp, "sandbox-1", { command: "test" })).rejects.toBe(limited);
    const calls = (mockHttp.requestStream as ReturnType<typeof vi.fn>).mock.calls;
    expect(calls[1][0]).toBe("GET");
    expect(mockHttp.requestVoid).toHaveBeenCalledWith(
      "DELETE",
      `/sandboxes/sandbox-1/executions/${calls[0][2].data.exec_id}`,
    );
  });

  it("stops waiting out a 429 when aborted, without posting again", async () => {
    const controller = new AbortController();
    const mockHttp = {
      requestStream: vi
        .fn()
        .mockRejectedValueOnce(new SandboxServiceError("too many requests", 429)),
      requestVoid: vi.fn().mockResolvedValue(undefined),
      retryDelayFor: vi.fn().mockReturnValue(60_000),
    } as unknown as HttpClient;

    const started = Date.now();
    const running = exec(mockHttp, "sandbox-1", {
      command: "test",
      abortSignal: controller.signal,
    });
    setTimeout(() => controller.abort(), 20);

    await expect(running).rejects.toThrow("Request aborted");
    expect(Date.now() - started).toBeLessThan(5_000);
    expect(mockHttp.requestStream).toHaveBeenCalledTimes(1);
    expect(mockHttp.requestVoid).toHaveBeenCalledWith(
      "DELETE",
      expect.stringMatching(/^\/sandboxes\/sandbox-1\/executions\/.+$/),
    );
  });

  it("deletes execution on abort signal", async () => {
    const controller = new AbortController();

    const stream = new Readable({
      read() {
        this.push(Buffer.from('{"seq":0,"type":"started","exec_id":"sess-abort"}\n'));
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

    // Should cancel using the client-defined exec_id, not the one from the started event
    expect(mockHttp.requestVoid).toHaveBeenCalledWith(
      "DELETE",
      expect.stringMatching(/^\/sandboxes\/sandbox-1\/executions\/.+$/),
    );
  });
});

describe("resumeExecution", () => {
  it("requests follow mode so running executions can complete", async () => {
    const http = createMockHttp([
      '{"seq":2,"type":"stdout","data":"part2"}',
      '{"seq":3,"type":"exit","exit_code":0,"success":true,"execution_time_ms":100,"timed_out":false,"killed":false}',
    ]);

    const result = await resumeExecution(http, "sandbox-1", "exec-1", 1);

    expect(result.stdout).toBe("part2");
    expect(result.exitCode).toBe(0);
    expect(http.requestStream).toHaveBeenCalledWith(
      "GET",
      "/sandboxes/sandbox-1/executions/exec-1",
      {
        params: { after: "1", follow: "true" },
      },
    );
  });
});

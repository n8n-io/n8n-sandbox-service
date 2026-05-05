import { Readable } from "node:stream";
import { describe, expect, it } from "vitest";
import { parseExecEvent, readNdjsonStream } from "../src/ndjson.js";

function streamFrom(lines: string[]): Readable {
  return Readable.from([Buffer.from(lines.join("\n") + "\n")]);
}

describe("readNdjsonStream", () => {
  it("parses stdout and stderr events", async () => {
    const stream = streamFrom([
      '{"type":"stdout","data":"hello\\n"}',
      '{"type":"stderr","data":"warn"}',
    ]);

    const events = [];
    for await (const event of readNdjsonStream(stream)) {
      events.push(event);
    }

    expect(events).toEqual([
      { type: "stdout", data: "hello\n" },
      { type: "stderr", data: "warn" },
    ]);
  });

  it("parses exit events", async () => {
    const stream = streamFrom([
      '{"type":"exit","exit_code":0,"success":true,"execution_time_ms":42,"timed_out":false,"killed":false}',
    ]);

    const events = [];
    for await (const event of readNdjsonStream(stream)) {
      events.push(event);
    }

    expect(events).toEqual([
      {
        type: "exit",
        exit_code: 0,
        success: true,
        execution_time_ms: 42,
        timed_out: false,
        killed: false,
      },
    ]);
  });

  it("parses error events", async () => {
    const stream = streamFrom(['{"type":"error","error":"something broke"}']);

    const events = [];
    for await (const event of readNdjsonStream(stream)) {
      events.push(event);
    }

    expect(events).toEqual([{ type: "error", error: "something broke" }]);
  });

  it("handles chunked data split across boundaries", async () => {
    const stream = Readable.from([
      Buffer.from('{"type":"stdout","data":"a"}\n{"type"'),
      Buffer.from(':"stdout","data":"b"}\n'),
    ]);

    const events = [];
    for await (const event of readNdjsonStream(stream)) {
      events.push(event);
    }

    expect(events).toEqual([
      { type: "stdout", data: "a" },
      { type: "stdout", data: "b" },
    ]);
  });

  it("preserves UTF-8 characters split across chunk boundaries", async () => {
    const stream = Readable.from([
      Buffer.from('{"type":"stdout","data":"caf'),
      Buffer.from([0xc3]),
      Buffer.from([0xa9]),
      Buffer.from('"}\n'),
    ]);

    const events = [];
    for await (const event of readNdjsonStream(stream)) {
      events.push(event);
    }

    expect(events).toEqual([{ type: "stdout", data: "café" }]);
  });

  it("returns error event for invalid JSON", async () => {
    const stream = streamFrom(["not-json"]);

    const events = [];
    for await (const event of readNdjsonStream(stream)) {
      events.push(event);
    }

    expect(events).toEqual([
      { type: "error", error: expect.stringContaining("Invalid exec event payload") },
    ]);
  });

  it("returns error event for unknown event type", async () => {
    const stream = streamFrom(['{"type":"unknown","foo":"bar"}']);

    const events = [];
    for await (const event of readNdjsonStream(stream)) {
      events.push(event);
    }

    expect(events).toEqual([
      { type: "error", error: 'Invalid exec event payload: {"type":"unknown","foo":"bar"}' },
    ]);
  });

  it("skips empty lines", async () => {
    const stream = Readable.from([Buffer.from('\n\n{"type":"stdout","data":"ok"}\n\n')]);

    const events = [];
    for await (const event of readNdjsonStream(stream)) {
      events.push(event);
    }

    expect(events).toEqual([{ type: "stdout", data: "ok" }]);
  });

  it("handles trailing data without newline", async () => {
    const stream = Readable.from([Buffer.from('{"type":"stdout","data":"last"}')]);

    const events = [];
    for await (const event of readNdjsonStream(stream)) {
      events.push(event);
    }

    expect(events).toEqual([{ type: "stdout", data: "last" }]);
  });
});

describe("parseExecEvent", () => {
  it("parses stdout event", () => {
    expect(parseExecEvent('{"type":"stdout","data":"hello"}')).toEqual({
      type: "stdout",
      data: "hello",
    });
  });

  it("parses stderr event", () => {
    expect(parseExecEvent('{"type":"stderr","data":"err"}')).toEqual({
      type: "stderr",
      data: "err",
    });
  });

  it("returns error for malformed JSON", () => {
    expect(parseExecEvent("{")).toEqual({
      type: "error",
      error: expect.stringContaining("Invalid exec event payload"),
    });
  });

  it("returns error for exit event with wrong field types", () => {
    expect(
      parseExecEvent('{"type":"exit","exit_code":"zero","success":true,"execution_time_ms":42,"timed_out":false,"killed":false}'),
    ).toEqual({
      type: "error",
      error: 'Invalid exec event payload: {"type":"exit","exit_code":"zero","success":true,"execution_time_ms":42,"timed_out":false,"killed":false}',
    });
  });
});

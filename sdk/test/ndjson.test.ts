import { Readable } from "node:stream";
import { describe, expect, it } from "vitest";
import { InvalidStreamEventError } from "../src/errors.js";
import { parseExecEvent, readNdjsonStream } from "../src/ndjson.js";

function streamFrom(lines: string[]): Readable {
  return Readable.from([Buffer.from(lines.join("\n") + "\n")]);
}

describe("readNdjsonStream", () => {
  it("parses stdout and stderr events", async () => {
    const stream = streamFrom([
      '{"seq":1,"type":"stdout","data":"hello\\n"}',
      '{"seq":2,"type":"stderr","data":"warn"}',
    ]);

    const events = [];
    for await (const event of readNdjsonStream(stream)) {
      events.push(event);
    }

    expect(events).toEqual([
      { seq: 1, type: "stdout", data: "hello\n" },
      { seq: 2, type: "stderr", data: "warn" },
    ]);
  });

  it("parses exit events", async () => {
    const stream = streamFrom([
      '{"seq":3,"type":"exit","exit_code":0,"success":true,"execution_time_ms":42,"timed_out":false,"killed":false}',
    ]);

    const events = [];
    for await (const event of readNdjsonStream(stream)) {
      events.push(event);
    }

    expect(events).toEqual([
      {
        seq: 3,
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
    const stream = streamFrom(['{"seq":1,"type":"error","error":"something broke"}']);

    const events = [];
    for await (const event of readNdjsonStream(stream)) {
      events.push(event);
    }

    expect(events).toEqual([{ seq: 1, type: "error", error: "something broke" }]);
  });

  it("handles chunked data split across boundaries", async () => {
    const stream = Readable.from([
      Buffer.from('{"seq":1,"type":"stdout","data":"a"}\n{"seq":2'),
      Buffer.from(',"type":"stdout","data":"b"}\n'),
    ]);

    const events = [];
    for await (const event of readNdjsonStream(stream)) {
      events.push(event);
    }

    expect(events).toEqual([
      { seq: 1, type: "stdout", data: "a" },
      { seq: 2, type: "stdout", data: "b" },
    ]);
  });

  it("decodes Uint8Array chunks from browser ReadableStreams", async () => {
    const encoder = new TextEncoder();
    const stream = Readable.from([
      encoder.encode('{"seq":1,"type":"stdout","data":"browser"}\n'),
      encoder.encode(
        '{"seq":2,"type":"exit","exit_code":0,"success":true,"execution_time_ms":1,"timed_out":false,"killed":false}\n',
      ),
    ]);

    const events = [];
    for await (const event of readNdjsonStream(stream)) {
      events.push(event);
    }

    expect(events).toEqual([
      { seq: 1, type: "stdout", data: "browser" },
      {
        seq: 2,
        type: "exit",
        exit_code: 0,
        success: true,
        execution_time_ms: 1,
        timed_out: false,
        killed: false,
      },
    ]);
  });

  it("preserves UTF-8 characters split across chunk boundaries", async () => {
    const stream = Readable.from([
      Buffer.from('{"seq":1,"type":"stdout","data":"caf'),
      Buffer.from([0xc3]),
      Buffer.from([0xa9]),
      Buffer.from('"}\n'),
    ]);

    const events = [];
    for await (const event of readNdjsonStream(stream)) {
      events.push(event);
    }

    expect(events).toEqual([{ seq: 1, type: "stdout", data: "café" }]);
  });

  it("throws error for invalid JSON", async () => {
    const stream = streamFrom(["not-json"]);

    await expect(async () => {
      const events = [];
      for await (const event of readNdjsonStream(stream)) {
        events.push(event);
      }
    }).rejects.toThrow(InvalidStreamEventError);
  });

  it("throws error for malformed newline-terminated JSON object", async () => {
    const stream = streamFrom(['{"seq":1,"type":"stdout","data":"ok",}']);

    await expect(async () => {
      const events = [];
      for await (const event of readNdjsonStream(stream)) {
        events.push(event);
      }
    }).rejects.toThrow(InvalidStreamEventError);
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
    const stream = Readable.from([Buffer.from('\n\n{"seq":1,"type":"stdout","data":"ok"}\n\n')]);

    const events = [];
    for await (const event of readNdjsonStream(stream)) {
      events.push(event);
    }

    expect(events).toEqual([{ seq: 1, type: "stdout", data: "ok" }]);
  });

  it("handles trailing data without newline", async () => {
    const stream = Readable.from([Buffer.from('{"seq":1,"type":"stdout","data":"last"}')]);

    const events = [];
    for await (const event of readNdjsonStream(stream)) {
      events.push(event);
    }

    expect(events).toEqual([{ seq: 1, type: "stdout", data: "last" }]);
  });
});

describe("parseExecEvent", () => {
  it("parses started event", () => {
    expect(parseExecEvent('{"seq":0,"type":"started","exec_id":"abc-123"}')).toEqual({
      seq: 0,
      type: "started",
      exec_id: "abc-123",
    });
  });

  it("parses stdout event", () => {
    expect(parseExecEvent('{"seq":1,"type":"stdout","data":"hello"}')).toEqual({
      seq: 1,
      type: "stdout",
      data: "hello",
    });
  });

  it("parses stderr event", () => {
    expect(parseExecEvent('{"seq":2,"type":"stderr","data":"err"}')).toEqual({
      seq: 2,
      type: "stderr",
      data: "err",
    });
  });

  it("throws error for malformed JSON", () => {
    expect(() => {
      parseExecEvent("{");
    }).toThrow(InvalidStreamEventError);
  });

  it("returns error for exit event with wrong field types", () => {
    expect(
      parseExecEvent(
        '{"seq":1,"type":"exit","exit_code":"zero","success":true,"execution_time_ms":42,"timed_out":false,"killed":false}',
      ),
    ).toEqual({
      type: "error",
      error:
        'Invalid exec event payload: {"seq":1,"type":"exit","exit_code":"zero","success":true,"execution_time_ms":42,"timed_out":false,"killed":false}',
    });
  });
});

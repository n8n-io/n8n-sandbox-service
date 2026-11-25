import { PassThrough, Readable } from "node:stream";
import { describe, expect, it } from "vitest";
import { destroyStreams, multipartBody } from "../../src/code-tasks/multipart.js";

async function readBody(body: Readable): Promise<string> {
  const chunks: Buffer[] = [];
  for await (const chunk of body) chunks.push(chunk as Buffer);
  return Buffer.concat(chunks).toString("utf-8");
}

function boundaryOf(contentType: string): string {
  const match = /^multipart\/form-data; boundary=(.+)$/.exec(contentType);
  if (!match) throw new Error(`unexpected content type ${contentType}`);
  return match[1];
}

describe("multipartBody", () => {
  it("encodes each part with its headers and content", async () => {
    const { contentType, body } = multipartBody([
      { name: "spec", contentType: "application/json", content: '{"a":1}' },
      { name: "input", filename: "bin/0", contentType: "image/png", content: Buffer.from("png") },
      {
        name: "input",
        filename: "bin/1",
        contentType: "application/octet-stream",
        content: new Uint8Array([104, 105]),
      },
      {
        name: "input",
        filename: "bin/2",
        contentType: "text/plain",
        content: Readable.from([Buffer.from("str"), Buffer.from("eam")]),
      },
    ]);
    const boundary = boundaryOf(contentType);

    expect(await readBody(body)).toBe(
      `--${boundary}\r\n` +
        'Content-Disposition: form-data; name="spec"\r\n' +
        "Content-Type: application/json\r\n\r\n" +
        '{"a":1}\r\n' +
        `--${boundary}\r\n` +
        'Content-Disposition: form-data; name="input"; filename="bin/0"\r\n' +
        "Content-Type: image/png\r\n\r\n" +
        "png\r\n" +
        `--${boundary}\r\n` +
        'Content-Disposition: form-data; name="input"; filename="bin/1"\r\n' +
        "Content-Type: application/octet-stream\r\n\r\n" +
        "hi\r\n" +
        `--${boundary}\r\n` +
        'Content-Disposition: form-data; name="input"; filename="bin/2"\r\n' +
        "Content-Type: text/plain\r\n\r\n" +
        "stream\r\n" +
        `--${boundary}--\r\n`,
    );
  });

  it("uses a new boundary for each body", () => {
    const first = multipartBody([]);
    const second = multipartBody([]);

    expect(boundaryOf(first.contentType)).not.toBe(boundaryOf(second.contentType));
  });

  it("reads stream content only while the body is read", async () => {
    let reads = 0;
    const content = new Readable({
      read() {
        reads++;
        this.push("x");
        this.push(null);
      },
    });
    const { body } = multipartBody([{ name: "input", contentType: "text/plain", content }]);

    expect(reads).toBe(0);
    await readBody(body);
    expect(reads).toBeGreaterThan(0);
  });

  it.each([
    ["name", { name: 'a"b' }, /name must not contain/],
    ["name", { name: "a\0b" }, /name must not contain/],
    ["filename", { filename: "a\\b" }, /filename must not contain/],
    ["filename", { filename: "a\r\nb" }, /filename must not contain/],
    ["content type", { contentType: "" }, /content type must not be empty/],
    ["content type", { contentType: "text/plain\nX: 1" }, /content type must not be empty/],
  ])("refuses an invalid %s and destroys the stream parts", (_, override, message) => {
    const content = new PassThrough();

    expect(() =>
      multipartBody([
        { name: "input", filename: "bin/0", contentType: "text/plain", content },
        { name: "input", filename: "bin/1", contentType: "text/plain", content: "x", ...override },
      ]),
    ).toThrow(message);
    expect(content.destroyed).toBe(true);
  });

  it("fails the body when a stream part fails before it is read", async () => {
    const failing = new PassThrough();
    const other = new PassThrough();
    const { body } = multipartBody([
      { name: "input", filename: "bin/0", contentType: "text/plain", content: other },
      { name: "input", filename: "bin/1", contentType: "text/plain", content: failing },
    ]);

    failing.destroy(new Error("disk failed"));

    await expect(readBody(body)).rejects.toThrow("disk failed");
    expect(other.destroyed).toBe(true);
  });

  it("destroys the body and the stream parts on destroy()", () => {
    const content = new PassThrough();
    const { body, destroy } = multipartBody([
      { name: "input", contentType: "text/plain", content },
    ]);

    destroy();

    expect(body.destroyed).toBe(true);
    expect(content.destroyed).toBe(true);
  });
});

describe("destroyStreams", () => {
  it("destroys the streams and skips other content", () => {
    const stream = new PassThrough();

    destroyStreams(["text", Buffer.from("x"), stream]);

    expect(stream.destroyed).toBe(true);
  });

  it("ignores a stream error after the destroy", async () => {
    const stream = new PassThrough();

    destroyStreams([stream]);
    // Without a listener, this error would be unhandled and fail the run.
    stream.emit("error", new Error("late"));

    await new Promise((resolve) => setImmediate(resolve));
  });
});

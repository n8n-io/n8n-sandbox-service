// Encodes the multipart/form-data body of a code task. We do not use a library:
// - The global FormData takes no Node Readable, so it must buffer each stream
//   input in memory.
// - The form-data package keeps the body open until it reaches a failed
//   stream input, so a slow input before it can stop the task from ending.
//   It also changes paths and percent-encodes quotes and line breaks in
//   names, where this file refuses them.
import { randomUUID } from "node:crypto";
import { Readable } from "node:stream";
import type { FileContent } from "../types";

/** A part of a multipart/form-data body. */
export interface MultipartPart {
  name: string;
  filename?: string;
  contentType: string;
  content: FileContent | Readable;
}

/** A streamed multipart/form-data body and its Content-Type header. */
export interface MultipartBody {
  contentType: string;
  body: Readable;
  /** Stops the body and destroys the stream content of its parts. */
  destroy: () => void;
}

/**
 * Encodes parts as a multipart/form-data stream. Stream content is read only
 * while the body is sent, so the body is never fully in memory. Invalid parts
 * throw, and their stream content is destroyed.
 */
export function multipartBody(parts: MultipartPart[]): MultipartBody {
  // A body waiting on a stream part cannot stop until the part ends, so the
  // parts are destroyed too.
  const destroyParts = () => destroyStreams(parts.map((part) => part.content));
  try {
    for (const part of parts) {
      checkHeaderValue("name", part.name);
      if (part.filename !== undefined) checkHeaderValue("filename", part.filename);
      checkContentType(part.contentType);
    }
  } catch (error) {
    destroyParts();
    throw error;
  }

  const boundary = `n8n-sandbox-${randomUUID()}`;
  const body = Readable.from(encode(boundary, parts), { objectMode: false });
  // A stream part can fail before the encoder reads it. Without a listener
  // its error would be unhandled and stop the process, so it fails the body.
  const failBody = (error: Error) => {
    body.destroy(error);
    destroyParts();
  };
  for (const part of parts) {
    if (part.content instanceof Readable) part.content.on("error", failBody);
  }
  return {
    contentType: `multipart/form-data; boundary=${boundary}`,
    body,
    destroy: () => {
      body.destroy();
      destroyParts();
    },
  };
}

/**
 * Destroys the streams among contents. Nothing reads them after this, so
 * their later errors, such as a file that fails to open, are ignored rather
 * than left unhandled.
 */
export function destroyStreams(contents: Array<FileContent | Readable>): void {
  for (const content of contents) {
    if (content instanceof Readable) {
      content.on("error", () => {});
      content.destroy();
    }
  }
}

async function* encode(boundary: string, parts: MultipartPart[]): AsyncGenerator<Buffer> {
  for (const part of parts) {
    let disposition = `form-data; name="${part.name}"`;
    if (part.filename !== undefined) disposition += `; filename="${part.filename}"`;
    yield Buffer.from(
      `--${boundary}\r\n` +
        `Content-Disposition: ${disposition}\r\n` +
        `Content-Type: ${part.contentType}\r\n\r\n`,
      "utf-8",
    );

    if (part.content instanceof Readable) {
      for await (const chunk of part.content) {
        yield toBuffer(chunk as FileContent);
      }
    } else {
      yield toBuffer(part.content);
    }
    yield Buffer.from("\r\n", "utf-8");
  }
  yield Buffer.from(`--${boundary}--\r\n`, "utf-8");
}

function toBuffer(content: FileContent): Buffer {
  if (typeof content === "string") return Buffer.from(content, "utf-8");
  return Buffer.isBuffer(content) ? content : Buffer.from(content);
}

// A quoted header parameter cannot hold these without escapes that servers
// read differently, so they are refused rather than escaped.
function checkHeaderValue(field: string, value: string): void {
  if (/["\\\r\n]/.test(value) || value.includes("\0")) {
    throw new Error(`Multipart ${field} must not contain quotes, backslashes or line breaks`);
  }
}

// A Content-Type can hold quoted parameters, but a line break would start a
// new header or part.
function checkContentType(value: string): void {
  if (value.length === 0 || /[\r\n]/.test(value) || value.includes("\0")) {
    throw new Error("Multipart content type must not be empty or contain line breaks");
  }
}

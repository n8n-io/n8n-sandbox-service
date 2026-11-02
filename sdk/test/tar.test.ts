import { execFileSync } from "node:child_process";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { gunzipSync } from "node:zlib";
import { afterEach, describe, expect, it } from "vitest";
import { gzipTar } from "../src/tar.js";

const BLOCK_SIZE = 512;

/**
 * Reads back the regular files of a gzipTar archive, checking each header's
 * checksum and each pax record's length prefix on the way. Independent of the
 * system tar, so path lengths are not bound by the OS.
 */
function readArchive(archive: Buffer): Array<{ path: string; content: Buffer }> {
  const tar = gunzipSync(archive);
  const files: Array<{ path: string; content: Buffer }> = [];
  let paxPath: string | undefined;
  let offset = 0;
  while (true) {
    const header = tar.subarray(offset, offset + BLOCK_SIZE);
    if (header.every((byte) => byte === 0)) break;

    const field = (start: number, length: number) =>
      header
        .subarray(start, start + length)
        .toString("latin1")
        .split("\0")[0];
    let checksum = 0;
    header.forEach((byte, i) => (checksum += i >= 148 && i < 156 ? 0x20 : byte));
    expect(parseInt(field(148, 8), 8), "header checksum").toBe(checksum);

    const size = parseInt(field(124, 12), 8);
    const typeflag = field(156, 1);
    const data = tar.subarray(offset + BLOCK_SIZE, offset + BLOCK_SIZE + size);
    offset += BLOCK_SIZE + Math.ceil(size / BLOCK_SIZE) * BLOCK_SIZE;

    if (typeflag === "x") {
      paxPath = undefined;
      let at = 0;
      while (at < data.length) {
        const space = data.indexOf(0x20, at);
        const length = Number(data.subarray(at, space).toString());
        const record = data.subarray(at, at + length).toString("utf-8");
        expect(record.endsWith("\n"), `pax record of ${length} bytes ends its line`).toBe(true);
        const [key, ...value] = record.slice(space - at + 1, -1).split("=");
        if (key === "path") paxPath = value.join("=");
        at += length;
      }
      expect(at, "pax records fill the header data").toBe(data.length);
      continue;
    }

    expect(typeflag).toBe("0");
    files.push({
      path: paxPath ?? Buffer.from(field(0, 100), "latin1").toString("utf-8"),
      content: Buffer.from(data),
    });
    paxPath = undefined;
  }
  return files;
}

/** A path of `bytes` bytes, split into short directory names. */
function pathOfLength(bytes: number): string {
  return `${"d/".repeat(bytes)}`.slice(0, bytes - 1) + "f";
}

describe("gzipTar", () => {
  let dir: string | undefined;

  afterEach(() => {
    if (dir) rmSync(dir, { recursive: true, force: true });
    dir = undefined;
  });

  // The system tar is an implementation independent of this one.
  it("builds an archive the system tar unpacks", () => {
    dir = mkdtempSync(join(tmpdir(), "sdk-tar-"));
    const longPath = `${"long-directory-name/".repeat(8)}ünïcode-file.txt`;
    const files = [
      { path: "a.txt", content: Buffer.from("hello") },
      { path: "nested/dir/empty.txt", content: Buffer.alloc(0) },
      { path: "block.bin", content: Buffer.alloc(512, 7) },
      { path: longPath, content: Buffer.from("long") },
    ];
    expect(Buffer.byteLength(longPath)).toBeGreaterThan(100);

    execFileSync("tar", ["-xzf", "-", "-C", dir], { input: gzipTar(files) });

    for (const { path, content } of files) {
      expect(readFileSync(join(dir, path)).equals(content), path).toBe(true);
    }
  });

  it.each([
    { name: "exactly 100 bytes, no pax header", path: pathOfLength(100) },
    { name: "101 bytes, the first with a pax header", path: pathOfLength(101) },
    { name: "a character cut at byte 100", path: `${"a".repeat(99)}ü/file.txt` },
  ])("keeps a path of $name", ({ path }) => {
    const files = [{ path, content: Buffer.from("x") }];

    expect(readArchive(gzipTar(files))).toEqual(files);
  });

  // A pax record is " path=<value>\n" (7 bytes plus the path) behind its own
  // length, so these put the length on and around a change in its digit count.
  it.each([996, 997, 998, 9995, 9996])(
    "writes a pax record whose length counts its own digits (%i bytes after the length)",
    (restBytes) => {
      const files = [{ path: pathOfLength(restBytes - 7), content: Buffer.from("x") }];

      expect(readArchive(gzipTar(files))).toEqual(files);
    },
  );

  it("sizes entries in bytes", () => {
    const files = [
      { path: "multibyte.txt", content: Buffer.from("héllo ✓", "utf-8") },
      { path: "after.txt", content: Buffer.from("after") },
    ];

    expect(readArchive(gzipTar(files))).toEqual(files);
  });

  it("refuses a path with a NUL byte instead of cutting it short", () => {
    expect(() => gzipTar([{ path: "x.sh\0.txt", content: Buffer.from("x") }])).toThrow(/NUL byte/);
  });
});

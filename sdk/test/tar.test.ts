import { execFileSync } from "node:child_process";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, describe, expect, it } from "vitest";
import { gzipTar } from "../src/tar.js";

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

  it("refuses a path with a NUL byte instead of cutting it short", () => {
    expect(() => gzipTar([{ path: "x.sh\0.txt", content: Buffer.from("x") }])).toThrow(/NUL byte/);
  });
});

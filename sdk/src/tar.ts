import { gzipSync } from "node:zlib";

const BLOCK_SIZE = 512;
const USTAR_NAME_BYTES = 100;

/**
 * Builds a gzip-compressed tar archive of regular files. A path longer than the
 * 100 bytes a ustar header holds is carried in a pax extended header.
 */
export function gzipTar(files: Array<{ path: string; content: Buffer }>): Buffer {
  const blocks: Buffer[] = [];
  for (const { path, content } of files) {
    // A ustar name ends at the first NUL, which would write a shorter path.
    if (path.includes("\0")) {
      throw new Error(`file path must not contain a NUL byte: ${JSON.stringify(path)}`);
    }
    if (Buffer.byteLength(path) > USTAR_NAME_BYTES) {
      blocks.push(...tarEntry("././@PaxHeader", "x", paxRecord("path", path)));
    }
    blocks.push(...tarEntry(path, "0", content));
  }
  blocks.push(Buffer.alloc(2 * BLOCK_SIZE));
  return gzipSync(Buffer.concat(blocks));
}

function tarEntry(name: string, typeflag: string, content: Buffer): Buffer[] {
  const header = Buffer.alloc(BLOCK_SIZE);
  // Cut to 100 bytes; a longer name is in the pax header before this one.
  header.write(name, 0, USTAR_NAME_BYTES, "utf-8");
  header.write("0000644\0", 100); // mode
  header.write("0000000\0", 108); // uid
  header.write("0000000\0", 116); // gid
  header.write(`${content.length.toString(8).padStart(11, "0")}\0`, 124);
  header.write("00000000000\0", 136); // mtime
  header.write(" ".repeat(8), 148); // checksum, counted as spaces
  header.write(typeflag, 156);
  header.write("ustar\u000000", 257); // magic "ustar\0", version "00"

  let checksum = 0;
  for (const byte of header) checksum += byte;
  header.write(`${checksum.toString(8).padStart(6, "0")}\0 `, 148);

  const padding = Buffer.alloc((BLOCK_SIZE - (content.length % BLOCK_SIZE)) % BLOCK_SIZE);
  return [header, content, padding];
}

/** A pax record is "<length> <key>=<value>\n", where length counts its own digits. */
function paxRecord(key: string, value: string): Buffer {
  const rest = ` ${key}=${value}\n`;
  const restBytes = Buffer.byteLength(rest);
  let length = restBytes + String(restBytes).length;
  if (String(length).length > String(restBytes).length) {
    length = restBytes + String(length).length;
  }
  return Buffer.from(`${length}${rest}`);
}

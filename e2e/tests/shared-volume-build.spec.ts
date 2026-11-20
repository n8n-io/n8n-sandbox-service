import { test, expect } from '@playwright/test';
import { execFileSync } from 'node:child_process';
import { createHash, randomUUID } from 'node:crypto';
import * as fs from 'node:fs';
import * as os from 'node:os';
import * as path from 'node:path';
import { PUBLISHER_API_KEY } from './helpers';
import { FIRECRACKER_ONLY } from './tags';

// With SANDBOX_API_VOLUME_BUILD=firecracker, a publish has the runner build the
// volume's ext4 image and stores it next to the tar. The image is read back
// from the local volume store's files, which end in a trailer of 48 bytes, and
// checked with the host's e2fsprogs.
const STORE_TRAILER_BYTES = 48;

type SharedVolume = {
  digest: string;
  bytes: number;
  image?: { bytes: number; digest: string };
};

const sha256 = (data: Buffer) => createHash('sha256').update(data).digest('hex');

const sbinPath = { ...process.env, PATH: `${process.env.PATH}:/usr/sbin:/sbin` };

test.describe('Shared volume images', FIRECRACKER_ONLY, () => {
  let work: string;
  let storeDir: string;

  test.beforeAll(() => {
    test.skip(!process.env.E2E_API_DATA_DIR, 'needs the API data dir from e2e/run-firecracker.sh');
    storeDir = path.join(process.env.E2E_API_DATA_DIR!, 'volumes', 'shared');
    work = fs.mkdtempSync(path.join(os.tmpdir(), 'shared-volume-build-'));
  });

  test.afterAll(() => {
    if (work) fs.rmSync(work, { recursive: true, force: true });
  });

  test('a publish has the runner build the image, stored under the digest it reports', async ({ request }) => {
    test.setTimeout(300_000);
    const publisher = { 'X-Api-Key': PUBLISHER_API_KEY };
    // New content on every run: the API's data dir outlives a run.
    const marker = `published ${randomUUID()}\n`;
    const tree = path.join(work, 'tree');
    fs.mkdirSync(path.join(tree, 'docs/guides'), { recursive: true });
    fs.writeFileSync(path.join(tree, 'docs/guides/readme.md'), marker);
    const tar = path.join(work, 'tree.tar');
    execFileSync('tar', ['-C', tree, '-cf', tar, '.']);
    const body = fs.readFileSync(tar);
    const digest = sha256(body);

    const published = await request.put(`/shared-volumes/sha256:${digest}`, {
      headers: { ...publisher, 'Content-Type': 'application/x-tar' },
      data: body,
      timeout: 280_000,
    });
    expect(published.status(), await published.text()).toBe(201);
    const volume = (await published.json()) as SharedVolume;
    expect(volume.digest).toBe(`sha256:${digest}`);
    expect(volume.image?.digest).toMatch(/^sha256:[0-9a-f]{64}$/);
    expect(volume.image!.bytes).toBeGreaterThan(0);

    // The image file of this volume, whose tar sits next to it.
    const imageDigest = volume.image!.digest.slice('sha256:'.length);
    const image = path.join(work, 'image.ext4');
    const withoutTrailer = (name: string) => {
      const stored = fs.readFileSync(path.join(storeDir, name));
      return stored.subarray(0, Math.max(0, stored.length - STORE_TRAILER_BYTES));
    };
    const matches = fs
      .readdirSync(storeDir)
      .filter((name) => name.endsWith('.ext4'))
      .filter((name) => sha256(withoutTrailer(name)) === imageDigest);
    expect(matches).toHaveLength(1);
    const content = withoutTrailer(matches[0]);
    expect(content.length).toBe(volume.image!.bytes);
    fs.writeFileSync(image, content);
    const tarFile = path.join(storeDir, matches[0].replace(/\.ext4$/, '.tar'));
    expect(sha256(fs.readFileSync(tarFile).subarray(0, body.length))).toBe(digest);

    execFileSync('e2fsck', ['-fn', image], { stdio: 'pipe', env: sbinPath });
    const read = execFileSync('debugfs', ['-R', 'cat /docs/guides/readme.md', image], {
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'pipe'],
      env: sbinPath,
    });
    expect(read).toBe(marker);

    const got = await request.get(`/shared-volumes/sha256:${digest}`, { headers: publisher });
    expect(got.status()).toBe(200);
    expect(((await got.json()) as SharedVolume).image).toEqual(volume.image);

    const deleted = await request.delete(`/shared-volumes/sha256:${digest}`, { headers: publisher });
    expect(deleted.status()).toBe(204);
    expect(fs.existsSync(path.join(storeDir, matches[0]))).toBe(false);
    expect(fs.existsSync(tarFile)).toBe(false);
  });
});

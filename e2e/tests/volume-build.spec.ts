import { test, expect } from '@playwright/test';
import { execFileSync, spawn } from 'node:child_process';
import { createHash } from 'node:crypto';
import * as fs from 'node:fs';
import * as os from 'node:os';
import * as path from 'node:path';
import { createSandbox, deleteSandbox, e2eProjectDir, execWithTransientRetry, scrapeRunnerMetrics } from './helpers';
import { parseCounter } from './metrics-helpers';
import { FIRECRACKER_ONLY } from './tags';

type BuildOutcome = { result: 'ok' | 'failed' | 'refused' | 'error'; reason?: string; image_bytes?: number; sha256?: string };

// The runner builds a volume image from a tar in a builder microVM. No API
// route asks for one yet, so the tests call the runner through e2e-runnerctl.
// The image is checked with debugfs and e2fsck from the host's e2fsprogs: what
// the builder says about its own work is not taken as proof.
test.describe('Volume build', FIRECRACKER_ONLY, () => {
  let work: string;
  let runnerctl: string;

  const builds = (result: string) =>
    parseCounter(scrapeRunnerMetrics(), 'sandbox_volume_builds_total', { role: 'runner', result });

  const lastLine = (stdout: string): BuildOutcome => JSON.parse(stdout.trim().split('\n').pop() || '{}');

  function buildVolume(args: string[]): BuildOutcome {
    try {
      return lastLine(execFileSync(runnerctl, ['build-volume', ...args], { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] }));
    } catch (err: unknown) {
      return lastLine(String((err as { stdout?: unknown }).stdout ?? ''));
    }
  }

  const debugfs = (image: string, request: string): string =>
    execFileSync('debugfs', ['-R', request, image], { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] });

  test.beforeAll(() => {
    test.skip(
      !process.env.E2E_RUNNER_CONTROL_GRPC_ADDR || !process.env.E2E_RUNNER_API_KEY,
      'needs the runner control address (from e2e/run-firecracker.sh)',
    );
    work = fs.mkdtempSync(path.join(os.tmpdir(), 'volume-build-'));
    runnerctl = path.join(work, 'e2e-runnerctl');
    // Built once: a second build must reach the runner while the first runs.
    execFileSync('go', ['build', '-o', runnerctl, './cmd/e2e-runnerctl'], { cwd: e2eProjectDir(), stdio: 'inherit' });

    const tree = path.join(work, 'tree');
    fs.mkdirSync(path.join(tree, 'docs/guides/deep'), { recursive: true });
    fs.mkdirSync(path.join(tree, 'bin'));
    fs.writeFileSync(path.join(tree, 'docs/guides/deep/readme.md'), 'read me\n');
    fs.writeFileSync(path.join(tree, 'bin/run.sh'), '#!/bin/sh\necho ran\n', { mode: 0o755 });
    fs.symlinkSync('../docs/guides/deep/readme.md', path.join(tree, 'bin/readme'));
    execFileSync('tar', ['-C', tree, '-cf', path.join(work, 'tree.tar'), '.']);

    const linked = path.join(work, 'linked');
    fs.mkdirSync(linked);
    fs.writeFileSync(path.join(linked, 'original'), 'linked twice');
    fs.linkSync(path.join(linked, 'original'), path.join(linked, 'copy'));
    execFileSync('tar', ['-C', linked, '-cf', path.join(work, 'linked.tar'), 'original', 'copy']);
  });

  test.afterAll(() => {
    if (work) fs.rmSync(work, { recursive: true, force: true });
  });

  test('a tar becomes an ext4 image with its folders, executable and link', async () => {
    test.setTimeout(180_000);
    const image = path.join(work, 'tree.ext4');
    const before = builds('success');

    const outcome = buildVolume([path.join(work, 'tree.tar'), image]);

    expect(outcome.result, JSON.stringify(outcome)).toBe('ok');
    expect(fs.statSync(image).size).toBe(outcome.image_bytes);
    expect(createHash('sha256').update(fs.readFileSync(image)).digest('hex')).toBe(outcome.sha256);
    expect(builds('success')).toBe(before + 1);

    execFileSync('e2fsck', ['-fn', image], { stdio: 'pipe' });
    expect(debugfs(image, 'cat /docs/guides/deep/readme.md')).toBe('read me\n');
    expect(debugfs(image, 'cat /bin/run.sh')).toBe('#!/bin/sh\necho ran\n');

    // The host user packed the tar; the image belongs to root.
    const script = debugfs(image, 'stat /bin/run.sh');
    expect(script).toMatch(/Type: regular\s+Mode:\s+0755/);
    expect(script).toMatch(/User:\s+0\s+Group:\s+0/);
    expect(debugfs(image, 'stat /docs/guides/deep/readme.md')).toMatch(/Type: regular\s+Mode:\s+0644/);
    expect(debugfs(image, 'stat /docs/guides/deep')).toMatch(/Type: directory\s+Mode:\s+0755/);

    const link = debugfs(image, 'stat /bin/readme');
    expect(link).toMatch(/Type: symlink/);
    expect(link).toContain('Fast link dest: "../docs/guides/deep/readme.md"');

    expect(debugfs(image, 'stats')).not.toMatch(/Filesystem features:.*has_journal/);
  });

  test('a tar with a hard link fails with reason tar', async () => {
    test.setTimeout(180_000);
    const before = builds('tar');

    // The counts are given: counting them would already refuse the hard link.
    const tar = path.join(work, 'linked.tar');
    const counts = ['-entries', '2', '-content-bytes', '12', '-decompressed-bytes', String(fs.statSync(tar).size)];
    const outcome = buildVolume([...counts, tar, path.join(work, 'linked.ext4')]);

    expect(outcome).toEqual({ result: 'failed', reason: 'tar' });
    expect(builds('tar')).toBe(before + 1);
  });

  test('a build leaves the runner to its sandboxes and refuses a second build', async () => {
    test.setTimeout(240_000);
    const busyBefore = builds('busy');

    // The first build holds the runner's one build from its header on.
    const first = spawn(runnerctl, ['build-volume', '-hold', '15s', path.join(work, 'tree.tar'), path.join(work, 'held.ext4')], {
      stdio: ['ignore', 'pipe', 'inherit'],
    });
    let firstStdout = '';
    first.stdout.on('data', (chunk) => (firstStdout += chunk));
    const firstDone = new Promise<number | null>((resolve) => first.on('close', resolve));
    await new Promise((resolve) => setTimeout(resolve, 3_000));

    let sandbox: string | undefined;
    try {
      expect(buildVolume([path.join(work, 'tree.tar'), path.join(work, 'second.ext4')])).toEqual({ result: 'refused' });
      expect(builds('busy')).toBe(busyBefore + 1);

      sandbox = await createSandbox();
      const served = await execWithTransientRetry(sandbox, "printf '%s' served-during-build");
      expect(served.stdout.trim()).toBe('served-during-build');
    } finally {
      if (sandbox) await deleteSandbox(sandbox);
      expect(await firstDone, firstStdout).toBe(0);
    }
    expect(lastLine(firstStdout).result).toBe('ok');
  });
});

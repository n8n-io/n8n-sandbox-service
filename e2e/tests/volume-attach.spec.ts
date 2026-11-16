import { execFile, spawnSync } from 'node:child_process';
import { createHash, randomBytes, randomUUID } from 'node:crypto';
import { chmodSync, createReadStream, mkdirSync, mkdtempSync, readFileSync, rmSync, statSync, writeFileSync } from 'node:fs';
import * as https from 'node:https';
import type { AddressInfo } from 'node:net';
import { tmpdir } from 'node:os';
import { basename, dirname, join } from 'node:path';
import { promisify } from 'node:util';
import { test, expect } from '@playwright/test';
import { scrapeRunnerMetrics } from './helpers';
import { parseGauge } from './metrics-helpers';
import { FIRECRACKER_ONLY } from './tags';

// Sandboxes here are created on the runner directly, through cmd/e2e-runnerctl:
// the API has no volumes field yet. The images are served over https from the
// host the runner registers with (127.0.0.1, see SANDBOX_RUNNER_API_GRPC_ADDR),
// with the API's registration server certificate, so the runner fetches them
// with its registration identity like objects the API serves.

const execFileAsync = promisify(execFile);
const JAILER_ID = '1000';
const PINNED = 'sandbox_volume_cache_pinned_entries';

type Image = { path: string; sha256: string; size: number };
type VolumeOption = {
  scope: string;
  sha256: string;
  size: number;
  kind: string;
  url: string;
  mount_path: string;
  optional?: boolean;
};
type AppliedVolume = { mount_path: string; attached: boolean; reason?: string; url?: string };
type CreateOutcome = { result: string; applied?: { egress?: string; volumes?: AppliedVolume[] }; code?: string };
type ExecOutcome = { status: number; exit_code: number; stdout: string; stderr: string; body?: string };

const token = `sig=e2e-${randomBytes(12).toString('hex')}`;
let workDir = '';
let runnerctl = '';
let server: https.Server | undefined;
let baseURL = '';
const served = new Map<string, Image>();

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

function run(command: string, args: string[]): { status: number | null; output: string } {
  const result = spawnSync(command, args, {
    encoding: 'utf8',
    env: { ...process.env, PATH: `${process.env.PATH}:/usr/sbin:/sbin` },
  });
  return { status: result.status, output: `${result.stdout}${result.stderr}`.trim() };
}

// buildImage makes an ext4 image of the given files on the host, without root.
function buildImage(name: string, files: Record<string, { content: string; mode?: number }>): Image {
  const source = join(workDir, `${name}-files`);
  for (const [path, file] of Object.entries(files)) {
    const target = join(source, path);
    mkdirSync(dirname(target), { recursive: true });
    writeFileSync(target, file.content);
    chmodSync(target, file.mode ?? 0o644);
  }
  const path = join(workDir, `${name}.ext4`);
  const made = run('mkfs.ext4', ['-q', '-F', '-E', 'root_owner=0:0', '-d', source, path, '16M']);
  if (made.status !== 0) {
    throw new Error(`mkfs.ext4 failed: ${made.output}`);
  }
  const content = readFileSync(path);
  return { path, sha256: createHash('sha256').update(content).digest('hex'), size: statSync(path).size };
}

function serve(name: string, image: Image): string {
  served.set(`/${name}`, image);
  return `${baseURL}/${name}?${token}`;
}

function volume(image: Image, url: string, mountPath: string, optional = false): VolumeOption {
  return { scope: 'shared', sha256: image.sha256, size: image.size, kind: 'ext4', url, mount_path: mountPath, optional };
}

async function runnerctlJSON<T>(args: string[]): Promise<T> {
  try {
    const { stdout } = await execFileAsync(runnerctl, args, { env: process.env, maxBuffer: 16 << 20 });
    return JSON.parse(stdout.trim().split('\n').pop() ?? '') as T;
  } catch (err) {
    const failed = err as { stdout?: string; stderr?: string };
    const line = (failed.stdout ?? '').trim().split('\n').pop();
    if (!line) {
      throw new Error(`e2e-runnerctl ${args[0]} failed: ${failed.stderr}`);
    }
    return JSON.parse(line) as T;
  }
}

async function create(id: string, volumes?: VolumeOption[]): Promise<CreateOutcome> {
  const options = volumes ? JSON.stringify({ volumes }) : '{}';
  return runnerctlJSON<CreateOutcome>(['create', id, options]);
}

async function remove(id: string): Promise<void> {
  await execFileAsync(runnerctl, ['delete', id], { env: process.env });
}

async function exec(id: string, command: string): Promise<ExecOutcome> {
  return runnerctlJSON<ExecOutcome>(['exec', id, command]);
}

async function stop(id: string): Promise<void> {
  await execFileAsync(runnerctl, ['stop', id], { env: process.env });
}

async function read(id: string, path: string): Promise<string> {
  const out = await exec(id, `cat ${path}`);
  expect(out.status, out.body).toBe(200);
  expect(out.exit_code, out.stderr).toBe(0);
  return out.stdout;
}

function vmID(sandboxId: string): string {
  return `sandbox-${createHash('sha256').update(sandboxId).digest('hex').slice(0, 12)}`;
}

function firecrackerPid(sandboxId: string): string {
  const processes = run('pgrep', ['-x', 'firecracker', '-a']).output.split('\n');
  const match = processes.find((line) => line.includes(vmID(sandboxId)));
  if (!match) {
    throw new Error(`no firecracker process for ${vmID(sandboxId)} among:\n${processes.join('\n')}`);
  }
  return match.split(' ')[0];
}

function asJailerUid(script: string): { status: number | null; output: string } {
  return run('sudo', ['setpriv', `--reuid=${JAILER_ID}`, `--regid=${JAILER_ID}`, '--clear-groups', 'sh', '-c', script]);
}

function hostMounts(sandboxId: string): string[] {
  return readFileSync('/proc/self/mountinfo', 'utf8')
    .split('\n')
    .filter((line) => line.includes(`/${vmID(sandboxId)}/`));
}

test.describe('Volumes attached on create', FIRECRACKER_ONLY, () => {
  test.describe.configure({ timeout: 180_000 });
  let data: Image;
  let other: Image;

  test.beforeAll(async () => {
    const tlsDir = process.env.E2E_TLS_DIR;
    test.skip(
      !tlsDir || !process.env.E2E_RUNNER_CONTROL_GRPC_ADDR || !process.env.E2E_RUNNER_HTTP_ADDR,
      'needs E2E_TLS_DIR and the runner addresses from e2e/run-firecracker.sh',
    );
    workDir = mkdtempSync(join(tmpdir(), 'volume-attach-'));
    runnerctl = join(workDir, 'e2e-runnerctl');
    const built = spawnSync('go', ['build', '-o', runnerctl, './cmd/e2e-runnerctl'], {
      cwd: process.env.E2E_PROJECT_DIR || join(process.cwd(), '..'),
      encoding: 'utf8',
    });
    if (built.status !== 0) {
      throw new Error(`build e2e-runnerctl: ${built.stderr}`);
    }

    server = https.createServer(
      {
        key: readFileSync(join(tlsDir!, 'api', 'grpc-server.key')),
        cert: readFileSync(join(tlsDir!, 'api', 'grpc-server.crt')),
        minVersion: 'TLSv1.3',
      },
      (req, res) => {
        const url = new URL(req.url ?? '/', 'https://localhost');
        const image = served.get(url.pathname);
        if (!image || `${url.search}` !== `?${token}`) {
          res.writeHead(404).end();
          return;
        }
        res.writeHead(200, { 'Content-Length': String(image.size) });
        createReadStream(image.path).pipe(res);
      },
    );
    await new Promise<void>((resolve) => server!.listen(0, '127.0.0.1', resolve));
    baseURL = `https://127.0.0.1:${(server.address() as AddressInfo).port}`;

    data = buildImage('data', {
      'hello.txt': { content: 'hello from a volume\n' },
      'bin/hello.sh': { content: '#!/bin/sh\necho volume-exec-ok\n', mode: 0o755 },
    });
    other = buildImage('other', { 'other.txt': { content: 'the other volume\n' } });
  });

  test.afterAll(async () => {
    await new Promise<void>((resolve) => (server ? server.close(() => resolve()) : resolve()));
    if (workDir) {
      rmSync(workDir, { recursive: true, force: true });
    }
  });

  test('a volume is readable, read-only, nodev and nosuid, and runs executables', async () => {
    const id = randomUUID();
    const created = await create(id, [volume(data, serve('data', data), '/mnt/data')]);
    try {
      expect(created.result).toBe('ok');
      expect(created.applied?.volumes).toEqual([
        expect.objectContaining({ mount_path: '/mnt/data', attached: true }),
      ]);
      expect(JSON.stringify(created.applied)).not.toContain(token);

      expect(await read(id, '/mnt/data/hello.txt')).toBe('hello from a volume\n');

      const write = await exec(id, 'touch /mnt/data/new');
      expect(write.exit_code).not.toBe(0);
      expect(write.stderr).toContain('Read-only file system');

      // The guest has no /proc: statvfs reports the mount flags.
      const flags = await exec(
        id,
        `python3 -c "import os; f = os.statvfs('/mnt/data').f_flag; print(f & os.ST_RDONLY, f & os.ST_NOSUID, f & os.ST_NODEV, f & os.ST_NOEXEC)"`,
      );
      expect(flags.stdout.trim(), flags.stderr).toBe('1 2 4 0');

      const ran = await exec(id, '/mnt/data/bin/hello.sh');
      expect(ran.stdout.trim(), ran.stderr).toBe('volume-exec-ok');

      // On the host, the jailed Firecracker can't write the image: uid 1000 is
      // refused by the file's mode, root by the read-only bind; and the drive
      // path is a mount point it can't replace.
      const image = `/proc/${firecrackerPid(id)}/root/vol0.img`;
      expect(asJailerUid(`head -c 2048 ${image} | wc -c`).output).toBe('2048');
      const jailerWrite = asJailerUid(`: >> ${image}`);
      expect(jailerWrite.status, jailerWrite.output).not.toBe(0);
      expect(jailerWrite.output).toContain('Permission denied');
      const rootWrite = run('sudo', ['sh', '-c', `: >> ${image}`]);
      expect(rootWrite.status, rootWrite.output).not.toBe(0);
      expect(rootWrite.output).toContain('Read-only file system');
      const replace = asJailerUid(`: > ${image}.new && mv -f ${image}.new ${image}`);
      expect(replace.status, replace.output).not.toBe(0);
      expect(run('sudo', ['stat', '-c', '%u %a', '-L', image]).output).toBe('0 444');
    } finally {
      await remove(id);
    }
  });

  test('content survives stop and wake, and a cold boot', async () => {
    const id = randomUUID();
    const created = await create(id, [volume(data, serve('data', data), '/mnt/data')]);
    try {
      expect(created.result).toBe('ok');

      await stop(id);
      expect(await read(id, '/mnt/data/hello.txt')).toBe('hello from a volume\n');

      // Kill the guest; the next request cold boots it and is refused with 409.
      await exec(id, 'kill -TERM 1').catch(() => undefined);
      const deadline = Date.now() + 90_000;
      let recovered = false;
      while (!recovered && Date.now() < deadline) {
        const out = await exec(id, 'true').catch(() => undefined);
        recovered = out?.status === 409;
        if (!recovered) {
          await sleep(500);
        }
      }
      expect(recovered, 'the guest was not recovered by a cold boot').toBe(true);
      expect(await read(id, '/mnt/data/hello.txt')).toBe('hello from a volume\n');
    } finally {
      await remove(id);
    }
  });

  test('two volumes at sibling paths, and two sandboxes on one image', async () => {
    const first = randomUUID();
    const second = randomUUID();
    const dataURL = serve('data', data);
    try {
      const both = await create(first, [
        volume(data, dataURL, '/mnt/a'),
        volume(other, serve('other', other), '/mnt/ab'),
      ]);
      expect(both.result).toBe('ok');
      expect(await read(first, '/mnt/a/hello.txt')).toBe('hello from a volume\n');
      expect(await read(first, '/mnt/ab/other.txt')).toBe('the other volume\n');

      expect((await create(second, [volume(data, dataURL, '/mnt/a')])).result).toBe('ok');
      expect(await read(second, '/mnt/a/hello.txt')).toBe('hello from a volume\n');

      // One cached file, bound into both jails.
      const inode = (sandboxId: string) =>
        run('sudo', ['stat', '-L', '-c', '%d:%i', `/proc/${firecrackerPid(sandboxId)}/root/vol0.img`]).output;
      expect(inode(first)).toBe(inode(second));
    } finally {
      await remove(first);
      await remove(second);
    }
  });

  test('an optional volume that fails is left out; a required one fails the create and leaves nothing', async () => {
    const id = randomUUID();
    const missing = `${baseURL}/missing?${token}`;
    const created = await create(id, [
      volume(data, serve('data', data), '/mnt/data'),
      volume(other, missing, '/mnt/optional', true),
    ]);
    try {
      expect(created.result).toBe('ok');
      expect(created.applied?.volumes).toEqual([
        expect.objectContaining({ mount_path: '/mnt/data', attached: true }),
        expect.objectContaining({ mount_path: '/mnt/optional', attached: false, reason: 'download' }),
      ]);
      expect(await read(id, '/mnt/data/hello.txt')).toBe('hello from a volume\n');
      expect((await exec(id, 'test -e /mnt/optional/other.txt')).exit_code).not.toBe(0);
    } finally {
      await remove(id);
    }

    const pinnedBefore = parseGauge(scrapeRunnerMetrics(), PINNED);
    const failed = randomUUID();
    const refused = await create(failed, [
      volume(data, serve('data', data), '/mnt/data'),
      volume(other, missing, '/mnt/required'),
    ]);
    expect(refused.result).toBe('error');
    expect(run('sudo', ['test', '-e', `/srv/jailer/firecracker/${vmID(failed)}`]).status).not.toBe(0);
    expect(hostMounts(failed)).toEqual([]);
    expect(parseGauge(scrapeRunnerMetrics(), PINNED)).toBe(pinnedBefore);
  });

  test('a sandbox without volumes is unchanged', async () => {
    const id = randomUUID();
    const created = await create(id);
    try {
      expect(created).toEqual({ result: 'ok', applied: { egress: 'public' } });
      // The mount point is the fifth field of a mountinfo line.
      const mountPoints = hostMounts(id).map((line) => basename(line.split(' ')[4]));
      expect(mountPoints.sort()).toEqual(['rootfs.ext4', 'snapshot_mem', 'snapshot_state', 'vmlinux']);
      expect((await exec(id, 'echo ok')).stdout.trim()).toBe('ok');
    } finally {
      await remove(id);
    }
  });

  test('the read URLs reach no runner log line', () => {
    const log = process.env.E2E_RUNNER_LOG;
    test.skip(!log, 'needs E2E_RUNNER_LOG from e2e/run-firecracker.sh');
    const content = readFileSync(log!, 'utf8');
    expect(content).toContain('volume cache download finished');
    expect(content).not.toContain(token.split('=')[1]);
  });
});

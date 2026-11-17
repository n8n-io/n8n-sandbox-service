import { execFile, spawnSync } from 'node:child_process';
import { createHash, randomBytes, randomUUID } from 'node:crypto';
import { chmodSync, mkdirSync, mkdtempSync, readFileSync, rmSync, statSync, symlinkSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { promisify } from 'node:util';
import { test, expect } from '@playwright/test';
import { scrapeRunnerMetrics } from './helpers';
import { parseGauge } from './metrics-helpers';
import { DOCKER_ONLY } from './tags';

// Sandboxes here are created on the runner directly, through cmd/e2e-runnerctl:
// the API has no volumes field yet. The tars are served over https from the
// host the runner registers with, the API container, by a file server sharing
// its network namespace, with the API's registration server certificate and
// client CA. It requires a client certificate, so the downloads pass only if
// the runner presents its registration identity, as for objects the API
// serves.

const execFileAsync = promisify(execFile);
const PINNED = 'sandbox_volume_cache_pinned_entries';
const SERVE_PORT = 9444;
const CACHE_DIR = '/var/sandboxes/volume-cache';

type Tar = { name: string; sha256: string; size: number; counts: Record<string, number> };
type VolumeOption = {
  scope: string;
  sha256: string;
  size: number;
  kind: string;
  url: string;
  mount_path: string;
  optional?: boolean;
  tar: Record<string, number>;
};
type AppliedVolume = { mount_path: string; attached: boolean; reason?: string };
type CreateOutcome = { result: string; applied?: { egress?: string; volumes?: AppliedVolume[] }; code?: string };
type ExecOutcome = { status: number; exit_code: number; stdout: string; stderr: string; body?: string };

const token = `sig=e2e-${randomBytes(12).toString('hex')}`;
const serverName = `e2e-volume-files-${process.pid}`;
let workDir = '';
let serveDir = '';
let runnerctl = '';
let baseURL = '';
let runnerContainer = '';

function run(command: string, args: string[], env: NodeJS.ProcessEnv = process.env): { status: number | null; output: string } {
  const result = spawnSync(command, args, { encoding: 'utf8', env });
  return { status: result.status, output: `${result.stdout}${result.stderr}`.trim() };
}

function mustRun(command: string, args: string[], env?: NodeJS.ProcessEnv): string {
  const result = run(command, args, env);
  if (result.status !== 0) {
    throw new Error(`${command} ${args.join(' ')} failed: ${result.output}`);
  }
  return result.output;
}

// runnerDocker runs the docker CLI inside the runner container, against the
// dockerd that runs the sandboxes.
function runnerDocker(args: string[]): string {
  return mustRun('docker', ['exec', runnerContainer, 'docker', ...args]);
}

function containerName(sandboxId: string): string {
  return `sandbox-${sandboxId.slice(0, 12)}`;
}

// buildTar tars the files and links with the system tar as ustar, gzipped if
// asked, into the served dir, and counts it as the API would.
function buildTar(
  name: string,
  entries: Record<string, { content?: string; mode?: number; link?: string }>,
  gzip = false,
): Tar {
  const source = join(workDir, `${name}-files`);
  for (const [path, entry] of Object.entries(entries)) {
    const target = join(source, path);
    mkdirSync(dirname(target), { recursive: true });
    if (entry.link) {
      symlinkSync(entry.link, target);
      continue;
    }
    writeFileSync(target, entry.content ?? '');
    chmodSync(target, entry.mode ?? 0o644);
  }
  const tarName = `${name}.tar${gzip ? '.gz' : ''}`;
  const path = join(serveDir, tarName);
  mustRun('tar', ['--format=ustar', gzip ? '-czf' : '-cf', path, '-C', source, '.'], {
    ...process.env,
    COPYFILE_DISABLE: '1',
  });
  const counts = JSON.parse(mustRun(runnerctl, ['tar-counts', path])) as Record<string, number>;
  return {
    name: tarName,
    sha256: createHash('sha256').update(readFileSync(path)).digest('hex'),
    size: statSync(path).size,
    counts,
  };
}

function url(name: string): string {
  return `${baseURL}/${name}?${token}`;
}

function volume(tar: Tar, mountPath: string, optional = false, from = url(tar.name)): VolumeOption {
  return {
    scope: 'shared',
    sha256: tar.sha256,
    size: tar.size,
    kind: 'tar',
    url: from,
    mount_path: mountPath,
    optional,
    tar: tar.counts,
  };
}

// e2e-runnerctl execs through the runner's HTTP listener, which e2e/run.sh
// publishes on the host.
function runnerctlEnv(): NodeJS.ProcessEnv {
  return { ...process.env, E2E_RUNNER_HTTP_ADDR: process.env.E2E_RUNNER_PUBLISHED_HTTP_ADDR };
}

async function runnerctlJSON<T>(args: string[]): Promise<T> {
  try {
    const { stdout } = await execFileAsync(runnerctl, args, { env: runnerctlEnv(), maxBuffer: 16 << 20 });
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
  await execFileAsync(runnerctl, ['delete', id], { env: runnerctlEnv() });
}

async function stop(id: string): Promise<void> {
  await execFileAsync(runnerctl, ['stop', id], { env: runnerctlEnv() });
}

async function exec(id: string, command: string): Promise<ExecOutcome> {
  return runnerctlJSON<ExecOutcome>(['exec', id, command]);
}

async function read(id: string, path: string): Promise<string> {
  const out = await exec(id, `cat ${path}`);
  expect(out.status, out.body).toBe(200);
  expect(out.exit_code, out.stderr).toBe(0);
  return out.stdout;
}

// mountOptions returns the per-mount options of the mount at path, as the
// sandbox sees it: the sixth field of its mountinfo line.
async function mountOptions(id: string, path: string): Promise<string[]> {
  const out = await exec(id, `grep ' ${path} ' /proc/self/mountinfo`);
  expect(out.exit_code, `no mount at ${path}: ${out.stderr}`).toBe(0);
  return out.stdout.trim().split(' ')[5].split(',');
}

test.describe('Docker volumes attached on create', DOCKER_ONLY, () => {
  test.describe.configure({ timeout: 180_000 });
  let data: Tar;
  let other: Tar;
  // Never served, so never cached: the cache would serve a cached object
  // whatever its URL.
  let absent: Tar;

  test.beforeAll(async () => {
    const tlsDir = process.env.E2E_TLS_DIR;
    const apiContainer = process.env.E2E_API_CONTAINER_NAME;
    runnerContainer = process.env.E2E_RUNNER_CONTAINER_NAME ?? '';
    test.skip(
      !tlsDir || !apiContainer || !runnerContainer || !process.env.E2E_RUNNER_PUBLISHED_HTTP_ADDR,
      'needs E2E_TLS_DIR, the API and runner containers, and the runner addresses from e2e/run.sh',
    );
    workDir = mkdtempSync(join(tmpdir(), 'volume-attach-docker-'));
    const projectDir = process.env.E2E_PROJECT_DIR || join(process.cwd(), '..');
    runnerctl = join(workDir, 'e2e-runnerctl');
    mustRun('go', ['build', '-C', projectDir, '-o', runnerctl, './cmd/e2e-runnerctl']);

    // The file server runs in the API container's network namespace, so the
    // runner reaches it at the API's host name.
    const linuxRunnerctl = join(workDir, 'e2e-runnerctl-linux');
    const arch = mustRun('docker', ['version', '-f', '{{.Server.Arch}}']);
    mustRun('go', ['build', '-C', projectDir, '-o', linuxRunnerctl, './cmd/e2e-runnerctl'], {
      ...process.env,
      GOOS: 'linux',
      GOARCH: arch,
      CGO_ENABLED: '0',
    });
    serveDir = join(workDir, 'serve');
    mkdirSync(serveDir);
    const apiImage = mustRun('docker', ['inspect', '-f', '{{.Config.Image}}', apiContainer!]);
    mustRun('docker', [
      'run', '-d', '--rm', '--name', serverName, '--network', `container:${apiContainer}`, '--user', '0:0',
      '-v', `${linuxRunnerctl}:/e2e-runnerctl:ro`, '-v', `${serveDir}:/srv:ro`, '-v', `${tlsDir}/api:/tls:ro`,
      '--entrypoint', '/e2e-runnerctl', apiImage,
      'serve-files', '-addr', `:${SERVE_PORT}`, '-cert', '/tls/grpc-server.crt', '-key', '/tls/grpc-server.key',
      '-client-ca', '/tls/ca.crt', '-dir', '/srv', '-token', token,
    ]);
    const deadline = Date.now() + 30_000;
    while (!run('docker', ['logs', serverName]).output.includes('listening')) {
      if (Date.now() > deadline) {
        throw new Error(`file server did not start: ${run('docker', ['logs', serverName]).output}`);
      }
      await new Promise((resolve) => setTimeout(resolve, 200));
    }
    baseURL = `https://${apiContainer}:${SERVE_PORT}`;

    data = buildTar('data', {
      'hello.txt': { content: 'hello from a volume\n' },
      'bin/hello.sh': { content: '#!/bin/sh\necho volume-exec-ok\n', mode: 0o755 },
      'node_modules/pkg/cli.js': { content: 'console.log("cli")\n' },
      'node_modules/.bin/cli': { link: '../pkg/cli.js' },
      'links/up': { link: '../hello.txt' },
      'links/absolute': { link: '/mnt/data/hello.txt' },
    });
    other = buildTar('other', { 'other.txt': { content: 'the other volume\n' } }, true);
    absent = buildTar('absent', { 'absent.txt': { content: 'never served\n' } });
  });

  test.afterAll(async () => {
    run('docker', ['rm', '-f', serverName]);
    if (workDir) {
      rmSync(workDir, { recursive: true, force: true });
    }
  });

  test('a volume is readable, read-only, nodev and nosuid, and runs executables', async () => {
    const id = randomUUID();
    const created = await create(id, [volume(data, '/mnt/data')]);
    try {
      expect(created.result, JSON.stringify(created)).toBe('ok');
      expect(created.applied?.volumes).toEqual([expect.objectContaining({ mount_path: '/mnt/data', attached: true })]);
      expect(JSON.stringify(created.applied)).not.toContain(token);

      expect(await read(id, '/mnt/data/hello.txt')).toBe('hello from a volume\n');
      const write = await exec(id, 'touch /mnt/data/new');
      expect(write.exit_code).not.toBe(0);
      expect(write.stderr).toContain('Read-only file system');

      const options = await mountOptions(id, '/mnt/data');
      expect(options).toEqual(expect.arrayContaining(['ro', 'nosuid', 'nodev']));
      expect(options).not.toContain('noexec');
      const ran = await exec(id, '/mnt/data/bin/hello.sh');
      expect(ran.stdout.trim(), ran.stderr).toBe('volume-exec-ok');

      // Root owns the tree and the mount target: the sandbox's user can read
      // it, change nothing in it, and not put a link in the target's place.
      const owners = await exec(id, 'stat -c "%n %u %a" /mnt /mnt/data /mnt/data/hello.txt /mnt/data/bin/hello.sh');
      expect(owners.stdout.trim().split('\n')).toEqual([
        '/mnt 0 755',
        '/mnt/data 0 755',
        '/mnt/data/hello.txt 0 644',
        '/mnt/data/bin/hello.sh 0 755',
      ]);
      expect((await exec(id, 'find /mnt/data ! -type l -perm /022')).stdout).toBe('');
      expect((await exec(id, 'ln -s /tmp /mnt/planted')).exit_code).not.toBe(0);
      expect((await exec(id, 'mv /mnt/data /mnt/moved')).exit_code).not.toBe(0);

      // In the runner: one tree, no tar or partial unpack left, and the cache
      // dir on a nosuid,nodev mount.
      const cached = mustRun('docker', ['exec', runnerContainer, 'ls', '-A', CACHE_DIR]).split('\n');
      expect(cached).toContainEqual(expect.stringMatching(new RegExp(`^shared\\.${data.sha256}\\.\\d+$`)));
      expect(cached.filter((name) => name.startsWith('.'))).toEqual([]);
      const cacheMount = mustRun('docker', ['exec', runnerContainer, 'grep', ` ${CACHE_DIR} `, '/proc/self/mountinfo']);
      expect(cacheMount.split('\n').pop()!.split(' ')[5].split(',')).toEqual(expect.arrayContaining(['nosuid', 'nodev']));
      expect(runnerDocker(['inspect', '-f', '{{index .Config.Labels "sandbox-service.volumes"}}', containerName(id)])).toBe('true');
    } finally {
      await remove(id);
    }
  });

  test('links with .. and absolute targets resolve inside the sandbox', async () => {
    const id = randomUUID();
    expect((await create(id, [volume(data, '/mnt/data')])).result).toBe('ok');
    try {
      expect(await read(id, '/mnt/data/links/up')).toBe('hello from a volume\n');
      expect(await read(id, '/mnt/data/links/absolute')).toBe('hello from a volume\n');
      expect(await read(id, '/mnt/data/node_modules/.bin/cli')).toBe('console.log("cli")\n');
      expect((await exec(id, 'readlink /mnt/data/links/absolute')).stdout.trim()).toBe('/mnt/data/hello.txt');
    } finally {
      await remove(id);
    }
  });

  test('content survives stop and wake', async () => {
    const id = randomUUID();
    expect((await create(id, [volume(data, '/mnt/data')])).result).toBe('ok');
    try {
      const pinned = parseGauge(scrapeRunnerMetrics(), PINNED);
      await stop(id);
      expect(runnerDocker(['inspect', '-f', '{{.State.Running}}', containerName(id)])).toBe('false');
      expect(parseGauge(scrapeRunnerMetrics(), PINNED)).toBe(pinned);

      expect(await read(id, '/mnt/data/hello.txt')).toBe('hello from a volume\n');
      expect(await mountOptions(id, '/mnt/data')).toEqual(expect.arrayContaining(['ro', 'nosuid', 'nodev']));
    } finally {
      await remove(id);
    }
  });

  test('two volumes at sibling paths, and two sandboxes on one object', async () => {
    const first = randomUUID();
    const second = randomUUID();
    try {
      const both = await create(first, [volume(data, '/mnt/data'), volume(other, '/mnt/data2')]);
      expect(both.result, JSON.stringify(both)).toBe('ok');
      expect(await read(first, '/mnt/data/hello.txt')).toBe('hello from a volume\n');
      expect(await read(first, '/mnt/data2/other.txt')).toBe('the other volume\n');

      expect((await create(second, [volume(data, '/mnt/data')])).result).toBe('ok');
      expect(await read(second, '/mnt/data/hello.txt')).toBe('hello from a volume\n');

      // One cached tree, bound into both containers.
      const source = (sandboxId: string) =>
        runnerDocker(['inspect', '-f', '{{range .Mounts}}{{if eq .Destination "/mnt/data"}}{{.Source}}{{end}}{{end}}', containerName(sandboxId)]);
      expect(source(first)).toMatch(new RegExp(`^${CACHE_DIR}/shared\\.${data.sha256}\\.\\d+$`));
      expect(source(second)).toBe(source(first));
    } finally {
      await remove(first);
      await remove(second);
    }
  });

  test('an optional volume that fails is left out; a required one fails the create and leaves nothing', async () => {
    const id = randomUUID();
    const missing = url('missing.tar');
    const created = await create(id, [volume(data, '/mnt/data'), volume(absent, '/mnt/optional', true, missing)]);
    try {
      expect(created.result, JSON.stringify(created)).toBe('ok');
      expect(created.applied?.volumes).toEqual([
        expect.objectContaining({ mount_path: '/mnt/data', attached: true }),
        expect.objectContaining({ mount_path: '/mnt/optional', attached: false, reason: 'download' }),
      ]);
      expect(await read(id, '/mnt/data/hello.txt')).toBe('hello from a volume\n');
      expect((await exec(id, 'test -e /mnt/optional')).exit_code).not.toBe(0);
    } finally {
      await remove(id);
    }

    const pinnedBefore = parseGauge(scrapeRunnerMetrics(), PINNED);
    const failed = randomUUID();
    const refused = await create(failed, [volume(data, '/mnt/data'), volume(absent, '/mnt/required', false, missing)]);
    expect(refused.result).toBe('error');
    expect(runnerDocker(['ps', '-aq', '--filter', `label=sandbox-service.id=${failed}`])).toBe('');
    expect(parseGauge(scrapeRunnerMetrics(), PINNED)).toBe(pinnedBefore);
  });

  test('a sandbox without volumes is unchanged', async () => {
    const id = randomUUID();
    const created = await create(id);
    try {
      expect(created).toEqual({ result: 'ok', applied: { egress: 'public' } });
      expect(runnerDocker(['inspect', '-f', '{{json .Mounts}}', containerName(id)])).toBe('[]');
      expect(runnerDocker(['inspect', '-f', '{{index .Config.Labels "sandbox-service.volumes"}}', containerName(id)])).toBe('');
      expect((await exec(id, 'echo ok')).stdout.trim()).toBe('ok');
    } finally {
      await remove(id);
    }
  });

  test('the read URLs reach no runner log line', () => {
    const log = mustRun('docker', ['logs', runnerContainer]);
    expect(log).toContain('volume cache unpack finished');
    expect(log).not.toContain(token.split('=')[1]);
  });
});

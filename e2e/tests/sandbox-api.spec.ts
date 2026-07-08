import { test, expect } from '@playwright/test';
import './matchers';
import {
  client,
  createSandbox,
  deleteSandbox,
  exec,
  execWithTransientRetry,
  uploadFile,
  downloadFile,
  apiRequest,
} from './helpers';
import { SandboxServiceError } from '@n8n/sandbox-client';
const API_KEY = process.env.SANDBOX_API_KEY || 'test';

test.describe('Auth', () => {
  test('rejects missing API key', async ({ request }) => {
    const resp = await request.post('/sandboxes', {
      headers: { 'Content-Type': 'application/json' },
      data: {},
    });
    expect(resp.status()).toBe(401);
  });

  test('rejects wrong API key', async ({ request }) => {
    const resp = await request.post('/sandboxes', {
      headers: { 'X-Api-Key': 'wrong-key', 'Content-Type': 'application/json' },
      data: {},
    });
    expect(resp.status()).toBe(401);
  });
});

test.describe('Sandbox lifecycle', () => {
  test('create, exec, delete', async ({ request }) => {
    const id = await createSandbox();
    expect(id).toBeTruthy();

    const result = await exec(id, 'echo hello world');
    expect(result.stdout).toBe('hello world\n');
    expect(result).toHaveSucceeded();

    await deleteSandbox(id);

    // GET after delete should be 404
    const resp = await apiRequest(request, 'GET', `/sandboxes/${id}`);
    expect(resp.status).toBe(404);
  });

  test('get non-existent sandbox returns 404', async ({ request }) => {
    const resp = await apiRequest(request, 'GET', '/sandboxes/00000000-0000-0000-0000-000000000000');
    expect(resp.status).toBe(404);
  });

  test('invalid sandbox ID returns 400', async ({ request }) => {
    const resp = await apiRequest(request, 'GET', '/sandboxes/not-a-uuid');
    expect(resp.status).toBe(400);
  });

  test('delete is idempotent', async ({ request }) => {
    const id = await createSandbox();
    const resp1 = await apiRequest(request, 'DELETE', `/sandboxes/${id}`);
    expect(resp1.status).toBe(204);
    const resp2 = await apiRequest(request, 'DELETE', `/sandboxes/${id}`);
    expect(resp2.status).toBe(204);
  });
});

test.describe('Exec', () => {
  let sandboxId: string;

  test.beforeEach(async () => {
    sandboxId = await createSandbox();
    await execWithTransientRetry(sandboxId, 'true', { timeoutMs: 5_000, retryWindowMs: 12_000 });
  });

  test.afterEach(async () => {
    await deleteSandbox(sandboxId);
  });

  test('simple echo', async () => {
    const result = await exec(sandboxId, 'echo hello');
    expect(result.stdout).toBe('hello\n');
    expect(result).toHaveSucceeded();
  });

  test('command with args', async () => {
    const result = await exec(sandboxId, 'ls /tmp');
    expect(result).toHaveSucceeded();
  });

  test('stderr output', async () => {
    const result = await exec(sandboxId, 'echo error >&2');
    expect(result.stderr).toContain('error');
    expect(result).toHaveSucceeded();
  });

  test('non-zero exit code', async () => {
    const result = await execWithTransientRetry(sandboxId, 'exit 42', {
      timeoutMs: 10_000,
      retryWindowMs: 12_000,
    });
    expect(result.exitCode).toBe(42);
    expect(result.success).toBe(false);
  });

  test('killed command process returns structured exec failure', async () => {
    const result = await exec(
      sandboxId,
      `python3 -c "import os, signal; os.kill(os.getpid(), signal.SIGKILL)"`,
    );
    expect(result.success).toBe(false);
    expect(result.exitCode).not.toBe(0);
    expect(result.timedOut).toBe(false);
  });

  test('killing daemon mid-exec returns sandbox exec stream error', async () => {
    let execErr: unknown;
    try {
      // Kill PID1 shortly after exec starts so the stream usually ends mid-flight.
      // Depending on timing/runtime, this can surface as a stream error or a
      // structured failed exec result.
      const result = await exec(sandboxId, 'sh -c "(sleep 0.1; kill -9 1) & sleep 5"');
      expect(result.success).toBe(false);
      expect(result.exitCode).not.toBe(0);
    } catch (err) {
      execErr = err;
    }

    if (execErr) {
      expect(execErr).toBeInstanceOf(Error);
    }
  });

  test('environment variables as map', async () => {
    const result = await exec(sandboxId, 'echo $MY_VAR', {
      env: { MY_VAR: 'sandbox_test' },
    });
    expect(result.stdout).toBe('sandbox_test\n');
  });

  test('environment variables as array returns 400', async ({ request }) => {
    const resp = await apiRequest(request, 'POST', `/sandboxes/${sandboxId}/executions`, {
      data: { command: 'echo $FOO', env: ['FOO=bar'] },
    });
    expect(resp.status).toBe(400);
  });

  test('working directory', async () => {
    const result = await exec(sandboxId, 'pwd', { workdir: '/tmp' });
    expect(result.stdout.trim()).toBe('/tmp');
  });

  test('pipe commands via shell', async () => {
    const result = await execWithTransientRetry(sandboxId, 'echo hello | tr a-z A-Z', {
      timeoutMs: 10_000,
      retryWindowMs: 12_000,
    });
    expect(result.stdout).toBe('HELLO\n');
  });

  test('tilde expands in shell command', async () => {
    const result = await execWithTransientRetry(sandboxId, 'echo ~/', {
      timeoutMs: 10_000,
      retryWindowMs: 12_000,
    });
    expect(result.stdout.trim()).toBe('/home/user/');
    expect(result).toHaveSucceeded();
  });

  test('missing command returns 400', async ({ request }) => {
    const resp = await apiRequest(request, 'POST', `/sandboxes/${sandboxId}/executions`, {
      data: {},
    });
    expect(resp.status).toBe(400);
  });

  test('timeout kills long-running command', async () => {
    const start = Date.now();
    const result = await exec(sandboxId, 'sleep 30', { timeoutMs: 5000 });
    const elapsed = Date.now() - start;

    expect(elapsed).toBeLessThan(10_000);
    expect(result.timedOut).toBe(true);
    expect(result.killed).toBe(true);
  });

  test('executionTimeMs is always present', async () => {
    const result = await exec(sandboxId, 'true');
    expect(result).toHaveProperty('executionTimeMs');
    expect(typeof result.executionTimeMs).toBe('number');
  });

  test('exitCode is present even when 0', async () => {
    const result = await exec(sandboxId, 'true');
    expect(result).toHaveProperty('exitCode');
    expect(result).toHaveSucceeded();
  });

  test('sandbox runs as non-root user', async () => {
    const result = await exec(sandboxId, 'id -u');
    expect(result.stdout.trim()).not.toBe('0');
  });

  test('exec on deleted sandbox returns 404', async ({ request }) => {
    const tempId = await createSandbox();
    await deleteSandbox(tempId);
    const resp = await apiRequest(request, 'POST', `/sandboxes/${tempId}/executions`, {
      data: { command: 'echo hi' },
    });
    expect(resp.status).toBe(404);
  });
});

test.describe('File operations', () => {
  let sandboxId: string;

  test.beforeEach(async () => {
    sandboxId = await createSandbox();
    await execWithTransientRetry(sandboxId, 'true', { timeoutMs: 5_000, retryWindowMs: 12_000 });
  });

  test.afterEach(async () => {
    await deleteSandbox(sandboxId);
  });

  test('upload and download returns raw content (not base64)', async () => {
    const content = 'hello from upload test';
    await uploadFile(sandboxId, 'tmp/test.txt', content);
    const downloaded = await downloadFile(sandboxId, 'tmp/test.txt');
    expect(downloaded).toBe(content);
  });

  test('file append', async ({ request }) => {
    await uploadFile(sandboxId, 'tmp/append.txt', 'first');
    const resp = await request.post(`/sandboxes/${sandboxId}/files?path=${encodeURIComponent('/tmp/append.txt')}`, {
      headers: { 'X-Api-Key': API_KEY, 'Content-Type': 'application/octet-stream' },
      data: '-second',
    });
    expect(resp.status()).toBe(200);
    const downloaded = await downloadFile(sandboxId, 'tmp/append.txt');
    expect(downloaded).toBe('first-second');
  });

  test('overwrite=false returns 409', async ({ request }) => {
    await uploadFile(sandboxId, 'tmp/nowrite.txt', 'original');
    const resp = await request.put(
      `/sandboxes/${sandboxId}/files?path=${encodeURIComponent('/tmp/nowrite.txt')}&overwrite=false`,
      {
        headers: { 'X-Api-Key': API_KEY, 'Content-Type': 'application/octet-stream' },
        data: 'overwrite attempt',
      },
    );
    expect(resp.status()).toBe(409);
  });

  test('stat file', async () => {
    await uploadFile(sandboxId, 'tmp/stat.txt', 'content');
    const stat = await client.stat(sandboxId, '/tmp/stat.txt');
    expect(stat.name).toBe('stat.txt');
    expect(stat.type).toBe('file');
    expect(stat.size).toBe(7);
  });

  test('list files', async () => {
    await uploadFile(sandboxId, 'tmp/list-a.txt', 'a');
    await uploadFile(sandboxId, 'tmp/list-b.py', 'b');
    await uploadFile(sandboxId, 'tmp/list-c.txt', 'c');

    const files = await client.listFiles(sandboxId, { path: '/tmp', extension: '.txt' });
    const names = files.map((f) => f.name);
    expect(names).toContain('list-a.txt');
    expect(names).toContain('list-c.txt');
    expect(names).not.toContain('list-b.py');
  });

  test('recursive list', async () => {
    await client.mkdir(sandboxId, '/tmp/sub', true);
    await uploadFile(sandboxId, 'tmp/top.txt', 'top');
    await uploadFile(sandboxId, 'tmp/sub/nested.txt', 'nested');

    const files = await client.listFiles(sandboxId, { path: '/tmp', recursive: true });
    const names = files.map((f) => f.name);
    expect(names).toContain('sub/nested.txt');
  });

  test('mkdir', async ({ request }) => {
    const resp = await apiRequest(
      request,
      'POST',
      `/sandboxes/${sandboxId}/mkdir?path=${encodeURIComponent('/tmp/newdir')}&recursive=true`,
    );
    expect(resp.status).toBe(201);
  });

  test('copy file', async () => {
    await uploadFile(sandboxId, 'tmp/original.txt', 'copy me');
    await client.copyFile(sandboxId, { src: '/tmp/original.txt', dest: '/tmp/copied.txt' });
    const downloaded = await downloadFile(sandboxId, 'tmp/copied.txt');
    expect(downloaded).toBe('copy me');
  });

  test('move file', async () => {
    await uploadFile(sandboxId, 'tmp/before.txt', 'move me');
    await client.moveFile(sandboxId, { src: '/tmp/before.txt', dest: '/tmp/after.txt' });

    const downloaded = await downloadFile(sandboxId, 'tmp/after.txt');
    expect(downloaded).toBe('move me');

    const error = await client.stat(sandboxId, '/tmp/before.txt').catch((e) => e);
    expect(error).toBeInstanceOf(SandboxServiceError);
    expect((error as SandboxServiceError).status).toBe(404);
  });

  test('delete file', async () => {
    await uploadFile(sandboxId, 'tmp/todelete.txt', 'bye');
    await client.deleteFile(sandboxId, '/tmp/todelete.txt');

    const error = await client.stat(sandboxId, '/tmp/todelete.txt').catch((e) => e);
    expect(error).toBeInstanceOf(SandboxServiceError);
    expect((error as SandboxServiceError).status).toBe(404);
  });

  test('delete with force on non-existent', async ({ request }) => {
    const resp = await apiRequest(
      request,
      'DELETE',
      `/sandboxes/${sandboxId}/files?path=${encodeURIComponent('/tmp/nonexistent')}&force=true`,
    );
    expect(resp.status).toBe(204);
  });

  test('delete non-empty dir without recursive returns 400', async ({ request }) => {
    await client.mkdir(sandboxId, '/tmp/notempty', true);
    await uploadFile(sandboxId, 'tmp/notempty/file.txt', 'content');
    const resp = await apiRequest(
      request,
      'DELETE',
      `/sandboxes/${sandboxId}/files?path=${encodeURIComponent('/tmp/notempty')}`,
    );
    expect(resp.status).toBe(400);
  });

  test('delete non-empty dir with recursive succeeds', async ({ request }) => {
    await client.mkdir(sandboxId, '/tmp/rmdir', true);
    await uploadFile(sandboxId, 'tmp/rmdir/file.txt', 'content');
    const resp = await apiRequest(
      request,
      'DELETE',
      `/sandboxes/${sandboxId}/files?path=${encodeURIComponent('/tmp/rmdir')}&recursive=true`,
    );
    expect(resp.status).toBe(204);
  });

  test('error messages do not leak internal paths', async ({ request }) => {
    const resp = await apiRequest(
      request,
      'GET',
      `/sandboxes/${sandboxId}/files/content?path=${encodeURIComponent('/nonexistent/file.txt')}`,
    );
    expect(resp.status).toBe(404);
    expect(resp.body).not.toContain('/sandbox/');
    expect(resp.body).not.toContain('/var/sandboxes/');
  });

  // --- Upload (PUT /sandboxes/{id}/files?path=...) ---

  test('upload missing path returns 400', async ({ request }) => {
    const resp = await request.put(`/sandboxes/${sandboxId}/files`, {
      headers: { 'X-Api-Key': API_KEY, 'Content-Type': 'application/octet-stream' },
      data: 'content',
    });
    expect(resp.status()).toBe(400);
  });

  test('upload creates intermediate directories', async () => {
    await uploadFile(sandboxId, '/tmp/a/b/c/deep.txt', 'deep content');
    const downloaded = await downloadFile(sandboxId, '/tmp/a/b/c/deep.txt');
    expect(downloaded).toBe('deep content');
  });

  test('upload overwrites existing file by default', async () => {
    await uploadFile(sandboxId, '/tmp/overwrite.txt', 'original');
    await uploadFile(sandboxId, '/tmp/overwrite.txt', 'replaced');
    const downloaded = await downloadFile(sandboxId, '/tmp/overwrite.txt');
    expect(downloaded).toBe('replaced');
  });

  test('upload empty file', async () => {
    await uploadFile(sandboxId, '/tmp/empty.txt', '');
    const downloaded = await downloadFile(sandboxId, '/tmp/empty.txt');
    expect(downloaded).toBe('');
  });

  test('upload binary content', async ({ request }) => {
    const buf = Buffer.from([0x00, 0x01, 0x02, 0xff, 0xfe, 0xfd]);
    await uploadFile(sandboxId, '/tmp/binary.bin', buf);
    const resp = await request.get(
      `/sandboxes/${sandboxId}/files/content?path=${encodeURIComponent('/tmp/binary.bin')}`,
      { headers: { 'X-Api-Key': API_KEY } },
    );
    expect(resp.status()).toBe(200);
    const body = await resp.body();
    expect(Buffer.compare(body, buf)).toBe(0);
  });

  test('upload path with spaces', async () => {
    await uploadFile(sandboxId, '/tmp/my folder/my file.txt', 'spaces work');
    const downloaded = await downloadFile(sandboxId, '/tmp/my folder/my file.txt');
    expect(downloaded).toBe('spaces work');
  });

  test('upload path with special characters', async () => {
    await uploadFile(sandboxId, '/tmp/special (1)/file [2].txt', 'special chars');
    const downloaded = await downloadFile(sandboxId, '/tmp/special (1)/file [2].txt');
    expect(downloaded).toBe('special chars');
  });

  // --- Download (GET /sandboxes/{id}/files/content?path=...) ---

  test('download missing path returns 400', async ({ request }) => {
    const resp = await apiRequest(request, 'GET', `/sandboxes/${sandboxId}/files/content`);
    expect(resp.status).toBe(400);
  });

  test('download non-existent file returns 404', async ({ request }) => {
    const resp = await apiRequest(
      request,
      'GET',
      `/sandboxes/${sandboxId}/files/content?path=${encodeURIComponent('/tmp/nope.txt')}`,
    );
    expect(resp.status).toBe(404);
  });

  // --- Append (POST /sandboxes/{id}/files?path=...) ---

  test('append missing path returns 400', async ({ request }) => {
    const resp = await request.post(`/sandboxes/${sandboxId}/files`, {
      headers: { 'X-Api-Key': API_KEY, 'Content-Type': 'application/octet-stream' },
      data: 'data',
    });
    expect(resp.status()).toBe(400);
  });

  test('append creates file if it does not exist', async ({ request }) => {
    const resp = await request.post(
      `/sandboxes/${sandboxId}/files?path=${encodeURIComponent('/tmp/append-new.txt')}`,
      {
        headers: { 'X-Api-Key': API_KEY, 'Content-Type': 'application/octet-stream' },
        data: 'created by append',
      },
    );
    expect(resp.status()).toBe(200);
    const downloaded = await downloadFile(sandboxId, '/tmp/append-new.txt');
    expect(downloaded).toBe('created by append');
  });

  test('append with spaces in path', async ({ request }) => {
    await uploadFile(sandboxId, '/tmp/append dir/log file.txt', 'line1');
    const resp = await request.post(
      `/sandboxes/${sandboxId}/files?path=${encodeURIComponent('/tmp/append dir/log file.txt')}`,
      {
        headers: { 'X-Api-Key': API_KEY, 'Content-Type': 'application/octet-stream' },
        data: '-line2',
      },
    );
    expect(resp.status()).toBe(200);
    const downloaded = await downloadFile(sandboxId, '/tmp/append dir/log file.txt');
    expect(downloaded).toBe('line1-line2');
  });

  // --- Delete (DELETE /sandboxes/{id}/files?path=...) ---

  test('delete missing path returns 400', async ({ request }) => {
    const resp = await apiRequest(request, 'DELETE', `/sandboxes/${sandboxId}/files`);
    expect(resp.status).toBe(400);
  });

  test('delete non-existent file without force returns error', async ({ request }) => {
    const resp = await apiRequest(
      request,
      'DELETE',
      `/sandboxes/${sandboxId}/files?path=${encodeURIComponent('/tmp/does-not-exist.txt')}`,
    );
    expect(resp.status).toBe(404);
  });

  test('delete file with spaces in path', async ({ request }) => {
    await uploadFile(sandboxId, '/tmp/to delete/file name.txt', 'bye');
    const resp = await apiRequest(
      request,
      'DELETE',
      `/sandboxes/${sandboxId}/files?path=${encodeURIComponent('/tmp/to delete/file name.txt')}`,
    );
    expect(resp.status).toBe(204);

    const statResp = await apiRequest(
      request,
      'GET',
      `/sandboxes/${sandboxId}/stat?path=${encodeURIComponent('/tmp/to delete/file name.txt')}`,
    );
    expect(statResp.status).toBe(404);
  });

  // --- Stat (GET /sandboxes/{id}/stat?path=...) ---

  test('stat missing path returns 400', async ({ request }) => {
    const resp = await apiRequest(request, 'GET', `/sandboxes/${sandboxId}/stat`);
    expect(resp.status).toBe(400);
  });

  test('stat non-existent file returns 404', async ({ request }) => {
    const resp = await apiRequest(
      request,
      'GET',
      `/sandboxes/${sandboxId}/stat?path=${encodeURIComponent('/tmp/nope.txt')}`,
    );
    expect(resp.status).toBe(404);
  });

  test('stat directory', async () => {
    await client.mkdir(sandboxId, '/tmp/statdir', true);
    const stat = await client.stat(sandboxId, '/tmp/statdir');
    expect(stat.type).toBe('directory');
  });

  test('stat file with spaces in path', async () => {
    await uploadFile(sandboxId, '/tmp/stat dir/stat file.txt', 'hello');
    const stat = await client.stat(sandboxId, '/tmp/stat dir/stat file.txt');
    expect(stat.name).toBe('stat file.txt');
    expect(stat.type).toBe('file');
    expect(stat.size).toBe(5);
  });

  // --- List (GET /sandboxes/{id}/files?path=...) ---

  test('list defaults to root when path omitted', async () => {
    const files = await client.listFiles(sandboxId);
    expect(Array.isArray(files)).toBe(true);
  });

  test('list non-existent directory returns 404', async ({ request }) => {
    const resp = await apiRequest(
      request,
      'GET',
      `/sandboxes/${sandboxId}/files?path=${encodeURIComponent('/tmp/no-such-dir')}`,
    );
    expect(resp.status).toBe(404);
  });

  test('list directory with spaces in path', async () => {
    await uploadFile(sandboxId, '/tmp/list dir/a.txt', 'a');
    await uploadFile(sandboxId, '/tmp/list dir/b.txt', 'b');
    const files = await client.listFiles(sandboxId, { path: '/tmp/list dir' });
    const names = files.map((f) => f.name);
    expect(names).toContain('a.txt');
    expect(names).toContain('b.txt');
  });

  // --- Mkdir (POST /sandboxes/{id}/mkdir?path=...) ---

  test('mkdir missing path returns 400', async ({ request }) => {
    const resp = await apiRequest(request, 'POST', `/sandboxes/${sandboxId}/mkdir`);
    expect(resp.status).toBe(400);
  });

  test('mkdir without recursive fails when parent missing', async ({ request }) => {
    const resp = await apiRequest(
      request,
      'POST',
      `/sandboxes/${sandboxId}/mkdir?path=${encodeURIComponent('/tmp/no-parent/child')}`,
    );
    expect(resp.status).not.toBe(201);
  });

  test('mkdir with spaces in path', async () => {
    await client.mkdir(sandboxId, '/tmp/my new dir/sub dir', true);
    const stat = await client.stat(sandboxId, '/tmp/my new dir/sub dir');
    expect(stat.type).toBe('directory');
  });

  // --- Copy (POST /sandboxes/{id}/files/copy) ---

  test('copy missing src/dest returns 400', async ({ request }) => {
    const resp = await apiRequest(request, 'POST', `/sandboxes/${sandboxId}/files/copy`, {
      data: { src: '/tmp/foo.txt' },
    });
    expect(resp.status).toBe(400);
  });

  test('copy non-existent source returns 404', async ({ request }) => {
    const resp = await apiRequest(request, 'POST', `/sandboxes/${sandboxId}/files/copy`, {
      data: { src: '/tmp/nope.txt', dest: '/tmp/dest.txt' },
    });
    expect(resp.status).toBe(404);
  });

  test('copy overwrite=false on existing dest returns 409', async ({ request }) => {
    await uploadFile(sandboxId, '/tmp/copy-src.txt', 'src');
    await uploadFile(sandboxId, '/tmp/copy-dest.txt', 'dest');
    const resp = await apiRequest(request, 'POST', `/sandboxes/${sandboxId}/files/copy`, {
      data: { src: '/tmp/copy-src.txt', dest: '/tmp/copy-dest.txt', overwrite: false },
    });
    expect(resp.status).toBe(409);
  });

  test('copy directory with recursive', async () => {
    await uploadFile(sandboxId, '/tmp/cpdir/a.txt', 'a');
    await uploadFile(sandboxId, '/tmp/cpdir/b.txt', 'b');
    await client.copyFile(sandboxId, { src: '/tmp/cpdir', dest: '/tmp/cpdir-copy', recursive: true });
    const a = await downloadFile(sandboxId, '/tmp/cpdir-copy/a.txt');
    expect(a).toBe('a');
    const b = await downloadFile(sandboxId, '/tmp/cpdir-copy/b.txt');
    expect(b).toBe('b');
  });

  test('copy directory without recursive returns 400', async ({ request }) => {
    await uploadFile(sandboxId, '/tmp/cpdir2/a.txt', 'a');
    const resp = await apiRequest(request, 'POST', `/sandboxes/${sandboxId}/files/copy`, {
      data: { src: '/tmp/cpdir2', dest: '/tmp/cpdir2-copy' },
    });
    expect(resp.status).toBe(400);
  });

  test('copy file with spaces in path', async () => {
    await uploadFile(sandboxId, '/tmp/copy src/my file.txt', 'spaced');
    await client.copyFile(sandboxId, { src: '/tmp/copy src/my file.txt', dest: '/tmp/copy dest/my file.txt' });
    const downloaded = await downloadFile(sandboxId, '/tmp/copy dest/my file.txt');
    expect(downloaded).toBe('spaced');
  });

  // --- Move (POST /sandboxes/{id}/files/move) ---

  test('move missing src/dest returns 400', async ({ request }) => {
    const resp = await apiRequest(request, 'POST', `/sandboxes/${sandboxId}/files/move`, {
      data: { src: '/tmp/foo.txt' },
    });
    expect(resp.status).toBe(400);
  });

  test('move non-existent source returns 404', async ({ request }) => {
    const resp = await apiRequest(request, 'POST', `/sandboxes/${sandboxId}/files/move`, {
      data: { src: '/tmp/nope.txt', dest: '/tmp/dest.txt' },
    });
    expect(resp.status).toBe(404);
  });

  test('move overwrite=false on existing dest returns 409', async ({ request }) => {
    await uploadFile(sandboxId, '/tmp/mv-src.txt', 'src');
    await uploadFile(sandboxId, '/tmp/mv-dest.txt', 'dest');
    const resp = await apiRequest(request, 'POST', `/sandboxes/${sandboxId}/files/move`, {
      data: { src: '/tmp/mv-src.txt', dest: '/tmp/mv-dest.txt', overwrite: false },
    });
    expect(resp.status).toBe(409);
  });

  test('move file with spaces in path', async () => {
    await uploadFile(sandboxId, '/tmp/move src/old name.txt', 'moving');
    await client.moveFile(sandboxId, { src: '/tmp/move src/old name.txt', dest: '/tmp/move dest/new name.txt' });
    const downloaded = await downloadFile(sandboxId, '/tmp/move dest/new name.txt');
    expect(downloaded).toBe('moving');

    const error = await client.stat(sandboxId, '/tmp/move src/old name.txt').catch((e) => e);
    expect(error).toBeInstanceOf(SandboxServiceError);
    expect((error as SandboxServiceError).status).toBe(404);
  });

  // --- File visibility between API and exec ---

  test('file uploaded via API is visible to exec', async () => {
    await uploadFile(sandboxId, '/tmp/exec-visible.txt', 'from api');
    const result = await execWithTransientRetry(sandboxId, 'cat /tmp/exec-visible.txt', {
      timeoutMs: 10_000,
      retryWindowMs: 12_000,
    });
    expect(result.stdout).toBe('from api\n');
    expect(result).toHaveSucceeded();
  });

  test('file created by exec is downloadable via API', async () => {
    await execWithTransientRetry(sandboxId, 'echo -n "from exec" > /tmp/api-visible.txt', {
      timeoutMs: 10_000,
      retryWindowMs: 12_000,
    });
    const downloaded = await downloadFile(sandboxId, '/tmp/api-visible.txt');
    expect(downloaded).toBe('from exec');
  });

  test('exec can read file with spaces in path', async () => {
    await uploadFile(sandboxId, '/tmp/space dir/space file.txt', 'spaced content');
    const downloaded = await downloadFile(sandboxId, '/tmp/space dir/space file.txt');
    expect(downloaded).toBe('spaced content');
    const result = await execWithTransientRetry(sandboxId, "cat '/tmp/space dir/space file.txt'", {
      timeoutMs: 10_000,
      retryWindowMs: 12_000,
    });
    expect(result.stdout).toBe('spaced content\n');
    expect(result).toHaveSucceeded();
  });
});

test.describe('Sandbox isolation', () => {
  test('files are isolated between sandboxes', async ({ request }) => {
    const id1 = await createSandbox();
    const id2 = await createSandbox();

    try {
      await uploadFile(id1, 'tmp/secret.txt', 'sandbox1 secret');

      const resp = await apiRequest(request, 'GET', `/sandboxes/${id2}/files/content?path=${encodeURIComponent('/tmp/secret.txt')}`);
      expect(resp.status).toBe(404);
    } finally {
      await deleteSandbox(id1);
      await deleteSandbox(id2);
    }
  });

  test('concurrent writes to the same path do not leak across sandboxes', async () => {
    const id1 = await createSandbox();
    const id2 = await createSandbox();
    const sharedPath = '/tmp/shared-isolation.txt';

    try {
      await Promise.all([
        uploadFile(id1, 'tmp/shared-isolation.txt', 'sandbox-one'),
        uploadFile(id2, 'tmp/shared-isolation.txt', 'sandbox-two'),
      ]);

      expect(await downloadFile(id1, sharedPath)).toBe('sandbox-one');
      expect(await downloadFile(id2, sharedPath)).toBe('sandbox-two');
    } finally {
      await deleteSandbox(id1);
      await deleteSandbox(id2);
    }
  });
});

test.describe('Deleted Sandbox 404 Tests', () => {
  test('file operations on deleted sandbox return 404', async ({ request }) => {
    const tempId = await createSandbox();
    await deleteSandbox(tempId);

    const endpoints = [
      { method: 'GET', path: `/sandboxes/${tempId}/files` },
      { method: 'GET', path: `/sandboxes/${tempId}/files/content?path=/tmp/test.txt` },
      { method: 'PUT', path: `/sandboxes/${tempId}/files`, data: { path: '/tmp/test.txt', content: 'test' } },
      { method: 'POST', path: `/sandboxes/${tempId}/files`, data: { path: '/tmp/test.txt', content: 'test' } },
      { method: 'DELETE', path: `/sandboxes/${tempId}/files?path=/tmp/test.txt` },
      { method: 'POST', path: `/sandboxes/${tempId}/files/copy`, data: { src: '/tmp/src.txt', dest: '/tmp/dest.txt' } },
      { method: 'POST', path: `/sandboxes/${tempId}/files/move`, data: { src: '/tmp/src.txt', dest: '/tmp/dest.txt' } },
      { method: 'POST', path: `/sandboxes/${tempId}/mkdir?path=/tmp/newdir` },
      { method: 'GET', path: `/sandboxes/${tempId}/stat?path=/tmp/test.txt` },
    ];

    for (const endpoint of endpoints) {
      const resp = await apiRequest(request, endpoint.method as any, endpoint.path, endpoint.data ? { data: endpoint.data } : {});
      expect(resp.status, `${endpoint.method} ${endpoint.path} should return 404`).toBe(404);
    }
  });
});

test.describe('Healthz', () => {
  test('returns ok without auth', async ({ request }) => {
    const resp = await request.get('/healthz');
    expect(resp.status()).toBe(200);
    const body = await resp.json();
    expect(body.status).toBe('ok');
  });
});

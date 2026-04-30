import { test, expect } from '@playwright/test';
import {
  createSandbox,
  deleteSandbox,
  exec,
  uploadFile,
  downloadFile,
  apiRequest,
} from './helpers';

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
    const id = await createSandbox(request);
    expect(id).toBeTruthy();

    const result = await exec(request, id, 'echo hello world');
    expect(result.stdout).toBe('hello world\n');
    expect(result.exit?.success).toBe(true);
    expect(result.exit?.exit_code).toBe(0);

    await deleteSandbox(request, id);

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
    const id = await createSandbox(request);
    const resp1 = await apiRequest(request, 'DELETE', `/sandboxes/${id}`);
    expect(resp1.status).toBe(204);
    const resp2 = await apiRequest(request, 'DELETE', `/sandboxes/${id}`);
    expect(resp2.status).toBe(204);
  });
});

test.describe('Exec', () => {
  let sandboxId: string;

  test.beforeEach(async ({ request }) => {
    sandboxId = await createSandbox(request);
  });

  test.afterEach(async ({ request }) => {
    await deleteSandbox(request, sandboxId);
  });

  test('simple echo', async ({ request }) => {
    const result = await exec(request, sandboxId, 'echo hello');
    expect(result.stdout).toBe('hello\n');
    expect(result.exit?.success).toBe(true);
  });

  test('command with args', async ({ request }) => {
    const result = await exec(request, sandboxId, 'ls /tmp');
    expect(result.exit?.success).toBe(true);
  });

  test('stderr output', async ({ request }) => {
    const result = await exec(request, sandboxId, 'echo error >&2');
    expect(result.stderr).toContain('error');
    expect(result.exit?.success).toBe(true);
  });

  test('non-zero exit code', async ({ request }) => {
    const result = await exec(request, sandboxId, 'exit 42');
    expect(result.exit?.exit_code).toBe(42);
    expect(result.exit?.success).toBe(false);
  });

  test('environment variables as map', async ({ request }) => {
    const result = await exec(request, sandboxId, 'echo $MY_VAR', {
      env: { MY_VAR: 'sandbox_test' },
    });
    expect(result.stdout).toBe('sandbox_test\n');
  });

  test('environment variables as array returns 400', async ({ request }) => {
    const resp = await apiRequest(request, 'POST', `/sandboxes/${sandboxId}/exec`, {
      data: { command: 'echo $FOO', env: ['FOO=bar'] },
    });
    expect(resp.status).toBe(400);
  });

  test('working directory', async ({ request }) => {
    const result = await exec(request, sandboxId, 'pwd', { workdir: '/tmp' });
    expect(result.stdout.trim()).toBe('/tmp');
  });

  test('pipe commands via shell', async ({ request }) => {
    const result = await exec(request, sandboxId, 'echo hello | tr a-z A-Z');
    expect(result.stdout).toBe('HELLO\n');
  });

  test('tilde expands in shell command', async ({ request }) => {
    const result = await exec(request, sandboxId, 'echo ~/');
    expect(result.stdout.trim()).toBe('/home/user/');
    expect(result.exit?.success).toBe(true);
  });

  test('missing command returns 400', async ({ request }) => {
    const resp = await apiRequest(request, 'POST', `/sandboxes/${sandboxId}/exec`, {
      data: {},
    });
    expect(resp.status).toBe(400);
  });

  // Fix 1: Timeout enforcement
  test('timeout kills long-running command', async ({ request }) => {
    const start = Date.now();
    const result = await exec(request, sandboxId, 'sleep 30', { timeout_ms: 5000 });
    const elapsed = Date.now() - start;

    expect(elapsed).toBeLessThan(10_000);
    expect(result.exit?.timed_out).toBe(true);
    expect(result.exit?.killed).toBe(true);
  });

  // Fix 6: execution_time_ms always present
  test('execution_time_ms is always present in exit event', async ({ request }) => {
    const result = await exec(request, sandboxId, 'true');
    expect(result.exit).toBeTruthy();
    expect(result.exit).toHaveProperty('execution_time_ms');
    expect(typeof result.exit!.execution_time_ms).toBe('number');
  });

  test('exit_code is present even when 0', async ({ request }) => {
    const result = await exec(request, sandboxId, 'true');
    expect(result.exit).toHaveProperty('exit_code');
    expect(result.exit!.exit_code).toBe(0);
  });

  // Fix 7: Non-root user
  test('sandbox runs as non-root user', async ({ request }) => {
    const result = await exec(request, sandboxId, 'id -u');
    expect(result.stdout.trim()).not.toBe('0');
  });

  // Fix 3: 404 on exec in deleted sandbox
  test('exec on deleted sandbox returns 404', async ({ request }) => {
    const tempId = await createSandbox(request);
    await deleteSandbox(request, tempId);
    const resp = await apiRequest(request, 'POST', `/sandboxes/${tempId}/exec`, {
      data: { command: 'echo hi' },
    });
    expect(resp.status).toBe(404);
  });
});

test.describe('File operations', () => {
  let sandboxId: string;

  test.beforeEach(async ({ request }) => {
    sandboxId = await createSandbox(request);
  });

  test.afterEach(async ({ request }) => {
    await deleteSandbox(request, sandboxId);
  });

  // Fix 2: File download returns raw bytes
  test('upload and download returns raw content (not base64)', async ({ request }) => {
    const content = 'hello from upload test';
    await uploadFile(request, sandboxId, 'tmp/test.txt', content);
    const downloaded = await downloadFile(request, sandboxId, 'tmp/test.txt');
    expect(downloaded).toBe(content);
  });

  test('file append', async ({ request }) => {
    await uploadFile(request, sandboxId, 'tmp/append.txt', 'first');
    // Append
    const resp = await request.post(`/sandboxes/${sandboxId}/files?path=${encodeURIComponent('/tmp/append.txt')}`, {
      headers: { 'X-Api-Key': API_KEY, 'Content-Type': 'application/octet-stream' },
      data: '-second',
    });
    expect(resp.status()).toBe(200);
    const downloaded = await downloadFile(request, sandboxId, 'tmp/append.txt');
    expect(downloaded).toBe('first-second');
  });

  test('overwrite=false returns 409', async ({ request }) => {
    await uploadFile(request, sandboxId, 'tmp/nowrite.txt', 'original');
    const resp = await request.put(
      `/sandboxes/${sandboxId}/files?path=${encodeURIComponent('/tmp/nowrite.txt')}&overwrite=false`,
      {
        headers: { 'X-Api-Key': API_KEY, 'Content-Type': 'application/octet-stream' },
        data: 'overwrite attempt',
      },
    );
    expect(resp.status()).toBe(409);
  });

  test('stat file', async ({ request }) => {
    await uploadFile(request, sandboxId, 'tmp/stat.txt', 'content');
    const resp = await apiRequest(request, 'GET', `/sandboxes/${sandboxId}/stat?path=${encodeURIComponent('/tmp/stat.txt')}`);
    expect(resp.status).toBe(200);
    const stat = (await resp.json()) as Record<string, unknown>;
    expect(stat.name).toBe('stat.txt');
    expect(stat.type).toBe('file');
    expect(stat.size).toBe(7);
  });

  test('list files', async ({ request }) => {
    await uploadFile(request, sandboxId, 'tmp/list-a.txt', 'a');
    await uploadFile(request, sandboxId, 'tmp/list-b.py', 'b');
    await uploadFile(request, sandboxId, 'tmp/list-c.txt', 'c');

    const resp = await apiRequest(
      request,
      'GET',
      `/sandboxes/${sandboxId}/files?path=/tmp&extension=.txt`,
    );
    expect(resp.status).toBe(200);
    const files = (await resp.json()) as Array<{ name: string }>;
    const names = files.map((f) => f.name);
    expect(names).toContain('list-a.txt');
    expect(names).toContain('list-c.txt');
    expect(names).not.toContain('list-b.py');
  });

  test('recursive list', async ({ request }) => {
    await apiRequest(request, 'POST', `/sandboxes/${sandboxId}/mkdir?path=${encodeURIComponent('/tmp/sub')}&recursive=true`);
    await uploadFile(request, sandboxId, 'tmp/top.txt', 'top');
    await uploadFile(request, sandboxId, 'tmp/sub/nested.txt', 'nested');

    const resp = await apiRequest(
      request,
      'GET',
      `/sandboxes/${sandboxId}/files?path=/tmp&recursive=true`,
    );
    expect(resp.status).toBe(200);
    const files = (await resp.json()) as Array<{ name: string }>;
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

  test('copy file', async ({ request }) => {
    await uploadFile(request, sandboxId, 'tmp/original.txt', 'copy me');
    const resp = await apiRequest(request, 'POST', `/sandboxes/${sandboxId}/files/copy`, {
      data: { src: '/tmp/original.txt', dest: '/tmp/copied.txt' },
    });
    expect(resp.status).toBe(200);
    const downloaded = await downloadFile(request, sandboxId, 'tmp/copied.txt');
    expect(downloaded).toBe('copy me');
  });

  test('move file', async ({ request }) => {
    await uploadFile(request, sandboxId, 'tmp/before.txt', 'move me');
    const resp = await apiRequest(request, 'POST', `/sandboxes/${sandboxId}/files/move`, {
      data: { src: '/tmp/before.txt', dest: '/tmp/after.txt' },
    });
    expect(resp.status).toBe(200);

    const downloaded = await downloadFile(request, sandboxId, 'tmp/after.txt');
    expect(downloaded).toBe('move me');

    // Old path should 404
    const oldResp = await apiRequest(
      request,
      'GET',
      `/sandboxes/${sandboxId}/stat?path=${encodeURIComponent('/tmp/before.txt')}`,
    );
    expect(oldResp.status).toBe(404);
  });

  test('delete file', async ({ request }) => {
    await uploadFile(request, sandboxId, 'tmp/todelete.txt', 'bye');
    const resp = await apiRequest(
      request,
      'DELETE',
      `/sandboxes/${sandboxId}/files?path=${encodeURIComponent('/tmp/todelete.txt')}`,
    );
    expect(resp.status).toBe(204);
  });

  test('delete with force on non-existent', async ({ request }) => {
    const resp = await apiRequest(
      request,
      'DELETE',
      `/sandboxes/${sandboxId}/files?path=${encodeURIComponent('/tmp/nonexistent')}&force=true`,
    );
    expect(resp.status).toBe(204);
  });

  // Fix 4: Non-recursive dir delete returns 400
  test('delete non-empty dir without recursive returns 400', async ({ request }) => {
    await apiRequest(request, 'POST', `/sandboxes/${sandboxId}/mkdir?path=${encodeURIComponent('/tmp/notempty')}&recursive=true`);
    await uploadFile(request, sandboxId, 'tmp/notempty/file.txt', 'content');
    const resp = await apiRequest(
      request,
      'DELETE',
      `/sandboxes/${sandboxId}/files?path=${encodeURIComponent('/tmp/notempty')}`,
    );
    expect(resp.status).toBe(400);
  });

  test('delete non-empty dir with recursive succeeds', async ({ request }) => {
    await apiRequest(request, 'POST', `/sandboxes/${sandboxId}/mkdir?path=${encodeURIComponent('/tmp/rmdir')}&recursive=true`);
    await uploadFile(request, sandboxId, 'tmp/rmdir/file.txt', 'content');
    const resp = await apiRequest(
      request,
      'DELETE',
      `/sandboxes/${sandboxId}/files?path=${encodeURIComponent('/tmp/rmdir')}&recursive=true`,
    );
    expect(resp.status).toBe(204);
  });

  // Fix 5: No internal paths in error messages
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

  test('upload creates intermediate directories', async ({ request }) => {
    await uploadFile(request, sandboxId, '/tmp/a/b/c/deep.txt', 'deep content');
    const downloaded = await downloadFile(request, sandboxId, '/tmp/a/b/c/deep.txt');
    expect(downloaded).toBe('deep content');
  });

  test('upload overwrites existing file by default', async ({ request }) => {
    await uploadFile(request, sandboxId, '/tmp/overwrite.txt', 'original');
    await uploadFile(request, sandboxId, '/tmp/overwrite.txt', 'replaced');
    const downloaded = await downloadFile(request, sandboxId, '/tmp/overwrite.txt');
    expect(downloaded).toBe('replaced');
  });

  test('upload empty file', async ({ request }) => {
    await uploadFile(request, sandboxId, '/tmp/empty.txt', '');
    const downloaded = await downloadFile(request, sandboxId, '/tmp/empty.txt');
    expect(downloaded).toBe('');
  });

  test('upload binary content', async ({ request }) => {
    const buf = Buffer.from([0x00, 0x01, 0x02, 0xff, 0xfe, 0xfd]);
    await uploadFile(request, sandboxId, '/tmp/binary.bin', buf);
    const resp = await request.get(
      `/sandboxes/${sandboxId}/files/content?path=${encodeURIComponent('/tmp/binary.bin')}`,
      { headers: { 'X-Api-Key': API_KEY } },
    );
    expect(resp.status()).toBe(200);
    const body = await resp.body();
    expect(Buffer.compare(body, buf)).toBe(0);
  });

  test('upload path with spaces', async ({ request }) => {
    await uploadFile(request, sandboxId, '/tmp/my folder/my file.txt', 'spaces work');
    const downloaded = await downloadFile(request, sandboxId, '/tmp/my folder/my file.txt');
    expect(downloaded).toBe('spaces work');
  });

  test('upload path with special characters', async ({ request }) => {
    await uploadFile(request, sandboxId, '/tmp/special (1)/file [2].txt', 'special chars');
    const downloaded = await downloadFile(request, sandboxId, '/tmp/special (1)/file [2].txt');
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
    const downloaded = await downloadFile(request, sandboxId, '/tmp/append-new.txt');
    expect(downloaded).toBe('created by append');
  });

  test('append with spaces in path', async ({ request }) => {
    await uploadFile(request, sandboxId, '/tmp/append dir/log file.txt', 'line1');
    const resp = await request.post(
      `/sandboxes/${sandboxId}/files?path=${encodeURIComponent('/tmp/append dir/log file.txt')}`,
      {
        headers: { 'X-Api-Key': API_KEY, 'Content-Type': 'application/octet-stream' },
        data: '-line2',
      },
    );
    expect(resp.status()).toBe(200);
    const downloaded = await downloadFile(request, sandboxId, '/tmp/append dir/log file.txt');
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
    await uploadFile(request, sandboxId, '/tmp/to delete/file name.txt', 'bye');
    const resp = await apiRequest(
      request,
      'DELETE',
      `/sandboxes/${sandboxId}/files?path=${encodeURIComponent('/tmp/to delete/file name.txt')}`,
    );
    expect(resp.status).toBe(204);

    // Verify it's gone
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

  test('stat directory', async ({ request }) => {
    await apiRequest(
      request,
      'POST',
      `/sandboxes/${sandboxId}/mkdir?path=${encodeURIComponent('/tmp/statdir')}&recursive=true`,
    );
    const resp = await apiRequest(
      request,
      'GET',
      `/sandboxes/${sandboxId}/stat?path=${encodeURIComponent('/tmp/statdir')}`,
    );
    expect(resp.status).toBe(200);
    const stat = (await resp.json()) as Record<string, unknown>;
    expect(stat.type).toBe('directory');
  });

  test('stat file with spaces in path', async ({ request }) => {
    await uploadFile(request, sandboxId, '/tmp/stat dir/stat file.txt', 'hello');
    const resp = await apiRequest(
      request,
      'GET',
      `/sandboxes/${sandboxId}/stat?path=${encodeURIComponent('/tmp/stat dir/stat file.txt')}`,
    );
    expect(resp.status).toBe(200);
    const stat = (await resp.json()) as Record<string, unknown>;
    expect(stat.name).toBe('stat file.txt');
    expect(stat.type).toBe('file');
    expect(stat.size).toBe(5);
  });

  // --- List (GET /sandboxes/{id}/files?path=...) ---

  test('list defaults to root when path omitted', async ({ request }) => {
    const resp = await apiRequest(request, 'GET', `/sandboxes/${sandboxId}/files`);
    expect(resp.status).toBe(200);
    const files = (await resp.json()) as Array<{ name: string }>;
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

  test('list directory with spaces in path', async ({ request }) => {
    await uploadFile(request, sandboxId, '/tmp/list dir/a.txt', 'a');
    await uploadFile(request, sandboxId, '/tmp/list dir/b.txt', 'b');
    const resp = await apiRequest(
      request,
      'GET',
      `/sandboxes/${sandboxId}/files?path=${encodeURIComponent('/tmp/list dir')}`,
    );
    expect(resp.status).toBe(200);
    const files = (await resp.json()) as Array<{ name: string }>;
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

  test('mkdir with spaces in path', async ({ request }) => {
    const resp = await apiRequest(
      request,
      'POST',
      `/sandboxes/${sandboxId}/mkdir?path=${encodeURIComponent('/tmp/my new dir/sub dir')}&recursive=true`,
    );
    expect(resp.status).toBe(201);

    // Verify via stat
    const statResp = await apiRequest(
      request,
      'GET',
      `/sandboxes/${sandboxId}/stat?path=${encodeURIComponent('/tmp/my new dir/sub dir')}`,
    );
    expect(statResp.status).toBe(200);
    const stat = (await statResp.json()) as Record<string, unknown>;
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
    await uploadFile(request, sandboxId, '/tmp/copy-src.txt', 'src');
    await uploadFile(request, sandboxId, '/tmp/copy-dest.txt', 'dest');
    const resp = await apiRequest(request, 'POST', `/sandboxes/${sandboxId}/files/copy`, {
      data: { src: '/tmp/copy-src.txt', dest: '/tmp/copy-dest.txt', overwrite: false },
    });
    expect(resp.status).toBe(409);
  });

  test('copy directory with recursive', async ({ request }) => {
    await uploadFile(request, sandboxId, '/tmp/cpdir/a.txt', 'a');
    await uploadFile(request, sandboxId, '/tmp/cpdir/b.txt', 'b');
    const resp = await apiRequest(request, 'POST', `/sandboxes/${sandboxId}/files/copy`, {
      data: { src: '/tmp/cpdir', dest: '/tmp/cpdir-copy', recursive: true },
    });
    expect(resp.status).toBe(200);
    const a = await downloadFile(request, sandboxId, '/tmp/cpdir-copy/a.txt');
    expect(a).toBe('a');
    const b = await downloadFile(request, sandboxId, '/tmp/cpdir-copy/b.txt');
    expect(b).toBe('b');
  });

  test('copy directory without recursive returns 400', async ({ request }) => {
    await uploadFile(request, sandboxId, '/tmp/cpdir2/a.txt', 'a');
    const resp = await apiRequest(request, 'POST', `/sandboxes/${sandboxId}/files/copy`, {
      data: { src: '/tmp/cpdir2', dest: '/tmp/cpdir2-copy' },
    });
    expect(resp.status).toBe(400);
  });

  test('copy file with spaces in path', async ({ request }) => {
    await uploadFile(request, sandboxId, '/tmp/copy src/my file.txt', 'spaced');
    const resp = await apiRequest(request, 'POST', `/sandboxes/${sandboxId}/files/copy`, {
      data: { src: '/tmp/copy src/my file.txt', dest: '/tmp/copy dest/my file.txt' },
    });
    expect(resp.status).toBe(200);
    const downloaded = await downloadFile(request, sandboxId, '/tmp/copy dest/my file.txt');
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
    await uploadFile(request, sandboxId, '/tmp/mv-src.txt', 'src');
    await uploadFile(request, sandboxId, '/tmp/mv-dest.txt', 'dest');
    const resp = await apiRequest(request, 'POST', `/sandboxes/${sandboxId}/files/move`, {
      data: { src: '/tmp/mv-src.txt', dest: '/tmp/mv-dest.txt', overwrite: false },
    });
    expect(resp.status).toBe(409);
  });

  test('move file with spaces in path', async ({ request }) => {
    await uploadFile(request, sandboxId, '/tmp/move src/old name.txt', 'moving');
    const resp = await apiRequest(request, 'POST', `/sandboxes/${sandboxId}/files/move`, {
      data: { src: '/tmp/move src/old name.txt', dest: '/tmp/move dest/new name.txt' },
    });
    expect(resp.status).toBe(200);
    const downloaded = await downloadFile(request, sandboxId, '/tmp/move dest/new name.txt');
    expect(downloaded).toBe('moving');

    // Old path should be gone
    const statResp = await apiRequest(
      request,
      'GET',
      `/sandboxes/${sandboxId}/stat?path=${encodeURIComponent('/tmp/move src/old name.txt')}`,
    );
    expect(statResp.status).toBe(404);
  });

  // --- File visibility between API and exec ---

  test('file uploaded via API is visible to exec', async ({ request }) => {
    await uploadFile(request, sandboxId, '/tmp/exec-visible.txt', 'from api');
    const result = await exec(request, sandboxId, 'cat /tmp/exec-visible.txt');
    expect(result.stdout).toBe('from api\n');
    expect(result.exit?.success).toBe(true);
  });

  test('file created by exec is downloadable via API', async ({ request }) => {
    await exec(request, sandboxId, 'echo -n "from exec" > /tmp/api-visible.txt');
    const downloaded = await downloadFile(request, sandboxId, '/tmp/api-visible.txt');
    expect(downloaded).toBe('from exec');
  });

  test('exec can read file with spaces in path', async ({ request }) => {
    await uploadFile(request, sandboxId, '/tmp/space dir/space file.txt', 'spaced content');
    // Verify file is readable via the file API first.
    const downloaded = await downloadFile(request, sandboxId, '/tmp/space dir/space file.txt');
    expect(downloaded).toBe('spaced content');
    // Now verify exec can read it too.
    const result = await exec(request, sandboxId, "cat '/tmp/space dir/space file.txt'");
    expect(result.stdout).toBe('spaced content\n');
    expect(result.exit?.success).toBe(true);
  });
});

test.describe('Sandbox isolation', () => {
  test('files are isolated between sandboxes', async ({ request }) => {
    const id1 = await createSandbox(request);
    const id2 = await createSandbox(request);

    try {
      await uploadFile(request, id1, 'tmp/secret.txt', 'sandbox1 secret');

      // Should not be visible in sandbox2
      const resp = await apiRequest(request, 'GET', `/sandboxes/${id2}/files/content?path=${encodeURIComponent('/tmp/secret.txt')}`);
      expect(resp.status).toBe(404);
    } finally {
      await deleteSandbox(request, id1);
      await deleteSandbox(request, id2);
    }
  });
});

test.describe('Deleted Sandbox 404 Tests', () => {
  test('file operations on deleted sandbox return 404', async ({ request }) => {
    const tempId = await createSandbox(request);
    await deleteSandbox(request, tempId);

    // Test all file endpoints return 404 for deleted sandbox
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

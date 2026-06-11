import { test, expect } from '@playwright/test';
import './matchers';
import {
  createSandbox,
  deleteSandbox,
  exec,
  execWithTransientRetry,
  uploadFile,
  downloadFile,
  apiRequest,
} from './helpers';
import { BOTH_RUNNERS } from './tags';

/**
 * E2E tests verifying that the file API and exec API operate on the same
 * filesystem — files created through one are visible through the other.
 */
test.describe.configure({ mode: 'serial' });

test.describe('File API and Exec API path consistency', BOTH_RUNNERS, () => {
  let sandboxId: string;

  test.beforeAll(async () => {
    sandboxId = await createSandbox();
    await execWithTransientRetry(sandboxId, 'true', { timeoutMs: 5_000, retryWindowMs: 12_000 });
  });

  test.afterAll(async () => {
    await deleteSandbox(sandboxId);
  });

  test('file written via file API is readable via exec', async () => {
    const path = '/tmp/test-file-api-to-exec.txt';
    const content = 'hello from file API';

    await uploadFile(sandboxId, path, content);

    const result = await exec(sandboxId, `cat ${path}`);
    expect(result).toHaveSucceeded();
    expect(result.stdout.trim()).toBe(content);
  });

  test('file written via exec is readable via file API', async () => {
    const path = '/tmp/test-exec-to-file-api.txt';
    const content = 'hello from exec';

    const writeResult = await exec(sandboxId, `echo -n '${content}' > ${path}`);
    expect(writeResult).toHaveSucceeded();

    const downloaded = await downloadFile(sandboxId, path);
    expect(downloaded).toBe(content);
  });

  test('directory created via exec is listable via file API', async () => {
    const dir = '/tmp/exec-created-dir';

    const mkdirResult = await exec(sandboxId, `mkdir -p ${dir}/sub && touch ${dir}/sub/a.txt ${dir}/sub/b.txt`);
    expect(mkdirResult).toHaveSucceeded();

    // Download one of the files via file API to confirm visibility
    const content = await downloadFile(sandboxId, `${dir}/sub/a.txt`);
    expect(content).toBeDefined();
  });

  test('file written via file API in nested dir is executable via exec', async () => {
    const dir = '/home/user/scripts';
    const scriptPath = `${dir}/greet.sh`;
    const scriptContent = '#!/bin/sh\necho "hello $1"';

    await uploadFile(sandboxId, scriptPath, scriptContent);

    await exec(sandboxId, `chmod +x ${scriptPath}`);
    const result = await execWithTransientRetry(sandboxId, `${scriptPath} world`, {
      timeoutMs: 10_000,
      retryWindowMs: 12_000,
    });
    expect(result).toHaveSucceeded();
    expect(result.stdout.trim()).toBe('hello world');
  });

  test('file overwritten via exec reflects in file API', async () => {
    const path = '/tmp/overwrite-test.txt';

    await uploadFile(sandboxId, path, 'version 1');
    const v1 = await downloadFile(sandboxId, path);
    expect(v1).toBe('version 1');

    await exec(sandboxId, `echo -n 'version 2' > ${path}`);
    const v2 = await downloadFile(sandboxId, path);
    expect(v2).toBe('version 2');
  });

  test('file deleted via exec is gone from file API', async ({ request }) => {
    const path = '/tmp/delete-test.txt';

    await uploadFile(sandboxId, path, 'to be deleted');
    // Confirm it exists
    const content = await downloadFile(sandboxId, path);
    expect(content).toBe('to be deleted');

    const rmResult = await exec(sandboxId, `rm -f ${path}`);
    expect(rmResult).toHaveSucceeded();

    // After unlink, some setups (busy CI / overlay) can briefly still serve the file;
    // poll until the file API observes the delete.
    const noStore = {
      'X-Api-Key': process.env.SANDBOX_API_KEY || 'test',
      'Cache-Control': 'no-store',
      Pragma: 'no-cache',
    };
    await expect
      .poll(
        async () => {
          const resp = await apiRequest(
            request,
            'GET',
            `/sandboxes/${sandboxId}/files/content?path=${encodeURIComponent(path)}`,
            { rawHeaders: noStore },
          );
          return resp.status;
        },
        { timeout: 10_000, intervals: [20, 50, 100, 200] },
      )
      .not.toBe(200);
  });

  test('file API write and exec read agree on multiline content', async () => {
    const path = '/tmp/multiline-test.txt';
    const content = 'line1\nline2\nline3';

    await uploadFile(sandboxId, path, content);

    // Use wc -l via exec to confirm line count
    const result = await execWithTransientRetry(sandboxId, `wc -l < ${path}`, {
      timeoutMs: 10_000,
      retryWindowMs: 12_000,
    });
    expect(result).toHaveSucceeded();
    expect(result.stdout.trim()).toBe('2');

    // Confirm full content via exec
    const catResult = await execWithTransientRetry(sandboxId, `cat ${path}`, {
      timeoutMs: 10_000,
      retryWindowMs: 12_000,
    });
    expect(catResult.stdout.trim()).toBe(content);
  });
});

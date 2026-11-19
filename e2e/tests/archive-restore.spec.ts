import { test, expect } from '@playwright/test';
import './matchers';
import { client, createSandbox, deleteSandbox, ensureTenantAuth, execWithTransientRetry } from './helpers';

test.describe.configure({ timeout: 300_000 });

// Archive keeps a sandbox's files, not its memory: what was written, deleted,
// linked and chmodded survives the archive and a restore, and a background
// process does not.
test.describe('Archive and restore', () => {
  test('files survive archive and restore; processes do not', async () => {
    await ensureTenantAuth();
    const id = await createSandbox();
    try {
      const setup = await execWithTransientRetry(
        id,
        [
          'set -e',
          'mkdir -p ~/work && echo kept > ~/work/a.txt',
          'echo gone > ~/work/b.txt && rm ~/work/b.txt',
          'ln -s a.txt ~/work/link && chmod 640 ~/work/a.txt',
          '(sleep 1000 &)',
        ].join('; '),
      );
      expect(setup).toHaveSucceeded();

      const archived = await client.archiveSandbox(id);
      expect(archived.status).toBe('archived');
      expect(archived.archivedAt).toBeGreaterThan(0);

      await expect
        .poll(
          async () => {
            await client.restoreSandbox(id).catch(() => undefined);
            return (await client.getSandbox(id)).status;
          },
          { timeout: 240_000, intervals: [3_000] },
        )
        .toBe('running');

      const check = await execWithTransientRetry(
        id,
        'cat ~/work/a.txt; test ! -e ~/work/b.txt && echo no-b; readlink ~/work/link; stat -c %a ~/work/a.txt; pgrep -f "sleep 1000" || echo no-sleep',
      );
      expect(check).toHaveSucceeded();
      expect(check.stdout.trim().split('\n')).toEqual(['kept', 'no-b', 'a.txt', '640', 'no-sleep']);
    } finally {
      await deleteSandbox(id);
    }
  });

  test('an ephemeral sandbox is never archived', async () => {
    await ensureTenantAuth();
    const id = await createSandbox({ ephemeral: true });
    try {
      const err = await client.archiveSandbox(id).catch((e) => e);
      expect(err.status).toBe(409);
      expect(err.reason).toBe('ephemeral');
    } finally {
      await deleteSandbox(id);
    }
  });
});

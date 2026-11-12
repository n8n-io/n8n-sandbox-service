import { spawnSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { test, expect } from '@playwright/test';
import { createSandbox, deleteSandbox } from './helpers';
import { FIRECRACKER_ONLY } from './tags';

const TEMPLATE_KERNEL = '/srv/firecracker/template/vmlinux';
// The uid and gid jailer gives every Firecracker process.
const JAILER_ID = '1000';

type CommandResult = { status: number | null; output: string };

function run(command: string, args: string[]): CommandResult {
  const result = spawnSync(command, args, { encoding: 'utf8' });
  return { status: result.status, output: `${result.stdout}${result.stderr}`.trim() };
}

function shellAsJailerUid(script: string): CommandResult {
  return run('sudo', [
    'setpriv',
    `--reuid=${JAILER_ID}`,
    `--regid=${JAILER_ID}`,
    '--clear-groups',
    'sh',
    '-c',
    script,
  ]);
}

// The runner names a microVM after the first 12 hex digits of the sandbox id's
// SHA-256, and jailer passes that name to Firecracker as its --id.
function firecrackerPid(sandboxId: string): string {
  const vmId = `sandbox-${createHash('sha256').update(sandboxId).digest('hex').slice(0, 12)}`;
  const processes = run('pgrep', ['-x', 'firecracker', '-a']).output.split('\n');
  const match = processes.find((line) => line.includes(vmId));
  if (!match) {
    throw new Error(`no firecracker process for ${vmId} among:\n${processes.join('\n')}`);
  }
  return match.split(' ')[0];
}

// Every jail on a host binds the same kernel file, so a Firecracker process that
// could write it would choose the kernel later sandboxes boot.
test.describe('Firecracker guest kernel is read-only', FIRECRACKER_ONLY, () => {
  // The runner checks the same on every start, so a runner that is ready after
  // a restart has passed it again.
  test('template kernel is root-owned and not writable', () => {
    const stat = run('stat', ['-c', '%u:%g %a', TEMPLATE_KERNEL]);
    expect(stat.output).toBe('0:0 444');
  });

  test('a jailed Firecracker cannot open the kernel for writing', async () => {
    const id = await createSandbox();
    try {
      // The kernel as the jailed process sees it, in its own mount namespace.
      const kernel = `/proc/${firecrackerPid(id)}/root/vmlinux`;

      // Reading has to work for the same user on the same path, or the refused
      // write below would prove nothing.
      const magic = shellAsJailerUid(`head -c 4 ${kernel} | od -An -tx1`);
      expect(magic.output.replace(/\s+/g, '')).toBe('7f454c46');

      const write = shellAsJailerUid(`: >> ${kernel}`);
      expect(write.status, write.output).not.toBe(0);

      // File permissions do not stop root, so this refusal is the read-only mount.
      const rootWrite = run('sudo', ['sh', '-c', `: >> ${kernel}`]);
      expect(rootWrite.status, rootWrite.output).not.toBe(0);
      expect(rootWrite.output).toContain('Read-only file system');
    } finally {
      await deleteSandbox(id);
    }
  });
});

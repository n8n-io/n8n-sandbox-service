import { test, expect } from '@playwright/test';
import './matchers';
import { createSandbox, deleteSandbox, exec, execWithTransientRetry } from './helpers';

// A sandbox has no root: /usr/local is read-only to it and apt-get is gone, so
// every install lands under /home/user. Nothing in the sandbox can add the two
// install roots or the toolchain back, so these tests pin all three.
test.describe('sandbox package installation', () => {
  test('pip installs into the prebuilt venv', async () => {
    test.setTimeout(180_000);
    const id = await createSandbox();
    try {
      const prefix = await execWithTransientRetry(
        id,
        'command -v pip; python3 -c "import sys; print(sys.prefix)"',
      );
      expect(prefix).toHaveSucceeded();
      expect(prefix.stdout.trim().split('\n')).toEqual([
        '/home/user/venv/bin/pip',
        '/home/user/venv',
      ]);

      // The same command against /usr/bin/python3 would be refused: Debian
      // marks its interpreter externally-managed.
      const installed = await exec(id, 'pip install --no-input six', { timeoutMs: 120_000 });
      expect(installed).toHaveSucceeded();

      const imported = await exec(id, 'python3 -c "import six; print(six.__file__)"');
      expect(imported).toHaveSucceeded();
      expect(imported.stdout).toContain('/home/user/venv');
    } finally {
      await deleteSandbox(id);
    }
  });

  test('npm installs globally into the per-user prefix', async () => {
    test.setTimeout(180_000);
    const id = await createSandbox();
    try {
      const prefix = await execWithTransientRetry(id, 'npm config get prefix', {
        timeoutMs: 60_000,
      });
      expect(prefix).toHaveSucceeded();
      expect(prefix.stdout.trim()).toBe('/home/user/.npm-global');

      const installed = await exec(id, 'npm install -g --ignore-scripts semver', {
        timeoutMs: 120_000,
      });
      expect(installed).toHaveSucceeded();

      // Resolving the name proves the prefix's bin is on PATH, not just that
      // the files exist.
      const ran = await exec(id, 'semver 1.2.3');
      expect(ran).toHaveSucceeded();
      expect(ran.stdout.trim()).toBe('1.2.3');
    } finally {
      await deleteSandbox(id);
    }
  });

  test('npm installs into a project directory', async () => {
    test.setTimeout(180_000);
    const id = await createSandbox();
    try {
      await execWithTransientRetry(id, 'true');

      // --no-audit: a project install otherwise calls npm's audit endpoint, which
      // can hang while the registry itself is healthy; -g installs never audit.
      const installed = await exec(
        id,
        'mkdir -p /home/user/proj && cd /home/user/proj && npm init -y >/dev/null && ' +
          `npm install --ignore-scripts --no-audit semver >/dev/null && node -e "require('semver'); console.log('ok')"`,
        { timeoutMs: 120_000 },
      );
      expect(installed).toHaveSucceeded();
      expect(installed.stdout.trim()).toBe('ok');
    } finally {
      await deleteSandbox(id);
    }
  });

  // The n8n assistant writes its own package.json into /home/user/workspace and
  // runs npm install there. That install is only free when the image already
  // holds everything, including TypeScript as a local package: the assistant
  // imports it from the workspace, which the global install cannot satisfy.
  test('workspace dependencies install offline and resolve from the workspace', async () => {
    test.setTimeout(180_000);
    const id = await createSandbox();
    try {
      const installed = await execWithTransientRetry(
        id,
        'npm install --ignore-scripts --no-audit --no-fund --offline',
        { workdir: '/home/user/workspace', timeoutMs: 120_000 },
      );
      expect(installed).toHaveSucceeded();

      const resolved = await exec(
        id,
        `node --input-type=module -e "await import('@n8n/workflow-sdk'); await import('typescript/unstable/async'); console.log('ok')"`,
        { workdir: '/home/user/workspace' },
      );
      expect(resolved).toHaveSucceeded();
      expect(resolved.stdout.trim()).toBe('ok');
    } finally {
      await deleteSandbox(id);
    }
  });

  // Every pip sdist and npm native addon needs a compiler, and a sandbox cannot
  // apt-get one.
  test('ships a toolchain for packages that build from source', async () => {
    const id = await createSandbox();
    try {
      const toolchain = await execWithTransientRetry(
        id,
        'cc --version >/dev/null && make --version >/dev/null && ls /usr/include/python3*/Python.h',
      );
      expect(toolchain).toHaveSucceeded();
      expect(toolchain.stdout).toContain('Python.h');
    } finally {
      await deleteSandbox(id);
    }
  });
});

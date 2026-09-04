# @n8n/sandbox-client

Client for the n8n sandbox service API.

## Install

```sh
pnpm add @n8n/sandbox-client
```

## Usage

```ts
import { SandboxClient } from '@n8n/sandbox-client';

const client = new SandboxClient({
  baseUrl: 'http://localhost:8080',
  apiKey: 'your-api-key',
});
```

### Retries (`retry` option)

By default the client retries transient failures: 3 extra attempts (four tries total), backoff with jitter, and HTTP statuses `429` and `503` (plus transport errors, represented as status `0`). `502` is not in the default retry set — the service uses it when repeating the same request is unlikely to help.

- Turn off retries: `retry: { attempts: 0 }`.
- Tune backoff: `retry: { attempts: 5, baseDelayMs: 100, maxDelayMs: 30_000, jitter: false }`.
- Also retry other statuses (only if you accept the risk): `retry: { retryOnStatuses: [429, 502, 503] }`.
- Idempotent methods (`GET`, `HEAD`, `OPTIONS`, `PUT`, `DELETE`) use this policy automatically.
- `POST` is not retried unless you set `isSafeToRetry: true` on that specific request (for example when the server makes the operation idempotent, as with `exec_id` on exec).

`exec` still has its own stream resume loop (`exec_id`, `POST` then `GET` follow). Constructor `retry` applies to each underlying HTTP call (so `GET` resume lines benefit from the default policy). It does not replace the exec event/state machine.

### Sandbox lifecycle

```ts
// Create a sandbox
const sandbox = await client.createSandbox();
console.log(sandbox.id); // UUID

// Create or reconnect to a deterministic sandbox
const stableSandbox = await client.createSandbox({
  id: '550e8400-e29b-41d4-a716-446655440000',
});

// Ephemeral: deleted instead of stopped once idle past the service's
// idle-stop window; it never reports `stopped` and then returns 404.
const scratch = await client.createSandbox({ ephemeral: true });
console.log(scratch.ephemeral); // true

// Get sandbox info
const info = await client.getSandbox(sandbox.id);

// Delete sandbox
await client.deleteSandbox(sandbox.id);
```

### Execute commands

```ts
const result = await client.exec(sandbox.id, {
  command: 'echo hello world',
  env: { NODE_ENV: 'production' },
  workdir: '/home/user',
  timeoutMs: 30_000,
});

console.log(result.stdout);          // "hello world\n"
console.log(result.exitCode);        // 0
console.log(result.success);         // true
console.log(result.executionTimeMs); // 42
```

Stream output as it arrives:

```ts
const result = await client.exec(sandbox.id, {
  command: 'npm install',
  onStdout: (data) => process.stdout.write(data),
  onStderr: (data) => process.stderr.write(data),
});
```

Cancel a running command:

```ts
const controller = new AbortController();
setTimeout(() => controller.abort(), 5000);

const result = await client.exec(sandbox.id, {
  command: 'sleep 60',
  abortSignal: controller.signal,
});
```

### File operations

```ts
// Write a file
await client.writeFile(sandbox.id, '/home/user/hello.txt', 'Hello, world!');

// Write without overwriting
await client.writeFile(sandbox.id, '/home/user/hello.txt', 'new content', false);

// Read a file
const buf = await client.readFile(sandbox.id, '/home/user/hello.txt');
console.log(buf.toString());

// Append to a file
await client.appendFile(sandbox.id, '/home/user/log.txt', 'new line\n');

// Delete a file
await client.deleteFile(sandbox.id, '/home/user/hello.txt');

// Delete a directory recursively
await client.deleteFile(sandbox.id, '/home/user/node_modules', { recursive: true });

// Create a directory
await client.mkdir(sandbox.id, '/home/user/src/components', true);

// List files
const files = await client.listFiles(sandbox.id, {
  path: '/home/user/src',
  recursive: true,
  extension: '.ts',
});

// Get file metadata
const stat = await client.stat(sandbox.id, '/home/user/hello.txt');
console.log(stat.size, stat.type); // 13 "file"

// Copy a file
await client.copyFile(sandbox.id, {
  src: '/home/user/hello.txt',
  dest: '/tmp/hello-copy.txt',
});

// Move / rename a file
await client.moveFile(sandbox.id, {
  src: '/tmp/hello-copy.txt',
  dest: '/tmp/renamed.txt',
});
```

### Error handling

All API errors throw `SandboxServiceError`:

```ts
import { SandboxServiceError } from '@n8n/sandbox-client';

try {
  await client.getSandbox('nonexistent-id');
} catch (err) {
  if (err instanceof SandboxServiceError) {
    console.log(err.status);  // 404
    console.log(err.message); // "sandbox not found"
  }
}
```

`deleteSandbox` treats HTTP 404 as success (already gone), so a retried delete after a dropped `204` does not fail.

### Sandbox restarts

If a sandbox's guest crashes, the service recovers it by rebooting its filesystem and then fails the request that triggered the recovery with `SandboxCrashedError` (HTTP 409). The files are intact; everything that was in memory is not. Retry the request once — the sandbox is already running again — and relaunch whatever it was running in the background:

```ts
import { SandboxCrashedError } from '@n8n/sandbox-client';

try {
  return await client.exec(sandboxId, { command: 'curl -s localhost:3000' });
} catch (err) {
  if (err instanceof SandboxCrashedError) {
    // Files survived; the dev server this depends on did not.
    await client.exec(sandboxId, { command: 'npm run dev &' });
    return await client.exec(sandboxId, { command: 'curl -s localhost:3000' });
  }
  throw err;
}
```

It is never retried automatically, by design: an invisible retry would hand back a working sandbox and hide the loss. Two consequences to plan for — a completed execution is no longer readable (`getExecution` returns 404), and a caller-supplied `execId` is no longer idempotent, so re-posting one that ran before the restart runs the command again.

A crash is not the only way memory goes, so do not treat this error as the only signal for it. A sandbox left idle long enough is stopped by the service, and depending on the deployment's runtime, waking it can cost the same three things — with no `SandboxCrashedError`, because an idle stop is reported through `status` instead: `getSandbox` returns `stopped` for it, while a crash leaves `running`. So checking `status` before relying on background processes is what catches the idle stop in advance. A crash cannot be caught that way — `status` stays `running` right through it — and the `SandboxCrashedError` above is its only notice.

## Development

```sh
pnpm install
pnpm build       # Build CJS + ESM + types
pnpm typecheck   # Type-check without emitting
pnpm test        # Run tests once
pnpm test:dev    # Run tests in watch mode
```

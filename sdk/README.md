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

### Sandbox lifecycle

```ts
// Create a sandbox
const sandbox = await client.createSandbox();
console.log(sandbox.id); // UUID

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

## Development

```sh
pnpm install
pnpm build       # Build CJS + ESM + types
pnpm typecheck   # Type-check without emitting
pnpm test        # Run tests once
pnpm test:dev    # Run tests in watch mode
```

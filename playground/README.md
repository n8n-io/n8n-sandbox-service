# Sandbox Playground

A browser-based UI for interacting with the sandbox service API. Useful for manually testing sandboxes without writing code.

## Features

- Create sandboxes (from a base image or with custom Dockerfile steps)
- List, select, and delete sandboxes
- Execute commands in a selected sandbox with streaming output (stdout/stderr)
- Command history (arrow keys)
- Abort running commands with Ctrl+C
- Browse and view/edit files inside a sandbox
- List and delete images

## Usage

```sh
pnpm install --frozen-lockfile
pnpm start
```

Then open `http://localhost:3000` in your browser.

Enter the **Base URL** of the sandbox service (default: `http://localhost:8080`) and your **API Key** in the sidebar, then create or select a sandbox to start.

## Settings persistence

The base URL and API key are saved to `localStorage` and restored on page load.

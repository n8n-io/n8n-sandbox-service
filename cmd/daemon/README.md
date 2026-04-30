# Sandbox Daemon

In-container HTTP service that runs inside each sandbox. Provides direct file system access and command execution within the sandboxed environment.

## Key Functions
- Execute commands with timeout and environment control
- File operations (read, write, delete, copy, move)
- Directory operations (create, list, stat)
- Health monitoring endpoint

## Architecture
Lightweight HTTP service that runs inside Docker containers, providing a secure API for file and process operations within the sandbox filesystem.
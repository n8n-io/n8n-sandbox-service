# Sandbox Runner Service

Low-level container management service. Handles Docker container lifecycle operations and provides direct file system access to running sandboxes.

## Key Functions
- Create/delete Docker containers
- Execute commands in containers
- File operations (read, write, move, copy, mkdir)
- Container health monitoring
- Image management

## Architecture
Stateless service that directly manages Docker containers and proxies file operations to the sandbox daemon running inside each container.
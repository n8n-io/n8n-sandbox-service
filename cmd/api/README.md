# Sandbox API Service

HTTP API for managing sandboxed environments. Provides stateful sandbox CRUD operations and proxies file/execution requests to the runner service.

## Key Functions
- Create/list/delete sandboxes with persistent state
- Proxy file operations (upload, download, copy, move) to runner
- Proxy command execution to runner
- Image management endpoints
- Authentication and request routing

## Architecture
Stateful API gateway that coordinates with the stateless runner service for container operations.
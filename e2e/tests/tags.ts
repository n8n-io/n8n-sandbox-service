// Specs run on every runner lane by default. Apply one of these markers only to
// a spec (or describe) that is backend-specific; each lane excludes the other
// lane's marker via `playwright test --grep-invert`.
export const DOCKER_ONLY = { tag: '@docker-only' };
export const FIRECRACKER_ONLY = { tag: '@firecracker-only' };

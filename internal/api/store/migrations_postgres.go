package store

const postgresSchema = `
CREATE TABLE IF NOT EXISTS sandboxes (
	id                        TEXT PRIMARY KEY,
	status                    TEXT NOT NULL,
	created_at                BIGINT NOT NULL,
	last_active_at            BIGINT NOT NULL,
	rootfs_path               TEXT,
	socket_path               TEXT,
	container_ip              TEXT,
	daemon_port               INTEGER NOT NULL DEFAULT 8081,
	runner_id                 TEXT NOT NULL DEFAULT '',
	runner_http_base_url      TEXT NOT NULL DEFAULT '',
	runner_control_grpc_addr  TEXT NOT NULL DEFAULT '',
	tenant_id                 TEXT NOT NULL DEFAULT '__admin__',
	ephemeral                 BOOLEAN NOT NULL DEFAULT false,
	egress                    TEXT NOT NULL DEFAULT 'public',
	kind                      VARCHAR(32) NOT NULL DEFAULT 'default' CONSTRAINT sandboxes_kind_check CHECK (kind IN ('default', 'code')),
	name                      VARCHAR(128) NOT NULL DEFAULT '',
	options_hash              VARCHAR(512) NOT NULL DEFAULT '',
	idle_timeout_ms           BIGINT NOT NULL DEFAULT 0,
	max_age_ms                BIGINT NOT NULL DEFAULT 0,
	draining_since            BIGINT NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS sandboxes_idle_reap_idx
	ON sandboxes (status, last_active_at);
`

const postgresSandboxTenantIndex = `CREATE INDEX IF NOT EXISTS sandboxes_tenant_idx ON sandboxes (tenant_id)`

const postgresRunnersSchema = `
CREATE TABLE IF NOT EXISTS runners (
	id                   TEXT PRIMARY KEY,
	http_base_url        TEXT NOT NULL,
	control_grpc_addr    TEXT NOT NULL DEFAULT '',
	healthy              BOOLEAN NOT NULL DEFAULT true,
	capacity_total       INTEGER NOT NULL DEFAULT 0,
	capacity_used        INTEGER NOT NULL DEFAULT 0,
	capacity_stopped     INTEGER NOT NULL DEFAULT 0,
	last_seen            TIMESTAMPTZ NOT NULL,
	registered_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS runners_eligible_idx
	ON runners (healthy, last_seen, capacity_used);
`

const postgresTenantsSchema = `
CREATE TABLE IF NOT EXISTS tenants (
	id             TEXT PRIMARY KEY,
	name           TEXT NOT NULL DEFAULT '',
	external_ref   TEXT NOT NULL DEFAULT '',
	max_sandboxes  INTEGER NOT NULL DEFAULT 50,
	created_at     BIGINT NOT NULL
);
CREATE TABLE IF NOT EXISTS api_keys (
	id          TEXT PRIMARY KEY,
	tenant_id   TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
	key_hash    TEXT NOT NULL,
	prefix      TEXT NOT NULL,
	created_at  BIGINT NOT NULL,
	revoked_at  BIGINT
);
CREATE INDEX IF NOT EXISTS api_keys_prefix_idx ON api_keys (prefix);
CREATE INDEX IF NOT EXISTS api_keys_tenant_idx ON api_keys (tenant_id);
`

const postgresAddTenantIDCol = `ALTER TABLE sandboxes ADD COLUMN IF NOT EXISTS tenant_id TEXT NOT NULL DEFAULT '__admin__'`

const postgresAddEphemeralCol = `ALTER TABLE sandboxes ADD COLUMN IF NOT EXISTS ephemeral BOOLEAN NOT NULL DEFAULT false`

const postgresAddEgressCol = `ALTER TABLE sandboxes ADD COLUMN IF NOT EXISTS egress TEXT NOT NULL DEFAULT 'public'`

// One statement, so the ACCESS EXCLUSIVE lock is taken once per startup.
const postgresAddCodeSandboxCols = `ALTER TABLE sandboxes
	ADD COLUMN IF NOT EXISTS kind VARCHAR(32) NOT NULL DEFAULT 'default' CONSTRAINT sandboxes_kind_check CHECK (kind IN ('default', 'code')),
	ADD COLUMN IF NOT EXISTS name VARCHAR(128) NOT NULL DEFAULT '',
	ADD COLUMN IF NOT EXISTS options_hash VARCHAR(512) NOT NULL DEFAULT '',
	ADD COLUMN IF NOT EXISTS idle_timeout_ms BIGINT NOT NULL DEFAULT 0,
	ADD COLUMN IF NOT EXISTS max_age_ms BIGINT NOT NULL DEFAULT 0,
	ADD COLUMN IF NOT EXISTS draining_since BIGINT NOT NULL DEFAULT 0`

// Runs after postgresAddCodeSandboxCols, so existing tables get the comments too.
const postgresCommentCodeSandboxCols = `
COMMENT ON COLUMN sandboxes.kind IS 'Sandbox kind: default or code';
COMMENT ON COLUMN sandboxes.name IS 'Code sandboxes: name chosen by the client. Unique per tenant among code sandboxes that are not draining';
COMMENT ON COLUMN sandboxes.options_hash IS 'Code sandboxes: hash of the options the sandbox was created with';
COMMENT ON COLUMN sandboxes.idle_timeout_ms IS 'Code sandboxes: delete after this many milliseconds without activity. 0 = not set';
COMMENT ON COLUMN sandboxes.max_age_ms IS 'Code sandboxes: drain this many milliseconds after creation. 0 = not set';
COMMENT ON COLUMN sandboxes.draining_since IS 'Unix seconds when the sandbox started draining. 0 = not draining';
`

// A name points to at most one code sandbox per tenant that is not draining.
const postgresSandboxCodeNameIndex = `CREATE UNIQUE INDEX IF NOT EXISTS ` + codeNameIndex + `
	ON sandboxes (tenant_id, name) WHERE kind = 'code' AND draining_since = 0`

const postgresAddRunnerCordonedCol = `ALTER TABLE runners ADD COLUMN IF NOT EXISTS cordoned BOOLEAN NOT NULL DEFAULT false`

// Backfill legacy empty tenant_id to the admin sentinel.
const postgresBackfillAdminTenantID = `UPDATE sandboxes SET tenant_id = '__admin__' WHERE tenant_id = ''`

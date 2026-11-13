package store

const sqliteSchema = `
CREATE TABLE IF NOT EXISTS sandboxes (
	id              TEXT    PRIMARY KEY,
	status          TEXT    NOT NULL,
	created_at      INTEGER NOT NULL,
	last_active_at  INTEGER NOT NULL,
	rootfs_path     TEXT,
	socket_path     TEXT,
	container_ip    TEXT,
	daemon_port     INTEGER DEFAULT 8081
);
`

const sqliteAddContainerIPCol = `ALTER TABLE sandboxes ADD COLUMN container_ip TEXT`

const sqliteAddDaemonPortCol = `ALTER TABLE sandboxes ADD COLUMN daemon_port INTEGER DEFAULT 8081`

const sqliteDropContainerIDCol = `ALTER TABLE sandboxes DROP COLUMN container_id`

const sqliteAddRunnerIDCol = `ALTER TABLE sandboxes ADD COLUMN runner_id TEXT DEFAULT ''`

const sqliteAddRunnerHTTPBaseURLCol = `ALTER TABLE sandboxes ADD COLUMN runner_http_base_url TEXT DEFAULT ''`

const sqliteAddRunnerControlGRPCAddrCol = `ALTER TABLE sandboxes ADD COLUMN runner_control_grpc_addr TEXT DEFAULT ''`

const sqliteAddTenantIDCol = `ALTER TABLE sandboxes ADD COLUMN tenant_id TEXT NOT NULL DEFAULT '__admin__'`

const sqliteAddEphemeralCol = `ALTER TABLE sandboxes ADD COLUMN ephemeral INTEGER NOT NULL DEFAULT 0`

const sqliteAddEgressCol = `ALTER TABLE sandboxes ADD COLUMN egress TEXT NOT NULL DEFAULT 'public'`

// SQLite ignores VARCHAR lengths, so CHECKs hold the same limits as Postgres.
const sqliteAddKindCol = `ALTER TABLE sandboxes ADD COLUMN kind TEXT NOT NULL DEFAULT 'default' CHECK (kind IN ('default', 'code'))`

const sqliteAddNameCol = `ALTER TABLE sandboxes ADD COLUMN name TEXT NOT NULL DEFAULT '' CHECK (length(name) <= 128)`

const sqliteAddOptionsHashCol = `ALTER TABLE sandboxes ADD COLUMN options_hash TEXT NOT NULL DEFAULT '' CHECK (length(options_hash) <= 512)`

const sqliteAddIdleTimeoutMsCol = `ALTER TABLE sandboxes ADD COLUMN idle_timeout_ms INTEGER NOT NULL DEFAULT 0`

const sqliteAddMaxAgeMsCol = `ALTER TABLE sandboxes ADD COLUMN max_age_ms INTEGER NOT NULL DEFAULT 0`

const sqliteAddDrainingSinceCol = `ALTER TABLE sandboxes ADD COLUMN draining_since INTEGER NOT NULL DEFAULT 0`

// A name points to at most one code sandbox per tenant that is not draining.
// Runs after the column migrations: an existing table gains the columns there.
const sqliteSandboxCodeNameIndex = `CREATE UNIQUE INDEX IF NOT EXISTS ` + codeNameIndex + `
	ON sandboxes (tenant_id, name) WHERE kind = 'code' AND draining_since = 0`

// Backfill legacy empty tenant_id to the admin sentinel.
const sqliteBackfillAdminTenantID = `UPDATE sandboxes SET tenant_id = '__admin__' WHERE tenant_id = ''`

const sqliteTenantsSchema = `
CREATE TABLE IF NOT EXISTS tenants (
	id             TEXT PRIMARY KEY,
	name           TEXT NOT NULL DEFAULT '',
	external_ref   TEXT NOT NULL DEFAULT '',
	max_sandboxes  INTEGER NOT NULL DEFAULT 50,
	created_at     INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS api_keys (
	id          TEXT PRIMARY KEY,
	tenant_id   TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
	key_hash    TEXT NOT NULL,
	prefix      TEXT NOT NULL,
	created_at  INTEGER NOT NULL,
	revoked_at  INTEGER
);
CREATE INDEX IF NOT EXISTS api_keys_prefix_idx ON api_keys (prefix);
CREATE INDEX IF NOT EXISTS api_keys_tenant_idx ON api_keys (tenant_id);
CREATE INDEX IF NOT EXISTS sandboxes_tenant_idx ON sandboxes (tenant_id);
`

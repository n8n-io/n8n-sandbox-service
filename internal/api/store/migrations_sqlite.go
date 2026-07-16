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

const sqliteAddTenantIDCol = `ALTER TABLE sandboxes ADD COLUMN tenant_id TEXT NOT NULL DEFAULT ''`

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

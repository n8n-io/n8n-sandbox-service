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
	template                  TEXT NOT NULL DEFAULT 'default',
	idle_stop_after           BIGINT NOT NULL DEFAULT 0,
	idle_delete_after         BIGINT NOT NULL DEFAULT 0,
	egress_proxy              TEXT NOT NULL DEFAULT '',
	model_blocked             BOOLEAN NOT NULL DEFAULT false
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

// postgresAddRunnerStateCols adds the cordon and the served templates (a JSON
// array of registry.Template, ” for a runner that lists none) to runner
// heartbeats.
const postgresAddRunnerStateCols = `
ALTER TABLE runners
	ADD COLUMN IF NOT EXISTS cordoned BOOLEAN NOT NULL DEFAULT false,
	ADD COLUMN IF NOT EXISTS templates TEXT NOT NULL DEFAULT ''`

// postgresAddTenantMaxIdleWindowCol caps a tenant's idle windows, in seconds;
// 0 is the server's cap.
const postgresAddTenantMaxIdleWindowCol = `ALTER TABLE tenants ADD COLUMN IF NOT EXISTS max_idle_window BIGINT NOT NULL DEFAULT 0`

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

// One statement so a pod on the previous schema gets all four columns or none.
const postgresAddCreateOptionCols = `ALTER TABLE sandboxes
	ADD COLUMN IF NOT EXISTS template TEXT NOT NULL DEFAULT 'default',
	ADD COLUMN IF NOT EXISTS idle_stop_after BIGINT NOT NULL DEFAULT 0,
	ADD COLUMN IF NOT EXISTS idle_delete_after BIGINT NOT NULL DEFAULT 0,
	ADD COLUMN IF NOT EXISTS egress_proxy TEXT NOT NULL DEFAULT '',
	ADD COLUMN IF NOT EXISTS model_blocked BOOLEAN NOT NULL DEFAULT false`

// Backfill legacy empty tenant_id to the admin sentinel.
const postgresBackfillAdminTenantID = `UPDATE sandboxes SET tenant_id = '__admin__' WHERE tenant_id = ''`

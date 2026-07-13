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
	runner_control_grpc_addr  TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS sandboxes_idle_reap_idx
	ON sandboxes (status, last_active_at);
`

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

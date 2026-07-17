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

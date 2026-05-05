package store

// schema is the SQL used to initialise the sandboxes table on first run.
// Using CREATE TABLE IF NOT EXISTS means it is safe to run on every startup.
const schema = `
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

const addContainerIPCol = `ALTER TABLE sandboxes ADD COLUMN container_ip TEXT`

const addDaemonPortCol = `ALTER TABLE sandboxes ADD COLUMN daemon_port INTEGER DEFAULT 8081`

const dropContainerIDCol = `ALTER TABLE sandboxes DROP COLUMN container_id`

const addRunnerIDCol = `ALTER TABLE sandboxes ADD COLUMN runner_id TEXT DEFAULT ''`

const addRunnerHTTPBaseURLCol = `ALTER TABLE sandboxes ADD COLUMN runner_http_base_url TEXT DEFAULT ''`

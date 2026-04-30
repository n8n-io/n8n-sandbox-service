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
	daemon_port     INTEGER DEFAULT 8081,
	image_id        TEXT    DEFAULT '',
	network_policy  TEXT    DEFAULT '{}',
	resource_limits TEXT    DEFAULT '{}'
);

CREATE TABLE IF NOT EXISTS images (
	id              TEXT    PRIMARY KEY,
	tag             TEXT    NOT NULL UNIQUE,
	base_image      TEXT    NOT NULL,
	docker_image_id TEXT    NOT NULL,
	steps_hash      TEXT    NOT NULL,
	dockerfile_steps TEXT   NOT NULL DEFAULT '[]',
	created_at      INTEGER NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_images_steps_hash ON images(steps_hash);
`

// addResourceLimitsCol adds the resource_limits column to existing databases.
// It is safe to run repeatedly — SQLite returns an error if the column already exists.
const addResourceLimitsCol = `ALTER TABLE sandboxes ADD COLUMN resource_limits TEXT DEFAULT '{}'`

const addContainerIDCol = `ALTER TABLE sandboxes ADD COLUMN container_id TEXT`

const addContainerIPCol = `ALTER TABLE sandboxes ADD COLUMN container_ip TEXT`

const addDaemonPortCol = `ALTER TABLE sandboxes ADD COLUMN daemon_port INTEGER DEFAULT 8081`

const addImageIDCol = `ALTER TABLE sandboxes ADD COLUMN image_id TEXT DEFAULT ''`

const dropContainerIDCol = `ALTER TABLE sandboxes DROP COLUMN container_id`

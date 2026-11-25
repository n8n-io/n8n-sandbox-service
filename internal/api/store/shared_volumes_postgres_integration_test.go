//go:build integration

package store

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestPostgresSharedVolumes(t *testing.T) {
	testSharedVolumes(t, openTestPostgresStore(t))
}

// Two stores on one database stand in for two API pods.
func TestPostgresSharedVolumesConcurrentStarts(t *testing.T) {
	testConcurrentSharedVolumeStarts(t, []SharedVolumes{openTestPostgresStore(t), openTestPostgresStore(t)})
}

func TestPostgresSharedVolumesConcurrentStartsStopAtTheLimit(t *testing.T) {
	first, second := openTestPostgresStore(t), openTestPostgresStore(t)
	testConcurrentSharedVolumeStartsStopAtTheLimit(t, []SharedVolumes{first, second}, func() int {
		return countLiveSharedVolumes(t, first.db)
	})
}

// A publish that waits for the live count lock gets its lease from the time
// of its insert, not from the start of its transaction.
func TestPostgresSharedVolumesLeaseStartsAfterTheLockWait(t *testing.T) {
	postgres := openTestPostgresStore(t)
	ctx := context.Background()
	holder, err := postgres.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback() }()
	if _, err := holder.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, sharedVolumesLiveCountLock); err != nil {
		t.Fatal(err)
	}

	row := publishingRow(randomSHA256(), "")
	started := make(chan error, 1)
	go func() {
		_, err := postgres.StartSharedVolumePublish(ctx, row, testLease, 1000)
		started <- err
	}()
	waitForAdvisoryLockWaiter(t, postgres)
	time.Sleep(2 * time.Second)

	// The database's own clock, so a clock that differs on this machine does
	// not matter.
	var releasedAt int64
	if err := holder.QueryRowContext(ctx, `SELECT CAST(FLOOR(EXTRACT(EPOCH FROM clock_timestamp())) AS BIGINT)`).Scan(&releasedAt); err != nil {
		t.Fatal(err)
	}
	if err := holder.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := <-started; err != nil {
		t.Fatalf("start: %v", err)
	}
	if want := releasedAt + leaseSeconds(testLease); row.LeaseUntil < want {
		t.Fatalf("lease_until = %d, want at least %d: the lease started before the wait for the lock ended", row.LeaseUntil, want)
	}
}

// waitForAdvisoryLockWaiter returns once a transaction waits for an advisory
// lock in this database.
func waitForAdvisoryLockWaiter(t *testing.T, postgres *PostgresStore) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var waiting int
		err := postgres.db.QueryRow(`SELECT COUNT(*) FROM pg_locks
			WHERE locktype = 'advisory' AND NOT granted
			AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`).Scan(&waiting)
		if err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no transaction waited for the lock")
}

// The limit holds when the server's default isolation is REPEATABLE READ,
// because the store asks for READ COMMITTED itself. Runs in its own database,
// which has that default.
func TestPostgresSharedVolumesConcurrentStartsStopAtTheLimitUnderRepeatableRead(t *testing.T) {
	cfg := testPostgresConfig(t)
	admin, err := sql.Open("pgx", cfg.DSN())
	if err != nil {
		t.Fatalf("open admin conn: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	cfg.Database = "repeatable_read_" + strings.ReplaceAll(uuid.New().String(), "-", "")
	if _, err := admin.Exec(`CREATE DATABASE ` + cfg.Database); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(`DROP DATABASE IF EXISTS ` + cfg.Database + ` WITH (FORCE)`) })
	if _, err := admin.Exec(`ALTER DATABASE ` + cfg.Database + ` SET default_transaction_isolation = 'repeatable read'`); err != nil {
		t.Fatalf("set the default isolation: %v", err)
	}

	first, err := NewPostgres(cfg)
	if err != nil {
		t.Fatalf("NewPostgres: %v", err)
	}
	t.Cleanup(func() { _ = first.Close() })
	second, err := NewPostgres(cfg)
	if err != nil {
		t.Fatalf("NewPostgres: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })
	testConcurrentSharedVolumeStartsStopAtTheLimit(t, []SharedVolumes{first, second}, func() int {
		return countLiveSharedVolumes(t, first.db)
	})
}

func TestPostgresSharedVolumesConcurrentDeletes(t *testing.T) {
	testConcurrentSharedVolumeDeletes(t, []SharedVolumes{openTestPostgresStore(t), openTestPostgresStore(t)})
}

func TestPostgresSharedVolumeDeleteDuringFinish(t *testing.T) {
	testDeleteDuringFinish(t, []SharedVolumes{openTestPostgresStore(t), openTestPostgresStore(t)})
}

// Every API pod runs the schema when it starts, several at once after a
// rollout.
func TestPostgresSharedVolumesSchemaRunsOnPodsStartingTogether(t *testing.T) {
	postgres := openTestPostgresStore(t)
	const pods, rounds = 4, 5
	for range rounds {
		var wait sync.WaitGroup
		start := make(chan struct{})
		for range pods {
			wait.Go(func() {
				<-start
				for _, statement := range postgresSharedVolumesSchema {
					if _, err := postgres.db.Exec(statement); err != nil {
						t.Errorf("schema: %v", err)
						return
					}
				}
			})
		}
		close(start)
		wait.Wait()
	}
}

//go:build integration

package store

import (
	"context"
	"sync"
	"testing"
	"time"
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

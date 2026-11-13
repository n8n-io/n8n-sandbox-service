//go:build integration

package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/n8n-io/sandbox-service/internal/api/config"
)

func testPostgresConfig(t *testing.T) config.PostgresConfig {
	t.Helper()
	host := os.Getenv("SANDBOX_TEST_POSTGRES_HOST")
	if host == "" {
		t.Skip("SANDBOX_TEST_POSTGRES_HOST not set")
	}
	port := 5432
	if v := os.Getenv("SANDBOX_TEST_POSTGRES_PORT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			t.Fatalf("SANDBOX_TEST_POSTGRES_PORT: %v", err)
		}
		port = n
	}
	user := os.Getenv("SANDBOX_TEST_POSTGRES_USER")
	if user == "" {
		user = "postgres"
	}
	password := os.Getenv("SANDBOX_TEST_POSTGRES_PASSWORD")
	db := os.Getenv("SANDBOX_TEST_POSTGRES_DB")
	if db == "" {
		db = "postgres"
	}
	sslmode := os.Getenv("SANDBOX_TEST_POSTGRES_SSLMODE")
	if sslmode == "" {
		sslmode = "disable"
	}
	return config.PostgresConfig{
		Host:     host,
		Port:     port,
		User:     user,
		Password: password,
		Database: db,
		SSLMode:  sslmode,
	}
}

func openTestPostgresStore(t *testing.T) *PostgresStore {
	t.Helper()
	s, err := NewPostgres(testPostgresConfig(t))
	if err != nil {
		t.Fatalf("NewPostgres: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// Rows from before the code sandbox columns existed read back as default-kind
// sandboxes without a name or lifetime settings. Runs in its own database,
// since it changes the schema.
func TestPostgresCodeSandboxColumnsMigrationBackfillsDefaults(t *testing.T) {
	cfg := testPostgresConfig(t)
	admin, err := sql.Open("pgx", cfg.DSN())
	if err != nil {
		t.Fatalf("open admin conn: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	cfg.Database = "migration_" + strings.ReplaceAll(uuid.New().String(), "-", "")
	if _, err := admin.Exec(`CREATE DATABASE ` + cfg.Database); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(`DROP DATABASE IF EXISTS ` + cfg.Database + ` WITH (FORCE)`) })

	s, err := NewPostgres(cfg)
	if err != nil {
		t.Fatalf("NewPostgres: %v", err)
	}
	// Roll the schema back to before the columns and plant a row there.
	if _, err := s.db.Exec(`DROP INDEX ` + codeNameIndex + `;
		ALTER TABLE sandboxes DROP COLUMN kind, DROP COLUMN name, DROP COLUMN options_hash,
			DROP COLUMN idle_timeout_ms, DROP COLUMN max_age_ms, DROP COLUMN draining_since`); err != nil {
		t.Fatalf("roll back schema: %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO sandboxes (id, status, created_at, last_active_at, rootfs_path, socket_path, container_ip)
		VALUES ('legacy', 'running', 1, 2, '', '', '')`); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s, err = NewPostgres(cfg)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	got, err := s.Get("legacy")
	if err != nil || got == nil {
		t.Fatalf("get legacy row: got=%v err=%v", got, err)
	}
	if got.Kind != KindDefault || got.Name != "" || got.OptionsHash != "" ||
		got.IdleTimeoutMs != 0 || got.MaxAgeMs != 0 || got.DrainingSince != 0 {
		t.Fatalf("legacy row: %+v, want default kind and zero code sandbox fields", got)
	}
	// The migration also brings back the name index and the kind check.
	testCodeSandboxNames(t, s, uuid.New().String(), uuid.New().String())
}

func TestPostgresStoreCRUD(t *testing.T) {
	s := openTestPostgresStore(t)

	rec := &SandboxRecord{
		ID:                    "pg-sandbox-1",
		Status:                "running",
		CreatedAt:             1,
		LastActiveAt:          2,
		ContainerIP:           "172.30.0.2",
		DaemonPort:            8081,
		RunnerID:              "r1",
		RunnerHTTPBase:        "http://runner:8080",
		RunnerControlGRPCAddr: "runner:9091",
	}
	if err := s.Create(rec); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = s.Delete(rec.ID) })

	got, err := s.Get(rec.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got == nil || got.ContainerIP != rec.ContainerIP {
		t.Fatalf("unexpected record: %+v", got)
	}

	// Running and idle: a stop candidate. Other tests share the database, so
	// look for this row rather than counting.
	rows, err := s.ListForIdleReapStop(9999999999, false)
	if err != nil {
		t.Fatalf("list stop: %v", err)
	}
	if !containsSandbox(rows, rec.ID) {
		t.Fatalf("stop candidates %v, want %s", sandboxIDs(rows), rec.ID)
	}

	if err := s.UpdateStatus(rec.ID, "stopped"); err != nil {
		t.Fatalf("update status: %v", err)
	}
	got, err = s.Get(rec.ID)
	if err != nil || got == nil || got.Status != "stopped" {
		t.Fatalf("after stop: %+v err=%v, want stopped", got, err)
	}

	// Stopped: no longer a stop candidate, now a delete candidate.
	rows, err = s.ListForIdleReapStop(9999999999, false)
	if err != nil {
		t.Fatalf("list stop: %v", err)
	}
	if containsSandbox(rows, rec.ID) {
		t.Fatalf("stopped sandbox %s is still a stop candidate", rec.ID)
	}
	rows, err = s.ListForIdleReapDelete(9999999999)
	if err != nil {
		t.Fatalf("list delete: %v", err)
	}
	if !containsSandbox(rows, rec.ID) {
		t.Fatalf("delete candidates %v, want %s", sandboxIDs(rows), rec.ID)
	}
}

func sandboxIDs(rows []*SandboxRecord) []string {
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	return ids
}

func containsSandbox(rows []*SandboxRecord, id string) bool {
	return slices.Contains(sandboxIDs(rows), id)
}

func TestPostgresRecordActivity(t *testing.T) {
	testRecordActivity(t, openTestPostgresStore(t), "pg-activity-"+uuid.New().String())
}

// Every API pod writes its own batch, so batches that share sandboxes run at
// the same time and must neither deadlock nor move a row back.
func TestPostgresRecordActivityConcurrentBatches(t *testing.T) {
	s := openTestPostgresStore(t)
	prefix := "pg-batch-" + uuid.New().String()
	ids := make([]string, 50)
	for i := range ids {
		ids[i] = prefix + "-" + strconv.Itoa(i)
		if err := s.Create(&SandboxRecord{ID: ids[i], Status: "running", CreatedAt: 1, LastActiveAt: 1}); err != nil {
			t.Fatalf("create: %v", err)
		}
		t.Cleanup(func() { _ = s.Delete(ids[i]) })
	}

	const pods, rounds = 4, 25
	errs := make(chan error, pods)
	var wg sync.WaitGroup
	for pod := range pods {
		wg.Go(func() {
			for round := range rounds {
				batch := make(map[string]int64, len(ids))
				for _, id := range ids {
					batch[id] = int64(1000 + round*pods + pod)
				}
				if err := s.RecordActivity(context.Background(), batch); err != nil {
					errs <- err
					return
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("record activity: %v", err)
	}
	const newest = 1000 + (rounds-1)*pods + pods - 1
	for _, id := range ids {
		if got, err := s.Get(id); err != nil || got == nil || got.LastActiveAt != newest {
			t.Fatalf("%s: %+v err=%v, want last_active_at %d", id, got, err, newest)
		}
	}
}

func TestPostgresCodeSandboxNames(t *testing.T) {
	testCodeSandboxNames(t, openTestPostgresStore(t), uuid.New().String(), uuid.New().String())
}

func TestPostgresUpdateTenantLimits(t *testing.T) {
	testUpdateTenantLimits(t, openTestPostgresStore(t), uuid.New().String())
}

func TestPostgresCreateEnforcesTenantQuota(t *testing.T) {
	testCreateEnforcesTenantQuota(t, openTestPostgresStore(t), uuid.New().String())
}

// Two stores on one database stand in for two API pods.
func TestPostgresConcurrentCreatesStopAtTenantQuota(t *testing.T) {
	stores := []SandboxStore{openTestPostgresStore(t), openTestPostgresStore(t)}
	testConcurrentCreatesStopAtTenantQuota(t, stores, uuid.New().String())
}

func TestPostgresSandboxLockKeepsStoreUsableAndReleasesAfterCancellation(t *testing.T) {
	s := openTestPostgresStore(t)
	s.db.SetMaxOpenConns(1)

	unlock, err := s.LockSandbox(context.Background(), "shared-sandbox")
	if err != nil {
		t.Fatalf("lock sandbox: %v", err)
	}
	var unlockOnce sync.Once
	release := func() { unlockOnce.Do(unlock) }
	defer release()

	countDone := make(chan error, 1)
	go func() {
		_, err := s.Count()
		countDone <- err
	}()
	select {
	case err := <-countDone:
		if err != nil {
			t.Fatalf("count while sandbox locked: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("store query blocked by sandbox lock")
	}

	waitCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	waitUnlock, err := s.LockSandbox(waitCtx, "shared-sandbox")
	if waitUnlock != nil {
		waitUnlock()
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("contended lock error = %v, want context deadline exceeded", err)
	}

	release()
	unlock, err = s.LockSandbox(context.Background(), "shared-sandbox")
	if err != nil {
		t.Fatalf("reacquire sandbox lock: %v", err)
	}
	unlock()
}

func TestTryRunExcludesConcurrentHolder(t *testing.T) {
	s := openTestPostgresStore(t)
	ctx := context.Background()

	hold := make(chan struct{})
	release := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		ran, err := TryRun(ctx, s.DB(), func() error {
			close(hold)
			<-release
			return nil
		})
		if err != nil {
			t.Errorf("holder TryRun: %v", err)
		}
		if !ran {
			t.Error("holder should have acquired lock")
		}
	})

	<-hold
	ran, err := TryRun(ctx, s.DB(), func() error { return nil })
	if err != nil {
		t.Fatalf("second TryRun: %v", err)
	}
	if ran {
		t.Fatal("second TryRun should not acquire lock while holder active")
	}
	close(release)
	wg.Wait()
}

func TestTryRunReleasesAfterFnReturns(t *testing.T) {
	s := openTestPostgresStore(t)
	ctx := context.Background()

	ran, err := TryRun(ctx, s.DB(), func() error { return nil })
	if err != nil || !ran {
		t.Fatalf("first TryRun = ran %v err %v", ran, err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ran, err = TryRun(ctx, s.DB(), func() error { return nil })
		if err != nil {
			t.Fatalf("retry TryRun: %v", err)
		}
		if ran {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("lock not released after first TryRun completed")
}

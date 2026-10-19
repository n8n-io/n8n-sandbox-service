//go:build integration

package store

import (
	"context"
	"errors"
	"os"
	"slices"
	"strconv"
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
	if err := s.UpdateLastActive(rec.ID); err != nil {
		t.Fatalf("update last active: %v", err)
	}
	got, err = s.Get(rec.ID)
	if err != nil || got == nil || got.Status != "stopped" || got.LastActiveAt <= rec.LastActiveAt {
		t.Fatalf("after stop: %+v err=%v, want stopped with a newer last_active_at", got, err)
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
	wg.Add(1)
	go func() {
		defer wg.Done()
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
	}()

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

//go:build integration

package store

import (
	"context"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

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

	if err := s.UpdateStatus(rec.ID, "stopped"); err != nil {
		t.Fatalf("update status: %v", err)
	}
	if err := s.UpdateLastActive(rec.ID); err != nil {
		t.Fatalf("update last active: %v", err)
	}

	rows, err := s.ListForIdleReapStop(9999999999)
	if err != nil {
		t.Fatalf("list stop: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("expected stop candidate")
	}
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

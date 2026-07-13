//go:build integration

package registry

import (
	"database/sql"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/n8n-io/sandbox-service/internal/api/config"
	"github.com/n8n-io/sandbox-service/internal/api/store"
)

func openTestPostgresRegistry(t *testing.T) (*PostgresRegistry, *sql.DB) {
	t.Helper()
	cfg := testPostgresConfig(t)
	s, err := store.NewPostgres(cfg)
	if err != nil {
		t.Fatalf("NewPostgres: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	reg := NewPostgres(s.DB(), time.Minute)
	t.Cleanup(func() {
		_, _ = s.DB().Exec(`DELETE FROM runners WHERE id LIKE 'pg-test-%'`)
	})
	return reg, s.DB()
}

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

func TestPostgresRegistryPickLowestUsed(t *testing.T) {
	reg, _ := openTestPostgresRegistry(t)

	reg.Upsert("pg-test-high", "http://127.0.0.1:8080", "127.0.0.1:9091", true, 10, 8, 0)
	reg.Upsert("pg-test-low", "http://127.0.0.1:8081", "127.0.0.1:9092", true, 10, 2, 0)

	run, err := reg.PickLowestUsed()
	if err != nil {
		t.Fatalf("PickLowestUsed: %v", err)
	}
	if run.ID != "pg-test-low" {
		t.Fatalf("got runner %q, want pg-test-low", run.ID)
	}
}

func TestPostgresRegistryGoneLongEnough(t *testing.T) {
	reg, db := openTestPostgresRegistry(t)

	reg.Upsert("pg-test-stale", "http://127.0.0.1:8080", "127.0.0.1:9091", true, 10, 0, 0)
	_, err := db.Exec(`UPDATE runners SET last_seen = now() - interval '10 minutes' WHERE id = $1`, "pg-test-stale")
	if err != nil {
		t.Fatalf("update last_seen: %v", err)
	}

	if !reg.GoneLongEnough("pg-test-stale", 5*time.Minute, time.Now()) {
		t.Fatal("expected stale runner to be gone long enough")
	}
}

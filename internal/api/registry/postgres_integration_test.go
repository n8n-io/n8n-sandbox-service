//go:build integration

package registry

import (
	"database/sql"
	"errors"
	"os"
	"strconv"
	"strings"
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

func TestPostgresRegistryPickLowestUsedSkipsExcluded(t *testing.T) {
	reg, _ := openTestPostgresRegistry(t)

	reg.Upsert("pg-test-high", "http://127.0.0.1:8080", "127.0.0.1:9091", true, 10, 8, 0)
	reg.Upsert("pg-test-low", "http://127.0.0.1:8081", "127.0.0.1:9092", true, 10, 2, 0)

	run, err := reg.PickLowestUsed("pg-test-low")
	if err != nil {
		t.Fatalf("PickLowestUsed: %v", err)
	}
	if run.ID != "pg-test-high" {
		t.Fatalf("got runner %q, want pg-test-high", run.ID)
	}
	if _, err := reg.PickLowestUsed("pg-test-low", "pg-test-high"); !errors.Is(err, ErrNoRunners) {
		t.Fatalf("PickLowestUsed(all) error = %v, want ErrNoRunners", err)
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

func TestPostgresRegistryListIncludesStaleRunners(t *testing.T) {
	reg, db := openTestPostgresRegistry(t)

	reg.Upsert("pg-test-list-b", "http://127.0.0.1:8081", "127.0.0.1:9092", false, 10, 4, 1)
	reg.Upsert("pg-test-list-a", "http://127.0.0.1:8080", "127.0.0.1:9091", true, 8, 2, 0)
	_, err := db.Exec(`UPDATE runners SET last_seen = now() - interval '10 minutes' WHERE id = $1`, "pg-test-list-b")
	if err != nil {
		t.Fatalf("update last_seen: %v", err)
	}

	all, err := reg.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var got []Runner
	for _, run := range all {
		if strings.HasPrefix(run.ID, "pg-test-list-") {
			got = append(got, run)
		}
	}
	if len(got) != 2 || got[0].ID != "pg-test-list-a" || got[1].ID != "pg-test-list-b" {
		t.Fatalf("List() = %+v, want pg-test-list-a then pg-test-list-b", got)
	}
	if b := got[1]; b.Healthy || b.CapacityTotal != 10 || b.CapacityUsed != 4 || b.CapacityStopped != 1 {
		t.Fatalf("List() pg-test-list-b = %+v, want unhealthy 10/4/1", b)
	}
	if age := time.Since(got[1].LastSeen); age < 9*time.Minute {
		t.Fatalf("List() pg-test-list-b last_seen age = %v, want about 10m", age)
	}
}

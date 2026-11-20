//go:build integration

package store

import "testing"

func TestPostgresSharedVolumes(t *testing.T) {
	testSharedVolumes(t, openTestPostgresStore(t))
}

// Two stores on one database stand in for two API pods.
func TestPostgresSharedVolumesConcurrentReserves(t *testing.T) {
	testConcurrentSharedVolumeReserves(t, []SharedVolumes{openTestPostgresStore(t), openTestPostgresStore(t)})
}

func TestPostgresSharedVolumesConcurrentDeletes(t *testing.T) {
	testConcurrentSharedVolumeDeletes(t, []SharedVolumes{openTestPostgresStore(t), openTestPostgresStore(t)})
}

// The schema runs on every start.
func TestPostgresSharedVolumesSchemaRunsTwice(t *testing.T) {
	first := openTestPostgresStore(t)
	if _, err := first.db.Exec(postgresSharedVolumesSchema); err != nil {
		t.Fatalf("schema again: %v", err)
	}
}

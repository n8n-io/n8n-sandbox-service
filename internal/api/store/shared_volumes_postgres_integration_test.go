//go:build integration

package store

import (
	"sync"
	"testing"
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

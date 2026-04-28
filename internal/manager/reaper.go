package manager

import (
	"log/slog"
	"time"
)

// StartReaper starts a background goroutine that periodically checks for and
// deletes idle sandboxes. It stops when the done channel is closed.
func (m *Manager) StartReaper(done <-chan struct{}) {
	ticker := time.NewTicker(30 * time.Second)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				m.reapStale()
			}
		}
	}()
}

// reapStale finds and deletes sandboxes that have exceeded the idle TTL.
func (m *Manager) reapStale() {
	stale, err := m.store.ListStale(int64(m.config.IdleTTLSeconds))
	if err != nil {
		slog.Warn("reaper: list stale", "err", err)
		return
	}

	for _, rec := range stale {
		slog.Info("reaper: deleting stale sandbox", "id", rec.ID, "last_active_at", rec.LastActiveAt)

		m.mu.Lock()
		sb, ok := m.sandboxes[rec.ID]
		if ok {
			delete(m.sandboxes, rec.ID)
		}
		m.mu.Unlock()

		if ok {
			if err := m.cleanupSandbox(sb); err != nil {
				slog.Warn("reaper: cleanup", "id", rec.ID, "err", err)
			}
		} else {
			sb := &Sandbox{
				ID:          rec.ID,
				Record:      rec,
				ContainerID: rec.ContainerID,
				ContainerIP: rec.ContainerIP,
			}
			if err := m.cleanupSandbox(sb); err != nil {
				slog.Warn("reaper: cleanup", "id", rec.ID, "err", err)
			}
		}
	}
}

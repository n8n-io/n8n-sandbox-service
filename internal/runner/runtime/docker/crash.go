package docker

import (
	"context"
	"log/slog"
	"time"
)

// watchGuestDeaths reports sandbox containers that died without the runner asking,
// which is what a Firecracker runner gets for free from the process it supervises.
// Docker owns the container's lifecycle instead, so the deaths have to be subscribed
// to, and the runner learns of them from the same stream whether the guest killed
// itself, hit its memory limit, or was killed on the host.
//
// It runs for the life of the runner and reconnects, because losing this stream is
// the one failure that is silent: containers keep working, and crashes stop being
// reported. A death that happens while it is down is not replayed and stays
// unreported — the container usually comes back on the address it had, so the sandbox
// goes on serving as if nothing was lost. Nothing recovers that; the reconnects are
// what keep the window small.
func (m *Runtime) watchGuestDeaths(ctx context.Context) {
	for ctx.Err() == nil {
		err := m.docker.watchContainerDeaths(ctx, m.handleContainerDeath)
		if ctx.Err() != nil {
			return
		}
		slog.Warn("docker event stream ended, reconnecting", "retry_in", m.watchBackoff, "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(m.watchBackoff):
		}
	}
}

// handleContainerDeath records a died container as a crash unless the runner is the
// one that stopped it. Exit codes are deliberately not consulted: a guest that exits
// 0 on its own has still lost everything it was running, and `docker stop` of a
// healthy sandbox produces the non-zero exit of a SIGTERM'd daemon.
//
// Nothing is torn down here. The container carries `--restart unless-stopped`, so
// Docker is already restarting it — a restart that is invisible to a client, and to
// the network rules pinned to the IP the container had before. Marking the sandbox is
// what makes both visible: DaemonURL reports it not running until the wake path has
// reapplied its policy, and that wake reports the restart to the client.
func (m *Runtime) handleContainerDeath(containerID, sandboxID string) {
	if m.takeExpectedStop(containerID) {
		return
	}
	if sandboxID == "" {
		// The event carries no sandbox label, so there is nothing to mark and no
		// request that could be told. Managed containers always have one.
		slog.Warn("managed container died without a sandbox label", "container_id", containerID)
		return
	}

	m.mu.Lock()
	m.restarted[sandboxID] = struct{}{}
	m.mu.Unlock()

	m.metrics.ObserveGuestDeath()
	slog.Warn("docker guest died", "sandbox_id", sandboxID, "container_id", containerID)
}

// expectedStopTTL bounds how long a recorded stop can excuse a death. Docker emits
// the event within milliseconds of the stop it is asked for, so anything older is a
// stop that never produced one — a container that was already exited when it was
// removed — and a mark kept past that would eventually excuse a real crash of a
// container whose ID it no longer refers to. A stop that reported failure does not
// wait for this bound; see forgetExpectedStop.
const expectedStopTTL = 2 * time.Minute

// expectedStop is one recorded stop. The token names the call that recorded it,
// which is what lets a call that failed take back its own mark and nothing else:
// only one mark exists per container, so a second lifecycle call on the same
// container overwrites the first, and this runtime serializes none of them.
type expectedStop struct {
	token uint64
	at    time.Time
}

// expectStop records that the runner is about to stop or remove a container, so the
// die event that follows is not read as a crash. Keyed by container rather than
// sandbox because that is what every caller has, and what the event names.
//
// The key has to be the full container ID, since that is what the event carries.
// Every ID in this package is one — see containerIDArgs, which is why.
//
// Recorded before the call it excuses, not after, because the event races the call's
// return and usually wins.
//
// The returned token is what the caller passes to forgetExpectedStop if its call
// fails. Tokens start at 1, so the zero token a skipped record returns matches
// nothing.
func (m *Runtime) expectStop(containerID string) uint64 {
	if containerID == "" {
		return 0
	}
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	// Swept here because it is the only place the map grows, and it holds at most the
	// stops of the last two minutes.
	for id, stop := range m.expectedStops {
		if now.Sub(stop.at) > expectedStopTTL {
			delete(m.expectedStops, id)
		}
	}
	m.stopToken++
	m.expectedStops[containerID] = expectedStop{token: m.stopToken, at: now}
	return m.stopToken
}

// forgetExpectedStop drops the mark for a stop that reported failure. Without this
// the container keeps running with a mark that outlives its own reason, and a real
// crash arriving within expectedStopTTL is read as the stop that never happened —
// served with no 409 and with network rules still naming the address the container
// had.
//
// A stop that failed after killing the container loses the race and is read as a
// crash instead. That is the right way round: a spurious restart report costs the
// client a retry, and on this runtime it is not even spurious, since a stopped
// container comes back from docker start without the memory it had.
//
// The token is what holds this to the caller's own mark. Nothing here serializes a
// stop against a delete or against the wake path's own cleanup — the gateway lock
// that separates stop from delete is not held over the proxy path a wake comes from
// — so another call can have recorded over this one's mark before it failed.
// Dropping that one would leave a death the runner did ask for to be read as a
// crash, and cost the client a 409 for a restart that never happened.
func (m *Runtime) forgetExpectedStop(containerID string, token uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.expectedStops[containerID].token == token {
		delete(m.expectedStops, containerID)
	}
}

// takeExpectedStop consumes one expected stop for a container. Consuming rather than
// reading is what keeps a stopped-then-restarted sandbox honest: the next death of
// the same container is a crash again.
func (m *Runtime) takeExpectedStop(containerID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	stop, ok := m.expectedStops[containerID]
	if !ok {
		return false
	}
	delete(m.expectedStops, containerID)
	return time.Since(stop.at) <= expectedStopTTL
}

// wasRestarted reports whether a sandbox's container died and Docker brought it
// back. It reads without consuming: the mark is cleared only once the runner has
// re-admitted the sandbox, so a wake that fails leaves the next request to try
// again rather than proxying into a container with stale network rules.
func (m *Runtime) wasRestarted(sandboxID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.restarted[sandboxID]
	return ok
}

// clearRestarted drops a sandbox's restart mark, which is what spends the one report
// a client gets for it. Also called when the sandbox is deleted: a crash nobody came
// back to ask about is not worth remembering.
func (m *Runtime) clearRestarted(sandboxID string) {
	m.mu.Lock()
	delete(m.restarted, sandboxID)
	m.mu.Unlock()
}

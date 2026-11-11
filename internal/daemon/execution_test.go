package daemon

import (
	"bytes"
	"context"
	"math"
	"slices"
	"sync"
	"testing"
)

func newTestExecution(events ...storedEvent) *Execution {
	ex := &Execution{
		ID:            "test",
		events:        events,
		maxEventBytes: 1 << 20,
		notify:        make(chan struct{}),
	}
	if len(events) > 0 {
		ex.minSeq = events[0].seq
		ex.nextSeq = events[len(events)-1].seq + 1
	}
	for _, e := range events {
		ex.size += int64(len(e.data))
	}
	return ex
}

func seqs(results [][]byte, ex *Execution) []uint64 {
	_ = ex // kept for API symmetry; data is self-contained
	out := make([]uint64, len(results))
	for i, e := range results {
		// storedEvent.data is the JSON-marshaled Response which contains "seq".
		// For these tests we only care about the byte identity, so find the
		// matching event by data pointer.
		for _, ev := range ex.events {
			if &ev.data[0] == &e[0] {
				out[i] = ev.seq
				break
			}
		}
	}
	return out
}

func TestEventsAfter(t *testing.T) {
	t.Parallel()

	ev := func(seq uint64) storedEvent {
		return storedEvent{seq: seq, data: []byte{byte(seq)}}
	}

	tests := []struct {
		name     string
		minSeq   uint64
		events   []storedEvent
		after    *uint64
		wantSeqs []uint64
	}{
		{
			name:     "nil after returns all",
			events:   []storedEvent{ev(0), ev(1), ev(2)},
			after:    nil,
			wantSeqs: []uint64{0, 1, 2},
		},
		{
			name:     "after first event",
			events:   []storedEvent{ev(0), ev(1), ev(2)},
			after:    new(uint64(0)),
			wantSeqs: []uint64{1, 2},
		},
		{
			name:     "after last event returns nil",
			events:   []storedEvent{ev(0), ev(1), ev(2)},
			after:    new(uint64(2)),
			wantSeqs: nil,
		},
		{
			name:     "after beyond last event returns nil",
			events:   []storedEvent{ev(0), ev(1), ev(2)},
			after:    new(uint64(5)),
			wantSeqs: nil,
		},
		{
			name:     "MaxUint64 returns nil",
			events:   []storedEvent{ev(0), ev(1), ev(2)},
			after:    new(uint64(math.MaxUint64)),
			wantSeqs: nil,
		},
		{
			name:     "trimmed events with offset minSeq",
			minSeq:   3,
			events:   []storedEvent{ev(3), ev(4), ev(5)},
			after:    new(uint64(3)),
			wantSeqs: []uint64{4, 5},
		},
		{
			name:     "after before minSeq returns all retained",
			minSeq:   3,
			events:   []storedEvent{ev(3), ev(4), ev(5)},
			after:    new(uint64(1)),
			wantSeqs: []uint64{3, 4, 5},
		},
		{
			name:     "MaxUint64 with trimmed events returns nil",
			minSeq:   5,
			events:   []storedEvent{ev(5), ev(6), ev(7)},
			after:    new(uint64(math.MaxUint64)),
			wantSeqs: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ex := newTestExecution(tt.events...)
			if tt.minSeq != 0 {
				ex.minSeq = tt.minSeq
			}

			ex.mu.Lock()
			got := ex.eventsAfter(tt.after)
			ex.mu.Unlock()

			if tt.wantSeqs == nil {
				if got != nil {
					t.Fatalf("expected nil, got %d events", len(got))
				}
				return
			}

			gotSeqs := seqs(got, ex)
			if len(gotSeqs) != len(tt.wantSeqs) {
				t.Fatalf("expected %d events, got %d", len(tt.wantSeqs), len(gotSeqs))
			}
			for i := range gotSeqs {
				if gotSeqs[i] != tt.wantSeqs[i] {
					t.Fatalf("event[%d]: expected seq %d, got %d", i, tt.wantSeqs[i], gotSeqs[i])
				}
			}
		})
	}
}

func TestHasHistoryAfter(t *testing.T) {
	t.Parallel()

	ev := func(seq uint64) storedEvent {
		return storedEvent{seq: seq, data: []byte{byte(seq)}}
	}

	tests := []struct {
		name   string
		minSeq uint64
		events []storedEvent
		after  *uint64
		want   bool
	}{
		{
			name:   "nil after with full history",
			events: []storedEvent{ev(0), ev(1)},
			after:  nil,
			want:   true,
		},
		{
			name:   "nil after with trimmed history",
			minSeq: 2,
			events: []storedEvent{ev(2), ev(3)},
			after:  nil,
			want:   false,
		},
		{
			name:   "after within retained range",
			minSeq: 2,
			events: []storedEvent{ev(2), ev(3)},
			after:  new(uint64(2)),
			want:   true,
		},
		{
			name:   "after just before minSeq",
			minSeq: 2,
			events: []storedEvent{ev(2), ev(3)},
			after:  new(uint64(1)),
			want:   true,
		},
		{
			name:   "after too far before minSeq",
			minSeq: 5,
			events: []storedEvent{ev(5), ev(6)},
			after:  new(uint64(3)),
			want:   false,
		},
		{
			name:   "MaxUint64 always has history",
			minSeq: 5,
			events: []storedEvent{ev(5), ev(6)},
			after:  new(uint64(math.MaxUint64)),
			want:   true,
		},
		{
			name:   "MaxUint64 with minSeq 0",
			events: []storedEvent{ev(0), ev(1)},
			after:  new(uint64(math.MaxUint64)),
			want:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ex := newTestExecution(tt.events...)
			if tt.minSeq != 0 {
				ex.minSeq = tt.minSeq
			}

			ex.mu.Lock()
			got := ex.hasHistoryAfter(tt.after)
			ex.mu.Unlock()

			if got != tt.want {
				t.Fatalf("expected %v, got %v", tt.want, got)
			}
		})
	}
}

func TestSnapshotMaxUint64(t *testing.T) {
	t.Parallel()

	ex := newTestExecution(
		storedEvent{seq: 0, data: []byte("a")},
		storedEvent{seq: 1, data: []byte("b")},
	)

	after := uint64(math.MaxUint64)
	events, ok := ex.Snapshot(&after)
	if !ok {
		t.Fatal("expected ok=true for MaxUint64 snapshot")
	}
	if len(events) != 0 {
		t.Fatalf("expected 0 events, got %d", len(events))
	}
}

func TestAfterNearOverflow(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		after uint64
	}{
		{name: "2^63-1", after: 1<<63 - 1},
		{name: "2^63", after: 1 << 63},
		{name: "2^64-2", after: 1<<64 - 2},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			ex := newTestExecution(
				storedEvent{seq: 0, data: []byte("a")},
				storedEvent{seq: 1, data: []byte("b")},
			)

			if !ex.HasHistory(&test.after) {
				t.Fatal("expected HasHistory=true")
			}

			events, ok := ex.Snapshot(&test.after)
			if !ok {
				t.Fatal("expected ok=true from Snapshot")
			}
			if len(events) != 0 {
				t.Fatalf("expected 0 events from Snapshot, got %d", len(events))
			}

			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			followed := 0
			if !ex.Follow(ctx, &test.after, func([]byte) { followed++ }) {
				t.Fatal("expected Follow to return true")
			}
			if followed != 0 {
				t.Fatalf("expected 0 events from Follow, got %d", followed)
			}
		})
	}
}

func FuzzEventsAfter(fuzz *testing.F) {
	fuzz.Add(uint64(0), uint64(0), uint8(3))
	fuzz.Add(uint64(4), uint64(3), uint8(3))
	fuzz.Add(uint64(1<<63-1), uint64(0), uint8(3))
	fuzz.Add(uint64(1<<63), uint64(1), uint8(3))
	fuzz.Add(uint64(1<<64-2), uint64(0), uint8(3))
	fuzz.Add(uint64(math.MaxUint64), uint64(5), uint8(0))
	fuzz.Add(uint64(3), uint64(5), uint8(3))
	fuzz.Add(uint64(4), uint64(5), uint8(3))

	fuzz.Fuzz(func(t *testing.T, after uint64, minSeq uint64, count uint8) {
		if minSeq > math.MaxUint64-uint64(count) {
			t.Skip("seqs would wrap")
		}
		events := make([]storedEvent, count)
		var wantEvents [][]byte
		for index := range events {
			events[index] = storedEvent{seq: minSeq + uint64(index), data: []byte{byte(index)}}
			if events[index].seq > after {
				wantEvents = append(wantEvents, events[index].data)
			}
		}
		ex := newTestExecution(events...)
		ex.minSeq = minSeq

		ex.mu.Lock()
		gotEvents := ex.eventsAfter(&after)
		gotHistory := ex.hasHistoryAfter(&after)
		ex.mu.Unlock()

		if !slices.EqualFunc(gotEvents, wantEvents, bytes.Equal) {
			t.Fatalf("eventsAfter: expected %v, got %v", wantEvents, gotEvents)
		}
		// The last trimmed seq is minSeq-1; history is complete unless it is after `after`.
		wantHistory := minSeq == 0 || minSeq-1 <= after
		if gotHistory != wantHistory {
			t.Fatalf("hasHistoryAfter: expected %v, got %v", wantHistory, gotHistory)
		}
	})
}

func TestAppendAndEventsAfterIntegration(t *testing.T) {
	t.Parallel()

	ex := &Execution{
		ID:            "test",
		maxEventBytes: 1 << 20,
		notify:        make(chan struct{}),
		mu:            sync.Mutex{},
	}

	ex.append(Response{Type: ResponseTypeStdout, Data: "hello"})
	ex.append(Response{Type: ResponseTypeStdout, Data: "world"})

	events, ok := ex.Snapshot(nil)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}

	after := uint64(0)
	events, ok = ex.Snapshot(&after)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event after seq 0, got %d", len(events))
	}
}

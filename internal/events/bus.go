package events

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

type subscription struct {
	filter Filter
	ch     chan Event
}

// Bus is a bounded, best-effort in-process event bus.
type Bus struct {
	nodeID string
	mu     sync.Mutex
	seq    uint64
	ring   []Event
	size   int
	subs   map[int]*subscription
	nextID int
	now    func() time.Time
}

// NewBus returns a bus retaining the last bufSize events for replay.
func NewBus(nodeID string, bufSize int) *Bus {
	if bufSize <= 0 {
		bufSize = 256
	}
	return &Bus{nodeID: nodeID, size: bufSize, subs: map[int]*subscription{}, now: time.Now}
}

// Publish assigns a seq/id/timestamp, buffers the event, and fans it out to
// matching subscribers without blocking (a full subscriber drops the event).
func (b *Bus) Publish(eventType, sandboxID string, payload any) Event {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.seq++
	var raw json.RawMessage
	if payload != nil {
		if enc, err := json.Marshal(payload); err == nil {
			raw = enc
		}
	}
	e := Event{
		ID: fmt.Sprintf("%s-%d", b.nodeID, b.seq), Seq: b.seq, TS: b.now(),
		Type: eventType, NodeID: b.nodeID, SandboxID: sandboxID, Payload: raw,
	}

	b.ring = append(b.ring, e)
	if len(b.ring) > b.size {
		b.ring = b.ring[len(b.ring)-b.size:]
	}

	for _, s := range b.subs {
		if !s.filter.matches(e) {
			continue
		}
		select {
		case s.ch <- e:
		default: // slow subscriber: drop (best-effort, ADR-0008)
		}
	}
	return e
}

// Replay returns buffered events with Seq > sinceSeq matching the filter.
func (b *Bus) Replay(f Filter, sinceSeq uint64) []Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []Event
	for _, e := range b.ring {
		if e.Seq > sinceSeq && f.matches(e) {
			out = append(out, e)
		}
	}
	return out
}

// Subscribe returns a channel of future matching events plus a cancel func.
// Buffered events after sinceSeq are NOT pushed to the channel here; callers
// that want backfill call Replay first (the SSE handler does).
func (b *Bus) Subscribe(f Filter, _ uint64) (<-chan Event, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.nextID
	b.nextID++
	s := &subscription{filter: f, ch: make(chan Event, 64)}
	b.subs[id] = s
	return s.ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if _, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(s.ch)
		}
	}
}

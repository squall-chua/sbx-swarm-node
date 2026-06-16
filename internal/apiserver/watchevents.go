package apiserver

import (
	"context"
	"sync"

	"github.com/squall-chua/sbx-swarm-node/internal/events"
	sbxv1 "github.com/squall-chua/sbx-swarm-node/internal/gen/sbxswarm/v1"
)

// ---- Merger ----------------------------------------------------------------

const mergerSeenCap = 512 // bounded seen-set to avoid unbounded growth

// Merger fans multiple event channels into one output channel, deduping by ID.
type Merger struct {
	out  chan<- events.Event
	mu   sync.Mutex
	seen []string // ring buffer of recently forwarded IDs
}

// NewMerger returns a Merger that writes deduplicated events to out.
func NewMerger(out chan events.Event) *Merger {
	return &Merger{out: out}
}

// Consume reads from ch until it is closed (or ctx is cancelled), forwarding
// events that have not been seen before (by ID) into the output channel. The
// send is guarded by ctx so that when the SSE client disconnects, this
// goroutine returns instead of blocking forever on a full/unread out channel.
func (m *Merger) Consume(ctx context.Context, ch <-chan events.Event) {
	for {
		select {
		case <-ctx.Done():
			return
		case e, ok := <-ch:
			if !ok {
				return
			}
			if m.markSeen(e.ID) {
				select {
				case m.out <- e:
				case <-ctx.Done():
					return
				}
			}
		}
	}
}

// markSeen returns true and records the id if it has not been seen before.
func (m *Merger) markSeen(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.seen {
		if s == id {
			return false
		}
	}
	m.seen = append(m.seen, id)
	if len(m.seen) > mergerSeenCap {
		m.seen = m.seen[len(m.seen)-mergerSeenCap:]
	}
	return true
}

// ---- EventService ----------------------------------------------------------

// EventService implements sbxv1.EventServiceServer, streaming events from the
// local bus.
type EventService struct {
	sbxv1.UnimplementedEventServiceServer
	bus *events.Bus
}

// NewEventService builds the EventService.
func NewEventService(bus *events.Bus) *EventService {
	return &EventService{bus: bus}
}

// WatchEvents streams matching events to the caller. It first replays buffered
// events since req.SinceSeq, then delivers live events until the context is
// cancelled.
func (s *EventService) WatchEvents(req *sbxv1.WatchRequest, stream sbxv1.EventService_WatchEventsServer) error {
	filter := events.Filter{
		Types:     req.Types,
		SandboxID: req.Sandbox,
	}

	// Backfill: events buffered before the subscription.
	for _, e := range s.bus.Replay(filter, req.SinceSeq) {
		if err := stream.Send(toEventMsg(e)); err != nil {
			return err
		}
	}

	ch, cancel := s.bus.Subscribe(filter, req.SinceSeq)
	defer cancel()

	ctx := stream.Context()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case e, ok := <-ch:
			if !ok {
				return nil
			}
			if err := stream.Send(toEventMsg(e)); err != nil {
				return err
			}
		}
	}
}

func toEventMsg(e events.Event) *sbxv1.EventMsg {
	return &sbxv1.EventMsg{
		Id:        e.ID,
		Seq:       e.Seq,
		Type:      e.Type,
		NodeId:    e.NodeID,
		SandboxId: e.SandboxID,
		Payload:   e.Payload,
	}
}

package debug

import "sync"

// eventQueue is a bounded, non-blocking event sink. When full, events
// are dropped and counted; a single overflow event carrying the drop
// count is emitted once capacity is available again. Memory and the
// pending queue stay bounded by design, and a closed queue never
// accepts further events (terminated sessions receive nothing more).
type eventQueue struct {
	ch      chan Event
	mu      sync.Mutex
	seq     int64
	dropped int
	closed  bool
}

func newEventQueue(capacity int) *eventQueue {
	if capacity <= 0 {
		capacity = 1
	}
	return &eventQueue{ch: make(chan Event, capacity)}
}

// emit appends an event without blocking. When the queue is full the
// oldest queued event is evicted to make room for an overflow notice
// carrying the running drop count, so consumers always learn about
// drops and memory stays bounded. Returns false if the event itself
// was not queued.
func (q *eventQueue) emit(typ EventType, taskID int, reason StopReason, msg string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return false
	}
	q.seq++
	ev := Event{Seq: q.seq, Type: typ, TaskID: taskID, Reason: reason, Message: msg}
	select {
	case q.ch <- ev:
		return true
	default:
	}
	// Full: count the drop, evict the oldest entry (preserving any
	// unreported drop count it carried), and enqueue an overflow
	// notice which is now guaranteed to fit.
	q.dropped++
	select {
	case old := <-q.ch:
		if old.Type == EventOverflow {
			q.dropped += old.Dropped
		}
	default:
	}
	q.seq++
	select {
	case q.ch <- Event{Seq: q.seq, Type: EventOverflow, Dropped: q.dropped}:
		q.dropped = 0
	default:
	}
	return false
}

// close marks the queue closed, discards any buffered events and
// closes the channel: a detached/terminated session receives nothing
// more, including events that were already queued.
func (q *eventQueue) close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	q.closed = true
	close(q.ch)
	// Drain buffered events; a receive on the closed channel reports
	// ok=false once the buffer is empty.
	for {
		select {
		case _, ok := <-q.ch:
			if !ok {
				return
			}
		default:
			return
		}
	}
}

// C returns the receive channel.
func (q *eventQueue) C() <-chan Event { return q.ch }

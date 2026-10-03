package debug

import (
	"context"
	"sync"
)

// Event kinds.
const (
	EvStopped    = "stopped"
	EvContinued  = "continued"
	EvTerminated = "terminated"
	EvThread     = "thread"
	EvDiagnostic = "diagnostic"
)

// Stop reasons.
const (
	ReasonBreakpoint = "breakpoint"
	ReasonStep       = "step"
	ReasonException  = "exception"
	ReasonPause      = "pause"
)

// Event is a session event. Seq is a per-session monotonic sequence number.
type Event struct {
	Seq  int
	Kind string

	// stopped
	TaskID       int
	Reason       string
	BreakpointID int
	// terminated
	Fatal     bool
	Exception string
	// thread
	ThreadStatus string // "started" | "exited"
	// diagnostic
	Message string
}

// critical reports events that must never be dropped.
func (e *Event) critical() bool { return e.Kind == EvTerminated }

// EventQueue is a bounded, ordered, closeable event buffer for one session.
// When full it drops the oldest non-critical event and counts the drop.
type EventQueue struct {
	mu      sync.Mutex
	cond    *sync.Cond
	buf     []*Event
	cap     int
	nextSeq int
	dropped int
	closed  bool
}

// NewEventQueue creates a queue with the given capacity.
func NewEventQueue(capacity int) *EventQueue {
	if capacity < 1 {
		capacity = 1
	}
	q := &EventQueue{
		buf: make([]*Event, 0, capacity),
		cap: capacity,
	}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// Emit appends an event, assigning the next sequence number.
func (q *EventQueue) Emit(e *Event) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	e.Seq = q.nextSeq
	q.nextSeq++
	q.appendLocked(e)
	q.cond.Broadcast()
}

// emitAt appends an event with an explicit sequence number (used by tests).
// Out-of-order appends are buffered in arrival order; Next always delivers
// them FIFO.
func (q *EventQueue) emitAt(e *Event) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	if e.Seq >= q.nextSeq {
		q.nextSeq = e.Seq + 1
	}
	q.appendLocked(e)
	q.cond.Broadcast()
}

func (q *EventQueue) appendLocked(e *Event) {
	if len(q.buf) >= q.cap {
		if e.critical() {
			// make room by dropping the oldest non-critical event
			for i := 0; i < len(q.buf); i++ {
				if !q.buf[i].critical() {
					q.buf = append(q.buf[:i], q.buf[i+1:]...)
					q.dropped++
					break
				}
			}
		} else {
			q.buf = q.buf[1:]
			q.dropped++
		}
	}
	q.buf = append(q.buf, e)
}

// Next returns the next event, blocking until one arrives or the context is
// done. The wait is FIFO and safe for concurrent callers.
func (q *EventQueue) Next(ctx context.Context) (*Event, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	// done is closed when Next returns so the context-watch goroutine exits.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			q.cond.Broadcast()
		case <-done:
		}
	}()

	for len(q.buf) == 0 && !q.closed {
		if ctx.Err() != nil {
			return nil, newError(KindTimeout, "event wait canceled: "+ctx.Err().Error())
		}
		q.cond.Wait()
	}
	if len(q.buf) == 0 && q.closed {
		return nil, newError(KindTerminated, "event queue closed")
	}
	// FIFO delivery: events are delivered in arrival order regardless of the
	// per-session sequence number.
	e := q.buf[0]
	q.buf = q.buf[1:]
	return e, nil
}

// Len returns the number of queued events.
func (q *EventQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.buf)
}

// Dropped returns the number of events dropped due to overflow.
func (q *EventQueue) Dropped() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.dropped
}

// Close closes the queue; waiters are woken and receive KindTerminated.
func (q *EventQueue) Close() {
	q.mu.Lock()
	q.closed = true
	q.cond.Broadcast()
	q.mu.Unlock()
}

// Drain removes and returns all currently queued events.
func (q *EventQueue) Drain() []*Event {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := q.buf
	q.buf = make([]*Event, 0, q.cap)
	return out
}

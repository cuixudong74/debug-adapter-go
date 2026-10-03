package debug

import (
	"sync"
	"time"
)

// AuditEntry records a security-relevant operation.
type AuditEntry struct {
	Time     time.Time
	User     string
	Action   string
	TargetID string
	SessionID string
	TaskID   int
	Variable string
	Granted  bool
	Reason   string
}

// AuditSink receives audit entries. Implementations must be safe for
// concurrent use.
type AuditSink interface {
	Record(AuditEntry)
}

// AuditSinkFunc adapts a function to AuditSink.
type AuditSinkFunc func(AuditEntry)

// Record implements AuditSink.
func (f AuditSinkFunc) Record(e AuditEntry) { f(e) }

// AuditLog is a bounded in-memory audit trail with an optional sink.
type AuditLog struct {
	mu      sync.Mutex
	entries []AuditEntry
	cap     int
	sink    AuditSink
}

// NewAuditLog creates an audit log retaining at most capacity entries.
func NewAuditLog(capacity int) *AuditLog {
	if capacity < 1 {
		capacity = 1
	}
	return &AuditLog{entries: make([]AuditEntry, 0, capacity), cap: capacity}
}

// SetSink installs an external sink (e.g. a logger).
func (l *AuditLog) SetSink(s AuditSink) {
	l.mu.Lock()
	l.sink = s
	l.mu.Unlock()
}

// Record appends an entry, dropping the oldest when full.
func (l *AuditLog) Record(e AuditEntry) {
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	l.mu.Lock()
	if len(l.entries) >= l.cap {
		l.entries = l.entries[1:]
	}
	l.entries = append(l.entries, e)
	sink := l.sink
	l.mu.Unlock()
	if sink != nil {
		sink.Record(e)
	}
}

// Entries returns a copy of all retained entries.
func (l *AuditLog) Entries() []AuditEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]AuditEntry, len(l.entries))
	copy(out, l.entries)
	return out
}

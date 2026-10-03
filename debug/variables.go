package debug

import (
	"context"
	"sync"

	"github.com/cuixudong74/debug-adapter-go/runtime"
)

type refKind int

const (
	refObject refKind = iota
	refArray
	refLocals
)

// refEntry is a variablesReference handle. References are only valid while the
// session stays paused; resuming invalidates all references.
type refEntry struct {
	kind  refKind
	value *runtime.Value // object/array
	frame *runtime.Frame // locals
}

// Variable is the client-facing presentation of a variable.
type Variable struct {
	Name               string `json:"name"`
	Value              string `json:"value"`
	Type               string `json:"type"`
	VariablesReference int    `json:"variablesReference"`
	Sensitive          bool   `json:"sensitive"`
}

// refTable maps integer references to runtime values.
type refTable struct {
	mu      sync.Mutex
	entries map[int]*refEntry
	next    int
}

func newRefTable() *refTable {
	return &refTable{entries: map[int]*refEntry{}, next: 1000}
}

func (t *refTable) put(e *refEntry) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	id := t.next
	t.next++
	t.entries[id] = e
	return id
}

func (t *refTable) get(id int) (*refEntry, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.entries[id]
	return e, ok
}

func (t *refTable) clear() {
	t.mu.Lock()
	t.entries = map[int]*refEntry{}
	t.mu.Unlock()
}

// sensitive reports whether a field requires elevated permission to read.
func fieldSensitive(f runtime.Field) bool {
	return f.Sensitive || IsSensitiveName(f.Name)
}

// Variables returns the children of a variablesReference.
func (s *Session) Variables(ctx context.Context, ref int) ([]Variable, error) {
	if err := ctx.Err(); err != nil {
		return nil, newError(KindCanceled, "variables canceled: "+err.Error())
	}
	s.mu.Lock()
	state := s.state
	s.mu.Unlock()
	if state != statePaused {
		return nil, newError(KindInvalidState, "variables only available while paused")
	}
	if !s.user.HasScope(ScopeReadVariables) {
		s.audit(AuditEntry{
			User: s.user.ID, Action: "variables:read", SessionID: s.id,
			TargetID: s.targetID, Granted: false,
			Reason: "missing scope variables:read",
		})
		return nil, newError(KindPermission, "variables denied: missing scope variables:read")
	}
	e, ok := s.refs.get(ref)
	if !ok {
		return nil, newError(KindNotFound, "unknown variablesReference")
	}

	check := func(name string, sensitive bool) error {
		if sensitive && !s.user.HasScope(ScopeReadSensitive) {
			s.audit(AuditEntry{
				User: s.user.ID, Action: "variables:read", SessionID: s.id,
				TargetID: s.targetID, Variable: name, Granted: false,
				Reason: "missing scope variables:read_sensitive",
			})
			return newError(KindPermission, "read of sensitive variable denied: "+name)
		}
		s.audit(AuditEntry{
			User: s.user.ID, Action: "variables:read", SessionID: s.id,
			TargetID: s.targetID, Variable: name, Granted: true,
		})
		return nil
	}

	var out []Variable
	switch e.kind {
	case refLocals:
		names := make([]string, 0, len(e.frame.Locals))
		for n := range e.frame.Locals {
			names = append(names, n)
		}
		sortStrings(names)
		for _, n := range names {
			v := e.frame.Locals[n]
			sens := IsSensitiveName(n)
			if err := check(n, sens); err != nil {
				return nil, err
			}
			out = append(out, s.makeVar(n, v, sens))
		}
	case refObject:
		for _, f := range e.value.Object.Fields {
			sens := fieldSensitive(f)
			if err := check(f.Name, sens); err != nil {
				return nil, err
			}
			out = append(out, s.makeVar(f.Name, f.Value, sens))
		}
	case refArray:
		for i, v := range e.value.Array.Items {
			name := indexName(i)
			if err := check(name, false); err != nil {
				return nil, err
			}
			out = append(out, s.makeVar(name, v, false))
		}
	}
	return out, nil
}

func (s *Session) makeVar(name string, v *runtime.Value, sensitive bool) Variable {
	ref := 0
	if v != nil && (v.Kind == runtime.KindObject || v.Kind == runtime.KindArray) {
		ref = s.refs.put(&refEntry{kind: refObject, value: v})
	}
	return Variable{
		Name:               name,
		Value:              v.Display(),
		Type:               v.TypeName(),
		VariablesReference: ref,
		Sensitive:          sensitive,
	}
}

func indexName(i int) string {
	return "[" + itoa(i) + "]"
}

// sortStrings sorts a slice in place (small helper to avoid importing sort
// just for one call site).
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}

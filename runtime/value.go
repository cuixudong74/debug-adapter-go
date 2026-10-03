// Package runtime implements a small, deterministic script runtime that runs
// entirely in-process. Execution units (tasks) advance one source line at a
// time and expose a Hook interface that the debug service uses to observe and
// control execution.
package runtime

import (
	"fmt"
	"strconv"
	"strings"
)

// ValueKind identifies the kind of a runtime value.
type ValueKind int

const (
	KindNil ValueKind = iota
	KindInt
	KindFloat
	KindBool
	KindString
	KindObject
	KindArray
	KindFunc
)

// Value is a runtime value. Values are immutable from the debugger's point of
// view: evaluation never mutates them.
type Value struct {
	Kind   ValueKind
	Int    int64
	Float  float64
	Str    string
	Bool   bool
	Object *ObjectData
	Array  *ArrayData
	Func   *FuncDef
}

// ObjectData is an ordered map of named fields.
type ObjectData struct {
	Fields []Field
}

// Field is a single object field. Sensitive marks fields whose reads require
// elevated permission.
type Field struct {
	Name      string
	Value     *Value
	Sensitive bool
}

// ArrayData is an ordered sequence of values.
type ArrayData struct {
	Items []*Value
}

// ---- constructors ----

func NilValue() *Value          { return &Value{Kind: KindNil} }
func IntValue(v int64) *Value   { return &Value{Kind: KindInt, Int: v} }
func FloatValue(v float64) *Value { return &Value{Kind: KindFloat, Float: v} }
func BoolValue(v bool) *Value   { return &Value{Kind: KindBool, Bool: v} }
func StringValue(v string) *Value { return &Value{Kind: KindString, Str: v} }

func ObjectValue(fields ...Field) *Value {
	return &Value{Kind: KindObject, Object: &ObjectData{Fields: fields}}
}

func ArrayValue(items ...*Value) *Value {
	return &Value{Kind: KindArray, Array: &ArrayData{Items: items}}
}

// SensitiveField returns a field marked as sensitive.
func SensitiveField(name string, v *Value) Field {
	return Field{Name: name, Value: v, Sensitive: true}
}

// FieldOf looks up a field by name.
func (o *ObjectData) FieldOf(name string) (*Value, bool) {
	for _, f := range o.Fields {
		if f.Name == name {
			return f.Value, true
		}
	}
	return nil, false
}

// FieldAt returns the i-th field.
func (o *ObjectData) FieldAt(i int) (Field, bool) {
	if i < 0 || i >= len(o.Fields) {
		return Field{}, false
	}
	return o.Fields[i], true
}

// Len returns the number of fields.
func (o *ObjectData) Len() int { return len(o.Fields) }

// ElemAt returns the i-th array element.
func (a *ArrayData) ElemAt(i int) (*Value, bool) {
	if i < 0 || i >= len(a.Items) {
		return nil, false
	}
	return a.Items[i], true
}

// Len returns the array length.
func (a *ArrayData) Len() int { return len(a.Items) }

// IsTruthy reports the boolean truthiness used by conditional breakpoints.
func (v *Value) IsTruthy() bool {
	if v == nil {
		return false
	}
	switch v.Kind {
	case KindNil:
		return false
	case KindBool:
		return v.Bool
	case KindInt:
		return v.Int != 0
	case KindFloat:
		return v.Float != 0
	case KindString:
		return len(v.Str) > 0
	default:
		return true
	}
}

// Display renders a short, human-readable representation without ever
// dereferencing into mutable state.
func (v *Value) Display() string {
	if v == nil {
		return "nil"
	}
	switch v.Kind {
	case KindNil:
		return "nil"
	case KindInt:
		return strconv.FormatInt(v.Int, 10)
	case KindFloat:
		return strconv.FormatFloat(v.Float, 'g', -1, 64)
	case KindBool:
		return strconv.FormatBool(v.Bool)
	case KindString:
		return strconv.Quote(v.Str)
	case KindObject:
		var b strings.Builder
		b.WriteString("{")
		for i, f := range v.Object.Fields {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(f.Name)
			b.WriteString(": ")
			b.WriteString(f.Value.Display())
		}
		b.WriteString("}")
		return b.String()
	case KindArray:
		var b strings.Builder
		b.WriteString("[")
		for i, it := range v.Array.Items {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(it.Display())
		}
		b.WriteString("]")
		return b.String()
	case KindFunc:
		return fmt.Sprintf("func %s", v.Func.Name)
	}
	return "?"
}

// TypeName returns the type name reported to clients.
func (v *Value) TypeName() string {
	if v == nil {
		return "nil"
	}
	switch v.Kind {
	case KindNil:
		return "nil"
	case KindInt:
		return "int"
	case KindFloat:
		return "float"
	case KindBool:
		return "bool"
	case KindString:
		return "string"
	case KindObject:
		return "object"
	case KindArray:
		return "array"
	case KindFunc:
		return "func"
	}
	return "unknown"
}

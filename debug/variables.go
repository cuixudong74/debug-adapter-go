package debug

import (
	"fmt"
	"sort"
	"strings"
)

// SensitivePrefix marks access-controlled variable names.
const SensitivePrefix = "secret_"

// isSensitiveName reports whether a variable name is access-controlled
// (names with the "secret_" prefix require the admin role).
func isSensitiveName(name string) bool {
	return strings.HasPrefix(name, SensitivePrefix)
}

// renderValue renders a value without mutating it. Nested structures
// are not expanded (lazy): callers drill in via Variables with a path.
func renderValue(v any) (valStr, typeStr string, hasChildren bool) {
	switch x := v.(type) {
	case nil:
		return "nil", "nil", false
	case bool:
		return fmt.Sprintf("%v", x), "bool", false
	case string:
		return fmt.Sprintf("%q", x), "string", false
	case int, int64, float64:
		return fmt.Sprintf("%v", x), "number", false
	case map[string]any:
		return fmt.Sprintf("map[%d]", len(x)), "map", true
	case []any:
		return fmt.Sprintf("list[%d]", len(x)), "list", true
	default:
		return fmt.Sprintf("%v", x), fmt.Sprintf("%T", x), false
	}
}

// childrenOf lists the immediate children of a nested value. Keys are
// sorted for deterministic output. Reading is side-effect-free.
func childrenOf(v any) []Variable {
	switch coll := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(coll))
		for k := range coll {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make([]Variable, 0, len(keys))
		for _, k := range keys {
			out = append(out, makeVariable(k, coll[k]))
		}
		return out
	case []any:
		out := make([]Variable, 0, len(coll))
		for i, item := range coll {
			out = append(out, makeVariable(fmt.Sprintf("[%d]", i), item))
		}
		return out
	}
	return nil
}

func makeVariable(name string, v any) Variable {
	val, typ, kids := renderValue(v)
	return Variable{
		Name:        name,
		Value:       val,
		Type:        typ,
		HasChildren: kids,
		Sensitive:   isSensitiveName(name),
	}
}

// resolvePath walks a nested value along path without mutating it.
func resolvePath(root any, path []string) (any, error) {
	cur := root
	for _, seg := range path {
		next, err := indexValue(cur, seg)
		if err != nil {
			return nil, err
		}
		cur = next
	}
	return cur, nil
}

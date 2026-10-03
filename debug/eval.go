package debug

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/cuixudong74/debug-adapter-go/runtime"
)

// EvalScope resolves identifiers during evaluation.
type EvalScope interface {
	Lookup(name string) (*runtime.Value, bool)
}

// mapScope is a simple scope backed by maps.
type mapScope struct {
	maps []map[string]*runtime.Value
}

func (s mapScope) Lookup(name string) (*runtime.Value, bool) {
	for _, m := range s.maps {
		if v, ok := m[name]; ok {
			return v, true
		}
	}
	return nil, false
}

// token kinds
type tokenKind int

const (
	tkEOF tokenKind = iota
	tkNumber
	tkString
	tkIdent
	tkOp
	tkPunct
)

type token struct {
	kind tokenKind
	val  string
	pos  int
}

type lexer struct {
	src string
	pos int
}

func (l *lexer) peek() byte {
	if l.pos >= len(l.src) {
		return 0
	}
	return l.src[l.pos]
}

func (l *lexer) next() token {
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			l.pos++
			continue
		}
		start := l.pos
		if unicode.IsDigit(rune(c)) {
			for l.pos < len(l.src) && (unicode.IsDigit(rune(l.src[l.pos])) || l.src[l.pos] == '.') {
				l.pos++
			}
			return token{tkNumber, l.src[start:l.pos], start}
		}
		if c == '"' {
			l.pos++
			var b strings.Builder
			for l.pos < len(l.src) && l.src[l.pos] != '"' {
				if l.src[l.pos] == '\\' && l.pos+1 < len(l.src) {
					l.pos++
					switch l.src[l.pos] {
					case 'n':
						b.WriteByte('\n')
					case 't':
						b.WriteByte('\t')
					case '"':
						b.WriteByte('"')
					case '\\':
						b.WriteByte('\\')
					default:
						b.WriteByte(l.src[l.pos])
					}
					l.pos++
					continue
				}
				b.WriteByte(l.src[l.pos])
				l.pos++
			}
			if l.pos >= len(l.src) {
				return token{tkString, b.String(), start}
			}
			l.pos++ // closing quote
			return token{tkString, b.String(), start}
		}
		if unicode.IsLetter(rune(c)) || c == '_' {
			for l.pos < len(l.src) && (unicode.IsLetter(rune(l.src[l.pos])) || unicode.IsDigit(rune(l.src[l.pos])) || l.src[l.pos] == '_') {
				l.pos++
			}
			return token{tkIdent, l.src[start:l.pos], start}
		}
		// operators / punctuation
		two := ""
		if l.pos+1 < len(l.src) {
			two = l.src[l.pos : l.pos+2]
		}
		switch two {
		case "==", "!=", "<=", ">=", "&&", "||":
			l.pos += 2
			return token{tkOp, two, start}
		}
		switch c {
		case '+', '-', '*', '/', '%', '<', '>', '!':
			l.pos++
			return token{tkOp, string(c), start}
		case '(', ')', '.', '[', ']':
			l.pos++
			return token{tkPunct, string(c), start}
		}
		return token{tkOp, string(c), start}
	}
	return token{tkEOF, "", l.pos}
}

type parser struct {
	lex    *lexer
	cur    token
	scope  EvalScope
	locals map[string]*runtime.Value // for error context
}

func newParser(src string, scope EvalScope) *parser {
	p := &parser{lex: &lexer{src: src}, scope: scope}
	p.cur = p.lex.next()
	return p
}

func (p *parser) advance() { p.cur = p.lex.next() }

func (p *parser) parse() (*runtime.Value, error) {
	v, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	if p.cur.kind != tkEOF {
		return nil, p.parseErr("unexpected token %q", p.cur.val)
	}
	return v, nil
}

func (p *parser) parseOr() (*runtime.Value, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.cur.kind == tkOp && p.cur.val == "||" {
		p.advance()
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left = runtime.BoolValue(left.IsTruthy() || right.IsTruthy())
	}
	return left, nil
}

func (p *parser) parseAnd() (*runtime.Value, error) {
	left, err := p.parseCmp()
	if err != nil {
		return nil, err
	}
	for p.cur.kind == tkOp && p.cur.val == "&&" {
		p.advance()
		right, err := p.parseCmp()
		if err != nil {
			return nil, err
		}
		left = runtime.BoolValue(left.IsTruthy() && right.IsTruthy())
	}
	return left, nil
}

func (p *parser) parseCmp() (*runtime.Value, error) {
	left, err := p.parseAdd()
	if err != nil {
		return nil, err
	}
	if p.cur.kind == tkOp && isCmpOp(p.cur.val) {
		op := p.cur.val
		p.advance()
		right, err := p.parseAdd()
		if err != nil {
			return nil, err
		}
		b, err := compare(left, right, op)
		if err != nil {
			return nil, err
		}
		return runtime.BoolValue(b), nil
	}
	return left, nil
}

func isCmpOp(s string) bool {
	switch s {
	case "==", "!=", "<", "<=", ">", ">=":
		return true
	}
	return false
}

func (p *parser) parseAdd() (*runtime.Value, error) {
	left, err := p.parseMul()
	if err != nil {
		return nil, err
	}
	for p.cur.kind == tkOp && (p.cur.val == "+" || p.cur.val == "-") {
		op := p.cur.val
		p.advance()
		right, err := p.parseMul()
		if err != nil {
			return nil, err
		}
		left, err = arith(left, right, op)
		if err != nil {
			return nil, err
		}
	}
	return left, nil
}

func (p *parser) parseMul() (*runtime.Value, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for p.cur.kind == tkOp && (p.cur.val == "*" || p.cur.val == "/" || p.cur.val == "%") {
		op := p.cur.val
		p.advance()
		right, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		left, err = arith(left, right, op)
		if err != nil {
			return nil, err
		}
	}
	return left, nil
}

func (p *parser) parseUnary() (*runtime.Value, error) {
	if p.cur.kind == tkOp && (p.cur.val == "!" || p.cur.val == "-") {
		op := p.cur.val
		p.advance()
		v, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		if op == "!" {
			return runtime.BoolValue(!v.IsTruthy()), nil
		}
		// unary minus
		switch v.Kind {
		case runtime.KindInt:
			return runtime.IntValue(-v.Int), nil
		case runtime.KindFloat:
			return runtime.FloatValue(-v.Float), nil
		default:
			return nil, p.evalErr("type", "unary minus on %s", v.TypeName())
		}
	}
	return p.parsePostfix()
}

func (p *parser) parsePostfix() (*runtime.Value, error) {
	v, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}
	for {
		if p.cur.kind == tkPunct && p.cur.val == "." {
			p.advance()
			if p.cur.kind != tkIdent {
				return nil, p.parseErr("expected field name after '.'")
			}
			name := p.cur.val
			p.advance()
			if v.Kind != runtime.KindObject {
				return nil, p.evalErr("type", "cannot read field %q from %s", name, v.TypeName())
			}
			fv, ok := v.Object.FieldOf(name)
			if !ok {
				return nil, p.evalErr("notFound", "no field %q", name)
			}
			v = fv
			continue
		}
		if p.cur.kind == tkPunct && p.cur.val == "[" {
			p.advance()
			idx, err := p.parseOr()
			if err != nil {
				return nil, err
			}
			if p.cur.kind != tkPunct || p.cur.val != "]" {
				return nil, p.parseErr("expected ']'")
			}
			p.advance()
			if v.Kind != runtime.KindArray {
				return nil, p.evalErr("type", "cannot index %s", v.TypeName())
			}
			if idx.Kind != runtime.KindInt {
				return nil, p.evalErr("type", "index must be int, got %s", idx.TypeName())
			}
			el, ok := v.Array.ElemAt(int(idx.Int))
			if !ok {
				return nil, p.evalErr("notFound", "index %d out of range (len %d)", idx.Int, v.Array.Len())
			}
			v = el
			continue
		}
		break
	}
	return v, nil
}

func (p *parser) parsePrimary() (*runtime.Value, error) {
	switch p.cur.kind {
	case tkNumber:
		raw := p.cur.val
		p.advance()
		if strings.Contains(raw, ".") {
			f, err := strconv.ParseFloat(raw, 64)
			if err != nil {
				return nil, p.parseErr("invalid number %q", raw)
			}
			return runtime.FloatValue(f), nil
		}
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, p.parseErr("invalid number %q", raw)
		}
		return runtime.IntValue(n), nil
	case tkString:
		raw := p.cur.val
		p.advance()
		return runtime.StringValue(raw), nil
	case tkIdent:
		name := p.cur.val
		p.advance()
		switch name {
		case "true":
			return runtime.BoolValue(true), nil
		case "false":
			return runtime.BoolValue(false), nil
		case "nil":
			return runtime.NilValue(), nil
		}
		v, ok := p.scope.Lookup(name)
		if !ok {
			return nil, p.evalErr("notFound", "name %q not found", name)
		}
		return v, nil
	case tkPunct:
		if p.cur.val == "(" {
			p.advance()
			v, err := p.parseOr()
			if err != nil {
				return nil, err
			}
			if p.cur.kind != tkPunct || p.cur.val != ")" {
				return nil, p.parseErr("expected ')'")
			}
			p.advance()
			return v, nil
		}
	}
	return nil, p.parseErr("unexpected token %q", p.cur.val)
}

func (p *parser) parseErr(format string, args ...any) error {
	return &Error{Kind: KindEval, EvalKind: "parse", Msg: fmt.Sprintf(format, args...)}
}

func (p *parser) evalErr(kind, format string, args ...any) error {
	return &Error{Kind: KindEval, EvalKind: kind, Msg: fmt.Sprintf(format, args...)}
}

func arith(a, b *runtime.Value, op string) (*runtime.Value, error) {
	if a.Kind == runtime.KindString && b.Kind == runtime.KindString && op == "+" {
		return runtime.StringValue(a.Str + b.Str), nil
	}
	if a.Kind == runtime.KindInt && b.Kind == runtime.KindInt {
		switch op {
		case "+":
			return runtime.IntValue(a.Int + b.Int), nil
		case "-":
			return runtime.IntValue(a.Int - b.Int), nil
		case "*":
			return runtime.IntValue(a.Int * b.Int), nil
		case "/":
			if b.Int == 0 {
				return nil, &Error{Kind: KindEval, EvalKind: "type", Msg: "division by zero"}
			}
			return runtime.IntValue(a.Int / b.Int), nil
		case "%":
			if b.Int == 0 {
				return nil, &Error{Kind: KindEval, EvalKind: "type", Msg: "modulo by zero"}
			}
			return runtime.IntValue(a.Int % b.Int), nil
		}
	}
	if a.Kind == runtime.KindFloat || b.Kind == runtime.KindFloat {
		af, bf := toFloat(a), toFloat(b)
		switch op {
		case "+":
			return runtime.FloatValue(af + bf), nil
		case "-":
			return runtime.FloatValue(af - bf), nil
		case "*":
			return runtime.FloatValue(af * bf), nil
		case "/":
			if bf == 0 {
				return nil, &Error{Kind: KindEval, EvalKind: "type", Msg: "division by zero"}
			}
			return runtime.FloatValue(af / bf), nil
		}
	}
	return nil, &Error{Kind: KindEval, EvalKind: "type", Msg: fmt.Sprintf("unsupported operand types for %s: %s and %s", op, a.TypeName(), b.TypeName())}
}

func toFloat(v *runtime.Value) float64 {
	if v.Kind == runtime.KindFloat {
		return v.Float
	}
	if v.Kind == runtime.KindInt {
		return float64(v.Int)
	}
	return 0
}

func compare(a, b *runtime.Value, op string) (bool, error) {
	switch op {
	case "==":
		return equal(a, b), nil
	case "!=":
		return !equal(a, b), nil
	}
	if a.Kind == runtime.KindInt && b.Kind == runtime.KindInt {
		return cmpInt(a.Int, b.Int, op), nil
	}
	if a.Kind == runtime.KindFloat || b.Kind == runtime.KindFloat {
		af, bf := toFloat(a), toFloat(b)
		return cmpFloat(af, bf, op), nil
	}
	if a.Kind == runtime.KindString && b.Kind == runtime.KindString {
		return cmpString(a.Str, b.Str, op), nil
	}
	return false, &Error{Kind: KindEval, EvalKind: "type", Msg: fmt.Sprintf("cannot compare %s and %s with %s", a.TypeName(), b.TypeName(), op)}
}

func equal(a, b *runtime.Value) bool {
	if a.Kind != b.Kind {
		return false
	}
	switch a.Kind {
	case runtime.KindNil:
		return true
	case runtime.KindBool:
		return a.Bool == b.Bool
	case runtime.KindInt:
		return a.Int == b.Int
	case runtime.KindFloat:
		return a.Float == b.Float
	case runtime.KindString:
		return a.Str == b.Str
	}
	return false
}

func cmpInt(a int64, b int64, op string) bool {
	switch op {
	case "<":
		return a < b
	case "<=":
		return a <= b
	case ">":
		return a > b
	case ">=":
		return a >= b
	}
	return false
}

func cmpFloat(a, b float64, op string) bool {
	switch op {
	case "<":
		return a < b
	case "<=":
		return a <= b
	case ">":
		return a > b
	case ">=":
		return a >= b
	}
	return false
}

func cmpString(a, b string, op string) bool {
	switch op {
	case "<":
		return a < b
	case "<=":
		return a <= b
	case ">":
		return a > b
	case ">=":
		return a >= b
	}
	return false
}

// EvalExpression parses and evaluates src against the given scope. Evaluation
// is side-effect free: there are no assignments or calls.
func EvalExpression(src string, scope EvalScope) (*runtime.Value, error) {
	p := newParser(src, scope)
	return p.parse()
}

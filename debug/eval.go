package debug

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// evalExpr evaluates a small side-effect-free expression language:
//
//	expr    := or
//	or      := and ("||" and)*
//	and     := cmp ("&&" cmp)*
//	cmp     := add (("=="|"!="|"<"|"<="|">"|">=") add)?
//	add     := mul (("+"|"-") mul)*
//	mul     := unary (("*"|"/"|"%") unary)*
//	unary   := ("!"|"-") unary | postfix
//	postfix := primary ("." ident | "[" expr "]")*
//	primary := number | string | bool | nil | ident | "(" expr ")"
//
// The scope is only ever read, never mutated; there are no calls, so
// evaluation cannot change the debugged program's semantic state.
// Cancellation is checked at every node so timeouts cannot leave the
// session in an intermediate state.
func evalExpr(ctx context.Context, scope map[string]any, expr string) (any, error) {
	p := &parser{src: expr, scope: scope, ctx: ctx}
	v, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	if err := p.checkCtx(); err != nil {
		return nil, err
	}
	p.skipWS()
	if p.pos < len(p.src) {
		return nil, fmt.Errorf("debug: unexpected trailing input %q", p.src[p.pos:])
	}
	return v, nil
}

type parser struct {
	src   string
	pos   int
	scope map[string]any
	ctx   context.Context
}

func (p *parser) checkCtx() error {
	select {
	case <-p.ctx.Done():
		return p.ctx.Err()
	default:
		return nil
	}
}

func (p *parser) skipWS() {
	for p.pos < len(p.src) && (p.src[p.pos] == ' ' || p.src[p.pos] == '\t') {
		p.pos++
	}
}

func (p *parser) peek() string {
	p.skipWS()
	return p.src[p.pos:]
}

func (p *parser) eat(s string) bool {
	if strings.HasPrefix(p.peek(), s) {
		p.pos += len(s)
		return true
	}
	return false
}

func (p *parser) parseOr() (any, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.eat("||") {
		if err := p.checkCtx(); err != nil {
			return nil, err
		}
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left = truthyVal(left) || truthyVal(right)
	}
	return left, nil
}

func (p *parser) parseAnd() (any, error) {
	left, err := p.parseCmp()
	if err != nil {
		return nil, err
	}
	for p.eat("&&") {
		if err := p.checkCtx(); err != nil {
			return nil, err
		}
		right, err := p.parseCmp()
		if err != nil {
			return nil, err
		}
		left = truthyVal(left) && truthyVal(right)
	}
	return left, nil
}

func (p *parser) parseCmp() (any, error) {
	left, err := p.parseAdd()
	if err != nil {
		return nil, err
	}
	for _, op := range []string{"==", "!=", "<=", ">=", "<", ">"} {
		if p.eat(op) {
			right, err := p.parseAdd()
			if err != nil {
				return nil, err
			}
			return compare(op, left, right)
		}
	}
	return left, nil
}

func (p *parser) parseAdd() (any, error) {
	left, err := p.parseMul()
	if err != nil {
		return nil, err
	}
	for {
		if p.eat("+") {
			right, err := p.parseMul()
			if err != nil {
				return nil, err
			}
			left, err = arithEval("+", left, right)
			if err != nil {
				return nil, err
			}
		} else if p.eat("-") {
			right, err := p.parseMul()
			if err != nil {
				return nil, err
			}
			left, err = arithEval("-", left, right)
			if err != nil {
				return nil, err
			}
		} else {
			return left, nil
		}
	}
}

func (p *parser) parseMul() (any, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for {
		if p.eat("*") {
			right, err := p.parseUnary()
			if err != nil {
				return nil, err
			}
			left, err = arithEval("*", left, right)
			if err != nil {
				return nil, err
			}
		} else if p.eat("/") {
			right, err := p.parseUnary()
			if err != nil {
				return nil, err
			}
			left, err = arithEval("/", left, right)
			if err != nil {
				return nil, err
			}
		} else if p.eat("%") {
			right, err := p.parseUnary()
			if err != nil {
				return nil, err
			}
			left, err = arithEval("%", left, right)
			if err != nil {
				return nil, err
			}
		} else {
			return left, nil
		}
	}
}

func (p *parser) parseUnary() (any, error) {
	if p.eat("!") {
		v, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return !truthyVal(v), nil
	}
	if p.eat("-") {
		v, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		f, ok := toFloat(v)
		if !ok {
			return nil, fmt.Errorf("debug: cannot negate %v", v)
		}
		return -f, nil
	}
	return p.parsePostfix()
}

func (p *parser) parsePostfix() (any, error) {
	v, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}
	for {
		if p.eat(".") {
			name, err := p.parseIdent()
			if err != nil {
				return nil, err
			}
			v, err = indexValue(v, name)
			if err != nil {
				return nil, err
			}
		} else if p.eat("[") {
			idx, err := p.parseOr()
			if err != nil {
				return nil, err
			}
			if !p.eat("]") {
				return nil, fmt.Errorf("debug: missing ]")
			}
			v, err = indexValue(v, idx)
			if err != nil {
				return nil, err
			}
		} else {
			return v, nil
		}
	}
}

func (p *parser) parsePrimary() (any, error) {
	if err := p.checkCtx(); err != nil {
		return nil, err
	}
	rest := p.peek()
	if rest == "" {
		return nil, fmt.Errorf("debug: unexpected end of expression")
	}
	if p.eat("(") {
		v, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		if !p.eat(")") {
			return nil, fmt.Errorf("debug: missing )")
		}
		return v, nil
	}
	if p.eat("true") {
		return true, nil
	}
	if p.eat("false") {
		return false, nil
	}
	if p.eat("nil") {
		return nil, nil
	}
	if rest[0] == '"' || rest[0] == '\'' {
		return p.parseString()
	}
	if c := rest[0]; c >= '0' && c <= '9' {
		return p.parseNumber()
	}
	name, err := p.parseIdent()
	if err != nil {
		return nil, err
	}
	v, ok := p.scope[name]
	if !ok {
		return nil, fmt.Errorf("debug: %w: %q", ErrUndefinedSymbol, name)
	}
	return v, nil
}

func (p *parser) parseIdent() (string, error) {
	p.skipWS()
	start := p.pos
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || p.pos > start && c >= '0' && c <= '9' {
			p.pos++
		} else {
			break
		}
	}
	if p.pos == start {
		return "", fmt.Errorf("debug: expected identifier near %q", p.src[p.pos:])
	}
	return p.src[start:p.pos], nil
}

func (p *parser) parseNumber() (any, error) {
	p.skipWS()
	start := p.pos
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c >= '0' && c <= '9' || c == '.' {
			p.pos++
		} else {
			break
		}
	}
	f, err := strconv.ParseFloat(p.src[start:p.pos], 64)
	if err != nil {
		return nil, fmt.Errorf("debug: bad number: %v", err)
	}
	return f, nil
}

func (p *parser) parseString() (any, error) {
	p.skipWS()
	quote := p.src[p.pos]
	p.pos++
	var sb strings.Builder
	for p.pos < len(p.src) && p.src[p.pos] != quote {
		if p.src[p.pos] == '\\' && p.pos+1 < len(p.src) {
			p.pos++
		}
		sb.WriteByte(p.src[p.pos])
		p.pos++
	}
	if p.pos >= len(p.src) {
		return nil, fmt.Errorf("debug: unterminated string")
	}
	p.pos++
	return sb.String(), nil
}

// indexValue reads v[key] from maps/slices without mutating anything.
func indexValue(v, key any) (any, error) {
	switch coll := v.(type) {
	case map[string]any:
		ks, ok := key.(string)
		if !ok {
			return nil, fmt.Errorf("debug: map key must be a string, got %v", key)
		}
		val, ok := coll[ks]
		if !ok {
			return nil, fmt.Errorf("debug: %w: key %q", ErrUndefinedSymbol, ks)
		}
		return val, nil
	case []any:
		f, ok := toFloat(key)
		if !ok {
			return nil, fmt.Errorf("debug: index must be a number, got %v", key)
		}
		i := int(f)
		if i < 0 || i >= len(coll) {
			return nil, fmt.Errorf("debug: index %d out of range", i)
		}
		return coll[i], nil
	}
	return nil, fmt.Errorf("debug: cannot index %T", v)
}

func arithEval(op string, a, b any) (any, error) {
	fa, oka := toFloat(a)
	fb, okb := toFloat(b)
	if !oka || !okb {
		return nil, fmt.Errorf("debug: arithmetic on non-numbers %v, %v", a, b)
	}
	switch op {
	case "+":
		return fa + fb, nil
	case "-":
		return fa - fb, nil
	case "*":
		return fa * fb, nil
	case "/":
		if fb == 0 {
			return nil, fmt.Errorf("debug: division by zero")
		}
		return fa / fb, nil
	case "%":
		if fb == 0 {
			return nil, fmt.Errorf("debug: division by zero")
		}
		return float64(int64(fa) % int64(fb)), nil
	}
	return nil, fmt.Errorf("debug: bad operator %q", op)
}

func compare(op string, a, b any) (any, error) {
	if op == "==" || op == "!=" {
		eq := fmt.Sprint(a) == fmt.Sprint(b)
		if op == "!=" {
			eq = !eq
		}
		return eq, nil
	}
	fa, oka := toFloat(a)
	fb, okb := toFloat(b)
	if !oka || !okb {
		return nil, fmt.Errorf("debug: comparison on non-numbers %v, %v", a, b)
	}
	switch op {
	case "<":
		return fa < fb, nil
	case "<=":
		return fa <= fb, nil
	case ">":
		return fa > fb, nil
	case ">=":
		return fa >= fb, nil
	}
	return nil, fmt.Errorf("debug: bad operator %q", op)
}

func toFloat(x any) (float64, bool) {
	switch n := x.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case float64:
		return n, true
	}
	return 0, false
}

func truthyVal(x any) bool {
	switch v := x.(type) {
	case nil:
		return false
	case bool:
		return v
	}
	if f, ok := toFloat(x); ok {
		return f != 0
	}
	return true
}

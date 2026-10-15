package toolscript

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/dop251/goja/ast"
	"github.com/dop251/goja/token"
)

// errShortCircuit unwinds an optional chain (a?.b) whose base is nullish.
// It never escapes the enclosing OptionalChain node.
var errShortCircuit = errors.New("optional chain short-circuit")

func constant(v any) evalFn { return func(*rt, *scope) (any, error) { return v, nil } }

// Static member names that would reach prototype machinery in JavaScript.
func unsafeKey(k string) bool {
	switch k {
	case "__proto__", "prototype", "constructor", "__defineGetter__", "__defineSetter__", "__lookupGetter__", "__lookupSetter__", "caller", "callee", "arguments":
		return true
	}
	return false
}

func propertyKey(n ast.Expression) (string, bool) {
	switch n := n.(type) {
	case *ast.Identifier:
		return n.Name.String(), true
	case *ast.StringLiteral:
		return n.Value.String(), true
	case *ast.NumberLiteral:
		f, ok := numberLiteral(n)
		if !ok {
			return "", false
		}
		return numberToString(f), true
	}
	return "", false
}

func numberLiteral(n *ast.NumberLiteral) (float64, bool) {
	switch v := n.Value.(type) {
	case int64:
		return float64(v), true
	case float64:
		return v, true
	}
	return 0, false
}

func (c *compiler) expression(node ast.Expression) (evalFn, error) {
	c.depth++
	defer func() { c.depth-- }()
	if c.depth > 90 {
		return nil, fmt.Errorf("expression nesting limit")
	}
	if node == nil {
		return constant(Undefined), nil
	}
	switch n := node.(type) {
	case *ast.NullLiteral:
		return constant(nil), nil
	case *ast.BooleanLiteral:
		return constant(n.Value), nil
	case *ast.StringLiteral:
		return constant(n.Value.String()), nil
	case *ast.NumberLiteral:
		f, ok := numberLiteral(n)
		if !ok {
			return nil, unsupported(n)
		}
		return constant(f), nil
	case *ast.TemplateLiteral:
		return c.template(n)
	case *ast.Identifier:
		return c.identifier(n.Name.String())
	case *ast.ArrayLiteral:
		return c.arrayLiteral(n)
	case *ast.ObjectLiteral:
		return c.objectLiteral(n)
	case *ast.DotExpression, *ast.BracketExpression:
		return c.member(n)
	case *ast.CallExpression:
		return c.call(n, false)
	case *ast.OptionalChain:
		inner, err := c.expression(n.Expression)
		if err != nil {
			return nil, err
		}
		return func(r *rt, s *scope) (any, error) {
			v, err := inner(r, s)
			if err == errShortCircuit {
				return Undefined, nil
			}
			return v, err
		}, nil
	case *ast.Optional:
		inner, err := c.expression(n.Expression)
		if err != nil {
			return nil, err
		}
		return func(r *rt, s *scope) (any, error) {
			v, err := inner(r, s)
			if err == nil && isNullish(v) {
				return nil, errShortCircuit
			}
			return v, err
		}, nil
	case *ast.UnaryExpression:
		return c.unary(n)
	case *ast.BinaryExpression:
		return c.binary(n)
	case *ast.AssignExpression:
		return c.assign(n)
	case *ast.ConditionalExpression:
		test, err := c.expression(n.Test)
		if err != nil {
			return nil, err
		}
		cons, err := c.expression(n.Consequent)
		if err != nil {
			return nil, err
		}
		alt, err := c.expression(n.Alternate)
		if err != nil {
			return nil, err
		}
		return func(r *rt, s *scope) (any, error) {
			v, err := test(r, s)
			if err != nil {
				return nil, err
			}
			if toBoolean(v) {
				return cons(r, s)
			}
			return alt(r, s)
		}, nil
	case *ast.SequenceExpression:
		list := make([]evalFn, len(n.Sequence))
		for i, e := range n.Sequence {
			f, err := c.expression(e)
			if err != nil {
				return nil, err
			}
			list[i] = f
		}
		return func(r *rt, s *scope) (v any, err error) {
			for _, f := range list {
				if v, err = f(r, s); err != nil {
					return nil, err
				}
			}
			return v, nil
		}, nil
	case *ast.ArrowFunctionLiteral:
		var body *ast.BlockStatement
		var expr ast.Expression
		switch b := n.Body.(type) {
		case *ast.BlockStatement:
			body = b
		case *ast.ExpressionBody:
			expr = b.Expression
		default:
			return nil, unsupported(b)
		}
		code, err := c.function(n.ParameterList, body, expr, n.Async, n.Source)
		if err != nil {
			return nil, err
		}
		return closureFn(code), nil
	case *ast.FunctionLiteral:
		return c.functionExpression(n)
	case *ast.AwaitExpression:
		if !c.opts.Batches || !c.fn.async {
			return nil, unsupported(n)
		}
		return c.expression(n.Argument)
	case *ast.NewExpression:
		return c.newExpression(n)
	case *ast.RegExpLiteral:
		p, ok, bad := compileRegexp(n.Pattern, n.Flags)
		if !ok || bad != "" {
			return nil, unsupported(n)
		}
		// Each evaluation of a literal creates a fresh RegExp object.
		return func(r *rt, _ *scope) (any, error) {
			if err := r.charge(allocHeader); err != nil {
				return nil, err
			}
			return &regexpValue{pat: p, lastIndex: float64(0)}, nil
		}, nil
	}
	return nil, unsupported(node)
}

func (c *compiler) functionExpression(n *ast.FunctionLiteral) (evalFn, error) {
	if n.Generator {
		return nil, unsupported(n)
	}
	if n.Name == nil {
		code, err := c.function(n.ParameterList, n.Body, nil, n.Async, n.Source)
		if err != nil {
			return nil, err
		}
		return closureFn(code), nil
	}
	// A named function expression sees its own name in an enclosing scope.
	name := n.Name.Name.String()
	sc := c.pushScope(false)
	v, err := c.declare(sc, name, kindFuncName)
	if err != nil {
		c.popScope()
		return nil, err
	}
	code, err := c.function(n.ParameterList, n.Body, nil, n.Async, n.Source)
	c.popScope()
	if err != nil {
		return nil, err
	}
	slot := v.slot
	init := sc.init
	return func(r *rt, s *scope) (any, error) {
		inner := newScope(s, init)
		f, err := r.closure(code, inner, name)
		if err != nil {
			return nil, err
		}
		inner.vars[slot] = f
		return f, nil
	}, nil
}

// closureFn evaluates a function literal to a new closure over the current
// scope.
func closureFn(code *funcCode) evalFn {
	return func(r *rt, s *scope) (any, error) {
		f, err := r.closure(code, s, "")
		if err != nil {
			return nil, err
		}
		return f, nil
	}
}

func (c *compiler) template(n *ast.TemplateLiteral) (evalFn, error) {
	if n.Tag != nil {
		return nil, unsupported(n)
	}
	parts := make([]string, len(n.Elements))
	for i, e := range n.Elements {
		if !e.Valid {
			return nil, unsupported(n)
		}
		parts[i] = e.Parsed.String()
	}
	exprs := make([]evalFn, len(n.Expressions))
	for i, e := range n.Expressions {
		f, err := c.expression(e)
		if err != nil {
			return nil, err
		}
		exprs[i] = f
	}
	return func(r *rt, s *scope) (any, error) {
		var b strings.Builder
		for i, p := range parts {
			b.WriteString(p)
			if i < len(exprs) {
				v, err := exprs[i](r, s)
				if err != nil {
					return nil, err
				}
				str, err := r.toString(v)
				if err != nil {
					return nil, err
				}
				b.WriteString(str)
				if b.Len() > r.x.maxString {
					return nil, r.rangeError("Invalid string length")
				}
			}
		}
		return r.stringValue(b.String())
	}, nil
}

// Global functions usable as values (e.g. xs.map(String), xs.filter(Boolean)).
var globalFunctionValues = map[string]*function{}

func init() {
	for _, name := range []string{"String", "Number", "Boolean", "parseInt", "parseFloat", "isNaN", "isFinite", "encodeURIComponent", "encodeURI", "decodeURIComponent", "decodeURI"} {
		name := name
		globalFunctionValues[name] = &function{name: name, native: func(r *rt, _ any, args []any) (any, error) {
			return r.callGlobal(name, args)
		}}
	}
}

// Global namespaces whose members compile to built-ins.
func isGlobalNamespace(name string) bool {
	if name == "RegExp" {
		return false
	}
	switch name {
	case "JSON", "Math", "Object", "Array", "Number", "String", "console", "Promise", "Date", "Intl":
		return true
	}
	return false
}

func isErrorConstructor(name string) bool {
	switch name {
	case "Error", "TypeError", "RangeError", "SyntaxError", "ReferenceError", "EvalError", "URIError":
		return true
	}
	return false
}

func (c *compiler) identifier(name string) (evalFn, error) {
	v, hops := c.lookup(name)
	if v == nil {
		switch name {
		case "undefined":
			return constant(Undefined), nil
		case "NaN":
			return constant(math.NaN()), nil
		case "Infinity":
			return constant(math.Inf(1)), nil
		}
		if f, ok := globalFunctionValues[name]; ok {
			return constant(f), nil
		}
		if (name == "mcp" || name == "tools") && c.opts.Bindings != nil {
			c.namespaceUsed = true
			return func(r *rt, _ *scope) (any, error) { return r.x.namespace, nil }, nil
		}
		return nil, fmt.Errorf("unbound identifier %s", name)
	}
	if v.alias != nil {
		if c.opts.Bindings == nil {
			return nil, fmt.Errorf("namespace alias %s used as a value", name)
		}
		c.namespaceUsed = true
		path := v.alias[1:]
		return func(r *rt, _ *scope) (any, error) {
			var cur any = r.x.namespace
			for _, k := range path {
				next, err := r.getProp(cur, k)
				if err != nil {
					return nil, err
				}
				cur = next
			}
			return cur, nil
		}, nil
	}
	slot := v.slot
	if v.kind == kindLet || v.kind == kindConst {
		return func(r *rt, s *scope) (any, error) {
			x := s.up(hops).vars[slot]
			if x == tdz {
				return nil, r.referenceError("Cannot access a variable before initialization")
			}
			return x, nil
		}, nil
	}
	switch hops {
	case 0:
		return func(_ *rt, s *scope) (any, error) { return s.vars[slot], nil }, nil
	case 1:
		return func(_ *rt, s *scope) (any, error) { return s.parent.vars[slot], nil }, nil
	}
	return func(_ *rt, s *scope) (any, error) { return s.up(hops).vars[slot], nil }, nil
}

func (c *compiler) arrayLiteral(n *ast.ArrayLiteral) (evalFn, error) {
	type elem struct {
		f      evalFn
		spread bool
	}
	elems := make([]elem, len(n.Value))
	for i, v := range n.Value {
		if v == nil {
			return nil, unsupported(n) // holes
		}
		e := elem{}
		if sp, ok := v.(*ast.SpreadElement); ok {
			e.spread = true
			v = sp.Expression
		}
		f, err := c.expression(v)
		if err != nil {
			return nil, err
		}
		e.f = f
		elems[i] = e
	}
	return func(r *rt, s *scope) (any, error) {
		if err := r.chargeArray(len(elems)); err != nil {
			return nil, err
		}
		out := make([]any, 0, len(elems))
		for _, e := range elems {
			v, err := e.f(r, s)
			if err != nil {
				return nil, err
			}
			if e.spread {
				items, err := r.iterableItems(v)
				if err != nil {
					return nil, err
				}
				if len(out)+len(items) > r.x.maxItems {
					return nil, r.rangeError("Invalid array length")
				}
				if err := r.charge(len(items) * allocSlot); err != nil {
					return nil, err
				}
				out = append(out, items...)
				continue
			}
			out = append(out, v)
		}
		return &array{items: out}, nil
	}, nil
}

func (c *compiler) objectLiteral(n *ast.ObjectLiteral) (evalFn, error) {
	type prop struct {
		key    evalFn
		value  evalFn
		name   string
		spread bool
		named  bool // an anonymous function under a computed key takes its name
	}
	props := make([]prop, 0, len(n.Value))
	for _, v := range n.Value {
		switch v := v.(type) {
		case *ast.PropertyShort:
			if v.Initializer != nil {
				return nil, unsupported(v)
			}
			f, err := c.identifier(v.Name.Name.String())
			if err != nil {
				return nil, err
			}
			props = append(props, prop{name: v.Name.Name.String(), value: f})
		case *ast.PropertyKeyed:
			p := prop{}
			switch v.Kind {
			case ast.PropertyKindValue, ast.PropertyKindMethod:
			default:
				return nil, unsupported(v)
			}
			if v.Computed {
				k, err := c.expression(v.Key)
				if err != nil {
					return nil, err
				}
				p.key = k
			} else {
				name, ok := propertyKey(v.Key)
				if !ok || name == "__proto__" {
					return nil, unsupported(v)
				}
				p.name = name
			}
			f, err := c.expression(v.Value)
			if err != nil {
				return nil, err
			}
			p.named = isFunctionLiteral(v.Value) && p.key != nil
			if isFunctionLiteral(v.Value) && p.key == nil {
				name := p.name
				inner := f
				f = func(r *rt, s *scope) (any, error) {
					v, err := inner(r, s)
					if fn, ok := v.(*function); ok && fn.name == "" {
						fn.name = name
					}
					return v, err
				}
			}
			p.value = f
			props = append(props, p)
		case *ast.SpreadElement:
			f, err := c.expression(v.Expression)
			if err != nil {
				return nil, err
			}
			props = append(props, prop{value: f, spread: true})
		default:
			return nil, unsupported(v)
		}
	}
	return func(r *rt, s *scope) (any, error) {
		o, err := r.newObj(len(props))
		if err != nil {
			return nil, err
		}
		for _, p := range props {
			name := p.name
			if p.key != nil {
				k, err := p.key(r, s)
				if err != nil {
					return nil, err
				}
				if name, err = r.toPropertyKey(k); err != nil {
					return nil, err
				}
			}
			v, err := p.value(r, s)
			if err != nil {
				return nil, err
			}
			if p.spread {
				if err := r.copyProps(o, v); err != nil {
					return nil, err
				}
				continue
			}
			if fn, ok := v.(*function); ok && p.named && fn.name == "" {
				fn.name = name
			}
			o.set(name, v)
		}
		return o, nil
	}, nil
}

func isFunctionLiteral(e ast.Expression) bool {
	switch e.(type) {
	case *ast.ArrowFunctionLiteral, *ast.FunctionLiteral:
		return true
	}
	return false
}

// copyProps implements object spread sources, charging for each property
// it adds to dst.
func (r *rt) copyProps(dst *object, src any) error {
	switch t := src.(type) {
	case *object:
		for _, k := range t.ownKeys() {
			v, _ := t.own(k)
			if err := r.put(dst, k, v); err != nil {
				return err
			}
		}
	case *hostObject:
		for _, k := range t.view.ownKeys() {
			v, _ := t.view.own(k)
			if err := r.put(dst, k, v); err != nil {
				return err
			}
		}
	case *array:
		for i, v := range t.items {
			if !isHole(v) {
				if err := r.put(dst, strconv.Itoa(i), v); err != nil {
					return err
				}
			}
		}
		if t.props != nil {
			for _, k := range t.props.ownKeys() {
				v, _ := t.props.own(k)
				if err := r.put(dst, k, v); err != nil {
					return err
				}
			}
		}
	case *dateValue, *collection, *regexpValue, *function:
		// Only script-assigned own properties are enumerable.
		var props *object
		switch t := t.(type) {
		case *dateValue:
			props = t.props
		case *collection:
			props = t.props
		case *regexpValue:
			props = t.props
		case *function:
			props = t.props
		}
		if props != nil {
			for _, k := range props.ownKeys() {
				v, _ := props.own(k)
				if err := r.put(dst, k, v); err != nil {
					return err
				}
			}
		}
	case string:
		i := 0
		for _, u := range toUnits(t) {
			ch := fromUnits([]uint16{u})
			if err := r.chargeString(ch); err != nil {
				return err
			}
			if err := r.put(dst, strconv.Itoa(i), ch); err != nil {
				return err
			}
			i++
		}
	}
	return nil
}

// memberParts compiles the object and key of a member expression.
func (c *compiler) memberParts(node ast.Expression) (evalFn, func(*rt, *scope) (string, error), error) {
	var left ast.Expression
	var key func(*rt, *scope) (string, error)
	switch n := node.(type) {
	case *ast.DotExpression:
		name := n.Identifier.Name.String()
		if unsafeKey(name) {
			return nil, nil, unsupported(n)
		}
		left = n.Left
		key = func(*rt, *scope) (string, error) { return name, nil }
	case *ast.BracketExpression:
		left = n.Left
		if name, ok := staticKey(n.Member); ok {
			if unsafeKey(name) {
				return nil, nil, unsupported(n)
			}
			key = func(*rt, *scope) (string, error) { return name, nil }
		} else {
			k, err := c.expression(n.Member)
			if err != nil {
				return nil, nil, err
			}
			key = func(r *rt, s *scope) (string, error) {
				v, err := k(r, s)
				if err != nil {
					return "", err
				}
				return r.toPropertyKey(v)
			}
		}
	default:
		return nil, nil, unsupported(node)
	}
	if path, ok := c.namespacePath(left); ok && c.opts.Bindings == nil {
		return nil, nil, fmt.Errorf("tool namespace %s used as a value", strings.Join(path, "."))
	}
	if id, ok := left.(*ast.Identifier); ok {
		if v, _ := c.lookup(id.Name.String()); v == nil && isGlobalNamespace(id.Name.String()) {
			return nil, nil, fmt.Errorf("global %s used as a value", id.Name.String())
		}
	}
	obj, err := c.expression(left)
	if err != nil {
		return nil, nil, err
	}
	return obj, key, nil
}

// staticKey returns a literal bracket key.
func staticKey(e ast.Expression) (string, bool) {
	switch e := e.(type) {
	case *ast.StringLiteral:
		return e.Value.String(), true
	case *ast.NumberLiteral:
		f, ok := numberLiteral(e)
		if !ok {
			return "", false
		}
		return numberToString(f), true
	}
	return "", false
}

var globalConstants = map[string]float64{
	"Math.PI": math.Pi, "Math.E": math.E, "Math.LN2": math.Ln2, "Math.LN10": math.Ln10,
	"Math.LOG2E": math.Log2E, "Math.LOG10E": math.Log10E, "Math.SQRT2": math.Sqrt2, "Math.SQRT1_2": math.Sqrt2 / 2,
	"Number.MAX_SAFE_INTEGER": 9007199254740991, "Number.MIN_SAFE_INTEGER": -9007199254740991,
	"Number.EPSILON": 2.220446049250313e-16, "Number.MAX_VALUE": math.MaxFloat64, "Number.MIN_VALUE": 5e-324,
	"Number.POSITIVE_INFINITY": math.Inf(1), "Number.NEGATIVE_INFINITY": math.Inf(-1), "Number.NaN": math.NaN(),
}

func (c *compiler) member(node ast.Expression) (evalFn, error) {
	if path, ok := staticPath(node); ok && len(path) == 2 {
		if v, _ := c.lookup(path[0]); v == nil {
			if f, ok := globalConstants[path[0]+"."+path[1]]; ok {
				return constant(f), nil
			}
		}
	}
	obj, key, err := c.memberParts(node)
	if err != nil {
		return nil, err
	}
	return func(r *rt, s *scope) (any, error) {
		o, err := obj(r, s)
		if err != nil {
			return nil, err
		}
		k, err := key(r, s)
		if err != nil {
			return nil, err
		}
		return r.getProp(o, k)
	}, nil
}

func (c *compiler) newExpression(n *ast.NewExpression) (evalFn, error) {
	if path, ok := staticPath(n.Callee); ok && len(path) == 2 && path[0] == "Intl" {
		if v, _ := c.lookup("Intl"); v != nil || intlKinds[path[1]] == nil {
			return nil, unsupported(n)
		}
		args, err := c.arguments(n.ArgumentList)
		if err != nil {
			return nil, err
		}
		name := path[1]
		return func(r *rt, s *scope) (any, error) {
			a, err := args(r, s)
			if err != nil {
				return nil, err
			}
			return r.newIntl(name, a)
		}, nil
	}
	id, ok := n.Callee.(*ast.Identifier)
	if !ok {
		return nil, unsupported(n)
	}
	name := id.Name.String()
	if v, _ := c.lookup(name); v != nil || !(isErrorConstructor(name) || name == "RegExp" || name == "Set" || name == "Map" || name == "Array" || name == "Date") {
		return nil, unsupported(n)
	}
	args, err := c.arguments(n.ArgumentList)
	if err != nil {
		return nil, err
	}
	return func(r *rt, s *scope) (any, error) {
		a, err := args(r, s)
		if err != nil {
			return nil, err
		}
		switch name {
		case "RegExp":
			return r.newRegExp(a)
		case "Set", "Map":
			return r.newCollectionFrom(name == "Map", a)
		case "Array":
			return r.newArrayFromArgs(a)
		case "Date":
			return r.newDateFromArgs(a)
		}
		return r.makeError(name, a)
	}, nil
}

func (r *rt) makeError(name string, args []any) (any, error) {
	if err := r.charge(allocHeader); err != nil {
		return nil, err
	}
	e := newError(name, "")
	// Goja keeps a non-string message as given (the spec would ToString it).
	e.errMsg = Undefined
	if len(args) > 0 && !isUndefined(args[0]) {
		e.errMsg = args[0]
		if isObjectValue(args[0]) {
			s, err := r.toString(args[0])
			if err != nil {
				return nil, err
			}
			e.errMsg = s
		}
	}
	if len(args) > 1 {
		if o, ok := args[1].(*object); ok {
			if cause, ok := o.own("cause"); ok {
				e.errCause, e.hasCause = cause, true
			}
		}
	}
	return e, nil
}

func (c *compiler) unary(n *ast.UnaryExpression) (evalFn, error) {
	switch n.Operator {
	case token.INCREMENT, token.DECREMENT:
		return c.update(n)
	case token.TYPEOF:
		if id, ok := n.Operand.(*ast.Identifier); ok {
			name := id.Name.String()
			if v, _ := c.lookup(name); v == nil {
				switch {
				case name == "Promise":
					// Hosts differ on whether a Promise global exists.
					return nil, fmt.Errorf("typeof Promise")
				case name == "String" || name == "Number" || name == "Object" || name == "Array" || name == "Boolean" || name == "RegExp" || name == "Set" || name == "Map" || name == "Date":
					return constant("function"), nil
				case name == "mcp" || name == "tools" || isGlobalNamespace(name):
					return constant("object"), nil
				case isErrorConstructor(name) || c.opts.HostFunctions != nil && hasHost(c.opts.HostFunctions, name):
					return constant("function"), nil
				case name == "undefined" || name == "NaN" || name == "Infinity" || globalFunctionValues[name] != nil:
				default:
					return constant("undefined"), nil
				}
			}
		}
		f, err := c.expression(n.Operand)
		if err != nil {
			return nil, err
		}
		return func(r *rt, s *scope) (any, error) {
			v, err := f(r, s)
			if err != nil {
				return nil, err
			}
			return typeOf(v), nil
		}, nil
	case token.DELETE:
		obj, key, err := c.memberParts(n.Operand)
		if err != nil {
			return nil, err
		}
		return func(r *rt, s *scope) (any, error) {
			o, err := obj(r, s)
			if err != nil {
				return nil, err
			}
			k, err := key(r, s)
			if err != nil {
				return nil, err
			}
			ok, err := r.deleteProp(o, k)
			return ok, err
		}, nil
	}
	if lit, ok := n.Operand.(*ast.NumberLiteral); ok && (n.Operator == token.MINUS || n.Operator == token.PLUS) {
		f, ok := numberLiteral(lit)
		if !ok {
			return nil, unsupported(n)
		}
		if n.Operator == token.MINUS {
			f = -f
		}
		return constant(f), nil
	}
	f, err := c.expression(n.Operand)
	if err != nil {
		return nil, err
	}
	var op func(r *rt, v any) (any, error)
	switch n.Operator {
	case token.NOT:
		op = func(_ *rt, v any) (any, error) { return !toBoolean(v), nil }
	case token.MINUS:
		op = func(r *rt, v any) (any, error) {
			x, err := r.toNumber(v)
			return -x, err
		}
	case token.PLUS:
		op = func(r *rt, v any) (any, error) { return r.toNumber(v) }
	case token.BITWISE_NOT:
		op = func(r *rt, v any) (any, error) {
			x, err := r.toNumber(v)
			return float64(^toInt32(x)), err
		}
	case token.VOID:
		op = func(*rt, any) (any, error) { return Undefined, nil }
	default:
		return nil, unsupported(n)
	}
	return func(r *rt, s *scope) (any, error) {
		v, err := f(r, s)
		if err != nil {
			return nil, err
		}
		return op(r, v)
	}, nil
}

func hasHost(m map[string]HostFunction, name string) bool {
	_, ok := m[name]
	return ok
}

// lvalue is a compiled assignment target: an identifier slot or a member.
// Resolving it yields a plain struct, so compound assignment allocates nothing.
type lvalue struct {
	get evalFn
	set binder
	obj evalFn
	key func(*rt, *scope) (string, error)
}

type place struct {
	lv  *lvalue
	obj any
	key string
}

func (c *compiler) reference(target ast.Expression) (*lvalue, error) {
	switch n := target.(type) {
	case *ast.Identifier:
		get, err := c.identifier(n.Name.String())
		if err != nil {
			return nil, err
		}
		set, err := c.identifierStore(n.Name.String(), false)
		if err != nil {
			return nil, err
		}
		return &lvalue{get: get, set: set}, nil
	case *ast.DotExpression, *ast.BracketExpression:
		obj, key, err := c.memberParts(n)
		if err != nil {
			return nil, err
		}
		return &lvalue{obj: obj, key: key}, nil
	}
	return nil, unsupported(target)
}

func (lv *lvalue) prepare(r *rt, s *scope) (place, error) {
	if lv.obj == nil {
		return place{lv: lv}, nil
	}
	o, err := lv.obj(r, s)
	if err != nil {
		return place{}, err
	}
	k, err := lv.key(r, s)
	if err != nil {
		return place{}, err
	}
	return place{lv: lv, obj: o, key: k}, nil
}

func (p place) get(r *rt, s *scope) (any, error) {
	if p.lv.obj == nil {
		return p.lv.get(r, s)
	}
	return r.getProp(p.obj, p.key)
}

func (p place) set(r *rt, s *scope, v any) error {
	if p.lv.obj == nil {
		return p.lv.set(r, s, v)
	}
	return r.setProp(p.obj, p.key, v)
}

func (c *compiler) update(n *ast.UnaryExpression) (evalFn, error) {
	target, err := c.reference(n.Operand)
	if err != nil {
		return nil, err
	}
	delta := 1.0
	if n.Operator == token.DECREMENT {
		delta = -1
	}
	postfix := n.Postfix
	return func(r *rt, s *scope) (any, error) {
		ref, err := target.prepare(r, s)
		if err != nil {
			return nil, err
		}
		old, err := ref.get(r, s)
		if err != nil {
			return nil, err
		}
		x, ok := old.(float64)
		if !ok {
			if x, err = r.toNumber(old); err != nil {
				return nil, err
			}
		}
		next := num(x + delta)
		if err := ref.set(r, s, next); err != nil {
			return nil, err
		}
		if postfix {
			return num(x), nil
		}
		return next, nil
	}, nil
}

func (c *compiler) assign(n *ast.AssignExpression) (evalFn, error) {
	if n.Operator == token.ASSIGN {
		value, err := c.expressionNamed(n.Right, n.Left)
		if err != nil {
			return nil, err
		}
		switch n.Left.(type) {
		case *ast.ArrayPattern, *ast.ObjectPattern:
			bind, err := c.bindingTarget(n.Left, false)
			if err != nil {
				return nil, err
			}
			return func(r *rt, s *scope) (any, error) {
				v, err := value(r, s)
				if err != nil {
					return nil, err
				}
				return v, bind(r, s, v)
			}, nil
		}
		target, err := c.reference(n.Left)
		if err != nil {
			return nil, err
		}
		return func(r *rt, s *scope) (any, error) {
			ref, err := target.prepare(r, s)
			if err != nil {
				return nil, err
			}
			v, err := value(r, s)
			if err != nil {
				return nil, err
			}
			return v, ref.set(r, s, v)
		}, nil
	}
	target, err := c.reference(n.Left)
	if err != nil {
		return nil, err
	}
	value, err := c.expressionNamed(n.Right, n.Left)
	if err != nil {
		return nil, err
	}
	switch n.Operator {
	case token.LOGICAL_AND, token.LOGICAL_OR, token.COALESCE:
		op := n.Operator
		return func(r *rt, s *scope) (any, error) {
			ref, err := target.prepare(r, s)
			if err != nil {
				return nil, err
			}
			old, err := ref.get(r, s)
			if err != nil {
				return nil, err
			}
			switch op {
			case token.LOGICAL_AND:
				if !toBoolean(old) {
					return old, nil
				}
			case token.LOGICAL_OR:
				if toBoolean(old) {
					return old, nil
				}
			default:
				if !isNullish(old) {
					return old, nil
				}
			}
			v, err := value(r, s)
			if err != nil {
				return nil, err
			}
			return v, ref.set(r, s, v)
		}, nil
	}
	op, err := binaryOp(n.Operator)
	if err != nil {
		return nil, err
	}
	return func(r *rt, s *scope) (any, error) {
		ref, err := target.prepare(r, s)
		if err != nil {
			return nil, err
		}
		old, err := ref.get(r, s)
		if err != nil {
			return nil, err
		}
		rhs, err := value(r, s)
		if err != nil {
			return nil, err
		}
		v, err := op(r, old, rhs)
		if err != nil {
			return nil, err
		}
		return v, ref.set(r, s, v)
	}, nil
}

func (c *compiler) binary(n *ast.BinaryExpression) (evalFn, error) {
	if n.Operator == token.INSTANCEOF {
		return c.instanceOf(n)
	}
	left, err := c.expression(n.Left)
	if err != nil {
		return nil, err
	}
	right, err := c.expression(n.Right)
	if err != nil {
		return nil, err
	}
	switch n.Operator {
	case token.LOGICAL_AND:
		return func(r *rt, s *scope) (any, error) {
			v, err := left(r, s)
			if err != nil || !toBoolean(v) {
				return v, err
			}
			return right(r, s)
		}, nil
	case token.LOGICAL_OR:
		return func(r *rt, s *scope) (any, error) {
			v, err := left(r, s)
			if err != nil || toBoolean(v) {
				return v, err
			}
			return right(r, s)
		}, nil
	case token.COALESCE:
		return func(r *rt, s *scope) (any, error) {
			v, err := left(r, s)
			if err != nil || !isNullish(v) {
				return v, err
			}
			return right(r, s)
		}, nil
	}
	op, err := binaryOp(n.Operator)
	if err != nil {
		return nil, err
	}
	return func(r *rt, s *scope) (any, error) {
		a, err := left(r, s)
		if err != nil {
			return nil, err
		}
		b, err := right(r, s)
		if err != nil {
			return nil, err
		}
		return op(r, a, b)
	}, nil
}

func (c *compiler) instanceOf(n *ast.BinaryExpression) (evalFn, error) {
	id, ok := n.Right.(*ast.Identifier)
	if !ok {
		return nil, unsupported(n)
	}
	name := id.Name.String()
	if v, _ := c.lookup(name); v != nil {
		return nil, unsupported(n)
	}
	var test func(any) bool
	switch {
	case name == "Error":
		test = func(v any) bool { o, ok := v.(*object); return ok && o.errName != "" }
	case isErrorConstructor(name):
		test = func(v any) bool { o, ok := v.(*object); return ok && o.errName == name }
	case name == "Array":
		test = func(v any) bool { _, ok := v.(*array); return ok }
	case name == "Object":
		test = isObjectValue
	case name == "RegExp":
		test = func(v any) bool { _, ok := v.(*regexpValue); return ok }
	case name == "Date":
		test = func(v any) bool { _, ok := v.(*dateValue); return ok }
	case name == "Set" || name == "Map":
		isMap := name == "Map"
		test = func(v any) bool { c, ok := v.(*collection); return ok && c.isMap == isMap }
	default:
		return nil, unsupported(n)
	}
	left, err := c.expression(n.Left)
	if err != nil {
		return nil, err
	}
	return func(r *rt, s *scope) (any, error) {
		v, err := left(r, s)
		if err != nil {
			return nil, err
		}
		return test(v), nil
	}, nil
}

type binop func(r *rt, a, b any) (any, error)

func binaryOp(op token.Token) (binop, error) {
	switch op {
	case token.PLUS:
		return (*rt).add, nil
	case token.MINUS:
		return func(r *rt, a, b any) (any, error) {
			if x, ok := a.(float64); ok {
				if y, ok := b.(float64); ok {
					return num(x - y), nil
				}
			}
			return numeric(func(a, b float64) float64 { return a - b })(r, a, b)
		}, nil
	case token.MULTIPLY:
		return func(r *rt, a, b any) (any, error) {
			if x, ok := a.(float64); ok {
				if y, ok := b.(float64); ok {
					return num(x * y), nil
				}
			}
			return numeric(func(a, b float64) float64 { return a * b })(r, a, b)
		}, nil
	case token.SLASH:
		return numeric(func(a, b float64) float64 { return a / b }), nil
	case token.REMAINDER:
		return func(r *rt, a, b any) (any, error) {
			if x, ok := a.(float64); ok {
				if y, ok := b.(float64); ok {
					if x >= 0 && !math.Signbit(x) && y > 0 && x < 1<<53 && y < 1<<53 && x == math.Trunc(x) && y == math.Trunc(y) {
						return num(float64(int64(x) % int64(y))), nil
					}
					return num(jsMod(x, y)), nil
				}
			}
			return numeric(jsMod)(r, a, b)
		}, nil
	case token.EXPONENT:
		return numeric(jsPow), nil
	case token.STRICT_EQUAL:
		return func(_ *rt, a, b any) (any, error) { return strictEquals(a, b), nil }, nil
	case token.STRICT_NOT_EQUAL:
		return func(_ *rt, a, b any) (any, error) { return !strictEquals(a, b), nil }, nil
	case token.EQUAL:
		return func(r *rt, a, b any) (any, error) { return r.looseEquals(a, b) }, nil
	case token.NOT_EQUAL:
		return func(r *rt, a, b any) (any, error) {
			eq, err := r.looseEquals(a, b)
			return !eq, err
		}, nil
	case token.LESS:
		return func(r *rt, a, b any) (any, error) {
			if x, ok := a.(float64); ok {
				if y, ok := b.(float64); ok {
					return x < y, nil
				}
			}
			lt, u, err := r.compareValues(a, b)
			return lt && !u, err
		}, nil
	case token.GREATER:
		return func(r *rt, a, b any) (any, error) {
			if x, ok := a.(float64); ok {
				if y, ok := b.(float64); ok {
					return x > y, nil
				}
			}
			lt, u, err := r.compareValues(b, a)
			return lt && !u, err
		}, nil
	case token.LESS_OR_EQUAL:
		return func(r *rt, a, b any) (any, error) {
			if x, ok := a.(float64); ok {
				if y, ok := b.(float64); ok {
					return x <= y, nil
				}
			}
			lt, u, err := r.compareValues(b, a)
			return !lt && !u, err
		}, nil
	case token.GREATER_OR_EQUAL:
		return func(r *rt, a, b any) (any, error) {
			if x, ok := a.(float64); ok {
				if y, ok := b.(float64); ok {
					return x >= y, nil
				}
			}
			lt, u, err := r.compareValues(a, b)
			return !lt && !u, err
		}, nil
	case token.AND:
		return bitwise(func(a, b int32) float64 { return float64(a & b) }), nil
	case token.OR:
		return bitwise(func(a, b int32) float64 { return float64(a | b) }), nil
	case token.EXCLUSIVE_OR:
		return bitwise(func(a, b int32) float64 { return float64(a ^ b) }), nil
	case token.SHIFT_LEFT:
		return bitwise(func(a, b int32) float64 { return float64(a << (uint32(b) & 31)) }), nil
	case token.SHIFT_RIGHT:
		return bitwise(func(a, b int32) float64 { return float64(a >> (uint32(b) & 31)) }), nil
	case token.UNSIGNED_SHIFT_RIGHT:
		return bitwise(func(a, b int32) float64 { return float64(uint32(a) >> (uint32(b) & 31)) }), nil
	case token.IN:
		return func(r *rt, a, b any) (any, error) {
			k, err := r.toPropertyKey(a)
			if err != nil {
				return nil, err
			}
			return r.hasProperty(b, k)
		}, nil
	}
	return nil, fmt.Errorf("unsupported operator %s", op)
}

func numeric(f func(a, b float64) float64) binop {
	return func(r *rt, a, b any) (any, error) {
		x, err := r.toNumber(a)
		if err != nil {
			return nil, err
		}
		y, err := r.toNumber(b)
		if err != nil {
			return nil, err
		}
		return f(x, y), nil
	}
}

func bitwise(f func(a, b int32) float64) binop {
	return func(r *rt, a, b any) (any, error) {
		x, err := r.toNumber(a)
		if err != nil {
			return nil, err
		}
		y, err := r.toNumber(b)
		if err != nil {
			return nil, err
		}
		return f(toInt32(x), toInt32(y)), nil
	}
}

func jsMod(a, b float64) float64 {
	if math.IsInf(b, 0) && !math.IsInf(a, 0) && !math.IsNaN(a) {
		return a
	}
	return math.Mod(a, b)
}

func jsPow(a, b float64) float64 {
	if math.IsNaN(b) || (math.Abs(a) == 1 && math.IsInf(b, 0)) {
		return math.NaN()
	}
	return math.Pow(a, b)
}

func (r *rt) add(a, b any) (any, error) {
	if x, ok := a.(float64); ok {
		if y, ok := b.(float64); ok {
			return num(x + y), nil
		}
	}
	if x, ok := a.(string); ok {
		if y, ok := b.(string); ok {
			return r.concat(x, y)
		}
	}
	pa, err := r.toPrimitive(a)
	if err != nil {
		return nil, err
	}
	pb, err := r.toPrimitive(b)
	if err != nil {
		return nil, err
	}
	_, sa := pa.(string)
	_, sb := pb.(string)
	if sa || sb {
		x, err := r.toString(pa)
		if err != nil {
			return nil, err
		}
		y, err := r.toString(pb)
		if err != nil {
			return nil, err
		}
		return r.concat(x, y)
	}
	x, err := r.toNumber(pa)
	if err != nil {
		return nil, err
	}
	y, err := r.toNumber(pb)
	if err != nil {
		return nil, err
	}
	return x + y, nil
}

func (r *rt) concat(a, b string) (any, error) {
	if len(a)+len(b) > r.x.maxString {
		return nil, r.rangeError("Invalid string length")
	}
	if a == "" || b == "" {
		return a + b, nil // the non-empty operand itself: nothing new
	}
	if err := r.charge(len(a) + len(b) + allocSlot); err != nil {
		return nil, err
	}
	return a + b, nil
}

// smallInts holds pre-boxed integral numbers; boxing a float64 into an
// interface otherwise allocates on every arithmetic result.
var smallInts [4096]any

func init() {
	for i := range smallInts {
		smallInts[i] = float64(i)
	}
}

func num(f float64) any {
	if f >= 1 && f < float64(len(smallInts)) {
		if i := int(f); float64(i) == f {
			return smallInts[i]
		}
	} else if f == 0 && !math.Signbit(f) {
		return smallInts[0]
	}
	return f
}

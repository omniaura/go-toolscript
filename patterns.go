package toolscript

import (
	"fmt"

	"github.com/dop251/goja/ast"
	"github.com/dop251/goja/token"
)

// bindingTarget compiles a store into an identifier, member expression or
// destructuring pattern. init is true for declarations (const may be set).
func (c *compiler) bindingTarget(target ast.Expression, init bool) (binder, error) {
	c.depth++
	defer func() { c.depth-- }()
	if c.depth > 64 {
		return nil, fmt.Errorf("binding nesting limit")
	}
	switch n := target.(type) {
	case *ast.Identifier:
		return c.identifierStore(n.Name.String(), init)
	case *ast.DotExpression, *ast.BracketExpression:
		if init {
			return nil, unsupported(n)
		}
		obj, key, err := c.memberParts(n)
		if err != nil {
			return nil, err
		}
		return func(r *rt, s *scope, v any) error {
			o, err := obj(r, s)
			if err != nil {
				return err
			}
			k, err := key(r, s)
			if err != nil {
				return err
			}
			return r.setProp(o, k, v)
		}, nil
	case *ast.ArrayPattern:
		return c.arrayPattern(n, init)
	case *ast.ObjectPattern:
		return c.objectPattern(n, init)
	}
	return nil, unsupported(target)
}

func (c *compiler) identifierStore(name string, init bool) (binder, error) {
	v, hops := c.lookup(name)
	if v == nil {
		return nil, fmt.Errorf("assignment to undeclared %s", name)
	}
	if v.alias != nil {
		return nil, fmt.Errorf("namespace alias %s reassigned", name)
	}
	slot := v.slot
	if !init && v.kind == kindFuncName {
		return func(*rt, *scope, any) error { return nil }, nil
	}
	if !init && v.kind == kindConst {
		return func(r *rt, s *scope, _ any) error {
			if s.up(hops).vars[slot] == tdz {
				return r.referenceError("Cannot access a variable before initialization")
			}
			return r.typeError("Assignment to constant variable.")
		}, nil
	}
	if init || (v.kind != kindLet) {
		if hops == 0 {
			return func(_ *rt, s *scope, val any) error { s.vars[slot] = val; return nil }, nil
		}
		return func(_ *rt, s *scope, val any) error { s.up(hops).vars[slot] = val; return nil }, nil
	}
	return func(r *rt, s *scope, val any) error {
		sc := s.up(hops)
		if sc.vars[slot] == tdz {
			return r.referenceError("Cannot access a variable before initialization")
		}
		sc.vars[slot] = val
		return nil
	}, nil
}

// withDefault splits `target = default` pattern elements.
func (c *compiler) withDefault(e ast.Expression, init bool) (binder, evalFn, error) {
	var def evalFn
	if a, ok := e.(*ast.AssignExpression); ok && a.Operator == token.ASSIGN {
		d, err := c.expressionNamed(a.Right, a.Left)
		if err != nil {
			return nil, nil, err
		}
		def, e = d, a.Left
	}
	b, err := c.bindingTarget(e, init)
	return b, def, err
}

func applyDefault(r *rt, s *scope, v any, def evalFn) (any, error) {
	if def != nil {
		if _, ok := v.(undefined); ok {
			return def(r, s)
		}
	}
	return v, nil
}

func (c *compiler) arrayPattern(n *ast.ArrayPattern, init bool) (binder, error) {
	type elem struct {
		bind binder
		def  evalFn
	}
	elems := make([]elem, len(n.Elements))
	for i, e := range n.Elements {
		if e == nil {
			continue
		}
		b, def, err := c.withDefault(e, init)
		if err != nil {
			return nil, err
		}
		elems[i] = elem{b, def}
	}
	var rest binder
	if n.Rest != nil {
		var err error
		if rest, err = c.bindingTarget(n.Rest, init); err != nil {
			return nil, err
		}
	}
	return func(r *rt, s *scope, v any) error {
		items, err := r.iterableItems(v)
		if err != nil {
			return err
		}
		for i, e := range elems {
			item := Undefined
			if i < len(items) {
				item = items[i]
			}
			if e.bind == nil {
				continue
			}
			item, err := applyDefault(r, s, item, e.def)
			if err != nil {
				return err
			}
			if err := e.bind(r, s, item); err != nil {
				return err
			}
		}
		if rest != nil {
			var tail []any
			if len(items) > len(elems) {
				if err := r.chargeArray(len(items) - len(elems)); err != nil {
					return err
				}
				tail = append(tail, items[len(elems):]...)
			}
			return rest(r, s, &array{items: nonNil(tail)})
		}
		return nil
	}, nil
}

func nonNil(a []any) []any {
	if a == nil {
		return []any{}
	}
	return a
}

// iterableItems materializes the values of an iterable for destructuring
// and spread: arrays by element, strings by code point.
func (r *rt) iterableItems(v any) ([]any, error) {
	switch t := v.(type) {
	case *array:
		return denseItems(t.items), nil
	case string:
		// One slot per code point, at most one per byte; each holds a
		// string of at most 4 bytes.
		if err := r.charge(len(t) * allocSlot); err != nil {
			return nil, err
		}
		out := make([]any, 0, len(t))
		for _, ch := range t {
			out = append(out, string(ch))
		}
		return out, nil
	}
	if it, ok := defaultIterator(v); ok {
		return it.drain(r)
	}
	return nil, r.notIterable(v)
}

func (c *compiler) objectPattern(n *ast.ObjectPattern, init bool) (binder, error) {
	type prop struct {
		key  evalFn
		name string
		bind binder
		def  evalFn
	}
	var props []prop
	static := map[string]bool{}
	dynamicKeys := false
	for _, p := range n.Properties {
		switch p := p.(type) {
		case *ast.PropertyShort:
			name := p.Name.Name.String()
			b, err := c.identifierStore(name, init)
			if err != nil {
				return nil, err
			}
			var def evalFn
			if p.Initializer != nil {
				if def, err = c.expressionNamed(p.Initializer, &p.Name); err != nil {
					return nil, err
				}
			}
			static[name] = true
			props = append(props, prop{name: name, bind: b, def: def})
		case *ast.PropertyKeyed:
			if p.Kind != ast.PropertyKindValue {
				return nil, unsupported(p)
			}
			pr := prop{}
			if p.Computed {
				k, err := c.expression(p.Key)
				if err != nil {
					return nil, err
				}
				pr.key = k
				dynamicKeys = true
			} else {
				name, ok := propertyKey(p.Key)
				if !ok {
					return nil, unsupported(p)
				}
				pr.name = name
				static[name] = true
			}
			b, def, err := c.withDefault(p.Value, init)
			if err != nil {
				return nil, err
			}
			pr.bind, pr.def = b, def
			props = append(props, pr)
		default:
			return nil, unsupported(p)
		}
	}
	var rest binder
	if n.Rest != nil {
		var err error
		if rest, err = c.bindingTarget(n.Rest, init); err != nil {
			return nil, err
		}
	}
	return func(r *rt, s *scope, v any) error {
		if isNullish(v) {
			return r.typeError("Value is not object coercible")
		}
		used := static
		if dynamicKeys && rest != nil {
			used = make(map[string]bool, len(static))
			for k := range static {
				used[k] = true
			}
		}
		for _, p := range props {
			name := p.name
			if p.key != nil {
				k, err := p.key(r, s)
				if err != nil {
					return err
				}
				if name, err = r.toPropertyKey(k); err != nil {
					return err
				}
				if rest != nil {
					used[name] = true
				}
			}
			val, err := r.getProp(v, name)
			if err != nil {
				return err
			}
			if val, err = applyDefault(r, s, val, p.def); err != nil {
				return err
			}
			if err := p.bind(r, s, val); err != nil {
				return err
			}
		}
		if rest != nil {
			out, err := r.newObj(0)
			if err != nil {
				return err
			}
			for _, k := range ownEnumerableKeys(v) {
				if used[k] {
					continue
				}
				val, err := r.getProp(v, k)
				if err != nil {
					return err
				}
				if err := r.put(out, k, val); err != nil {
					return err
				}
			}
			return rest(r, s, out)
		}
		return nil
	}, nil
}

func nullishName(v any) string {
	if v == nil {
		return "null"
	}
	return "undefined"
}

package toolscript

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/dop251/goja/ast"
)

type argsFn func(r *rt, s *scope) ([]any, error)

func (c *compiler) arguments(list []ast.Expression) (argsFn, error) {
	type arg struct {
		f      evalFn
		spread bool
	}
	args := make([]arg, len(list))
	hasSpread := false
	for i, e := range list {
		a := arg{}
		if sp, ok := e.(*ast.SpreadElement); ok {
			a.spread, hasSpread = true, true
			e = sp.Expression
		}
		f, err := c.expression(e)
		if err != nil {
			return nil, err
		}
		a.f = f
		args[i] = a
	}
	if !hasSpread {
		fs := make([]evalFn, len(args))
		for i, a := range args {
			fs[i] = a.f
		}
		return func(r *rt, s *scope) ([]any, error) {
			out := make([]any, len(fs))
			for i, f := range fs {
				v, err := f(r, s)
				if err != nil {
					return nil, err
				}
				out[i] = v
			}
			return out, nil
		}, nil
	}
	return func(r *rt, s *scope) ([]any, error) {
		out := make([]any, 0, len(args))
		for _, a := range args {
			v, err := a.f(r, s)
			if err != nil {
				return nil, err
			}
			if a.spread {
				items, err := r.iterableItems(v)
				if err != nil {
					return nil, err
				}
				out = append(out, items...)
				continue
			}
			out = append(out, v)
		}
		return out, nil
	}, nil
}

// Methods that exist on Goja's prototypes but are outside the subset. Calls
// to these names decline compilation instead of diverging at run time.
var declinedMethods = map[string]bool{
	"copyWithin": true, "matchAll": true, "normalize": true,
	"toExponential": true, "isPrototypeOf": true, "propertyIsEnumerable": true,
}

// call compiles a call expression. optional is set for a?.() callees.
func (c *compiler) call(n *ast.CallExpression, _ bool) (evalFn, error) {
	callee := n.Callee
	optionalCallee := false
	if o, ok := callee.(*ast.Optional); ok {
		optionalCallee = true
		callee = o.Expression
	}
	if !optionalCallee {
		if path, ok := c.namespacePath(callee); ok && (c.opts.Bindings == nil || c.staticTool(path)) {
			return c.toolCall(path, n)
		}
		if f, ok, err := c.globalCall(callee, n); ok || err != nil {
			return f, err
		}
	}
	args, err := c.arguments(n.ArgumentList)
	if err != nil {
		return nil, err
	}
	switch m := callee.(type) {
	case *ast.DotExpression, *ast.BracketExpression:
		var name string
		if d, ok := m.(*ast.DotExpression); ok {
			name = d.Identifier.Name.String()
		} else if k, ok := staticKey(m.(*ast.BracketExpression).Member); ok {
			name = k
		}
		if declinedMethods[name] {
			return nil, fmt.Errorf("unsupported method %s", name)
		}
		obj, key, err := c.memberParts(m)
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
			if optionalCallee {
				f, err := r.getMethodValue(o, k)
				if err != nil {
					return nil, err
				}
				if isNullish(f) {
					return nil, errShortCircuit
				}
			}
			a, err := args(r, s)
			if err != nil {
				return nil, err
			}
			return r.callMethod(o, k, a)
		}, nil
	}
	fn, err := c.expression(callee)
	if err != nil {
		return nil, err
	}
	var direct []evalFn
	for _, e := range n.ArgumentList {
		if _, spread := e.(*ast.SpreadElement); spread {
			direct = nil
			break
		}
		f, err := c.expression(e)
		if err != nil {
			return nil, err
		}
		direct = append(direct, f)
	}
	spreadFree := len(direct) == len(n.ArgumentList)
	return func(r *rt, s *scope) (any, error) {
		f, err := fn(r, s)
		if err != nil {
			return nil, err
		}
		if optionalCallee && isNullish(f) {
			return nil, errShortCircuit
		}
		if cl, ok := f.(*function); ok && spreadFree && cl.native == nil && cl.code.direct {
			return r.invokeDirect(cl, direct, s)
		}
		a, err := args(r, s)
		if err != nil {
			return nil, err
		}
		return r.call(f, a)
	}, nil
}

// getMethodValue reads a callee for optional calls without throwing on
// missing members of primitives.
func (r *rt) getMethodValue(o any, k string) (any, error) {
	if isNullish(o) {
		return nil, r.typeError("Cannot read property '" + k + "' of undefined or null")
	}
	return r.getProp(o, k)
}

// staticTool reports whether a namespace path resolves to a tool at compile
// time (or is mcp.call with a literal name); others use runtime lookup.
func (c *compiler) staticTool(path []string) bool {
	if len(path) == 2 && (path[0] == "mcp" || path[0] == "tools") && path[1] == "call" {
		return c.opts.ResolveCall != nil
	}
	if c.opts.Resolve == nil {
		return false
	}
	_, ok := c.opts.Resolve(path)
	return ok
}

func (c *compiler) toolCall(path []string, n *ast.CallExpression) (evalFn, error) {
	var name string
	var ok bool
	list := n.ArgumentList
	if len(path) == 2 && (path[0] == "mcp" || path[0] == "tools") && path[1] == "call" {
		if c.opts.ResolveCall == nil || len(list) < 1 {
			return nil, unsupported(n)
		}
		raw, literal := list[0].(*ast.StringLiteral)
		if !literal {
			return nil, unsupported(n)
		}
		name, ok = c.opts.ResolveCall(raw.Value.String())
		list = list[1:]
	} else {
		if c.opts.Resolve == nil {
			return nil, unsupported(n)
		}
		name, ok = c.opts.Resolve(path)
	}
	if !ok {
		return nil, fmt.Errorf("unknown binding %s", strings.Join(path, "."))
	}
	for _, a := range list {
		if _, spread := a.(*ast.SpreadElement); spread {
			return nil, unsupported(a)
		}
	}
	args, err := c.arguments(list)
	if err != nil {
		return nil, err
	}
	return func(r *rt, s *scope) (any, error) {
		a, err := args(r, s)
		if err != nil {
			return nil, err
		}
		var arg any = Undefined
		if len(a) > 0 {
			arg = a[0]
		}
		return r.callTool(name, arg)
	}, nil
}

// globalCall compiles calls to unshadowed globals: host functions, console,
// JSON/Object/Array/Math/Number/String helpers, Promise batches and errors.
func (c *compiler) globalCall(callee ast.Expression, n *ast.CallExpression) (evalFn, bool, error) {
	if root := rootName(callee); root == "" {
		return nil, false, nil
	} else if v, _ := c.lookup(root); v != nil {
		return nil, false, nil
	}
	path, ok := staticPath(callee)
	if !ok {
		return nil, false, nil
	}
	if v, _ := c.lookup(path[0]); v != nil {
		return nil, false, nil
	}
	if len(path) == 4 && path[1] == "prototype" && path[3] == "call" {
		f, err := c.prototypeCall(path[0], path[2], n)
		return f, true, err
	}
	if path[0] == "Intl" && (len(path) == 2 || len(path) == 3) {
		full := strings.Join(path, ".")
		impl, ok := staticFunctions[full]
		if !ok {
			if impl, ok = intlStatic(full); !ok {
				return nil, true, fmt.Errorf("unsupported %s", full)
			}
		}
		args, err := c.arguments(n.ArgumentList)
		if err != nil {
			return nil, true, err
		}
		return func(r *rt, s *scope) (any, error) {
			a, err := args(r, s)
			if err != nil {
				return nil, err
			}
			return impl(r, a)
		}, true, nil
	}
	if len(path) > 2 {
		return nil, false, nil
	}
	if len(path) == 1 {
		name := path[0]
		if spec, ok := c.opts.HostFunctions[name]; ok {
			f, err := c.hostCall(name, spec, n)
			return f, true, err
		}
		if name == "Date" {
			// Date(...) called as a function ignores its arguments.
			args, err := c.arguments(n.ArgumentList)
			if err != nil {
				return nil, true, err
			}
			return func(r *rt, s *scope) (any, error) {
				if _, err := args(r, s); err != nil {
					return nil, err
				}
				return r.dateCall()
			}, true, nil
		}
		if name == "Array" {
			args, err := c.arguments(n.ArgumentList)
			if err != nil {
				return nil, true, err
			}
			return func(r *rt, s *scope) (any, error) {
				a, err := args(r, s)
				if err != nil {
					return nil, err
				}
				return r.newArrayFromArgs(a)
			}, true, nil
		}
		if name == "RegExp" {
			args, err := c.arguments(n.ArgumentList)
			if err != nil {
				return nil, true, err
			}
			return func(r *rt, s *scope) (any, error) {
				a, err := args(r, s)
				if err != nil {
					return nil, err
				}
				if rx, ok := arg(a, 0).(*regexpValue); ok && isUndefined(arg(a, 1)) {
					return rx, nil
				}
				return r.newRegExp(a)
			}, true, nil
		}
		if isErrorConstructor(name) {
			args, err := c.arguments(n.ArgumentList)
			if err != nil {
				return nil, true, err
			}
			return func(r *rt, s *scope) (any, error) {
				a, err := args(r, s)
				if err != nil {
					return nil, err
				}
				return r.makeError(name, a)
			}, true, nil
		}
		if _, ok := globalFunctionValues[name]; !ok {
			return nil, true, fmt.Errorf("unsupported global function %s", name)
		}
		args, err := c.arguments(n.ArgumentList)
		if err != nil {
			return nil, true, err
		}
		return func(r *rt, s *scope) (any, error) {
			a, err := args(r, s)
			if err != nil {
				return nil, err
			}
			return r.callGlobal(name, a)
		}, true, nil
	}
	if len(path) != 2 || !isGlobalNamespace(path[0]) {
		return nil, false, nil
	}
	full := path[0] + "." + path[1]
	if path[0] == "Promise" {
		f, err := c.batch(path[1], n)
		return f, true, err
	}
	if path[0] == "console" {
		switch path[1] {
		case "log", "info", "warn", "error", "debug":
		default:
			return nil, true, fmt.Errorf("unsupported %s", full)
		}
		args, err := c.arguments(n.ArgumentList)
		if err != nil {
			return nil, true, err
		}
		level := path[1]
		return func(r *rt, s *scope) (any, error) {
			a, err := args(r, s)
			if err != nil {
				return nil, err
			}
			return Undefined, r.console(level, a)
		}, true, nil
	}
	impl, ok := staticFunctions[full]
	if !ok {
		return nil, true, fmt.Errorf("unsupported %s", full)
	}
	if full == "JSON.parse" && len(n.ArgumentList) > 1 {
		return nil, true, fmt.Errorf("JSON.parse reviver")
	}
	args, err := c.arguments(n.ArgumentList)
	if err != nil {
		return nil, true, err
	}
	return func(r *rt, s *scope) (any, error) {
		a, err := args(r, s)
		if err != nil {
			return nil, err
		}
		return impl(r, a)
	}, true, nil
}

func (c *compiler) hostCall(name string, spec HostFunction, n *ast.CallExpression) (evalFn, error) {
	if spec.MinArgs < 0 || spec.MaxArgs < spec.MinArgs || len(n.ArgumentList) < spec.MinArgs || len(n.ArgumentList) > spec.MaxArgs {
		return nil, unsupported(n)
	}
	for _, index := range spec.LiteralStringArgs {
		if index < 0 || index >= len(n.ArgumentList) {
			return nil, unsupported(n)
		}
		if _, ok := n.ArgumentList[index].(*ast.StringLiteral); !ok {
			return nil, unsupported(n)
		}
	}
	for _, a := range n.ArgumentList {
		if _, spread := a.(*ast.SpreadElement); spread {
			return nil, unsupported(a)
		}
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
		for _, i := range spec.StringArgs {
			if i < len(a) && !isUndefined(a[i]) {
				if a[i], err = r.toString(a[i]); err != nil {
					return nil, err
				}
			}
		}
		for _, i := range spec.JSONArgs {
			if i < len(a) && !isUndefined(a[i]) {
				text, err := r.stringify(a[i], Undefined, Undefined)
				if err != nil {
					return nil, err
				}
				str, _ := text.(string)
				a[i] = JSONText(str)
			}
		}
		return r.callHost(name, a)
	}, nil
}

// ---- batches ----

// batch compiles Promise.all/allSettled. Inline arrays and .map callbacks are
// evaluated per item (concurrently when the host allows and the items cannot
// observe each other); any other array value is taken as already settled.
func (c *compiler) batch(kind string, n *ast.CallExpression) (evalFn, error) {
	if !c.opts.Batches || (kind != "all" && kind != "allSettled") || len(n.ArgumentList) != 1 || c.batchDepth > 0 {
		return nil, unsupported(n)
	}
	settled := kind == "allSettled"
	c.batchDepth++
	defer func() { c.batchDepth-- }()
	arg := n.ArgumentList[0]
	isolated := c.isolated(arg)
	if lit, ok := arg.(*ast.ArrayLiteral); ok {
		items := make([]evalFn, len(lit.Value))
		for i, e := range lit.Value {
			if e == nil {
				return nil, unsupported(lit)
			}
			if _, spread := e.(*ast.SpreadElement); spread {
				return nil, unsupported(lit)
			}
			f, err := c.expression(e)
			if err != nil {
				return nil, err
			}
			items[i] = f
		}
		return func(r *rt, s *scope) (any, error) {
			// Workers share s: capture it here so they never mark it.
			if err := r.captureScope(s); err != nil {
				return nil, err
			}
			return r.runBatch(len(items), settled, isolated, func(w *rt, i int) (any, error) { return items[i](w, s) })
		}, nil
	}
	if call, ok := arg.(*ast.CallExpression); ok {
		if dot, ok := call.Callee.(*ast.DotExpression); ok && dot.Identifier.Name.String() == "map" && len(call.ArgumentList) == 1 && isFunctionLiteral(call.ArgumentList[0]) {
			if _, ns := c.namespacePath(dot.Left); !ns {
				recv, err := c.expression(dot.Left)
				if err != nil {
					return nil, err
				}
				cb, err := c.expression(call.ArgumentList[0])
				if err != nil {
					return nil, err
				}
				return func(r *rt, s *scope) (any, error) {
					v, err := recv(r, s)
					if err != nil {
						return nil, err
					}
					a, ok := v.(*array)
					if !ok {
						// Not an array: defer to the ordinary method semantics.
						f, err := cb(r, s)
						if err != nil {
							return nil, err
						}
						out, err := r.callMethod(v, "map", []any{f})
						if err != nil {
							return nil, err
						}
						return r.settle(out, settled)
					}
					fv, err := cb(r, s)
					if err != nil {
						return nil, err
					}
					f := fv.(*function) // a closure over s: s is captured
					items := append([]any(nil), denseItems(a.items)...)
					return r.runBatch(len(items), settled, isolated, func(w *rt, i int) (any, error) {
						return w.call(f, []any{items[i], float64(i), a})
					})
				}, nil
			}
		}
	}
	f, err := c.expression(arg)
	if err != nil {
		return nil, err
	}
	return func(r *rt, s *scope) (any, error) {
		v, err := f(r, s)
		if err != nil {
			return nil, err
		}
		return r.settle(v, settled)
	}, nil
}

// settle resolves a batch over values that are already computed.
func (r *rt) settle(v any, settled bool) (any, error) {
	items, err := r.iterableItems(v)
	if err != nil {
		return nil, err
	}
	n := allocHeader + len(items)*allocSlot
	if settled {
		n += len(items) * (allocHeader + 2*allocProp)
	}
	if err := r.charge(n); err != nil {
		return nil, err
	}
	out := make([]any, len(items))
	for i, item := range items {
		if settled {
			o := newObject(2)
			o.set("status", "fulfilled")
			o.set("value", item)
			item = o
		}
		out[i] = item
	}
	return &array{items: out}, nil
}

func (r *rt) runBatch(size int, settled, isolated bool, eval func(w *rt, i int) (any, error)) (any, error) {
	if size > r.x.opts.MaxItems {
		return nil, errors.New("batch item limit exceeded")
	}
	n := allocHeader + size*allocSlot
	if settled {
		n += size * (allocHeader + 2*allocProp)
	}
	if err := r.charge(n); err != nil {
		return nil, err
	}
	out := make([]any, size)
	errs := make([]error, size)
	runOne := func(w *rt, i int) {
		v, err := eval(w, i)
		if settled {
			var thrown *Throw
			switch {
			case err == nil:
				o := newObject(2)
				o.set("status", "fulfilled")
				o.set("value", v)
				out[i] = o
				return
			case errors.As(err, &thrown):
				o := newObject(2)
				o.set("status", "rejected")
				o.set("reason", thrown.reason())
				out[i] = o
				return
			}
		}
		out[i], errs[i] = v, err
	}
	workers := 1
	if isolated {
		workers = min(size, r.x.opts.Parallelism)
	}
	if workers <= 1 {
		// Every item starts, as with JS promises; the first failure in input
		// order wins. Uncatchable errors (limits, aborts) stop immediately.
		var first error
		for i := range size {
			runOne(r, i)
			var thrown *Throw
			if err := errs[i]; err != nil && !errors.As(err, &thrown) {
				return nil, err
			}
			if first == nil && errs[i] != nil {
				first = errs[i]
			}
		}
		if first != nil {
			return nil, first
		}
		return &array{items: out}, nil
	}
	var next atomic.Int64
	var wg sync.WaitGroup
	seq := newSequencer(size, r)
	for range workers {
		wg.Add(1)
		w := &rt{ctx: r.ctx, x: r.x, depth: r.depth, seq: seq}
		go func() {
			defer wg.Done()
			defer func() {
				if p := recover(); p != nil {
					r.x.recordPanic(p)
				}
			}()
			for {
				i := int(next.Add(1) - 1)
				if i >= size {
					r.x.steps.Add(int64(w.pending))
					r.x.alloc.Add(int64(w.allocPending))
					return
				}
				w.item, w.logs = i, nil
				func() {
					// Always release the item, even on panic, so later items
					// waiting for their turn cannot deadlock.
					defer func() { seq.finish(i, w.logs) }()
					runOne(w, i)
				}()
			}
		}()
	}
	wg.Wait()
	if err := r.x.panicErr(); err != nil {
		return nil, err
	}
	if err := r.ctx.Err(); err != nil {
		return nil, err
	}
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return &array{items: out}, nil
}

// Mutating array methods; batch items calling them run sequentially.
var mutatingMethods = map[string]bool{
	"push": true, "pop": true, "shift": true, "unshift": true, "splice": true, "sort": true,
	"reverse": true, "fill": true, "copyWithin": true, "add": true, "set": true, "delete": true, "clear": true,
}

// isolated reports whether batch items cannot observe each other's effects:
// no writes outside names declared inside the batch, no mutating methods,
// no calls into outer user functions and no console output.
func (c *compiler) isolated(node ast.Node) bool {
	local := map[string]bool{}
	walkAST(node, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.Binding:
			collectNames(n.Target, local)
		case *ast.ForDeclaration:
			collectNames(n.Target, local)
		case *ast.CatchStatement:
			if n.Parameter != nil {
				collectNames(n.Parameter, local)
			}
		case *ast.FunctionLiteral:
			if n.Name != nil {
				local[n.Name.Name.String()] = true
			}
		}
		return true
	})
	for name := range local {
		if v, _ := c.lookup(name); v != nil {
			return false // shadowing makes local/outer indistinguishable here
		}
	}
	ok := true
	walkAST(node, func(n ast.Node) bool {
		if !ok {
			return false
		}
		switch n := n.(type) {
		case *ast.AssignExpression:
			if id, isID := n.Left.(*ast.Identifier); !isID || !local[id.Name.String()] {
				ok = false
			}
		case *ast.UnaryExpression:
			if n.Operator.String() == "++" || n.Operator.String() == "--" || n.Operator.String() == "delete" {
				if id, isID := n.Operand.(*ast.Identifier); !isID || !local[id.Name.String()] {
					ok = false
				}
			}
		case *ast.CallExpression:
			// Host functions, SequentialTools and console output do not
			// force a batch sequential: the runtime sequencer orders them.
			switch cal := n.Callee.(type) {
			case *ast.Identifier:
				if v, _ := c.lookup(cal.Name.String()); v != nil && !local[cal.Name.String()] {
					ok = false
				}
			case *ast.DotExpression:
				name := cal.Identifier.Name.String()
				if mutatingMethods[name] || strings.HasPrefix(name, "set") {
					ok = false // includes Date setters
				}
				if p, isPath := staticPath(cal.Left); isPath && len(p) == 1 && p[0] == "Object" && name == "assign" {
					ok = false
				}
				if id, isID := cal.Left.(*ast.Identifier); isID {
					if v, _ := c.lookup(id.Name.String()); v != nil && v.alias == nil && !local[id.Name.String()] && arrayMethods[name] == nil && stringMethods[name] == nil {
						ok = false // may be a user closure stored on an outer object
					}
				}
			default:
				ok = false
			}
		}
		return ok
	})
	return ok
}

func collectNames(target ast.Expression, into map[string]bool) {
	walkAST(target, func(n ast.Node) bool {
		if id, ok := n.(*ast.Identifier); ok {
			into[id.Name.String()] = true
		}
		return true
	})
}

// prototypeCall compiles Ctor.prototype.method.call(receiver, ...args), the
// pre-ES2015 way to borrow a built-in (e.g. Object.prototype.hasOwnProperty).
func (c *compiler) prototypeCall(ctor, method string, n *ast.CallExpression) (evalFn, error) {
	switch ctor {
	case "Object":
		switch method {
		case "hasOwnProperty", "toString":
		default:
			return nil, fmt.Errorf("unsupported Object.prototype.%s.call", method)
		}
	case "Array":
		if arrayMethods[method] == nil {
			return nil, fmt.Errorf("unsupported Array.prototype.%s.call", method)
		}
	case "String":
		if stringMethods[method] == nil {
			return nil, fmt.Errorf("unsupported String.prototype.%s.call", method)
		}
	default:
		return nil, fmt.Errorf("unsupported %s.prototype.%s.call", ctor, method)
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
		recv := arg(a, 0)
		var rest []any
		if len(a) > 1 {
			rest = a[1:]
		}
		switch ctor {
		case "Object":
			if method == "toString" {
				return classString(recv), nil
			}
			if isNullish(recv) {
				return nil, r.typeError("Cannot convert undefined or null to object")
			}
			return r.callBuiltinMethodOwn(recv, arg(rest, 0))
		case "Array":
			arr, ok := recv.(*array)
			if !ok {
				return nil, errRuntimeUnsupported("Array.prototype." + method + " on a non-array")
			}
			return arrayMethods[method](r, arr, rest)
		}
		str, err := r.toString(recv)
		if isNullish(recv) {
			return nil, r.typeError("String.prototype." + method + " called on null or undefined")
		}
		if err != nil {
			return nil, err
		}
		return stringMethods[method](r, str, rest)
	}, nil
}

// classString implements Object.prototype.toString.
func classString(v any) string {
	switch t := v.(type) {
	case nil:
		return "[object Null]"
	case undefined:
		return "[object Undefined]"
	case string:
		return "[object String]"
	case float64:
		return "[object Number]"
	case bool:
		return "[object Boolean]"
	case *array:
		return "[object Array]"
	case *function:
		return "[object Function]"
	case *regexpValue:
		return "[object RegExp]"
	case *dateValue:
		return "[object Date]"
	case *collection:
		if t.isMap {
			return "[object Map]"
		}
		return "[object Set]"
	case *iterator:
		return "[object " + t.name + " Iterator]"
	case *object:
		if t.errName != "" {
			return "[object Error]"
		}
	case *intlObject:
		return "[object Intl." + t.kind.name + "]"
	}
	return "[object Object]"
}

// sequencer orders the effects of a parallel batch that must not overlap or
// reorder: host functions, tools the host marks sequential, and console
// output. Item i may perform such an effect only after items 0..i-1 have
// finished; its console lines are released in item order as items finish.
// Items are claimed in index order, so every earlier item is already running
// or done and waiting can never deadlock.
type sequencer struct {
	mu   sync.Mutex
	cond *sync.Cond
	done []bool
	logs [][]consoleLine
	r    *rt
	turn int
}

type consoleLine struct{ level, line string }

func newSequencer(n int, r *rt) *sequencer {
	q := &sequencer{done: make([]bool, n), logs: make([][]consoleLine, n), r: r}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func (q *sequencer) wait(i int) {
	q.mu.Lock()
	for q.turn < i {
		q.cond.Wait()
	}
	q.mu.Unlock()
}

func (q *sequencer) finish(i int, logs []consoleLine) {
	q.mu.Lock()
	q.done[i], q.logs[i] = true, logs
	for q.turn < len(q.done) && q.done[q.turn] {
		for _, l := range q.logs[q.turn] {
			q.r.emitConsole(l.level, l.line)
		}
		q.logs[q.turn] = nil
		q.turn++
	}
	q.cond.Broadcast()
	q.mu.Unlock()
}

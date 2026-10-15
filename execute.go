package toolscript

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/text/collate"
)

// Undefined represents an absent JS value. Marshal omits it in objects and
// encodes it as null in arrays and at the top level.
var Undefined any = undefined{}

type undefined struct{}

// Object preserves hosts whose property view differs from their JSON export
// (for example, a Go struct with omitzero fields exposed through Goja).
// Fields and Export must contain only immutable JSON data. Order optionally
// lists Fields keys in declaration order for enumeration and JSON.stringify.
type Object struct {
	Fields map[string]any
	Export any
	Order  []string
}

// Dispatch exposes named tool capabilities to a plan. Arguments/results must
// be JSON-compatible Go values (float64 numbers), or Undefined. Implementations
// must honor context cancellation; with Parallelism > 1 they must be thread safe.
type Dispatch func(context.Context, string, any) (any, error)

// ExecuteOptions bounds each execution independently. Zero values use defaults.
type ExecuteOptions struct {
	// HostDispatch handles only explicitly admitted global calls. It has the same
	// cancellation, immutable-data and concurrency contract as Dispatch.
	HostDispatch func(context.Context, string, []any) (any, error)
	Dispatch     Dispatch
	// Console receives console.log/info/warn/error/debug lines. Arguments are
	// formatted like Goja's exported values: objects and arrays as compact JSON
	// (encoding/json of the export), everything else with JavaScript ToString,
	// joined by single spaces. Nil discards output.
	Console      func(level, line string)
	Fatal        func(error) bool
	MaxHostCalls int // default 64, independent of the tool-call budget
	MaxCalls     int // default 64
	MaxSteps     int // default 1,000,000 statements, calls and loop iterations
	MaxItems     int // default 1024 items per Promise.all/allSettled batch
	Parallelism  int // default 1; concurrent items per Promise batch (host-chosen)
	// MaxArrayLength and MaxStringBytes bound values built by the script
	// (defaults 1<<24 elements and 16 MiB). Exceeding them throws RangeError.
	MaxArrayLength int
	MaxStringBytes int
	MaxCallDepth   int // default 1000 nested function calls
	// MaxAllocBytes bounds the memory one execution may allocate for values
	// the script can reach (strings, arrays, objects, closures, Map/Set
	// entries, JSON.parse output, converted tool and host results; see
	// alloc.go). Charges are never credited back when a value becomes
	// garbage, so the total bounds what the run can hold live at once.
	// Exceeding it throws a catchable RangeError ("allocation budget
	// exceeded"), and every later allocation in the run throws as well.
	// Zero (the default) means unlimited.
	MaxAllocBytes int64
	// Now is the clock for Date (default time.Now); Location is the local
	// time zone for Date (default time.Local), both as in Goja.
	Now      func() time.Time
	Location *time.Location
	// Random is the source for Math.random (default math/rand/v2); it must be
	// safe for concurrent use when Parallelism > 1.
	Random func() float64
}

// Result retains the call count and fatal status even after execution fails.
type Result struct {
	Value     any
	Calls     int
	HostCalls int
	Steps     int // statements, calls and loop iterations executed
	// Allocated is the bytes charged against MaxAllocBytes (0 when unlimited).
	Allocated int64
	Aborted   bool
}

type execution struct {
	sequential func(string) bool // CompileOptions.SequentialTools
	namespace  any               // the mcp/tools object, when the program uses it as a value
	opts       ExecuteOptions
	fatal      error
	panicked   error
	calls      atomic.Int64
	hostCalls  atomic.Int64
	steps      atomic.Int64
	fatalMu    sync.Mutex
	maxString  int
	maxItems   int
	maxDepth   int
	batch      int // steps per publication; 1 for tiny budgets
	aborted    atomic.Bool
	alloc      atomic.Int64 // bytes charged against maxAlloc
	allocSpent atomic.Bool  // alloc passed maxAlloc: every charge throws
	maxAlloc   int64        // 0 = unlimited
	allocBatch int          // bytes charged per publication (see charge)
}

// rt is the per-goroutine interpreter state for one execution.
type rt struct {
	ctx     context.Context
	x       *execution
	joining map[*array]bool // arrays being converted by join/toLocaleString (cycle guard)
	nesting int             // conversion levels active on this stack (see enterNesting)
	depth   int
	pending int // steps not yet published to x.steps
	// allocPending is bytes charged but not yet published to x.alloc.
	allocPending int
	frames  []*scope8
	coll    *collate.Collator
	label   string        // target of a labeled break/continue in flight
	seq     *sequencer    // set on workers of a parallel batch
	logs    []consoleLine // this batch item's buffered console output
	item    int           // this worker's current batch item
}

// Throw is an uncaught JavaScript exception. Tool and host failures surface
// as Goja-style GoError objects and unwrap to the original error.
type Throw struct {
	Value any
	cause error
	text  *string // ToString(Value) when it ran script code (own toString)
}

// Error has no live runtime (the host usually calls it after the script has
// finished), so it renders the thrown value with the bounded detached
// formatter (see errorToString): a cyclic or deeply nested thrown value cannot
// overflow the host's stack when the host formats the error.
func (t *Throw) Error() string {
	if t.cause != nil {
		return t.cause.Error()
	}
	if t.text != nil {
		return *t.text
	}
	switch v := t.Value.(type) {
	case *object:
		if v.errName != "" {
			return errorToString(v)
		}
		return "[object Object]"
	case string:
		return v
	}
	if s, ok := detachedString(t.Value); ok {
		return s
	}
	return "exception"
}

func (t *Throw) Unwrap() error { return t.cause }

// reason renders an allSettled rejection: host error text, or the thrown value.
func (t *Throw) reason() any {
	if t.cause != nil {
		return t.cause.Error()
	}
	return t.Value
}

func (r *rt) throw(name, msg string) error    { return &Throw{Value: newError(name, msg)} }
func (r *rt) typeError(msg string) error      { return r.throw("TypeError", msg) }
func (r *rt) rangeError(msg string) error     { return r.throw("RangeError", msg) }
func (r *rt) referenceError(msg string) error { return r.throw("ReferenceError", msg) }
func (r *rt) syntaxError(msg string) error    { return r.throw("SyntaxError", msg) }

// TypeError lets Dispatch/HostDispatch raise a JavaScript TypeError (as a
// JS host function would) instead of a GoError. It stays catchable.
type TypeError struct{ Message string }

func (e *TypeError) Error() string { return "TypeError: " + e.Message }

func goError(err error) *Throw {
	e := newError("GoError", err.Error())
	e.set("value", newObject(0))
	return &Throw{Value: e, cause: err}
}

var (
	errStackOverflow = errors.New("RangeError: Maximum call stack size exceeded")
	errStepLimit     = errors.New("step limit exceeded")
	errAborted       = errors.New("tool execution aborted")
)

// Execute runs a validated program. It never retries a call. Parallel batches
// join all workers before returning, including on errors; callers needing a
// hard deadline must retain admission until exit. Execution errors must never
// trigger fallback, except ErrRuntimeUnsupported before any effect.
func (p *Program) Execute(ctx context.Context, opts ExecuteOptions) (res Result, err error) {
	if opts.MaxHostCalls <= 0 {
		opts.MaxHostCalls = 64
	}
	if opts.MaxCalls <= 0 {
		opts.MaxCalls = 64
	}
	if opts.MaxSteps <= 0 {
		opts.MaxSteps = 1_000_000
	}
	if opts.MaxItems <= 0 {
		opts.MaxItems = 1024
	}
	if opts.Parallelism <= 0 {
		opts.Parallelism = 1
	}
	if opts.MaxArrayLength <= 0 {
		opts.MaxArrayLength = 1 << 24
	}
	if opts.MaxStringBytes <= 0 {
		opts.MaxStringBytes = 16 << 20
	}
	if opts.MaxCallDepth <= 0 {
		opts.MaxCallDepth = 1000
	}
	x := &execution{opts: opts, maxString: opts.MaxStringBytes, maxItems: opts.MaxArrayLength, maxDepth: opts.MaxCallDepth, batch: 64}
	if opts.MaxSteps < 64*1024 {
		x.batch = 1
	}
	if opts.MaxAllocBytes > 0 {
		// Publishing every 1/64th of the budget (at most 64 KiB) bounds how
		// far parallel workers can overrun it before one of them notices.
		x.maxAlloc = opts.MaxAllocBytes
		x.allocBatch = int(min(allocBatchMax, opts.MaxAllocBytes/64))
	}
	r := &rt{ctx: ctx, x: x}
	x.sequential = p.sequential
	if p.namespace != nil {
		x.namespace = p.buildNamespace()
	} else {
		x.namespace = Undefined
	}
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("toolscript: internal error: %v", p)
			res = Result{Value: Undefined, Calls: int(x.calls.Load()), HostCalls: int(x.hostCalls.Load()), Steps: int(x.steps.Load()), Allocated: x.alloc.Load(), Aborted: x.aborted.Load()}
		}
	}()
	var v any = Undefined
	if err = ctx.Err(); err == nil {
		v, err = r.invoke(&function{code: p.main}, nil)
		x.steps.Add(int64(r.pending))
		x.alloc.Add(int64(r.allocPending))
	}
	if err == nil {
		v, err = exportValue(v, r.location())
	}
	var thrown *Throw
	if errors.As(err, &thrown) && thrown.cause == nil {
		// Goja's runner reports ToString(value), which runs a script-defined
		// toString/valueOf; Error() alone cannot call back into the script.
		if o, ok := thrown.Value.(*object); ok && userConversion(o) {
			if s, serr := r.toString(o); serr == nil {
				thrown.text = &s
			}
		}
	}
	if x.fatal != nil {
		err = x.fatal
	}
	if err != nil {
		v = Undefined
	}
	return Result{Value: v, Calls: int(x.calls.Load()), HostCalls: int(x.hostCalls.Load()), Steps: int(x.steps.Load()), Allocated: x.alloc.Load(), Aborted: x.aborted.Load()}, err
}

// tick charges one step. Steps are accounted locally and published in small
// batches, so the shared atomics stay off the per-statement path.
func (r *rt) tick() error {
	r.pending++
	if r.pending < r.x.batch {
		return nil
	}
	return r.flush()
}

func (r *rt) flush() error {
	x := r.x
	n := x.steps.Add(int64(r.pending))
	r.pending = 0
	if n > int64(x.opts.MaxSteps) {
		return errStepLimit
	}
	if x.aborted.Load() {
		return errAborted
	}
	return r.ctx.Err()
}

func (x *execution) recordPanic(p any) {
	x.fatalMu.Lock()
	defer x.fatalMu.Unlock()
	if x.panicked == nil {
		x.panicked = fmt.Errorf("toolscript: internal error: %v", p)
	}
}

func (x *execution) panicErr() error {
	x.fatalMu.Lock()
	defer x.fatalMu.Unlock()
	return x.panicked
}

// invoke calls a user function or the program body.
func (r *rt) invoke(f *function, args []any) (any, error) {
	if err := r.tick(); err != nil {
		return nil, err
	}
	if r.depth >= r.x.maxDepth {
		return nil, errStackOverflow
	}
	r.depth++
	v, err := r.invokeBody(f, args)
	r.depth--
	return v, err
}

// invokeDirect calls a closure with simple parameters, evaluating argument
// expressions straight into the callee's frame (no argument slice).
func (r *rt) invokeDirect(f *function, argFns []evalFn, caller *scope) (any, error) {
	if err := r.tick(); err != nil {
		return nil, err
	}
	if r.depth >= r.x.maxDepth {
		return nil, errStackOverflow
	}
	code := f.code
	var frame *scope
	var pooled *scope8
	if code.leaf && len(code.init) <= 8 && len(r.frames) > 0 {
		pooled = r.frames[len(r.frames)-1]
		r.frames = r.frames[:len(r.frames)-1]
		pooled.parent, pooled.vars = f.scope, pooled.buf[:len(code.init)]
		copy(pooled.vars, code.init)
		frame = &pooled.scope
	} else if code.leaf && len(code.init) <= 8 {
		pooled = &scope8{}
		pooled.parent, pooled.vars = f.scope, pooled.buf[:len(code.init)]
		copy(pooled.vars, code.init)
		frame = &pooled.scope
	} else {
		frame = newScope(f.scope, code.init)
	}
	for i, a := range argFns {
		v, err := a(r, caller)
		if err != nil {
			return nil, err
		}
		if i < len(code.simple) {
			frame.vars[code.simple[i]] = v
		}
	}
	r.depth++
	v, err := r.runBody(f, frame)
	r.depth--
	if pooled != nil && len(r.frames) < 64 {
		// A leaf frame is unreachable after return: nothing captured it.
		pooled.buf = [8]any{}
		pooled.parent = nil
		pooled.charged = false
		r.frames = append(r.frames, pooled)
	}
	return v, err
}

func (r *rt) invokeBody(f *function, args []any) (any, error) {
	code := f.code
	s := newScope(f.scope, code.init)
	for i, p := range code.params {
		var v any = Undefined
		if i < len(args) {
			v = args[i]
		}
		if code.defaults[i] != nil {
			if _, ok := v.(undefined); ok {
				var err error
				if v, err = code.defaults[i](r, s); err != nil {
					return nil, err
				}
			}
		}
		if slot := code.simple[i]; slot >= 0 {
			s.vars[slot] = v
			continue
		}
		if err := p(r, s, v); err != nil {
			return nil, err
		}
	}
	if code.rest != nil {
		var tail []any
		if len(args) > len(code.params) {
			if err := r.chargeArray(len(args) - len(code.params)); err != nil {
				return nil, err
			}
			tail = append(tail, args[len(code.params):]...)
		}
		if err := code.rest(r, s, &array{items: nonNil(tail)}); err != nil {
			return nil, err
		}
	}
	if code.bodyInit != nil {
		body := newScope(s, code.bodyInit)
		for _, c := range code.bodyCopies {
			body.vars[c[1]] = s.vars[c[0]]
		}
		s = body
	}
	return r.runBody(f, s)
}

func (r *rt) runBody(f *function, s *scope) (any, error) {
	code := f.code
	for _, h := range code.hoisted {
		fn, err := r.closure(h.code, s, h.name)
		if err != nil {
			return nil, err
		}
		s.vars[h.slot] = fn
	}
	if code.exprBody != nil {
		return code.exprBody(r, s)
	}
	k, v, err := runList(r, s, code.body)
	if err != nil {
		if err == errShortCircuit {
			return nil, errInternal
		}
		return nil, err
	}
	if k == ctlReturn {
		return v, nil
	}
	return Undefined, nil
}

// call applies a callable value with an undefined receiver.
func (r *rt) call(f any, args []any) (any, error) { return r.callThis(f, Undefined, args) }

// callThis applies a callable with an explicit receiver. Script functions
// cannot observe `this` (it declines compilation); built-in prototype
// methods operate on it.
func (r *rt) callThis(f any, this any, args []any) (any, error) {
	fn, ok := f.(*function)
	if !ok {
		return nil, r.notCallable(f)
	}
	if fn.native != nil {
		if fn.binds == 0 {
			return fn.native(r, this, args)
		}
		// A bound function calls its target through callThis again: one
		// more Go frame per bind level that invoke's depth accounting never
		// sees. Count each level toward MaxCallDepth like a script call, so
		// a long bind chain fails like deep recursion instead of growing the
		// goroutine stack without limit.
		if r.depth >= r.x.maxDepth {
			return nil, errStackOverflow
		}
		r.depth++
		v, err := fn.native(r, this, args)
		r.depth--
		return v, err
	}
	return r.invoke(fn, args)
}

func (r *rt) notCallable(f any) error {
	s, _ := r.toString(f)
	if isObjectValue(f) {
		return r.typeError("Not a function: " + s)
	}
	return r.typeError("Value is not an object: " + s)
}

func (r *rt) callTool(name string, arg any) (any, error) {
	exported, err := exportArgument(arg, r.location())
	if err != nil {
		return nil, r.typeError("invalid arguments for " + name + ": " + err.Error())
	}
	if r.seq != nil && r.x.sequential != nil && r.x.sequential(name) {
		r.seq.wait(r.item)
	}
	v, err := r.effect(&r.x.calls, r.x.opts.MaxCalls, "tool-call", func() (any, error) {
		if r.x.opts.Dispatch == nil {
			return nil, errors.New("no dispatcher configured")
		}
		return r.x.opts.Dispatch(r.ctx, name, exported)
	})
	if err != nil {
		return nil, err
	}
	return r.fromGo(v)
}

func (r *rt) callHost(name string, args []any) (any, error) {
	if r.seq != nil {
		// Host functions share host state (a shell, a filesystem): in a
		// parallel batch they run one at a time in item order.
		r.seq.wait(r.item)
	}
	exported := make([]any, len(args))
	for i, a := range args {
		v, err := exportValue(a, r.location())
		if err != nil {
			return nil, r.typeError("invalid arguments for " + name + ": " + err.Error())
		}
		exported[i] = v
	}
	v, err := r.effect(&r.x.hostCalls, r.x.opts.MaxHostCalls, "host-call", func() (any, error) {
		if r.x.opts.HostDispatch == nil {
			return nil, errors.New("no host dispatcher configured")
		}
		return r.x.opts.HostDispatch(r.ctx, name, exported)
	})
	if err != nil {
		return nil, err
	}
	return r.fromGo(v)
}

// effect charges and performs one tool/host call. Ordinary failures (and
// exhausted budgets) become catchable GoError exceptions, as in the Goja
// runner; cancellation, fatal errors and host panics are uncatchable.
func (r *rt) effect(counter *atomic.Int64, limit int, label string, dispatch func() (any, error)) (value any, err error) {
	if err = r.flush(); err != nil {
		return nil, err
	}
	if err = r.ctx.Err(); err != nil {
		return nil, err
	}
	for {
		n := counter.Load()
		if n >= int64(limit) {
			return nil, goError(fmt.Errorf("%s limit (%d) exceeded", label, limit))
		}
		if counter.CompareAndSwap(n, n+1) {
			break
		}
	}
	panicked := false
	func() {
		defer func() {
			if p := recover(); p != nil {
				panicked = true
				err = fmt.Errorf("host panic: %v", p)
			}
		}()
		value, err = dispatch()
	}()
	if panicked {
		return nil, err
	}
	if err == nil {
		return value, nil
	}
	x := r.x
	if x.opts.Fatal != nil && x.opts.Fatal(err) {
		x.fatalMu.Lock()
		if x.fatal == nil {
			x.fatal = err
		}
		x.aborted.Store(true)
		x.fatalMu.Unlock()
		return nil, err
	}
	if cerr := r.ctx.Err(); cerr != nil && errors.Is(err, cerr) {
		return nil, err
	}
	var te *TypeError
	if errors.As(err, &te) {
		return nil, &Throw{Value: newError("TypeError", te.Message), cause: err}
	}
	return nil, goError(err)
}

// fromGo converts host (Dispatch) data into runtime values. Object keys from
// Go maps have no order, so they enumerate sorted (Goja's order is random).
// The converted value is the script's, so it is charged against the
// allocation budget as it is built: a tool returning a large result costs
// what holding that result costs.
func (r *rt) fromGo(v any) (any, error) { return r.fromGoDepth(v, 0) }

// fromGoDepth bounds the nesting it descends: a tool or host dispatcher that
// returns data nested past maxConversionNesting would otherwise recurse once
// per level until the goroutine stack overflows fatally, which recover()
// cannot catch. It fails the conversion instead.
func (r *rt) fromGoDepth(v any, depth int) (any, error) {
	switch v := v.(type) {
	case string:
		if err := r.chargeString(v); err != nil {
			return nil, err
		}
		return v, nil
	case nil, bool, float64, undefined:
		return v, nil
	case int:
		return float64(v), nil
	case int64:
		return float64(v), nil
	case int32:
		return float64(v), nil
	case float32:
		return float64(v), nil
	case uint64:
		return float64(v), nil
	case json.Number:
		f, err := v.Float64()
		return f, err
	case map[string]any:
		if depth >= maxConversionNesting {
			return nil, errStackOverflow
		}
		keys := make([]string, 0, len(v))
		n := allocHeader
		for k := range v {
			keys = append(keys, k)
			n += allocProp + len(k)
		}
		if err := r.charge(n); err != nil {
			return nil, err
		}
		sort.Strings(keys)
		o := newObject(len(v))
		for _, k := range keys {
			c, err := r.fromGoDepth(v[k], depth+1)
			if err != nil {
				return nil, err
			}
			o.set(k, c)
		}
		return o, nil
	case []any:
		if depth >= maxConversionNesting {
			return nil, errStackOverflow
		}
		if err := r.chargeArray(len(v)); err != nil {
			return nil, err
		}
		items := make([]any, len(v))
		for i, e := range v {
			c, err := r.fromGoDepth(e, depth+1)
			if err != nil {
				return nil, err
			}
			items[i] = c
		}
		return &array{items: items}, nil
	case *Object:
		if depth >= maxConversionNesting {
			return nil, errStackOverflow
		}
		// Export is the host's own immutable copy of Fields, made before
		// the run sees it; the view built from Fields is charged.
		n := 2 * allocHeader
		for k := range v.Fields {
			n += allocProp + len(k)
		}
		if err := r.charge(n); err != nil {
			return nil, err
		}
		view := newObject(len(v.Fields))
		keys := v.Order
		if keys == nil {
			for k := range v.Fields {
				keys = append(keys, k)
			}
			sort.Strings(keys)
		}
		for _, k := range keys {
			f, ok := v.Fields[k]
			if !ok {
				continue
			}
			c, err := r.fromGoDepth(f, depth+1)
			if err != nil {
				return nil, err
			}
			view.set(k, c)
		}
		return &hostObject{view: view, export: v.Export}, nil
	case *object, *array, *function, *hostObject:
		return v, nil
	}
	return nil, fmt.Errorf("non-JSON host value %T", v)
}

// Export depth matches the Goja runner's clamp: deeper values (and cycles)
// render as this marker instead of failing.
const (
	maxExportDepth   = 64
	maxExportNodes   = 100000
	maxDepthExceeded = "[max depth exceeded]"
)

// exportValue converts a runtime value to host data like Goja's Value.Export:
// objects become map[string]any (undefined properties keep the Undefined
// sentinel), arrays []any, functions Undefined.
func exportValue(v any, loc *time.Location) (any, error) {
	x := exporter{clamp: true, loc: loc}
	return x.export(v, 0)
}

// exportArgument exports a tool argument like Goja's Value.Export followed by
// json.Marshal: no depth clamp, and functions, non-finite numbers and cycles
// fail with encoding/json's messages (the host binding reports them).
func exportArgument(v any, loc *time.Location) (any, error) {
	x := exporter{strict: true, onStack: map[any]bool{}, loc: loc}
	return x.export(v, 0)
}

// exportConsole exports a console argument; a function inside fails so the
// caller falls back to String(value), as Goja's console does.
func exportConsole(v any, loc *time.Location) (any, error) {
	x := exporter{clamp: true, funcsFail: true, loc: loc}
	return x.export(v, 0)
}

type exporter struct {
	loc       *time.Location // Dates export as time.Time in this zone
	onStack   map[any]bool
	nodes     int
	clamp     bool // replace values at maxExportDepth with a marker
	strict    bool // mirror json.Marshal failures
	funcsFail bool
}

var errFuncExport = errors.New("json: unsupported type: func(goja.FunctionCall) goja.Value")

func (x *exporter) export(v any, depth int) (any, error) {
	x.nodes++
	if x.nodes > maxExportNodes {
		return nil, errors.New("JSON value limit exceeded")
	}
	if x.clamp && depth >= maxExportDepth {
		return maxDepthExceeded, nil
	}
	switch t := v.(type) {
	case *object:
		if x.strict {
			if x.onStack[t] {
				return nil, errors.New("json: unsupported value: encountered a cycle via map[string]interface {}")
			}
			x.onStack[t] = true
			defer delete(x.onStack, t)
		}
		out := make(map[string]any, len(t.keys))
		for i, k := range t.keys {
			p := t.vals[i]
			e, err := x.export(p, depth+1)
			if err != nil {
				return nil, err
			}
			out[k] = e
		}
		return out, nil
	case *array:
		if x.strict {
			if x.onStack[t] {
				return nil, errors.New("json: unsupported value: encountered a cycle via []interface {}")
			}
			x.onStack[t] = true
			defer delete(x.onStack, t)
		}
		out := make([]any, len(t.items))
		for i, p := range t.items {
			e, err := x.export(p, depth+1)
			if err != nil {
				return nil, err
			}
			out[i] = e
		}
		return out, nil
	case *hostObject:
		if !t.dirty {
			return t.export, nil
		}
		return x.export(t.view, depth)
	case *function:
		if x.strict || x.funcsFail {
			return nil, errFuncExport
		}
		return Undefined, nil
	case tdzMarker:
		return Undefined, nil
	case holeMarker:
		return nil, nil // Goja exports a missing element as nil
	case *regexpValue, *iterator:
		return map[string]any{}, nil
	case *intlObject:
		if t.props != nil {
			return x.export(t.props, depth)
		}
		return map[string]any{}, nil
	case *dateValue:
		// Goja exports a Date as time.Time (nil when invalid).
		if !t.isSet() {
			return nil, nil
		}
		loc := x.loc
		if loc == nil {
			loc = time.Local
		}
		tm := timeFromMsec(t.msec).In(loc)
		if x.strict && (tm.Year() < 0 || tm.Year() > 9999) {
			// Report exactly what encoding/json says for the exported value
			// inside an argument map (the text varies across Go versions).
			if _, err := json.Marshal(map[string]any{"": tm}); err != nil {
				return nil, err
			}
		}
		return tm, nil
	case *collection:
		// Goja exports a Set as its values and a Map as [key, value] pairs.
		out := make([]any, 0, t.live)
		for i := range t.keys {
			if t.dead[i] {
				continue
			}
			var item any = t.keys[i]
			if t.isMap {
				item = &array{items: []any{t.keys[i], t.vals[i]}}
			}
			e, err := x.export(item, depth+1)
			if err != nil {
				return nil, err
			}
			out = append(out, e)
		}
		return out, nil
	case float64:
		if x.strict && (math.IsNaN(t) || math.IsInf(t, 0)) {
			return nil, fmt.Errorf("json: unsupported value: %s", strconv.FormatFloat(t, 'g', -1, 64))
		}
	}
	return v, nil
}

// Marshal converts JSON data plus Undefined to JSON with bounded traversal.
// It prevents exponential expansion through repeated references in a plan.
func Marshal(v any) ([]byte, error) {
	nodes := 0
	clean, err := jsonValue(v, 0, &nodes, true)
	if err != nil {
		return nil, err
	}
	return json.Marshal(clean)
}

func jsonValue(v any, depth int, nodes *int, omitUndefined bool) (any, error) {
	*nodes++
	if *nodes > maxExportNodes || depth > 10000 {
		return nil, errors.New("JSON value limit exceeded")
	}
	switch v := v.(type) {
	case undefined:
		return nil, nil
	case *Object:
		return jsonValue(v.Export, depth+1, nodes, omitUndefined)
	case map[string]any:
		out := make(map[string]any, len(v))
		var firstErr error
		firstKey := ""
		for k, a := range v {
			if _, ok := a.(undefined); ok && omitUndefined {
				continue
			}
			b, err := jsonValue(a, depth+1, nodes, omitUndefined)
			if err != nil {
				// encoding/json reports the failure under the smallest key;
				// map iteration order must not pick the message.
				if firstErr == nil || k < firstKey {
					firstErr, firstKey = err, k
				}
				continue
			}
			out[k] = b
		}
		if firstErr != nil {
			return nil, firstErr
		}
		return out, nil
	case []any:
		out := make([]any, len(v))
		for i, a := range v {
			b, err := jsonValue(a, depth+1, nodes, omitUndefined)
			if err != nil {
				return nil, err
			}
			out[i] = b
		}
		return out, nil
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, fmt.Errorf("json: unsupported value: %v", v)
		}
		return v, nil
	case nil, string, bool:
		return v, nil
	case JSONText:
		return string(v), nil
	default:
		if rv := reflect.ValueOf(v); rv.Kind() == reflect.Struct || rv.Kind() == reflect.Pointer {
			return v, nil // host export (e.g. a Go struct) marshals as itself
		}
		return nil, fmt.Errorf("non-JSON host value %T", v)
	}
}

// MarshalExport matches Goja Value.Export followed by encoding/json, including
// undefined object properties encoded as null (unlike JSON.stringify).
func MarshalExport(v any) ([]byte, error) {
	nodes := 0
	clean, err := jsonValue(v, 0, &nodes, false)
	if err != nil {
		return nil, err
	}
	return json.Marshal(clean)
}

// buildNamespace mirrors a host's binding installation: `call` first, then
// each tool in order, grouping server tools under a nested object.
func (p *Program) buildNamespace() *object {
	root := newObject(len(p.namespace) + 1)
	if p.resolveCall != nil {
		resolve := p.resolveCall
		root.set("call", &function{name: "call", native: func(r *rt, _ any, args []any) (any, error) {
			raw, err := r.toString(arg(args, 0))
			if err != nil {
				return nil, err
			}
			name, ok := resolve(raw)
			if !ok {
				return nil, goError(fmt.Errorf("unknown tool %q", raw))
			}
			return r.callTool(name, arg(args, 1))
		}})
	}
	for _, b := range p.namespace {
		name := b.name
		fn := &function{name: b.path[len(b.path)-1], native: func(r *rt, _ any, args []any) (any, error) {
			return r.callTool(name, arg(args, 0))
		}}
		parent := root
		for _, k := range b.path[:len(b.path)-1] {
			nv, _ := parent.own(k)
			next, ok := nv.(*object)
			if !ok {
				next = newObject(4)
				parent.set(k, next)
			}
			parent = next
		}
		parent.set(b.path[len(b.path)-1], fn)
	}
	return root
}

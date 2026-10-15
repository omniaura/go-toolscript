// Package toolscript runs model-written JavaScript tool orchestration without
// a JavaScript VM. Parsing uses Goja's lexer and AST, never its VM. Programs
// are compiled into a tree of Go closures over a JSON-shaped value model;
// anything outside the supported subset is rejected before any host function
// is called, so the host can fall back to a real JavaScript runtime.
package toolscript

import (
	"errors"
	"fmt"

	"github.com/dop251/goja/ast"
	"github.com/dop251/goja/parser"
	"github.com/dop251/goja/token"
)

// ErrUnsupported means the caller may safely fall back to another executor.
// Compile returns it for programs outside the subset. Execute returns
// ErrRuntimeUnsupported instead, which permits fallback only while
// Result.Calls and Result.HostCalls are both zero.
var ErrUnsupported = errors.New("unsupported tool script")

// ErrRuntimeUnsupported reports an operation outside the subset that could
// only be detected while running. Falling back is safe only when the Result
// records no tool or host calls (no effects have happened yet).
var ErrRuntimeUnsupported = errors.New("unsupported tool script operation")

func errRuntimeUnsupported(what string) error {
	return fmt.Errorf("%w: %s", ErrRuntimeUnsupported, what)
}

var errInternal = errors.New("toolscript: invalid execution plan")

// HostFunction explicitly admits a bare global function with positional arguments.
// LiteralStringArgs requires those argument positions to be string literals.
// StringArgs converts those positions with JavaScript ToString (undefined stays
// Undefined). JSONArgs passes those positions as JSONText produced by the
// engine's JSON.stringify (insertion-ordered keys); undefined stays Undefined.
// No implementation or ambient capability is installed by declaring a function.
type HostFunction struct {
	LiteralStringArgs []int
	StringArgs        []int
	JSONArgs          []int
	MinArgs           int
	MaxArgs           int
}

// JSONText is a host-function argument serialized by JSON.stringify. It is
// empty when JSON.stringify returned undefined (e.g. for a function).
type JSONText string

// CompileOptions selects the host's exposed namespace and language extensions.
type CompileOptions struct {
	HostFunctions map[string]HostFunction
	// Resolve maps a static property path (e.g. ["mcp","search"]) to a
	// dispatcher name. Return false for unknown bindings. Never executes a tool.
	Resolve func([]string) (string, bool)
	// ResolveCall maps mcp.call("raw-name", args) separately from property
	// bindings, so it cannot collide with mcp.call.some_tool(args).
	ResolveCall func(string) (string, bool)
	// Bindings optionally lists every tool binding path (e.g. {"search"},
	// {"github", "list_prs"}) in the host's installation order. When set, the
	// mcp/tools namespaces are also ordinary values (Object.keys(mcp),
	// typeof mcp.x, mcp.srv ? ... : ...), and calls to paths that are not
	// bindings compile to the runtime TypeError JavaScript would raise
	// instead of declining. The list must be complete.
	Bindings [][]string
	// SequentialTools marks tools whose calls must never overlap or reorder
	// (stateful documents, a live browser session). In a parallel batch, a
	// call to one waits until every earlier item has finished; the rest of
	// the batch stays parallel. Host functions are always treated this way.
	SequentialTools func(tool string) bool
	// Batches admits await, async functions and Promise.all/allSettled.
	// This is an explicit asynchronous-tool dialect, not arbitrary JS promises.
	Batches bool
}

// Program is immutable and safe to execute concurrently with separate hosts.
type Program struct {
	sequential  func(string) bool
	main        *funcCode
	namespace   []nsBinding // non-nil when the namespace is used as a value
	resolveCall func(string) (string, bool)
}

type nsBinding struct {
	path []string
	name string
}

type (
	evalFn func(r *rt, s *scope) (any, error)
	stmtFn func(r *rt, s *scope) (ctl, any, error)
	// binder stores a value into a binding target (identifier or pattern).
	binder func(r *rt, s *scope, v any) error
)

type ctl uint8

const (
	ctlNormal ctl = iota
	ctlBreak
	ctlContinue
	ctlReturn
)

// scope is a runtime environment record: one slot per declared name.
type scope struct {
	parent *scope
	vars   []any
	// charged is set once a closure has captured this scope and it has been
	// charged against the allocation budget (see captureScope).
	charged bool
}

func (s *scope) up(hops int) *scope {
	for ; hops > 0; hops-- {
		s = s.parent
	}
	return s
}

type funcCode struct {
	init       []any // slot template: Undefined for vars/params, tdz for lexicals
	params     []binder
	defaults   []evalFn
	rest       binder
	hoisted    []hoistedFunc
	body       []stmtFn
	exprBody   evalFn
	source     string
	simple     []int // slot per simple identifier parameter, or -1
	length     int
	async      bool
	usesScopes bool
	direct     bool     // only simple identifier parameters: args bind straight into slots
	leaf       bool     // creates no closures, so its frame cannot outlive the call
	bodyInit   []any    // separate body scope template when parameters have expressions
	bodyCopies [][2]int // parameter slot -> same-named body var slot
}

type hoistedFunc struct {
	code *funcCode
	name string
	slot int
}

type varKind uint8

const (
	kindVar varKind = iota
	kindLet
	kindConst
	kindFunc
	kindParam
	kindFuncName // a named function expression's own name: writes are ignored
)

type cvar struct {
	scope *cscope
	alias []string // compile-time namespace alias (const gh = mcp.github)
	slot  int
	kind  varKind
}

type cscope struct {
	parent *cscope
	vars   map[string]*cvar
	fn     *cfunc
	init   []any
}

type cfunc struct {
	parent *cfunc
	top    *cscope
	async  bool
}

type compiler struct {
	pendingLabels []string // labels for the loop being compiled next
	functions     int      // function literals compiled so far (closure detection)
	opts          CompileOptions
	namespaceUsed bool
	fn            *cfunc
	sc            *cscope
	depth         int
	batchDepth    int
	loops         int
	breakable     int
}

// Compile validates the WHOLE source before producing an executable plan.
// Source and nesting limits also bound parser/compiler work on untrusted input.
func Compile(source string, opts CompileOptions) (prog *Program, err error) {
	if len(source) > 32<<10 {
		return nil, fmt.Errorf("%w: source exceeds 32 KiB", ErrUnsupported)
	}
	defer func() {
		if r := recover(); r != nil {
			prog, err = nil, fmt.Errorf("%w: compiler panic: %v", ErrUnsupported, r)
		}
	}()
	// Async wrapper admits top-level await only when explicitly opted in below.
	tree, err := parser.ParseFile(nil, "toolscript.js", "(async function(){\n"+source+"\n})", 0, parser.WithDisableSourceMaps)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnsupported, err)
	}
	if len(tree.Body) != 1 {
		return nil, ErrUnsupported
	}
	s, ok := tree.Body[0].(*ast.ExpressionStatement)
	if !ok {
		return nil, ErrUnsupported
	}
	fn, ok := s.Expression.(*ast.FunctionLiteral)
	if !ok {
		return nil, ErrUnsupported
	}
	if opts.Batches {
		if err := checkAsyncShapes(fn); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrUnsupported, err)
		}
	}
	c := &compiler{opts: opts}
	main, err := c.function(fn.ParameterList, fn.Body, nil, opts.Batches, "")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnsupported, err)
	}
	prog = &Program{main: main, sequential: opts.SequentialTools}
	if c.namespaceUsed {
		prog.resolveCall = opts.ResolveCall
		prog.namespace = []nsBinding{}
		for _, b := range opts.Bindings {
			full := append([]string{"mcp"}, b...)
			if opts.Resolve == nil || len(b) == 0 {
				continue
			}
			if name, ok := opts.Resolve(full); ok {
				prog.namespace = append(prog.namespace, nsBinding{path: b, name: name})
			}
		}
	}
	return prog, nil
}

func unsupported(n any) error { return fmt.Errorf("unsupported syntax %T", n) }

// ---- scopes ----

func (c *compiler) pushScope(fnScope bool) *cscope {
	sc := &cscope{parent: c.sc, vars: map[string]*cvar{}, fn: c.fn}
	if fnScope {
		c.fn.top = sc
	}
	c.sc = sc
	return sc
}

func (c *compiler) popScope() { c.sc = c.sc.parent }

func (c *compiler) declare(sc *cscope, name string, kind varKind) (*cvar, error) {
	if name == "arguments" || name == "eval" {
		return nil, fmt.Errorf("reserved binding %s", name)
	}
	if v, ok := sc.vars[name]; ok {
		// `var` may repeat and may redeclare parameters and hoisted functions.
		if kind == kindVar && (v.kind == kindVar || v.kind == kindParam || v.kind == kindFunc) {
			return v, nil
		}
		if kind == kindFunc && (v.kind == kindVar || v.kind == kindParam) {
			v.kind = kindFunc
			return v, nil
		}
		return nil, fmt.Errorf("redeclared binding %s", name)
	}
	v := &cvar{scope: sc, slot: len(sc.init), kind: kind}
	sc.vars[name] = v
	if kind == kindLet || kind == kindConst {
		sc.init = append(sc.init, tdz)
	} else {
		sc.init = append(sc.init, Undefined)
	}
	return v, nil
}

// lookup resolves a name lexically. hops counts runtime scopes to walk.
func (c *compiler) lookup(name string) (*cvar, int) {
	hops := 0
	for sc := c.sc; sc != nil; sc = sc.parent {
		if v, ok := sc.vars[name]; ok {
			return v, hops
		}
		hops++
	}
	return nil, 0
}

// Small frames carry their slots inline: one allocation per scope.
type scope4 struct {
	scope
	buf [4]any
}

type scope8 struct {
	scope
	buf [8]any
}

// newScope materializes a compile-time scope at runtime.
func newScope(parent *scope, init []any) *scope {
	switch n := len(init); {
	case n <= 4:
		s := &scope4{}
		s.parent, s.vars = parent, s.buf[:n]
		copy(s.vars, init)
		return &s.scope
	case n <= 8:
		s := &scope8{}
		s.parent, s.vars = parent, s.buf[:n]
		copy(s.vars, init)
		return &s.scope
	}
	vars := make([]any, len(init))
	copy(vars, init)
	return &scope{parent: parent, vars: vars}
}

// ---- functions ----

// function compiles a function body in a fresh function scope. params may be
// nil for the program wrapper. exprBody is used for concise arrow bodies.
func (c *compiler) function(params *ast.ParameterList, body *ast.BlockStatement, exprBody ast.Expression, async bool, source string) (*funcCode, error) {
	if async && !c.opts.Batches {
		return nil, fmt.Errorf("async functions require batches")
	}
	savedFn, savedSc, savedLoops, savedBreak := c.fn, c.sc, c.loops, c.breakable
	defer func() { c.fn, c.sc, c.loops, c.breakable = savedFn, savedSc, savedLoops, savedBreak }()
	c.fn = &cfunc{parent: savedFn, async: async}
	c.loops, c.breakable = 0, 0
	c.functions++
	functionsAtStart := c.functions
	sc := c.pushScope(true)
	code := &funcCode{async: async, source: source}

	// With parameter expressions (defaults, patterns) the parameters live in
	// their own scope, initialized left to right (TDZ), and the body gets a
	// separate scope that the defaults cannot see.
	hasExprs := false
	paramKind := kindParam
	if params != nil {
		for _, p := range params.List {
			if _, id := p.Target.(*ast.Identifier); p.Initializer != nil || !id {
				hasExprs = true
			}
		}
		if hasExprs {
			paramKind = kindLet
		}
		for _, p := range params.List {
			if err := c.declarePattern(sc, p.Target, paramKind); err != nil {
				return nil, err
			}
		}
		if params.Rest != nil {
			if err := c.declarePattern(sc, params.Rest, paramKind); err != nil {
				return nil, err
			}
		}
	}
	declareBody := func(bs *cscope, stmts []ast.Statement) error {
		if err := c.hoistVars(bs, stmts); err != nil {
			return err
		}
		for _, st := range stmts {
			if fd, ok := st.(*ast.FunctionDeclaration); ok {
				if _, err := c.declare(bs, fd.Function.Name.Name.String(), kindFunc); err != nil {
					return err
				}
			}
		}
		return c.declareLexical(bs, stmts)
	}
	var stmts []ast.Statement
	if body != nil {
		stmts = body.List
		if !hasExprs {
			if err := declareBody(sc, stmts); err != nil {
				return nil, err
			}
		}
	}
	if params != nil {
		code.length = len(params.List)
		for i, p := range params.List {
			if p.Initializer != nil && code.length > i {
				code.length = i
			}
			b, err := c.bindingTarget(p.Target, true)
			if err != nil {
				return nil, err
			}
			code.params = append(code.params, b)
			slot := -1
			if id, ok := p.Target.(*ast.Identifier); ok {
				v, _ := c.lookup(id.Name.String())
				slot = v.slot
			}
			code.simple = append(code.simple, slot)
			var def evalFn
			if p.Initializer != nil {
				def, err = c.expression(p.Initializer)
				if err != nil {
					return nil, err
				}
			}
			code.defaults = append(code.defaults, def)
		}
		if params.Rest != nil {
			b, err := c.bindingTarget(params.Rest, true)
			if err != nil {
				return nil, err
			}
			code.rest = b
		}
	}
	bodyScope := sc
	if hasExprs {
		bodyScope = c.pushScope(false)
		if body != nil {
			if err := declareBody(bodyScope, stmts); err != nil {
				return nil, err
			}
		}
		// A body var named like a parameter starts with the parameter's value.
		for name, v := range bodyScope.vars {
			if p, ok := sc.vars[name]; ok && v.kind == kindVar {
				code.bodyCopies = append(code.bodyCopies, [2]int{p.slot, v.slot})
			}
		}
	}
	if body != nil {
		for _, st := range stmts {
			if fd, ok := st.(*ast.FunctionDeclaration); ok {
				f := fd.Function
				if f.Generator {
					return nil, unsupported(f)
				}
				inner, err := c.function(f.ParameterList, f.Body, nil, f.Async, f.Source)
				if err != nil {
					return nil, err
				}
				name := f.Name.Name.String()
				code.hoisted = append(code.hoisted, hoistedFunc{code: inner, name: name, slot: bodyScope.vars[name].slot})
			}
		}
		list, err := c.statements(stmts)
		if err != nil {
			return nil, err
		}
		code.body = list
	} else {
		e, err := c.expression(exprBody)
		if err != nil {
			return nil, err
		}
		code.exprBody = e
	}
	code.leaf = c.functions == functionsAtStart
	code.init = sc.init
	if hasExprs {
		code.bodyInit = bodyScope.init
		if code.bodyInit == nil {
			code.bodyInit = []any{}
		}
	}
	code.direct = code.rest == nil && !hasExprs
	for i, slot := range code.simple {
		if slot < 0 || code.defaults[i] != nil {
			code.direct = false
		}
	}
	return code, nil
}

// hoistVars declares every `var` in a function body (not nested functions).
func (c *compiler) hoistVars(sc *cscope, list []ast.Statement) error {
	for _, st := range list {
		if err := c.hoistVarStmt(sc, st); err != nil {
			return err
		}
	}
	return nil
}

func (c *compiler) hoistVarStmt(sc *cscope, st ast.Statement) error {
	switch n := st.(type) {
	case *ast.VariableStatement:
		for _, b := range n.List {
			if err := c.declarePattern(sc, b.Target, kindVar); err != nil {
				return err
			}
		}
	case *ast.BlockStatement:
		return c.hoistVars(sc, n.List)
	case *ast.IfStatement:
		if err := c.hoistVarStmt(sc, n.Consequent); err != nil {
			return err
		}
		if n.Alternate != nil {
			return c.hoistVarStmt(sc, n.Alternate)
		}
	case *ast.ForStatement:
		if init, ok := n.Initializer.(*ast.ForLoopInitializerVarDeclList); ok {
			for _, b := range init.List {
				if err := c.declarePattern(sc, b.Target, kindVar); err != nil {
					return err
				}
			}
		}
		return c.hoistVarStmt(sc, n.Body)
	case *ast.ForOfStatement:
		if v, ok := n.Into.(*ast.ForIntoVar); ok {
			if err := c.declarePattern(sc, v.Binding.Target, kindVar); err != nil {
				return err
			}
		}
		return c.hoistVarStmt(sc, n.Body)
	case *ast.ForInStatement:
		if v, ok := n.Into.(*ast.ForIntoVar); ok {
			if err := c.declarePattern(sc, v.Binding.Target, kindVar); err != nil {
				return err
			}
		}
		return c.hoistVarStmt(sc, n.Body)
	case *ast.WhileStatement:
		return c.hoistVarStmt(sc, n.Body)
	case *ast.DoWhileStatement:
		return c.hoistVarStmt(sc, n.Body)
	case *ast.TryStatement:
		if err := c.hoistVars(sc, n.Body.List); err != nil {
			return err
		}
		if n.Catch != nil {
			if err := c.hoistVars(sc, n.Catch.Body.List); err != nil {
				return err
			}
		}
		if n.Finally != nil {
			return c.hoistVars(sc, n.Finally.List)
		}
	case *ast.SwitchStatement:
		for _, cs := range n.Body {
			if err := c.hoistVars(sc, cs.Consequent); err != nil {
				return err
			}
		}
	case *ast.LabelledStatement:
		return c.hoistVarStmt(sc, n.Statement)
	}
	return nil
}

// declareLexical declares let/const in a block's own statement list.
func (c *compiler) declareLexical(sc *cscope, list []ast.Statement) error {
	for _, st := range list {
		switch n := st.(type) {
		case *ast.LexicalDeclaration:
			kind := kindLet
			if n.Token == token.CONST {
				kind = kindConst
			}
			for _, b := range n.List {
				if err := c.declarePattern(sc, b.Target, kind); err != nil {
					return err
				}
			}
		case *ast.ClassDeclaration:
			return unsupported(n)
		}
	}
	return nil
}

func (c *compiler) declarePattern(sc *cscope, target ast.Expression, kind varKind) error {
	switch n := target.(type) {
	case *ast.Identifier:
		_, err := c.declare(sc, n.Name.String(), kind)
		return err
	case *ast.ArrayPattern:
		for _, e := range n.Elements {
			if e == nil {
				continue
			}
			if a, ok := e.(*ast.AssignExpression); ok {
				e = a.Left
			}
			if err := c.declarePattern(sc, e, kind); err != nil {
				return err
			}
		}
		if n.Rest != nil {
			return c.declarePattern(sc, n.Rest, kind)
		}
		return nil
	case *ast.ObjectPattern:
		for _, p := range n.Properties {
			switch p := p.(type) {
			case *ast.PropertyShort:
				if err := c.declarePattern(sc, &p.Name, kind); err != nil {
					return err
				}
			case *ast.PropertyKeyed:
				v := p.Value
				if a, ok := v.(*ast.AssignExpression); ok {
					v = a.Left
				}
				if err := c.declarePattern(sc, v, kind); err != nil {
					return err
				}
			default:
				return unsupported(p)
			}
		}
		if n.Rest != nil {
			return c.declarePattern(sc, n.Rest, kind)
		}
		return nil
	}
	return unsupported(target)
}

// ---- statements ----

func (c *compiler) statements(list []ast.Statement) ([]stmtFn, error) {
	out := make([]stmtFn, 0, len(list))
	for i, st := range list {
		if es, ok := st.(*ast.ExpressionStatement); ok && i == 0 {
			if _, directive := es.Expression.(*ast.StringLiteral); directive {
				return nil, unsupported(es) // "use strict" changes semantics
			}
		}
		f, err := c.statement(st)
		if err != nil {
			return nil, err
		}
		if f != nil {
			out = append(out, f)
		}
	}
	return out, nil
}

func runList(r *rt, s *scope, list []stmtFn) (ctl, any, error) {
	for _, f := range list {
		k, v, err := f(r, s)
		if err != nil || k != ctlNormal {
			return k, v, err
		}
	}
	return ctlNormal, nil, nil
}

// block compiles statements with their own lexical scope when needed.
func (c *compiler) block(list []ast.Statement) (stmtFn, error) {
	needs := false
	for _, st := range list {
		switch st.(type) {
		case *ast.LexicalDeclaration, *ast.ClassDeclaration:
			needs = true
		case *ast.FunctionDeclaration:
			return nil, fmt.Errorf("function declaration in a nested block")
		}
	}
	if !needs {
		body, err := c.statements(list)
		if err != nil {
			return nil, err
		}
		return func(r *rt, s *scope) (ctl, any, error) { return runList(r, s, body) }, nil
	}
	sc := c.pushScope(false)
	defer c.popScope()
	if err := c.declareLexical(sc, list); err != nil {
		return nil, err
	}
	body, err := c.statements(list)
	if err != nil {
		return nil, err
	}
	return func(r *rt, s *scope) (ctl, any, error) {
		return runList(r, newScope(s, sc.init), body)
	}, nil
}

func (c *compiler) statement(node ast.Statement) (stmtFn, error) {
	switch n := node.(type) {
	case *ast.EmptyStatement, *ast.FunctionDeclaration:
		return nil, nil // function declarations are hoisted
	case *ast.ExpressionStatement:
		e, err := c.expression(n.Expression)
		if err != nil {
			return nil, err
		}
		return func(r *rt, s *scope) (ctl, any, error) {
			if err := r.tick(); err != nil {
				return 0, nil, err
			}
			_, err := e(r, s)
			return ctlNormal, nil, err
		}, nil
	case *ast.ReturnStatement:
		if n.Argument == nil {
			return func(r *rt, s *scope) (ctl, any, error) { return ctlReturn, Undefined, nil }, nil
		}
		e, err := c.expression(n.Argument)
		if err != nil {
			return nil, err
		}
		return func(r *rt, s *scope) (ctl, any, error) {
			if err := r.tick(); err != nil {
				return 0, nil, err
			}
			v, err := e(r, s)
			return ctlReturn, v, err
		}, nil
	case *ast.VariableStatement:
		return c.declarations(n.List, false)
	case *ast.LexicalDeclaration:
		return c.declarations(n.List, true)
	case *ast.BlockStatement:
		return c.block(n.List)
	case *ast.IfStatement:
		return c.ifStatement(n)
	case *ast.ThrowStatement:
		e, err := c.expression(n.Argument)
		if err != nil {
			return nil, err
		}
		return func(r *rt, s *scope) (ctl, any, error) {
			if err := r.tick(); err != nil {
				return 0, nil, err
			}
			v, err := e(r, s)
			if err != nil {
				return 0, nil, err
			}
			return 0, nil, &Throw{Value: v}
		}, nil
	case *ast.TryStatement:
		return c.tryStatement(n)
	case *ast.LabelledStatement:
		return c.labelled(n)
	case *ast.BranchStatement:
		if n.Label != nil {
			label := n.Label.Name.String()
			k := ctlBreak
			if n.Token == token.CONTINUE {
				k = ctlContinue
			}
			return func(r *rt, _ *scope) (ctl, any, error) { r.label = label; return k, nil, nil }, nil
		}
		if n.Token == token.BREAK {
			if c.breakable == 0 {
				return nil, unsupported(n)
			}
			return func(*rt, *scope) (ctl, any, error) { return ctlBreak, nil, nil }, nil
		}
		if c.loops == 0 {
			return nil, unsupported(n)
		}
		return func(*rt, *scope) (ctl, any, error) { return ctlContinue, nil, nil }, nil
	case *ast.WhileStatement:
		return c.whileStatement(n.Test, n.Body, false)
	case *ast.DoWhileStatement:
		return c.whileStatement(n.Test, n.Body, true)
	case *ast.ForStatement:
		return c.forStatement(n)
	case *ast.ForOfStatement:
		return c.forInOf(n.Into, n.Source, n.Body, true)
	case *ast.ForInStatement:
		return c.forInOf(n.Into, n.Source, n.Body, false)
	case *ast.SwitchStatement:
		return c.switchStatement(n)
	}
	return nil, unsupported(node)
}

func (c *compiler) declarations(list []*ast.Binding, lexical bool) (stmtFn, error) {
	type step struct {
		init evalFn
		bind binder
	}
	var steps []step
	for _, b := range list {
		if b.Initializer == nil {
			if !lexical {
				continue // `var x;` keeps the hoisted value
			}
			id, ok := b.Target.(*ast.Identifier)
			if !ok {
				return nil, unsupported(b)
			}
			bind, err := c.bindingTarget(id, true)
			if err != nil {
				return nil, err
			}
			steps = append(steps, step{init: constant(Undefined), bind: bind})
			continue
		}
		if alias, ok := c.namespaceAlias(b); ok {
			v, _ := c.lookup(b.Target.(*ast.Identifier).Name.String())
			v.alias = alias
			continue
		}
		init, err := c.expressionNamed(b.Initializer, b.Target)
		if err != nil {
			return nil, err
		}
		bind, err := c.bindingTarget(b.Target, true)
		if err != nil {
			return nil, err
		}
		steps = append(steps, step{init: init, bind: bind})
	}
	return func(r *rt, s *scope) (ctl, any, error) {
		if err := r.tick(); err != nil {
			return 0, nil, err
		}
		for _, st := range steps {
			v, err := st.init(r, s)
			if err != nil {
				return 0, nil, err
			}
			if err := st.bind(r, s, v); err != nil {
				return 0, nil, err
			}
		}
		return ctlNormal, nil, nil
	}, nil
}

// expressionNamed compiles an initializer, naming anonymous functions after
// their binding as JavaScript does (visible through .name).
func (c *compiler) expressionNamed(e ast.Expression, target ast.Expression) (evalFn, error) {
	f, err := c.expression(e)
	if err != nil {
		return nil, err
	}
	id, ok := target.(*ast.Identifier)
	if !ok {
		return f, nil
	}
	switch e.(type) {
	case *ast.ArrowFunctionLiteral, *ast.FunctionLiteral:
		name := id.Name.String()
		return func(r *rt, s *scope) (any, error) {
			v, err := f(r, s)
			if fn, ok := v.(*function); ok && fn.name == "" {
				fn.name = name
			}
			return v, err
		}, nil
	}
	return f, nil
}

// namespaceAlias recognizes `const gh = mcp.github` (a static namespace that
// is not itself a tool) so gh.list(...) resolves like mcp.github.list(...).
func (c *compiler) namespaceAlias(b *ast.Binding) ([]string, bool) {
	id, ok := b.Target.(*ast.Identifier)
	if !ok {
		return nil, false
	}
	v, _ := c.lookup(id.Name.String())
	if v == nil || v.kind != kindConst {
		return nil, false
	}
	path, ok := c.namespacePath(b.Initializer)
	if !ok || len(path) < 2 {
		return nil, false
	}
	if c.opts.Resolve != nil {
		if _, tool := c.opts.Resolve(path); tool {
			return nil, false
		}
	}
	return path, true
}

// namespacePath returns the static tool path of an expression rooted at an
// unshadowed mcp/tools global or at a namespace alias.
func (c *compiler) namespacePath(e ast.Expression) ([]string, bool) {
	root := rootName(e)
	if root == "" {
		return nil, false
	}
	if v, _ := c.lookup(root); (v == nil && root != "mcp" && root != "tools") || (v != nil && v.alias == nil) {
		return nil, false // cheap rejection before building the path
	}
	path, ok := staticPath(e)
	if !ok || len(path) == 0 {
		return nil, false
	}
	v, _ := c.lookup(path[0])
	if v != nil {
		if v.alias == nil {
			return nil, false
		}
		return append(append([]string(nil), v.alias...), path[1:]...), true
	}
	if path[0] != "mcp" && path[0] != "tools" {
		return nil, false
	}
	return path, true
}

func staticPath(n ast.Expression) ([]string, bool) {
	switch n := n.(type) {
	case *ast.Identifier:
		return []string{n.Name.String()}, true
	case *ast.DotExpression:
		p, ok := staticPath(n.Left)
		return append(p, n.Identifier.Name.String()), ok
	case *ast.BracketExpression:
		p, ok := staticPath(n.Left)
		s, yes := n.Member.(*ast.StringLiteral)
		if ok && yes {
			return append(p, s.Value.String()), true
		}
	}
	return nil, false
}

func (c *compiler) ifStatement(n *ast.IfStatement) (stmtFn, error) {
	test, err := c.expression(n.Test)
	if err != nil {
		return nil, err
	}
	cons, err := c.subStatement(n.Consequent)
	if err != nil {
		return nil, err
	}
	var alt stmtFn
	if n.Alternate != nil {
		if alt, err = c.subStatement(n.Alternate); err != nil {
			return nil, err
		}
	}
	return func(r *rt, s *scope) (ctl, any, error) {
		if err := r.tick(); err != nil {
			return 0, nil, err
		}
		v, err := test(r, s)
		if err != nil {
			return 0, nil, err
		}
		if toBoolean(v) {
			return cons(r, s)
		}
		if alt != nil {
			return alt(r, s)
		}
		return ctlNormal, nil, nil
	}, nil
}

// subStatement compiles a statement in a position that cannot hold
// declarations (if/loop bodies); an empty statement becomes a no-op.
func (c *compiler) subStatement(st ast.Statement) (stmtFn, error) {
	switch st.(type) {
	case *ast.LexicalDeclaration, *ast.FunctionDeclaration, *ast.ClassDeclaration:
		return nil, unsupported(st)
	}
	f, err := c.statement(st)
	if err != nil {
		return nil, err
	}
	if f == nil {
		return func(*rt, *scope) (ctl, any, error) { return ctlNormal, nil, nil }, nil
	}
	return f, nil
}

func (c *compiler) loopBody(st ast.Statement) (stmtFn, error) {
	c.loops++
	c.breakable++
	defer func() { c.loops--; c.breakable-- }()
	return c.subStatement(st)
}

func (c *compiler) whileStatement(testExpr ast.Expression, bodyStmt ast.Statement, do bool) (stmtFn, error) {
	labels := c.takeLabels()
	test, err := c.expression(testExpr)
	if err != nil {
		return nil, err
	}
	body, err := c.loopBody(bodyStmt)
	if err != nil {
		return nil, err
	}
	return func(r *rt, s *scope) (ctl, any, error) {
		for first := true; ; first = false {
			if err := r.tick(); err != nil {
				return 0, nil, err
			}
			if !(do && first) {
				v, err := test(r, s)
				if err != nil {
					return 0, nil, err
				}
				if !toBoolean(v) {
					return ctlNormal, nil, nil
				}
			}
			k, v, err := body(r, s)
			if err != nil {
				return 0, nil, err
			}
			if stop, res := r.loopControl(k, labels); stop {
				return res, v, nil
			}
		}
	}, nil
}

func (c *compiler) forStatement(n *ast.ForStatement) (stmtFn, error) {
	labels := c.takeLabels()
	functionsAtStart := c.functions
	var sc *cscope
	var initFn stmtFn
	var err error
	switch init := n.Initializer.(type) {
	case nil:
	case *ast.ForLoopInitializerExpression:
		e, err := c.expression(init.Expression)
		if err != nil {
			return nil, err
		}
		initFn = func(r *rt, s *scope) (ctl, any, error) { _, err := e(r, s); return ctlNormal, nil, err }
	case *ast.ForLoopInitializerVarDeclList:
		if initFn, err = c.declarations(init.List, false); err != nil {
			return nil, err
		}
	case *ast.ForLoopInitializerLexicalDecl:
		sc = c.pushScope(false)
		defer c.popScope()
		if err := c.declareLexical(sc, []ast.Statement{&init.LexicalDeclaration}); err != nil {
			return nil, err
		}
		if initFn, err = c.declarations(init.LexicalDeclaration.List, true); err != nil {
			return nil, err
		}
	default:
		return nil, unsupported(init)
	}
	var test, update evalFn
	if n.Test != nil {
		if test, err = c.expression(n.Test); err != nil {
			return nil, err
		}
	}
	if n.Update != nil {
		if update, err = c.expression(n.Update); err != nil {
			return nil, err
		}
	}
	body, err := c.loopBody(n.Body)
	if err != nil {
		return nil, err
	}
	perIteration := sc != nil && c.functions != functionsAtStart
	return func(r *rt, s *scope) (ctl, any, error) {
		loop := s
		if sc != nil {
			loop = newScope(s, sc.init)
		}
		if initFn != nil {
			if _, _, err := initFn(r, loop); err != nil {
				return 0, nil, err
			}
		}
		for first := true; ; first = false {
			if err := r.tick(); err != nil {
				return 0, nil, err
			}
			if perIteration {
				// Closures capture a fresh copy of the loop bindings per iteration.
				loop = &scope{parent: s, vars: append([]any(nil), loop.vars...)}
			}
			if !first && update != nil {
				if _, err := update(r, loop); err != nil {
					return 0, nil, err
				}
			}
			if test != nil {
				v, err := test(r, loop)
				if err != nil {
					return 0, nil, err
				}
				if !toBoolean(v) {
					return ctlNormal, nil, nil
				}
			}
			k, v, err := body(r, loop)
			if err != nil {
				return 0, nil, err
			}
			if stop, res := r.loopControl(k, labels); stop {
				return res, v, nil
			}
		}
	}, nil
}

func (c *compiler) forInOf(into ast.ForInto, source ast.Expression, bodyStmt ast.Statement, of bool) (stmtFn, error) {
	labels := c.takeLabels()
	src, err := c.expression(source)
	if err != nil {
		return nil, err
	}
	var sc *cscope
	var bind binder
	switch t := into.(type) {
	case *ast.ForIntoVar:
		if t.Binding.Initializer != nil {
			return nil, unsupported(t)
		}
		if bind, err = c.bindingTarget(t.Binding.Target, true); err != nil {
			return nil, err
		}
	case *ast.ForDeclaration:
		sc = c.pushScope(false)
		defer c.popScope()
		kind := kindLet
		if t.IsConst {
			kind = kindConst
		}
		if err := c.declarePattern(sc, t.Target, kind); err != nil {
			return nil, err
		}
		if bind, err = c.bindingTarget(t.Target, true); err != nil {
			return nil, err
		}
	case *ast.ForIntoExpression:
		if bind, err = c.bindingTarget(t.Expression, false); err != nil {
			return nil, err
		}
	default:
		return nil, unsupported(into)
	}
	body, err := c.loopBody(bodyStmt)
	if err != nil {
		return nil, err
	}
	return func(r *rt, s *scope) (ctl, any, error) {
		if err := r.tick(); err != nil {
			return 0, nil, err
		}
		v, err := src(r, s)
		if err != nil {
			return 0, nil, err
		}
		step := func(item any) (ctl, any, error) {
			if err := r.tick(); err != nil {
				return 0, nil, err
			}
			iter := s
			if sc != nil {
				iter = newScope(s, sc.init)
			}
			if err := bind(r, iter, item); err != nil {
				return 0, nil, err
			}
			k, rv, err := body(r, iter)
			if err != nil {
				return 0, nil, err
			}
			// Own labels resolve here; foreign labeled jumps propagate.
			if (k == ctlBreak || k == ctlContinue) && r.label != "" && hasLabel(labels, r.label) {
				r.label = ""
			}
			if k == ctlContinue && r.label == "" {
				return ctlNormal, nil, nil
			}
			return k, rv, nil
		}
		if of {
			return r.iterate(v, step)
		}
		if isNullish(v) {
			return ctlNormal, nil, nil
		}
		for _, k := range ownEnumerableKeys(v) {
			if o, ok := v.(*object); ok {
				if _, still := o.own(k); !still {
					continue // deleted during iteration
				}
			}
			k, rv, err := step(k)
			if err != nil {
				return 0, nil, err
			}
			if k == ctlBreak && r.label == "" {
				break
			}
			if k != ctlNormal {
				return k, rv, nil
			}
		}
		return ctlNormal, nil, nil
	}, nil
}

// iterate runs step over an iterable (arrays live, strings by code point).
func (r *rt) iterate(v any, step func(any) (ctl, any, error)) (ctl, any, error) {
	switch t := v.(type) {
	case *array:
		for i := 0; i < len(t.items); i++ {
			k, rv, err := step(unhole(t.items[i]))
			if err != nil {
				return 0, nil, err
			}
			if k == ctlBreak && r.label == "" {
				break
			}
			if k != ctlNormal {
				return k, rv, nil
			}
		}
		return ctlNormal, nil, nil
	case string:
		for _, ch := range t {
			k, rv, err := step(string(ch))
			if err != nil {
				return 0, nil, err
			}
			if k == ctlBreak && r.label == "" {
				break
			}
			if k != ctlNormal {
				return k, rv, nil
			}
		}
		return ctlNormal, nil, nil
	}
	if it, ok := defaultIterator(v); ok {
		for {
			item, more := it.next()
			if !more {
				return ctlNormal, nil, nil
			}
			if err := r.charge(it.cost); err != nil {
				return 0, nil, err
			}
			k, rv, err := step(item)
			if err != nil {
				return 0, nil, err
			}
			if k == ctlBreak && r.label == "" {
				return ctlNormal, nil, nil
			}
			if k != ctlNormal {
				return k, rv, nil
			}
		}
	}
	return 0, nil, r.notIterable(v)
}

func (r *rt) notIterable(v any) error {
	if isNullish(v) {
		return r.typeError("Cannot convert undefined or null to object")
	}
	return r.typeError("object is not iterable")
}

func (c *compiler) switchStatement(n *ast.SwitchStatement) (stmtFn, error) {
	disc, err := c.expression(n.Discriminant)
	if err != nil {
		return nil, err
	}
	sc := c.pushScope(false)
	defer c.popScope()
	var all []ast.Statement
	for _, cs := range n.Body {
		all = append(all, cs.Consequent...)
	}
	if err := c.declareLexical(sc, all); err != nil {
		return nil, err
	}
	for _, st := range all {
		if _, ok := st.(*ast.FunctionDeclaration); ok {
			return nil, unsupported(st)
		}
	}
	type clause struct {
		test evalFn
		body []stmtFn
	}
	clauses := make([]clause, len(n.Body))
	c.breakable++
	defer func() { c.breakable-- }()
	for i, cs := range n.Body {
		if cs.Test != nil {
			if clauses[i].test, err = c.expression(cs.Test); err != nil {
				return nil, err
			}
		}
		if clauses[i].body, err = c.statements(cs.Consequent); err != nil {
			return nil, err
		}
	}
	def := n.Default
	return func(r *rt, s *scope) (ctl, any, error) {
		if err := r.tick(); err != nil {
			return 0, nil, err
		}
		v, err := disc(r, s)
		if err != nil {
			return 0, nil, err
		}
		inner := newScope(s, sc.init)
		start := -1
		for i, cl := range clauses {
			if cl.test == nil {
				continue
			}
			t, err := cl.test(r, inner)
			if err != nil {
				return 0, nil, err
			}
			if strictEquals(v, t) {
				start = i
				break
			}
		}
		if start < 0 {
			start = def
		}
		if start < 0 {
			return ctlNormal, nil, nil
		}
		for _, cl := range clauses[start:] {
			k, rv, err := runList(r, inner, cl.body)
			if err != nil {
				return 0, nil, err
			}
			if k == ctlBreak && r.label == "" {
				return ctlNormal, nil, nil
			}
			if k != ctlNormal {
				return k, rv, nil
			}
		}
		return ctlNormal, nil, nil
	}, nil
}

func (c *compiler) tryStatement(n *ast.TryStatement) (stmtFn, error) {
	body, err := c.block(n.Body.List)
	if err != nil {
		return nil, err
	}
	var catchBody stmtFn
	var catchScope *cscope
	var catchBind binder
	if n.Catch != nil {
		if n.Catch.Parameter != nil {
			catchScope = c.pushScope(false)
			if err := c.declarePattern(catchScope, n.Catch.Parameter, kindLet); err != nil {
				c.popScope()
				return nil, err
			}
			catchBind, err = c.bindingTarget(n.Catch.Parameter, true)
			if err != nil {
				c.popScope()
				return nil, err
			}
		}
		catchBody, err = c.block(n.Catch.Body.List)
		if catchScope != nil {
			c.popScope()
		}
		if err != nil {
			return nil, err
		}
	}
	var finally stmtFn
	if n.Finally != nil {
		if finally, err = c.block(n.Finally.List); err != nil {
			return nil, err
		}
	}
	return func(r *rt, s *scope) (ctl, any, error) {
		k, v, err := body(r, s)
		var thrown *Throw
		if err != nil && errors.As(err, &thrown) && catchBody != nil && !r.x.aborted.Load() {
			cs := s
			if catchScope != nil {
				// The caught value is reachable now: charge for it, but run
				// the handler even past the budget so that the budget's own
				// RangeError stays catchable. Past it, any allocation the
				// handler makes to keep the value throws.
				_ = r.charge(allocHeader)
				cs = newScope(s, catchScope.init)
				if err := catchBind(r, cs, thrown.Value); err != nil {
					return r.finish(finally, s, 0, nil, err)
				}
			}
			k, v, err = catchBody(r, cs)
		}
		return r.finish(finally, s, k, v, err)
	}, nil
}

// finish runs a finally block; its abrupt completion overrides the try's.
// Uncatchable errors (limits, cancellation, aborts) skip finally entirely.
func (r *rt) finish(finally stmtFn, s *scope, k ctl, v any, err error) (ctl, any, error) {
	if finally == nil {
		return k, v, err
	}
	var thrown *Throw
	if err != nil && !errors.As(err, &thrown) {
		return k, v, err
	}
	fk, fv, ferr := finally(r, s)
	if ferr != nil || fk != ctlNormal {
		return fk, fv, ferr
	}
	return k, v, err
}

func hasLabel(labels []string, l string) bool {
	for _, x := range labels {
		if x == l {
			return true
		}
	}
	return false
}

// loopControl resolves a loop body's completion. stop reports leaving the
// loop with result; unlabeled or own-labeled break/continue act here.
func (r *rt) loopControl(k ctl, labels []string) (bool, ctl) {
	switch k {
	case ctlBreak:
		if r.label == "" || hasLabel(labels, r.label) {
			r.label = ""
			return true, ctlNormal
		}
		return true, ctlBreak
	case ctlContinue:
		if r.label == "" || hasLabel(labels, r.label) {
			r.label = ""
			return false, ctlNormal
		}
		return true, ctlContinue
	case ctlReturn:
		return true, ctlReturn
	}
	return false, ctlNormal
}

func (c *compiler) takeLabels() []string {
	l := c.pendingLabels
	c.pendingLabels = nil
	return l
}

// labelled compiles `label: statement`. Loop labels go to the loop so that
// `continue label` works; any labeled statement can be exited with break.
func (c *compiler) labelled(n *ast.LabelledStatement) (stmtFn, error) {
	labels := []string{n.Label.Name.String()}
	st := n.Statement
	for {
		inner, ok := st.(*ast.LabelledStatement)
		if !ok {
			break
		}
		labels = append(labels, inner.Label.Name.String())
		st = inner.Statement
	}
	switch st.(type) {
	case *ast.ForStatement, *ast.ForOfStatement, *ast.ForInStatement, *ast.WhileStatement, *ast.DoWhileStatement:
		c.pendingLabels = labels
		return c.statement(st)
	case *ast.LexicalDeclaration, *ast.FunctionDeclaration, *ast.ClassDeclaration:
		return nil, unsupported(st)
	}
	c.breakable++
	body, err := c.subStatement(st)
	c.breakable--
	if err != nil {
		return nil, err
	}
	return func(r *rt, s *scope) (ctl, any, error) {
		k, v, err := body(r, s)
		if err == nil && k == ctlBreak && hasLabel(labels, r.label) {
			r.label = ""
			return ctlNormal, nil, nil
		}
		return k, v, err
	}, nil
}

// rootName returns the identifier at the base of a member chain, or "".
func rootName(e ast.Expression) string {
	for {
		switch n := e.(type) {
		case *ast.Identifier:
			return n.Name.String()
		case *ast.DotExpression:
			e = n.Left
		case *ast.BracketExpression:
			e = n.Left
		default:
			return ""
		}
	}
}

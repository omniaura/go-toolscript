package toolscript

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"runtime/debug"
	"strings"
	"testing"
)

// The value-conversion builtins recurse in Go once per level of array/object
// nesting. A script can nest containers far deeper than the goroutine stack
// allows within its ordinary step budget, and overflowing that stack is a
// fatal runtime error the host process cannot recover() — it takes the whole
// backend down. These tests pin that every such builtin instead throws a
// catchable RangeError past maxConversionNesting (and that a cycle still
// renders the way engines do), while leaving conversions within the bound
// unchanged. See maxConversionNesting.

// deepNest builds an array nested `levels` deep: [[[...]]].
func deepNest(levels int) string {
	return "let o = []; for (let i = 0; i < " + itoa(levels) + "; i++) o = [o]; "
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func runScript(t *testing.T, src string) (Result, error) {
	t.Helper()
	return runWith(t, src, ExecuteOptions{})
}

// runWith runs src with opts, answering tool calls with echo by default.
func runWith(t *testing.T, src string, opts ExecuteOptions) (Result, error) {
	t.Helper()
	p, err := Compile(src, options())
	if err != nil {
		t.Fatalf("compile %q: %v", src, err)
	}
	if opts.Dispatch == nil {
		opts.Dispatch = echo
	}
	return p.Execute(context.Background(), opts)
}

// childEnv names the one (sub)test a child copy of the test binary runs.
const childEnv = "TOOLSCRIPT_NESTING_CHILD"

// childMaxStack caps every goroutine stack in a child. A conversion at
// maxConversionNesting needs a few MB, while an unbounded recursion now dies
// at this cap within a second instead of first growing a 1 GB stack.
const childMaxStack = 128 << 20

// inChild runs fn in a child copy of the test binary. A regression here
// overflows the goroutine stack, which is a fatal runtime error recover()
// cannot catch: in-process it would kill the whole test binary and every
// test still to run. In a child it fails only this (sub)test, and the
// child's output says why.
func inChild(t *testing.T, fn func(t *testing.T)) {
	t.Helper()
	if os.Getenv(childEnv) == t.Name() {
		debug.SetMaxStack(childMaxStack)
		fn(t)
		return
	}
	run := strings.Split(t.Name(), "/")
	for i, name := range run {
		run[i] = "^" + regexp.QuoteMeta(name) + "$"
	}
	cmd := exec.Command(os.Args[0], "-test.run="+strings.Join(run, "/"), "-test.count=1", "-test.v", "-test.timeout=2m")
	// Under -race a process sleeps a second at exit (GORACE atexit_sleep_ms)
	// to catch late races; a child has none to catch.
	gorace := strings.TrimSpace(os.Getenv("GORACE") + " atexit_sleep_ms=0")
	cmd.Env = append(os.Environ(), childEnv+"="+t.Name(), "GORACE="+gorace)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child test process failed (%v):\n%s", err, excerpt(out))
	}
	if !bytes.Contains(out, []byte("--- PASS: "+t.Name()+" ")) {
		t.Fatalf("child test process did not run %s:\n%s", t.Name(), excerpt(out))
	}
}

// excerpt keeps the head of a child's output (a fatal error's reason comes
// first, before a long goroutine dump) and its tail (the test verdicts).
func excerpt(out []byte) string {
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) <= 40 {
		return strings.Join(lines, "\n")
	}
	return strings.Join(lines[:25], "\n") + "\n...\n" + strings.Join(lines[len(lines)-15:], "\n")
}

// TestDeepConversionsThrowRangeError covers every builtin that recurses once
// per level of nesting: each must throw a RangeError well short of the
// goroutine stack limit rather than crash the process. The nesting depth
// (20000) is deliberately past maxConversionNesting (10000); the data array is
// built with a loop, so only the conversion recurses in Go, and the fix caps
// that recursion at 10000 frames regardless of how deep the data goes.
func TestDeepConversionsThrowRangeError(t *testing.T) {
	deep := deepNest(20000)
	for _, tc := range []struct{ name, src string }{
		{"String()", deep + `return String(o).length;`},
		{"template-literal", deep + "return `${o}`.length;"},
		{"join", deep + `return o.join("-").length;`},
		{"toString", deep + `return o.toString().length;`},
		{"loose-equals", deep + `return o == 1;`},
		{"relational", deep + `return o < 1;`},
		{"sort-default-compare", deep + `return [o, [1]].sort().length;`},
		{"flat-infinity", deep + `return o.flat(Infinity).length;`},
		{"toLocaleString", deep + `return o.toLocaleString().length;`},
		// JSON.stringify already bounds its recursion at 10000 (stringifier.str).
		{"JSON.stringify", deep + `return JSON.stringify(o).length;`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := runScript(t, tc.src)
			if err == nil || !strings.Contains(err.Error(), "RangeError: Maximum call stack size exceeded") {
				t.Fatalf("got value=%#v err=%v, want a RangeError (stack size)", res.Value, err)
			}
		})
	}
}

// TestDeepJSONParseIsBounded: decodeToken recurses once per level of nesting,
// so it stops at maxConversionNesting itself with a SyntaxError instead of
// recursing until the goroutine stack is exhausted. It must not rely on the
// standard library: json.Decoder's streaming Token() enforces a depth cap on
// some Go releases and not on others (Go 1.26 parses 20000 levels here).
func TestDeepJSONParseIsBounded(t *testing.T) {
	src := `const s = "[".repeat(20000) + "]".repeat(20000); return JSON.parse(s).length;`
	res, err := runScript(t, src)
	if err == nil {
		t.Fatalf("got value=%#v, want a bounded parse error", res.Value)
	}
	if !strings.Contains(err.Error(), "max depth") {
		t.Fatalf("got err=%v, want a depth-bounded SyntaxError", err)
	}
}

// TestDeepConversionsAreCatchable proves the RangeError is an ordinary
// exception the script can catch, not a process-level failure.
func TestDeepConversionsAreCatchable(t *testing.T) {
	src := deepNest(20000) + `try { String(o); return "no throw"; } catch (e) { return e.name; }`
	res, err := runScript(t, src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Value != "RangeError" {
		t.Fatalf("got %#v, want RangeError caught", res.Value)
	}
}

// TestCyclicConversions covers self-referential containers, where the depth
// bound is not enough on its own: join and toLocaleString must terminate via a
// cycle guard (engines render a cycle as the empty string), and flat — which
// has no such guard in any engine — must throw rather than loop forever.
func TestCyclicConversions(t *testing.T) {
	for _, tc := range []struct {
		name, src string
		want      any
	}{
		{"String", `const a = []; a.push(a); return String(a);`, ""},
		{"join", `const a = []; a.push(a); return a.join("-");`, ""},
		{"toString", `const a = []; a.push(a); return a.toString();`, ""},
		// A cycle with a sibling: join renders the cyclic branch as "".
		{"join-sibling", `const a = [1]; a.push(a); return a.join(",");`, "1,"},
		{"toLocaleString", `const a = []; a.push(a); return a.toLocaleString();`, ""},
		{"toLocaleString-sibling", `const a = [1]; a.push(a); return a.toLocaleString();`, "1,"},
		// String(cyclic) coerces to "" then to number 0, so 0 == 1 is false;
		// the point is that it returns rather than recursing forever.
		{"loose-equals", `const a = []; a.push(a); return a == 1;`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inChild(t, func(t *testing.T) {
				res, err := runScript(t, tc.src)
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if !reflect.DeepEqual(res.Value, tc.want) {
					t.Fatalf("got %#v, want %#v", res.Value, tc.want)
				}
			})
		})
	}
}

// TestCyclicFlatThrows pins flat(Infinity) on a cyclic array: there is no cycle
// guard (engines have none either), so it must hit the nesting bound and throw
// a RangeError instead of recursing until the stack overflows.
func TestCyclicFlatThrows(t *testing.T) {
	inChild(t, func(t *testing.T) {
		res, err := runScript(t, `const a = []; a.push(a); return a.flat(Infinity).length;`)
		if err == nil || !strings.Contains(err.Error(), "RangeError: Maximum call stack size exceeded") {
			t.Fatalf("got value=%#v err=%v, want a RangeError", res.Value, err)
		}
	})
}

// TestConversionsWithinBoundUnchanged proves the bound does not alter ordinary,
// shallow conversions: nesting comfortably under maxConversionNesting still
// converts, flattens, serialises and round-trips exactly as before.
func TestConversionsWithinBoundUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name, src string
		want      any
	}{
		{"String-100-deep", `let o = [1]; for (let i = 0; i < 100; i++) o = [o]; return String(o);`, "1"},
		{"flat-100-deep", `let o = [1]; for (let i = 0; i < 100; i++) o = [o]; return o.flat(Infinity).length;`, float64(1)},
		{"toLocaleString-100-deep", `let o = [1]; for (let i = 0; i < 100; i++) o = [o]; return o.toLocaleString();`, "1"},
		{"join-shallow", `return [1, [2, 3], 4].join(",");`, "1,2,3,4"},
		{"JSON.parse-shallow", `return JSON.parse("[[[[1]]]]")[0][0][0][0];`, float64(1)},
		{"JSON.roundtrip-1000-deep", `let o = 1; for (let i = 0; i < 1000; i++) o = [o]; return JSON.parse(JSON.stringify(o)).length;`, float64(1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := runScript(t, tc.src)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(res.Value, tc.want) {
				t.Fatalf("got %#v, want %#v", res.Value, tc.want)
			}
		})
	}
}

// TestFromGoBoundsNesting covers the host-data converter: a tool or host
// dispatcher that returns data nested past maxConversionNesting must fail the
// conversion rather than recurse until the stack overflows. The dispatcher
// here returns a Go structure directly, bypassing the script-side export
// limits that would otherwise cap it first.
func TestFromGoBoundsNesting(t *testing.T) {
	var deep any = float64(1)
	for i := 0; i < 20000; i++ {
		deep = []any{deep}
	}
	p, err := Compile(`return mcp.deep({}).length;`, options())
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	_, err = p.Execute(context.Background(), ExecuteOptions{
		Dispatch: func(context.Context, string, any) (any, error) { return deep, nil },
	})
	if err == nil || !strings.Contains(err.Error(), "Maximum call stack size exceeded") {
		t.Fatalf("got err=%v, want a stack-size error from fromGo", err)
	}
	// Host data within the bound still converts.
	var shallow any = float64(1)
	for i := 0; i < 100; i++ {
		shallow = []any{shallow}
	}
	res, err := p.Execute(context.Background(), ExecuteOptions{
		Dispatch: func(context.Context, string, any) (any, error) { return shallow, nil },
	})
	if err != nil || res.Value != float64(1) {
		t.Fatalf("got value=%#v err=%v, want length 1", res.Value, err)
	}
}

// cyclicError starts a script with an Error e whose message is e itself.
const cyclicError = `const e = new Error("x"); e.message = e; `

// errorChain builds e, an Error nested `levels` deep through message.
func errorChain(levels int) string {
	return `let e = new Error("y"); for (let i = 0; i < ` + itoa(levels) + `; i++) { const n = new Error("y"); n.message = e; e = n; } `
}

// sharedErrorGraph builds x, 64 levels of Errors whose name and message are
// both the level below: a conversion that does not stop visits 2^64 Errors.
const sharedErrorGraph = `let x = ""; for (let i = 0; i < 64; i++) { const n = new Error(); n.name = x; n.message = x; x = n; } `

// TestErrorConversionsThrowRangeError covers Error.prototype.toString, which
// recursed through ToString(name) and ToString(message) with no bound, on a
// fresh runtime per Error that also reset join's bound at every Error. A
// cyclic Error, a long chain of Errors, or arrays linked through Errors must
// throw a catchable RangeError, as V8 does, instead of overflowing the stack.
func TestErrorConversionsThrowRangeError(t *testing.T) {
	for _, tc := range []struct{ name, src string }{
		{"cyclic-message-String", cyclicError + `return String(e);`},
		{"cyclic-message-concat", cyclicError + `return "" + e;`},
		{"cyclic-message-toString", cyclicError + `return e.toString();`},
		{"cyclic-message-join", cyclicError + `return [e].join(",");`},
		{"cyclic-message-toLocaleString", cyclicError + `return [e].toLocaleString();`},
		{"cyclic-name-template", `const e = new Error("x"); e.name = e; return ` + "`${e}`;"},
		{"cyclic-through-two-errors", `const a = new Error("a"), b = new Error("b"); a.message = b; b.message = a; return String(a);`},
		// No cycle, just 50000 Errors each the message of the next.
		{"deep-error-chain", errorChain(50000) + `return String(e).length;`},
		// Three Errors whose messages each nest 9000 arrays around the previous
		// Error: 27000 levels, though no single run of arrays reaches the bound.
		{"arrays-through-errors", `let e = new Error("z"); for (let k = 0; k < 3; k++) { let o = e; for (let i = 0; i < 9000; i++) o = [o]; const n = new Error("w"); n.message = o; e = n; } return String(e).length;`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inChild(t, func(t *testing.T) {
				res, err := runScript(t, tc.src)
				if err == nil || !strings.Contains(err.Error(), "RangeError: Maximum call stack size exceeded") {
					t.Fatalf("got value=%#v err=%v, want a RangeError (stack size)", res.Value, err)
				}
			})
		})
	}
}

// TestErrorConversionRangeErrorIsCatchable: the RangeError from a cyclic
// Error is an ordinary exception the script can catch.
func TestErrorConversionRangeErrorIsCatchable(t *testing.T) {
	inChild(t, func(t *testing.T) {
		res, err := runScript(t, cyclicError+`try { String(e); return "no throw"; } catch (x) { return x.name; }`)
		if err != nil || res.Value != "RangeError" {
			t.Fatalf("got value=%#v err=%v, want RangeError caught", res.Value, err)
		}
	})
}

// TestErrorConversionsMatchV8 pins Error conversions that terminate the way
// V8's do. A cycle through an array is cut by join's guard, which renders the
// cyclic array as "". Name and message convert on the script's own runtime,
// so an own toString on either runs, and an exception from it propagates.
func TestErrorConversionsMatchV8(t *testing.T) {
	for _, tc := range []struct {
		name, src string
		want      any
	}{
		{"message-array-cycle", `const e = new Error("x"); e.message = [e]; return String(e);`, "Error: Error"},
		{"own-toString-name", `const e = new Error("m"); e.name = {toString() { return "Custom"; }}; return String(e);`, "Custom: m"},
		{"own-toString-message", `const e = new Error("m"); e.message = {toString() { return "from toString"; }}; return ` + "`${e}`;", "Error: from toString"},
		{"throwing-name", `const e = new Error("m"); e.name = {toString() { throw new TypeError("name boom"); }}; try { String(e); return "no throw"; } catch (x) { return x.name + ": " + x.message; }`, "TypeError: name boom"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inChild(t, func(t *testing.T) {
				res, err := runScript(t, tc.src)
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if !reflect.DeepEqual(res.Value, tc.want) {
					t.Fatalf("got %#v, want %#v", res.Value, tc.want)
				}
			})
		})
	}
}

// TestErrorConversionWorkIsCharged covers an Error graph whose levels share
// sub-Errors: converting it visits exponentially many Errors with no deep
// recursion at all. Each Error level costs a step, so MaxSteps stops it.
func TestErrorConversionWorkIsCharged(t *testing.T) {
	inChild(t, func(t *testing.T) {
		res, err := runScript(t, sharedErrorGraph+`return String(x).length;`)
		if err == nil || !strings.Contains(err.Error(), "step limit exceeded") {
			t.Fatalf("got value=%#v err=%v, want the step limit", res.Value, err)
		}
	})
}

// TestDetachedErrorTextIsBounded covers error text rendered with no live
// runtime: Throw.Error() (the host formatting an uncaught exception), an
// Error's stack property, and the console's fallback text for an Error.
// These cannot throw, so a cyclic part renders as nothing, nesting stops at
// maxConversionNesting, and a shared graph stops at the visit budget.
func TestDetachedErrorTextIsBounded(t *testing.T) {
	// An unbroken chain renders "Error: " for every level up to the bound;
	// the level past it renders nothing, so the last one shows just "Error".
	chainText := strings.Repeat("Error: ", maxConversionNesting-1) + "Error"
	for _, tc := range []struct {
		name, src string
		thrown    *string // want err.Error()
		value     any     // want res.Value when thrown is nil
		console   string  // want this console line, if set
	}{
		{name: "throw-cyclic-message", src: cyclicError + `throw e;`, thrown: ptr("Error")},
		{name: "throw-cyclic-name", src: `const e = new Error("x"); e.name = e; throw e;`, thrown: ptr("x")},
		{name: "throw-array-with-cyclic-error", src: cyclicError + `throw [e, 2];`, thrown: ptr("Error,2")},
		{name: "throw-cyclic-array", src: `const a = [1]; a.push(a); throw a;`, thrown: ptr("1,")},
		{name: "throw-deep-error-chain", src: errorChain(50000) + `throw e;`, thrown: &chainText},
		{name: "throw-shared-error-graph", src: sharedErrorGraph + `throw x;`, thrown: ptr("")},
		{name: "stack-cyclic-message", src: cyclicError + `return e.stack;`, value: "Error"},
		{name: "stack-deep-error-chain", src: errorChain(50000) + `return e.stack === "Error: ".repeat(` + itoa(maxConversionNesting-1) + `) + "Error";`, value: true},
		{name: "stack-shared-error-graph", src: sharedErrorGraph + `return x.stack.length;`, value: float64(0)},
		// console.log falls back to the Error's text when it cannot export it
		// (here, because of the function property).
		{name: "console-cyclic-message", src: cyclicError + `e.f = () => 1; console.log(e); return 1;`, value: float64(1), console: "Error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inChild(t, func(t *testing.T) {
				var lines []string
				res, err := runWith(t, tc.src, ExecuteOptions{Console: func(_, line string) { lines = append(lines, line) }})
				if tc.thrown != nil {
					if err == nil {
						t.Fatalf("got value=%#v, want a thrown error", res.Value)
					}
					if got := err.Error(); got != *tc.thrown {
						t.Fatalf("got error text of %d bytes %.80q, want %d bytes %.80q", len(got), got, len(*tc.thrown), *tc.thrown)
					}
					return
				}
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if !reflect.DeepEqual(res.Value, tc.value) {
					t.Fatalf("got %#v, want %#v", res.Value, tc.value)
				}
				if tc.console != "" && (len(lines) != 1 || lines[0] != tc.console) {
					t.Fatalf("got console %q, want [%q]", lines, tc.console)
				}
			})
		})
	}
}

func ptr(s string) *string { return &s }

// TestErrorTextWithinBoundUnchanged proves ordinary Errors render exactly as
// before through every path: String(), stack, the uncaught-exception text and
// the console fallback. (A positive control: it passes before this fix too.)
func TestErrorTextWithinBoundUnchanged(t *testing.T) {
	deepMsg := `const e = new Error("x"); let o = [1]; for (let i = 0; i < 100; i++) o = [o]; e.message = o; `
	for _, tc := range []struct {
		name, src string
		thrown    string
		value     any
		console   string
	}{
		{name: "String", src: `return String(new Error("x"));`, value: "Error: x"},
		{name: "stack", src: `return new TypeError("bad").stack;`, value: "TypeError: bad"},
		{name: "empty-name", src: `const e = new Error("m"); e.name = ""; return [String(e), e.stack];`, value: []any{"m", "m"}},
		{name: "empty-message", src: `const e = new RangeError(""); return [String(e), e.stack];`, value: []any{"RangeError", "RangeError"}},
		{name: "nested-error", src: `const e = new TypeError("outer"); e.message = new Error("inner"); return [String(e), e.stack];`, value: []any{"TypeError: Error: inner", "TypeError: Error: inner"}},
		{name: "deep-array-message", src: deepMsg + `return [String(e), e.stack];`, value: []any{"Error: 1", "Error: 1"}},
		{name: "throw-error", src: `throw new Error("boom");`, thrown: "Error: boom"},
		{name: "throw-deep-array-message", src: deepMsg + `throw e;`, thrown: "Error: 1"},
		{name: "throw-array", src: `throw [1, [2, 3], null, "s"];`, thrown: "1,2,3,,s"},
		{name: "console-error-fallback", src: `const e = new RangeError("r"); e.f = () => 1; console.log(e); return 1;`, value: float64(1), console: "RangeError: r"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var lines []string
			res, err := runWith(t, tc.src, ExecuteOptions{Console: func(_, line string) { lines = append(lines, line) }})
			if tc.thrown != "" {
				if err == nil || err.Error() != tc.thrown {
					t.Fatalf("got value=%#v err=%v, want thrown %q", res.Value, err, tc.thrown)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(res.Value, tc.value) {
				t.Fatalf("got %#v, want %#v", res.Value, tc.value)
			}
			if tc.console != "" && (len(lines) != 1 || lines[0] != tc.console) {
				t.Fatalf("got console %q, want [%q]", lines, tc.console)
			}
		})
	}
}

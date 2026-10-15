package toolscript

import (
	"context"
	"reflect"
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
	p, err := Compile(src, options())
	if err != nil {
		t.Fatalf("compile %q: %v", src, err)
	}
	return p.Execute(context.Background(), ExecuteOptions{Dispatch: echo})
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

// TestCyclicFlatThrows pins flat(Infinity) on a cyclic array: there is no cycle
// guard (engines have none either), so it must hit the nesting bound and throw
// a RangeError instead of recursing until the stack overflows.
func TestCyclicFlatThrows(t *testing.T) {
	res, err := runScript(t, `const a = []; a.push(a); return a.flat(Infinity).length;`)
	if err == nil || !strings.Contains(err.Error(), "RangeError: Maximum call stack size exceeded") {
		t.Fatalf("got value=%#v err=%v, want a RangeError", res.Value, err)
	}
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

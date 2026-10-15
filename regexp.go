package toolscript

import (
	"errors"
	"fmt"
	"regexp"
	"regexp/syntax"
	"strings"
	"unicode/utf16"

	"github.com/dlclark/regexp2/v2"
	"github.com/dop251/goja/parser"
)

// Regular expressions run on Go's RE2 engine after Goja's own JS-to-RE2
// transform, which is exactly Goja's fast path. Patterns that need
// backtracking (backreferences, lookaround) or the u flag decline, so the
// host falls back to its JavaScript runtime.

type compiledRegexp struct {
	re          *regexp.Regexp
	source      string
	flags       string
	global      bool
	ignoreCase  bool
	multiline   bool
	dotAll      bool
	sticky      bool
	contextFree bool // no ^, \b or \B: matching a suffix equals matching from an offset
}

type regexpValue struct {
	pat       *compiledRegexp
	lastIndex any
	props     *object // own properties assigned by the script
}

// compileRegexp validates flags and translates a JavaScript pattern.
// It returns ok=false for patterns outside the RE2-compatible subset and a
// non-nil syntaxErr for patterns that are invalid in JavaScript too.
func compileRegexp(pattern, flags string) (p *compiledRegexp, ok bool, syntaxErr string) {
	p = &compiledRegexp{source: pattern}
	seen := map[rune]bool{}
	for _, f := range flags {
		if seen[f] {
			return nil, true, fmt.Sprintf("Invalid flags supplied to RegExp constructor '%s'", flags)
		}
		seen[f] = true
		switch f {
		case 'g':
			p.global = true
		case 'i':
			p.ignoreCase = true
		case 'm':
			p.multiline = true
		case 's':
			p.dotAll = true
		case 'y':
			p.sticky = true
		case 'u':
			return nil, false, "" // unicode mode maps positions differently
		default:
			return nil, true, fmt.Sprintf("Invalid flags supplied to RegExp constructor '%s'", flags)
		}
	}
	for _, r := range pattern {
		if r > 0xFFFF {
			return nil, false, "" // Goja rewrites astral literals into surrogate escapes
		}
	}
	for _, f := range "gimsy" {
		if seen[f] {
			p.flags += string(f)
		}
	}
	if p.flags != "" {
		// Match Goja's canonical flags order (g i m s u y).
		var b strings.Builder
		for _, f := range "gimsuy" {
			if seen[f] {
				b.WriteRune(f)
			}
		}
		p.flags = b.String()
	}
	re2, err := parser.TransformRegExp(pattern, p.dotAll, false)
	if err != nil {
		var incompat parser.RegexpErrorIncompatible
		if asIncompatible(err, &incompat) {
			bad := backtrackingSyntaxError(pattern, p)
			return nil, bad != "", bad
		}
		return nil, true, err.Error() // Goja throws the parser's text as is
	}
	prefix := ""
	if p.multiline {
		prefix += "m"
	}
	if p.dotAll {
		prefix += "s"
	}
	if p.ignoreCase {
		prefix += "i"
	}
	if prefix != "" {
		re2 = "(?" + prefix + ":" + re2 + ")"
	}
	re, err := regexp.Compile(re2)
	if err != nil {
		// Goja reports RE2's rejection as a SyntaxError, except an invalid
		// repeat size, which it retries with its backtracking engine.
		var serr *syntax.Error
		if !errors.As(err, &serr) || serr.Code != syntax.ErrInvalidRepeatSize {
			return nil, true, fmt.Sprintf("Invalid regular expression (re2): %s (%v)", re2, err)
		}
		bad := backtrackingSyntaxError(pattern, p)
		return nil, bad != "", bad
	}
	p.re = re
	parsed, err := syntax.Parse(re2, syntax.Perl)
	p.contextFree = err == nil && !hasContextAssertion(parsed)
	return p, true, ""
}

// backtrackingSyntaxError validates a pattern RE2 cannot run the way Goja
// does, with regexp2 (never used for matching here). It returns Goja's
// SyntaxError text when regexp2 rejects the pattern too; "" means valid,
// so the caller declines instead.
func backtrackingSyntaxError(pattern string, p *compiledRegexp) string {
	opts := regexp2.ECMAScript
	if p.multiline {
		opts |= regexp2.Multiline
	}
	if p.dotAll {
		opts |= regexp2.Singleline
	}
	if p.ignoreCase {
		opts |= regexp2.IgnoreCase
	}
	if _, err := regexp2.Compile(pattern, opts); err != nil {
		inner := fmt.Sprintf("Invalid regular expression (regexp2): %s (%v)", pattern, err)
		return fmt.Sprintf("Invalid regular expression (regexp2): %s (%s)", pattern, inner)
	}
	return ""
}

func asIncompatible(err error, target *parser.RegexpErrorIncompatible) bool {
	e, ok := err.(parser.RegexpErrorIncompatible)
	if ok {
		*target = e
	}
	return ok
}

func hasContextAssertion(re *syntax.Regexp) bool {
	switch re.Op {
	case syntax.OpBeginLine, syntax.OpBeginText, syntax.OpWordBoundary, syntax.OpNoWordBoundary:
		return true
	}
	for _, sub := range re.Sub {
		if hasContextAssertion(sub) {
			return true
		}
	}
	return false
}

// subject is a string prepared for RE2 matching in UTF-16 code units. For
// non-ASCII text each unit becomes one rune (surrogates map into a private
// plane) so indexes line up with JavaScript's.
type subject struct {
	units []uint16
	str   string
	unit  []int // byte offset -> unit index, for non-ASCII subjects
	ascii bool
	orig  string
}

func newSubject(s string) *subject {
	if isASCII(s) {
		return &subject{str: s, ascii: true, orig: s}
	}
	sub := &subject{units: toUnits(s), orig: s}
	var b strings.Builder
	sub.unit = make([]int, 0, len(s)+1)
	for i, u := range sub.units {
		r := rune(u)
		if utf16.IsSurrogate(r) {
			r = 0xF0000 + (r - 0xD800)
		}
		n, _ := b.WriteRune(r)
		for range n {
			sub.unit = append(sub.unit, i)
		}
	}
	sub.unit = append(sub.unit, len(sub.units))
	sub.str = b.String()
	return sub
}

func (s *subject) length() int {
	if s.ascii {
		return len(s.str)
	}
	return len(s.units)
}

func (s *subject) slice(from, to int) string {
	if s.ascii {
		return s.str[from:to]
	}
	return fromUnits(s.units[from:to])
}

// byteOffset maps a unit index back to a byte offset in s.str.
func (s *subject) byteOffset(unit int) int {
	if s.ascii {
		return unit
	}
	for b, u := range s.unit {
		if u == unit {
			return b
		}
	}
	return len(s.str)
}

func (s *subject) mapIndexes(m []int, base int) []int {
	if m == nil {
		return nil
	}
	out := make([]int, len(m))
	for i, b := range m {
		switch {
		case b < 0:
			out[i] = -1
		case s.ascii:
			out[i] = b + base
		default:
			out[i] = s.unit[b+s.byteOffset(base)]
		}
	}
	return out
}

// execAt finds the first match at or after unit index start.
func (r *rt) execAt(p *compiledRegexp, s *subject, start int) ([]int, error) {
	if start > s.length() {
		return nil, nil
	}
	var m []int
	if start == 0 {
		m = s.mapIndexes(p.re.FindStringSubmatchIndex(s.str), 0)
	} else {
		if !p.contextFree {
			return nil, errRuntimeUnsupported("regular expression assertion at a non-zero lastIndex")
		}
		off := s.byteOffset(start)
		m = s.mapIndexes(p.re.FindStringSubmatchIndex(s.str[off:]), start)
	}
	if m != nil && p.sticky && m[0] != start {
		return nil, nil
	}
	return m, nil
}

// findAll returns successive matches from the start, as Goja does for
// global match/replace/split.
func (p *compiledRegexp) findAll(s *subject) [][]int {
	all := p.re.FindAllStringSubmatchIndex(s.str, -1)
	out := make([][]int, 0, len(all))
	pos := 0
	for _, m := range all {
		mapped := s.mapIndexes(m, 0)
		if p.sticky {
			if mapped[0] != pos {
				break
			}
			pos = mapped[1]
		}
		out = append(out, mapped)
	}
	return out
}

func (rx *regexpValue) lastIndexInt(r *rt) (int, error) {
	f, err := r.toNumber(rx.lastIndex)
	if err != nil {
		return 0, err
	}
	f = toIntegerOrInfinity(f)
	if f < 0 {
		return 0, nil
	}
	if f > 1<<31 {
		return 1 << 31, nil
	}
	return int(f), nil
}

// exec implements RegExpBuiltinExec including lastIndex updates.
func (r *rt) regexpExec(rx *regexpValue, str string) ([]int, *subject, error) {
	p := rx.pat
	start := 0
	if p.global || p.sticky {
		var err error
		if start, err = rx.lastIndexInt(r); err != nil {
			return nil, nil, err
		}
	}
	s := newSubject(str)
	m, err := r.execAt(p, s, start)
	if err != nil {
		return nil, nil, err
	}
	if p.global || p.sticky {
		if m != nil {
			rx.lastIndex = float64(m[1])
		} else {
			rx.lastIndex = float64(0)
		}
	}
	return m, s, nil
}

func (p *compiledRegexp) groupNames() []string { return p.re.SubexpNames() }

// matchArray builds an exec() result: captures plus index, input, groups.
func (p *compiledRegexp) matchArray(s *subject, m []int) *array {
	n := len(m) / 2
	items := make([]any, n)
	for i := range n {
		if m[2*i] >= 0 {
			items[i] = s.slice(m[2*i], m[2*i+1])
		} else {
			items[i] = Undefined
		}
	}
	a := &array{items: items, props: newObject(3)}
	a.props.set("input", s.orig)
	a.props.set("index", float64(m[0]))
	a.props.set("groups", p.groupsObject(items))
	return a
}

// matchValue builds an exec/match result and charges for it: the array and
// its captures, and its index/input/groups properties (input is the subject
// string itself, which the result shares).
func (r *rt) matchValue(p *compiledRegexp, s *subject, m []int) (any, error) {
	a := p.matchArray(s, m)
	n := freshSize(a, 1) + allocHeader + 3*allocProp
	if g, ok := a.props.own("groups"); ok {
		n += freshSize(g, 1)
	}
	if err := r.charge(n); err != nil {
		return nil, err
	}
	return a, nil
}

func (p *compiledRegexp) groupsObject(captures []any) any {
	var groups *object
	for i, name := range p.groupNames() {
		if i == 0 || name == "" || i >= len(captures) {
			continue
		}
		if groups == nil {
			groups = newObject(2)
		}
		groups.set(name, captures[i])
	}
	if groups == nil {
		return Undefined
	}
	return groups
}

func (r *rt) regexpMethod(rx *regexpValue, key string, args []any) (any, error) {
	switch key {
	case "test", "exec":
		str, err := r.toString(arg(args, 0))
		if err != nil {
			return nil, err
		}
		m, s, err := r.regexpExec(rx, str)
		if err != nil {
			return nil, err
		}
		if key == "test" {
			return m != nil, nil
		}
		if m == nil {
			return nil, nil
		}
		return r.matchValue(rx.pat, s, m)
	case "toString":
		return r.stringValue("/" + rx.pat.source + "/" + rx.pat.flags)
	}
	return nil, r.typeError("Object has no member '" + key + "'")
}

func (rx *regexpValue) get(key string) (any, bool) {
	p := rx.pat
	switch key {
	case "lastIndex":
		return rx.lastIndex, true
	case "source":
		return p.source, true
	case "flags":
		return p.flags, true
	case "global":
		return p.global, true
	case "ignoreCase":
		return p.ignoreCase, true
	case "multiline":
		return p.multiline, true
	case "dotAll":
		return p.dotAll, true
	case "sticky":
		return p.sticky, true
	case "unicode", "hasIndices":
		return false, true
	}
	return nil, false
}

// ---- String.prototype methods with a RegExp argument ----

func (r *rt) stringMatch(s string, rx *regexpValue) (any, error) {
	if !rx.pat.global {
		m, sub, err := r.regexpExec(rx, s)
		if err != nil || m == nil {
			return nil, err
		}
		return r.matchValue(rx.pat, sub, m)
	}
	rx.resetLastIndex()
	sub := newSubject(s)
	all := rx.pat.findAll(sub)
	if len(all) == 0 {
		return nil, nil
	}
	if len(all) > r.x.maxItems {
		return nil, r.rangeError("Invalid array length")
	}
	out := make([]any, len(all))
	for i, m := range all {
		out[i] = sub.slice(m[0], m[1])
	}
	a := &array{items: out}
	return a, r.chargeFresh(a)
}

func (r *rt) stringSearch(s string, rx *regexpValue) (any, error) {
	sub := newSubject(s)
	m, err := r.execAt(&compiledRegexp{re: rx.pat.re, contextFree: rx.pat.contextFree}, sub, 0)
	if err != nil {
		return nil, err
	}
	if m == nil || (rx.pat.sticky && m[0] != 0) {
		return float64(-1), nil
	}
	return float64(m[0]), nil
}

func (r *rt) stringReplaceRegexp(s string, rx *regexpValue, repl any, all bool) (any, error) {
	p := rx.pat
	if all && !p.global {
		return nil, r.typeError("String.prototype.replaceAll called with a non-global RegExp argument")
	}
	sub := newSubject(s)
	var found [][]int
	if p.global {
		found = p.findAll(sub)
		rx.resetLastIndex()
	} else {
		start := 0
		if p.sticky {
			var err error
			if start, err = rx.lastIndexInt(r); err != nil {
				return nil, err
			}
		}
		m, err := r.execAt(p, sub, start)
		if err != nil {
			return nil, err
		}
		if m != nil {
			found = [][]int{m}
		}
		if p.sticky {
			if m != nil {
				rx.lastIndex = float64(m[1])
			} else {
				rx.lastIndex = float64(0)
			}
		}
	}
	if len(found) == 0 {
		return s, nil
	}
	fn, isFn := repl.(*function)
	var replStr string
	if !isFn {
		var err error
		if replStr, err = r.toString(repl); err != nil {
			return nil, err
		}
	}
	var b strings.Builder
	last := 0
	for _, m := range found {
		if m[0] != last {
			b.WriteString(sub.slice(last, m[0]))
		}
		n := len(m) / 2
		captures := make([]any, n)
		for i := range n {
			if m[2*i] >= 0 {
				captures[i] = sub.slice(m[2*i], m[2*i+1])
			} else {
				captures[i] = Undefined
			}
		}
		if isFn {
			callArgs := append(append([]any{}, captures...), float64(m[0]), s)
			if g := p.groupsObject(captures); g != Undefined {
				callArgs = append(callArgs, g)
			}
			v, err := r.call(fn, callArgs)
			if err != nil {
				return nil, err
			}
			str, err := r.toString(v)
			if err != nil {
				return nil, err
			}
			b.WriteString(str)
		} else {
			writeSubstitution(&b, sub, m[0], captures, p, replStr)
		}
		last = m[1]
		if b.Len() > r.x.maxString {
			return nil, r.rangeError("Invalid string length")
		}
	}
	if last < sub.length() {
		b.WriteString(sub.slice(last, sub.length()))
	}
	return r.stringValue(b.String())
}

// writeSubstitution ports Goja's GetSubstitution for regular expressions.
func writeSubstitution(b *strings.Builder, s *subject, position int, captures []any, p *compiledRegexp, repl string) {
	capture := func(i int) string {
		if v, ok := captures[i].(string); ok {
			return v
		}
		return ""
	}
	matched := capture(0)
	tail := position + utf16Len(matched)
	names := p.groupNames()
	named := false
	for _, n := range names {
		if n != "" {
			named = true
		}
	}
	units := toUnits(repl)
	rl := len(units)
	for i := 0; i < rl; i++ {
		c := units[i]
		if c != '$' || i >= rl-1 {
			b.WriteString(fromUnits(units[i : i+1]))
			continue
		}
		ch := units[i+1]
		switch ch {
		case '$':
			b.WriteByte('$')
		case '`':
			b.WriteString(s.slice(0, position))
		case '\'':
			if tail < s.length() {
				b.WriteString(s.slice(tail, s.length()))
			}
		case '&':
			b.WriteString(matched)
		case '<':
			end := -1
			for j := i + 2; j < rl; j++ {
				if units[j] == '>' {
					end = j
					break
				}
			}
			if end >= 0 && named {
				ref := fromUnits(units[i+2 : end])
				for gi, n := range names {
					if n == ref && gi < len(captures) {
						b.WriteString(capture(gi))
					}
				}
				i = end
				continue
			}
			b.WriteString("$<")
		default:
			index := 0
			j := i + 1
			lim := min(j+2, rl)
			for ; j < lim; j++ {
				d := units[j]
				if d < '0' || d > '9' {
					break
				}
				m := index*10 + int(d-'0')
				if m >= len(captures) {
					break
				}
				index = m
			}
			if index > 0 {
				b.WriteString(capture(index))
				i = j - 1
				continue
			}
			b.WriteByte('$')
			b.WriteString(fromUnits([]uint16{ch}))
		}
		i++
	}
}

func (r *rt) stringSplitRegexp(s string, rx *regexpValue, limitValue any) (any, error) {
	limit := -1
	if !isUndefined(limitValue) {
		f, err := r.toNumber(limitValue)
		if err != nil {
			return nil, err
		}
		limit = int(toUint32(f))
	}
	if limit == 0 {
		return &array{items: []any{}}, nil
	}
	sub := newSubject(s)
	target := sub.length()
	out := []any{}
	results := (&compiledRegexp{re: rx.pat.re}).findAll(sub)
	if target == 0 {
		if len(results) == 0 {
			out = append(out, s)
		}
		return &array{items: out}, nil
	}
	last, found := 0, 0
	for _, m := range results {
		if m[0] == m[1] && (m[0] == 0 || m[0] == target) {
			continue
		}
		if last != m[0] {
			out = append(out, sub.slice(last, m[0]))
		} else {
			out = append(out, "")
		}
		found++
		last = m[1]
		if found == limit {
			return &array{items: out}, nil
		}
		for i := 1; i < len(m)/2; i++ {
			if m[2*i] >= 0 {
				out = append(out, sub.slice(m[2*i], m[2*i+1]))
			} else {
				out = append(out, Undefined)
			}
			found++
			if found == limit {
				return &array{items: out}, nil
			}
		}
	}
	if last != target {
		out = append(out, sub.slice(last, target))
	} else {
		out = append(out, "")
	}
	return &array{items: out}, nil
}

// newRegExp implements RegExp(pattern, flags) at run time.
func (r *rt) newRegExp(args []any) (any, error) {
	pattern := ""
	flags := ""
	if rx, ok := arg(args, 0).(*regexpValue); ok {
		pattern, flags = rx.pat.source, rx.pat.flags
	} else if v := arg(args, 0); !isUndefined(v) {
		s, err := r.toString(v)
		if err != nil {
			return nil, err
		}
		pattern = s
	}
	if v := arg(args, 1); !isUndefined(v) {
		s, err := r.toString(v)
		if err != nil {
			return nil, err
		}
		flags = s
	}
	if pattern == "" {
		pattern = "(?:)"
	}
	// Compiling builds a program about one instruction per pattern byte,
	// which the RegExp keeps.
	if err := r.charge(allocHeader + allocRegexpUnit*len(pattern)); err != nil {
		return nil, err
	}
	p, ok, bad := compileRegexp(pattern, flags)
	if bad != "" {
		return nil, r.syntaxError(bad)
	}
	if !ok {
		return nil, errRuntimeUnsupported("regular expression outside the RE2 subset: /" + pattern + "/")
	}
	return &regexpValue{pat: p, lastIndex: float64(0)}, nil
}

// resetLastIndex writes only when needed, so parallel batch items sharing a
// global regex through match/replace never write concurrently.
func (rx *regexpValue) resetLastIndex() {
	if f, ok := rx.lastIndex.(float64); ok && f == 0 {
		return
	}
	rx.lastIndex = float64(0)
}

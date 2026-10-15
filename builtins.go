package toolscript

import (
	"encoding/json"
	"math"
	"math/rand/v2"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/dop251/goja/ftoa"
	"golang.org/x/text/cases"
	"golang.org/x/text/collate"
	"golang.org/x/text/language"
	"golang.org/x/text/unicode/norm"
)

// collator matches Goja's String.prototype.localeCompare (root collation).
// Collators keep scratch buffers, so each interpreter goroutine owns one.
func (r *rt) collator() *collate.Collator {
	if r.coll == nil {
		r.coll = collate.New(language.Und)
	}
	return r.coll
}

type (
	arrayMethod  func(r *rt, a *array, args []any) (any, error)
	stringMethod func(r *rt, s string, args []any) (any, error)
	numberMethod func(r *rt, f float64, args []any) (any, error)
)

var (
	arrayMethods  map[string]arrayMethod
	stringMethods map[string]stringMethod
	numberMethods map[string]numberMethod
	// staticFunctions implements Namespace.member(...) globals.
	staticFunctions map[string]func(r *rt, args []any) (any, error)
)

func arg(args []any, i int) any {
	if i < len(args) {
		return args[i]
	}
	return Undefined
}

// callMethod implements recv.key(...args).
func (r *rt) callMethod(recv any, key string, args []any) (any, error) {
	switch t := recv.(type) {
	case nil, undefined:
		return nil, r.typeError("Cannot read property '" + key + "' of undefined or null")
	case *object:
		if f, ok := t.get(key); ok {
			return r.callThis(f, t, args)
		}
		return r.objectMethod(t, key, args)
	case *hostObject:
		if f, ok := t.view.get(key); ok {
			return r.callThis(f, t, args)
		}
		return r.objectMethod(t, key, args)
	case *regexpValue:
		if f, ok := ownProp(t.props, key); ok {
			return r.callThis(f, t, args)
		}
		return r.regexpMethod(t, key, args)
	case *collection:
		if f, ok := ownProp(t.props, key); ok {
			return r.callThis(f, t, args)
		}
		return r.collectionMethod(t, key, args)
	case *dateValue:
		if t.props != nil {
			if f, ok := t.props.get(key); ok {
				return r.callThis(f, t, args)
			}
		}
		if m := dateMethods[key]; m != nil {
			return m(r, t, args)
		}
		return r.objectMethod(t, key, args)
	case *iterator:
		return r.iteratorMethod(t, key)
	case *intlObject:
		return r.intlMethodCall(t, key, args)
	case *function:
		if t.props != nil {
			if f, ok := t.props.get(key); ok {
				return r.callThis(f, t, args)
			}
		}
		if v, ok, err := r.functionMethod(t, key, args); ok {
			return v, err
		}
		if key == "toString" {
			return r.toString(t)
		}
		return nil, r.typeError("Object has no member '" + key + "'")
	}
	return r.callBuiltinMethod(recv, key, args)
}

// objectMethod handles the supported Object.prototype methods.
func (r *rt) objectMethod(o any, key string, args []any) (any, error) {
	switch key {
	case "toString", "toLocaleString":
		return r.toString(o)
	case "valueOf":
		return o, nil
	case "hasOwnProperty":
		k, err := r.toPropertyKey(arg(args, 0))
		if err != nil {
			return nil, err
		}
		switch t := o.(type) {
		case *object:
			_, ok := t.own(k)
			return ok || t.hasOwnHidden(k), nil
		case *hostObject:
			_, ok := t.view.own(k)
			return ok, nil
		case *dateValue:
			if t.props == nil {
				return false, nil
			}
			_, ok := t.props.own(k)
			return ok, nil
		}
	}
	return nil, r.typeError("Object has no member '" + key + "'")
}

func (r *rt) callBuiltinMethod(recv any, key string, args []any) (any, error) {
	switch t := recv.(type) {
	case *array:
		if t.props != nil {
			if f, ok := t.props.get(key); ok {
				return r.callThis(f, t, args)
			}
		}
		if m := arrayMethods[key]; m != nil {
			return m(r, t, args)
		}
		if key == "hasOwnProperty" {
			k, err := r.toPropertyKey(arg(args, 0))
			if err != nil {
				return nil, err
			}
			i := arrayIndex(k)
			return k == "length" || (i >= 0 && i < len(t.items)), nil
		}
		if key == "valueOf" {
			return t, nil
		}
	case string:
		if m := stringMethods[key]; m != nil {
			return m(r, t, args)
		}
	case float64:
		if m := numberMethods[key]; m != nil {
			return m(r, t, args)
		}
	case bool:
		switch key {
		case "toString":
			return r.toString(t)
		case "valueOf":
			return t, nil
		}
	case *object, *hostObject, *dateValue:
		return r.objectMethod(t, key, args)
	}
	return nil, r.typeError("Object has no member '" + key + "'")
}

// callback invokes a user callback with (value, index, array).
func (r *rt) callback(f any, args ...any) (any, error) {
	return r.call(f, args)
}

func (r *rt) requireCallable(f any) error {
	if _, ok := f.(*function); !ok {
		s, _ := r.toString(f)
		if isObjectValue(f) {
			return r.typeError("Value is not callable: " + s)
		}
		return r.typeError("Value is not an object: " + s)
	}
	return nil
}

// relIndex resolves a relative start/end argument against length.
func (r *rt) relIndex(v any, length, def int) (int, error) {
	if _, ok := v.(undefined); ok {
		return def, nil
	}
	f, err := r.toNumber(v)
	if err != nil {
		return 0, err
	}
	f = toIntegerOrInfinity(f)
	if f < 0 {
		f = math.Max(float64(length)+f, 0)
	} else {
		f = math.Min(f, float64(length))
	}
	return int(f), nil
}

func (r *rt) newArray(items []any) (any, error) {
	if len(items) > r.x.maxItems {
		return nil, r.rangeError("Invalid array length")
	}
	return r.arrayValue(items)
}

func init() {
	arrayMethods = map[string]arrayMethod{
		"at": func(r *rt, a *array, args []any) (any, error) {
			i, err := r.toNumber(arg(args, 0))
			if err != nil {
				return nil, err
			}
			n := int(toIntegerOrInfinity(i))
			if n < 0 {
				n += len(a.items)
			}
			if n < 0 || n >= len(a.items) {
				return Undefined, nil
			}
			return unhole(a.items[n]), nil
		},
		"concat": func(r *rt, a *array, args []any) (any, error) {
			n := len(a.items)
			for _, v := range args {
				if b, ok := v.(*array); ok {
					n += len(b.items)
				} else {
					n++
				}
			}
			if n > r.x.maxItems {
				return nil, r.rangeError("Invalid array length")
			}
			if err := r.chargeArray(n); err != nil {
				return nil, err
			}
			out := make([]any, 0, n)
			out = append(out, a.items...)
			for _, v := range args {
				if b, ok := v.(*array); ok {
					out = append(out, b.items...)
				} else {
					out = append(out, v)
				}
			}
			return &array{items: out}, nil
		},
		"every": func(r *rt, a *array, args []any) (any, error) {
			return iterateArray(r, a, args, func(v, res any, i int) (any, bool) {
				if !toBoolean(res) {
					return false, true
				}
				return nil, false
			}, true, true)
		},
		"some": func(r *rt, a *array, args []any) (any, error) {
			return iterateArray(r, a, args, func(v, res any, i int) (any, bool) {
				if toBoolean(res) {
					return true, true
				}
				return nil, false
			}, false, true)
		},
		"find": func(r *rt, a *array, args []any) (any, error) {
			return iterateArray(r, a, args, func(v, res any, i int) (any, bool) {
				if toBoolean(res) {
					return v, true
				}
				return nil, false
			}, Undefined)
		},
		"findIndex": func(r *rt, a *array, args []any) (any, error) {
			return iterateArray(r, a, args, func(v, res any, i int) (any, bool) {
				if toBoolean(res) {
					return float64(i), true
				}
				return nil, false
			}, float64(-1))
		},
		"findLast": func(r *rt, a *array, args []any) (any, error) {
			return iterateArrayReverse(r, a, args, func(v, res any, i int) (any, bool) {
				if toBoolean(res) {
					return v, true
				}
				return nil, false
			}, Undefined)
		},
		"findLastIndex": func(r *rt, a *array, args []any) (any, error) {
			return iterateArrayReverse(r, a, args, func(v, res any, i int) (any, bool) {
				if toBoolean(res) {
					return float64(i), true
				}
				return nil, false
			}, float64(-1))
		},
		"forEach": func(r *rt, a *array, args []any) (any, error) {
			return iterateArray(r, a, args, func(v, res any, i int) (any, bool) { return nil, false }, Undefined, true)
		},
		"map": func(r *rt, a *array, args []any) (any, error) {
			f := arg(args, 0)
			if err := r.requireCallable(f); err != nil {
				return nil, err
			}
			n := len(a.items)
			if err := r.chargeArray(n); err != nil {
				return nil, err
			}
			out := make([]any, n)
			for i := range out {
				out[i] = hole
			}
			for i := 0; i < n && i < len(a.items); i++ {
				if isHole(a.items[i]) {
					continue
				}
				v, err := r.callback(f, a.items[i], float64(i), a)
				if err != nil {
					return nil, err
				}
				out[i] = v
			}
			return &array{items: out}, nil
		},
		"filter": func(r *rt, a *array, args []any) (any, error) {
			f := arg(args, 0)
			if err := r.requireCallable(f); err != nil {
				return nil, err
			}
			if err := r.chargeArray(0); err != nil {
				return nil, err
			}
			out := []any{}
			n := len(a.items)
			for i := 0; i < n && i < len(a.items); i++ {
				v := a.items[i]
				if isHole(v) {
					continue
				}
				keep, err := r.callback(f, v, float64(i), a)
				if err != nil {
					return nil, err
				}
				if toBoolean(keep) {
					if err := r.charge(allocSlot); err != nil {
						return nil, err
					}
					out = append(out, v)
				}
			}
			return &array{items: out}, nil
		},
		"flat": func(r *rt, a *array, args []any) (any, error) {
			depth := 1.0
			if _, ok := arg(args, 0).(undefined); !ok {
				d, err := r.toNumber(args[0])
				if err != nil {
					return nil, err
				}
				depth = toIntegerOrInfinity(d)
			}
			if err := r.chargeArray(0); err != nil {
				return nil, err
			}
			out := []any{}
			if err := r.flatten(&out, a.items, depth, 0); err != nil {
				return nil, err
			}
			return &array{items: out}, nil
		},
		"flatMap": func(r *rt, a *array, args []any) (any, error) {
			f := arg(args, 0)
			if err := r.requireCallable(f); err != nil {
				return nil, err
			}
			if err := r.chargeArray(0); err != nil {
				return nil, err
			}
			out := []any{}
			n := len(a.items)
			for i := 0; i < n && i < len(a.items); i++ {
				if isHole(a.items[i]) {
					continue
				}
				v, err := r.callback(f, a.items[i], float64(i), a)
				if err != nil {
					return nil, err
				}
				if b, ok := v.(*array); ok {
					if len(out)+len(b.items) > r.x.maxItems {
						return nil, r.rangeError("Invalid array length")
					}
					if err := r.charge(len(b.items) * allocSlot); err != nil {
						return nil, err
					}
					out = append(out, b.items...)
				} else {
					if err := r.charge(allocSlot); err != nil {
						return nil, err
					}
					out = append(out, v)
				}
				if len(out) > r.x.maxItems {
					return nil, r.rangeError("Invalid array length")
				}
			}
			return &array{items: out}, nil
		},
		"includes": func(r *rt, a *array, args []any) (any, error) {
			from, err := r.relIndex(arg(args, 1), len(a.items), 0)
			if err != nil {
				return nil, err
			}
			for i := from; i < len(a.items); i++ {
				if sameValueZero(unhole(a.items[i]), arg(args, 0)) {
					return true, nil
				}
			}
			return false, nil
		},
		"indexOf": func(r *rt, a *array, args []any) (any, error) {
			from, err := r.relIndex(arg(args, 1), len(a.items), 0)
			if err != nil {
				return nil, err
			}
			for i := from; i < len(a.items); i++ {
				if !isHole(a.items[i]) && strictEquals(a.items[i], arg(args, 0)) {
					return float64(i), nil
				}
			}
			return float64(-1), nil
		},
		"lastIndexOf": func(r *rt, a *array, args []any) (any, error) {
			from := len(a.items) - 1
			if len(args) > 1 {
				f, err := r.toNumber(args[1])
				if err != nil {
					return nil, err
				}
				f = toIntegerOrInfinity(f)
				if f < 0 {
					f += float64(len(a.items))
				}
				from = int(math.Min(f, float64(len(a.items)-1)))
			}
			for i := from; i >= 0; i-- {
				if !isHole(a.items[i]) && strictEquals(a.items[i], arg(args, 0)) {
					return float64(i), nil
				}
			}
			return float64(-1), nil
		},
		"join": func(r *rt, a *array, args []any) (any, error) {
			sep := ","
			if v := arg(args, 0); !isUndefined(v) {
				s, err := r.toString(v)
				if err != nil {
					return nil, err
				}
				sep = s
			}
			return r.join(a, sep)
		},
		"toString": func(r *rt, a *array, args []any) (any, error) { return r.join(a, ",") },
		"entries":  func(r *rt, a *array, args []any) (any, error) { return r.iteratorValue(arrayIterator(a, "entries")) },
		"keys":     func(r *rt, a *array, args []any) (any, error) { return r.iteratorValue(arrayIterator(a, "keys")) },
		"values":   func(r *rt, a *array, args []any) (any, error) { return r.iteratorValue(arrayIterator(a, "values")) },
		"toLocaleString": func(r *rt, a *array, args []any) (any, error) {
			// A nested array converts through this method again, and a cyclic
			// array would recurse forever, so it shares join's cycle guard and
			// nesting bound keyed on the original array (the parts array built
			// below is fresh each level, so join's own guard cannot see the
			// cycle). See maxConversionNesting.
			if cycle, err := r.enterJoin(a); err != nil {
				return nil, err
			} else if cycle {
				return "", nil
			}
			defer r.leaveJoin(a)
			parts := make([]any, len(a.items))
			for i, v := range a.items {
				if isNullish(v) {
					parts[i] = ""
					continue
				}
				s, err := r.callMethod(v, "toLocaleString", nil)
				if err != nil {
					return nil, err
				}
				parts[i] = s
			}
			return r.join(&array{items: parts}, ",")
		},
		"pop": func(r *rt, a *array, args []any) (any, error) {
			if len(a.items) == 0 {
				return Undefined, nil
			}
			v := a.items[len(a.items)-1]
			a.items = a.items[:len(a.items)-1]
			return unhole(v), nil
		},
		"push": func(r *rt, a *array, args []any) (any, error) {
			if len(a.items)+len(args) > r.x.maxItems {
				return nil, r.rangeError("Invalid array length")
			}
			if err := r.growArray(a, len(a.items)+len(args)); err != nil {
				return nil, err
			}
			a.items = append(a.items, args...)
			return float64(len(a.items)), nil
		},
		"shift": func(r *rt, a *array, args []any) (any, error) {
			if len(a.items) == 0 {
				return Undefined, nil
			}
			v := a.items[0]
			a.items = append(a.items[:0:0], a.items[1:]...)
			return unhole(v), nil
		},
		"unshift": func(r *rt, a *array, args []any) (any, error) {
			if len(a.items)+len(args) > r.x.maxItems {
				return nil, r.rangeError("Invalid array length")
			}
			if err := r.growArray(a, len(a.items)+len(args)); err != nil {
				return nil, err
			}
			a.items = append(append([]any(nil), args...), a.items...)
			return float64(len(a.items)), nil
		},
		"reduce": func(r *rt, a *array, args []any) (any, error) {
			return r.reduce(a, args, false)
		},
		"reduceRight": func(r *rt, a *array, args []any) (any, error) {
			return r.reduce(a, args, true)
		},
		"reverse": func(r *rt, a *array, args []any) (any, error) {
			for i, j := 0, len(a.items)-1; i < j; i, j = i+1, j-1 {
				a.items[i], a.items[j] = a.items[j], a.items[i]
			}
			return a, nil
		},
		"toReversed": func(r *rt, a *array, args []any) (any, error) {
			if err := r.chargeArray(len(a.items)); err != nil {
				return nil, err
			}
			out := make([]any, len(a.items))
			for i, v := range a.items {
				out[len(out)-1-i] = unhole(v)
			}
			return &array{items: out}, nil
		},
		"slice": func(r *rt, a *array, args []any) (any, error) {
			n := len(a.items)
			start, err := r.relIndex(arg(args, 0), n, 0)
			if err != nil {
				return nil, err
			}
			end, err := r.relIndex(arg(args, 1), n, n)
			if err != nil {
				return nil, err
			}
			if start >= end {
				return r.arrayValue([]any{})
			}
			if err := r.chargeArray(end - start); err != nil {
				return nil, err
			}
			return &array{items: append([]any(nil), a.items[start:end]...)}, nil
		},
		"sort": func(r *rt, a *array, args []any) (any, error) {
			if err := r.sortItems(a.items, arg(args, 0)); err != nil {
				return nil, err
			}
			return a, nil
		},
		"toSorted": func(r *rt, a *array, args []any) (any, error) {
			if err := r.chargeArray(len(a.items)); err != nil {
				return nil, err
			}
			out := append([]any(nil), denseItems(a.items)...)
			if err := r.sortItems(out, arg(args, 0)); err != nil {
				return nil, err
			}
			return &array{items: nonNil(out)}, nil
		},
		"splice": func(r *rt, a *array, args []any) (any, error) {
			removed, err := r.splice(a, args, true)
			if err != nil {
				return nil, err
			}
			return removed, nil
		},
		"toSpliced": func(r *rt, a *array, args []any) (any, error) {
			if err := r.chargeArray(len(a.items)); err != nil {
				return nil, err
			}
			c := &array{items: append([]any(nil), denseItems(a.items)...)}
			if _, err := r.splice(c, args, false); err != nil {
				return nil, err
			}
			c.items = nonNil(c.items)
			return c, nil
		},
		"fill": func(r *rt, a *array, args []any) (any, error) {
			n := len(a.items)
			start, err := r.relIndex(arg(args, 1), n, 0)
			if err != nil {
				return nil, err
			}
			end, err := r.relIndex(arg(args, 2), n, n)
			if err != nil {
				return nil, err
			}
			for i := start; i < end; i++ {
				a.items[i] = arg(args, 0)
			}
			return a, nil
		},
		"with": func(r *rt, a *array, args []any) (any, error) {
			f, err := r.toNumber(arg(args, 0))
			if err != nil {
				return nil, err
			}
			i := int(toIntegerOrInfinity(f))
			if i < 0 {
				i += len(a.items)
			}
			if i < 0 || i >= len(a.items) {
				return nil, r.rangeError("Invalid index " + numberToString(toIntegerOrInfinity(f)))
			}
			if err := r.chargeArray(len(a.items)); err != nil {
				return nil, err
			}
			out := append([]any(nil), denseItems(a.items)...)
			out[i] = arg(args, 1)
			return &array{items: out}, nil
		},
	}

	stringMethods = map[string]stringMethod{
		"at": func(r *rt, s string, args []any) (any, error) {
			f, err := r.toNumber(arg(args, 0))
			if err != nil {
				return nil, err
			}
			n, l := int(toIntegerOrInfinity(f)), utf16Len(s)
			if n < 0 {
				n += l
			}
			if n < 0 || n >= l {
				return Undefined, nil
			}
			c, _ := utf16At(s, n)
			return c, nil
		},
		"charAt": func(r *rt, s string, args []any) (any, error) {
			f, err := r.toNumber(arg(args, 0))
			if err != nil {
				return nil, err
			}
			n := toIntegerOrInfinity(f)
			if n < 0 || n >= float64(utf16Len(s)) {
				return "", nil
			}
			c, _ := utf16At(s, int(n))
			return c, nil
		},
		"charCodeAt": func(r *rt, s string, args []any) (any, error) {
			f, err := r.toNumber(arg(args, 0))
			if err != nil {
				return nil, err
			}
			n := toIntegerOrInfinity(f)
			u := toUnits(s)
			if n < 0 || n >= float64(len(u)) {
				return math.NaN(), nil
			}
			return float64(u[int(n)]), nil
		},
		"codePointAt": func(r *rt, s string, args []any) (any, error) {
			f, err := r.toNumber(arg(args, 0))
			if err != nil {
				return nil, err
			}
			n := toIntegerOrInfinity(f)
			u := toUnits(s)
			if n < 0 || n >= float64(len(u)) {
				return Undefined, nil
			}
			i := int(n)
			if utf16.IsSurrogate(rune(u[i])) && i+1 < len(u) {
				if cp := utf16.DecodeRune(rune(u[i]), rune(u[i+1])); cp != utf8.RuneError {
					return float64(cp), nil
				}
			}
			return float64(u[i]), nil
		},
		"concat": func(r *rt, s string, args []any) (any, error) {
			var b strings.Builder
			b.WriteString(s)
			for _, a := range args {
				t, err := r.toString(a)
				if err != nil {
					return nil, err
				}
				b.WriteString(t)
			}
			if _, err := r.checkString(b.String()); err != nil {
				return nil, err
			}
			return r.stringValue(b.String())
		},
		"endsWith": func(r *rt, s string, args []any) (any, error) {
			if _, ok := arg(args, 0).(*regexpValue); ok {
				return nil, r.typeError("First argument to String.prototype.endsWith must not be a regular expression")
			}
			sub, err := r.toString(arg(args, 0))
			if err != nil {
				return nil, err
			}
			end := utf16Len(s)
			if v := arg(args, 1); !isUndefined(v) {
				if end, err = r.clampPos(v, end); err != nil {
					return nil, err
				}
			}
			start := end - utf16Len(sub)
			if start < 0 {
				return false, nil
			}
			return utf16Slice(s, start, end) == sub, nil
		},
		"startsWith": func(r *rt, s string, args []any) (any, error) {
			if _, ok := arg(args, 0).(*regexpValue); ok {
				return nil, r.typeError("First argument to String.prototype.startsWith must not be a regular expression")
			}
			sub, err := r.toString(arg(args, 0))
			if err != nil {
				return nil, err
			}
			start, err := r.clampPos(arg(args, 1), utf16Len(s))
			if err != nil {
				return nil, err
			}
			end := start + utf16Len(sub)
			if end > utf16Len(s) {
				return false, nil
			}
			return utf16Slice(s, start, end) == sub, nil
		},
		"includes": func(r *rt, s string, args []any) (any, error) {
			if _, ok := arg(args, 0).(*regexpValue); ok {
				return nil, r.typeError("First argument to String.prototype.includes must not be a regular expression")
			}
			sub, err := r.toString(arg(args, 0))
			if err != nil {
				return nil, err
			}
			start, err := r.clampPos(arg(args, 1), utf16Len(s))
			if err != nil {
				return nil, err
			}
			return utf16Index(s, sub, start) >= 0, nil
		},
		"indexOf": func(r *rt, s string, args []any) (any, error) {
			sub, err := r.toString(arg(args, 0))
			if err != nil {
				return nil, err
			}
			start, err := r.clampPos(arg(args, 1), utf16Len(s))
			if err != nil {
				return nil, err
			}
			return float64(utf16Index(s, sub, start)), nil
		},
		"lastIndexOf": func(r *rt, s string, args []any) (any, error) {
			sub, err := r.toString(arg(args, 0))
			if err != nil {
				return nil, err
			}
			pos := math.Inf(1)
			if v := arg(args, 1); !isUndefined(v) {
				f, err := r.toNumber(v)
				if err != nil {
					return nil, err
				}
				if !math.IsNaN(f) {
					pos = toIntegerOrInfinity(f)
				}
			}
			l := utf16Len(s)
			start := int(math.Min(math.Max(pos, 0), float64(l)))
			return float64(utf16LastIndex(s, sub, start)), nil
		},
		"padEnd": func(r *rt, s string, args []any) (any, error) {
			return r.pad(s, args, false)
		},
		"padStart": func(r *rt, s string, args []any) (any, error) {
			return r.pad(s, args, true)
		},
		"repeat": func(r *rt, s string, args []any) (any, error) {
			f, err := r.toNumber(arg(args, 0))
			if err != nil {
				return nil, err
			}
			n := toIntegerOrInfinity(f)
			if n < 0 || math.IsInf(n, 0) {
				return nil, r.rangeError("Invalid count value")
			}
			if float64(len(s))*n > float64(r.x.maxString) {
				return nil, r.rangeError("Invalid string length")
			}
			if err := r.charge(len(s)*int(n) + allocSlot); err != nil {
				return nil, err
			}
			return strings.Repeat(s, int(n)), nil
		},
		"replace": func(r *rt, s string, args []any) (any, error) {
			if rx, ok := arg(args, 0).(*regexpValue); ok {
				return r.stringReplaceRegexp(s, rx, arg(args, 1), false)
			}
			return r.replace(s, args, false)
		},
		"replaceAll": func(r *rt, s string, args []any) (any, error) {
			if rx, ok := arg(args, 0).(*regexpValue); ok {
				return r.stringReplaceRegexp(s, rx, arg(args, 1), true)
			}
			return r.replace(s, args, true)
		},
		"match": func(r *rt, s string, args []any) (any, error) {
			rx, err := r.toRegexp(arg(args, 0))
			if err != nil {
				return nil, err
			}
			return r.stringMatch(s, rx)
		},
		"search": func(r *rt, s string, args []any) (any, error) {
			rx, err := r.toRegexp(arg(args, 0))
			if err != nil {
				return nil, err
			}
			return r.stringSearch(s, rx)
		},
		"slice": func(r *rt, s string, args []any) (any, error) {
			l := utf16Len(s)
			start, err := r.relIndex(arg(args, 0), l, 0)
			if err != nil {
				return nil, err
			}
			end, err := r.relIndex(arg(args, 1), l, l)
			if err != nil {
				return nil, err
			}
			return r.slice16(s, start, end)
		},
		"substring": func(r *rt, s string, args []any) (any, error) {
			l := utf16Len(s)
			start, err := r.clampPos(arg(args, 0), l)
			if err != nil {
				return nil, err
			}
			end := l
			if v := arg(args, 1); !isUndefined(v) {
				if end, err = r.clampPos(v, l); err != nil {
					return nil, err
				}
			}
			if start > end {
				start, end = end, start
			}
			return r.slice16(s, start, end)
		},
		"substr": func(r *rt, s string, args []any) (any, error) {
			l := utf16Len(s)
			start, err := r.relIndex(arg(args, 0), l, 0)
			if err != nil {
				return nil, err
			}
			length := float64(l - start)
			if v := arg(args, 1); !isUndefined(v) {
				f, err := r.toNumber(v)
				if err != nil {
					return nil, err
				}
				length = math.Min(math.Max(toIntegerOrInfinity(f), 0), float64(l-start))
			}
			if length <= 0 {
				return "", nil
			}
			return r.slice16(s, start, start+int(length))
		},
		"split": func(r *rt, s string, args []any) (any, error) {
			if rx, ok := arg(args, 0).(*regexpValue); ok {
				v, err := r.stringSplitRegexp(s, rx, arg(args, 1))
				if err != nil {
					return nil, err
				}
				return v, r.chargeFresh(v)
			}
			limit := -1
			if v := arg(args, 1); !isUndefined(v) {
				f, err := r.toNumber(v)
				if err != nil {
					return nil, err
				}
				limit = int(toUint32(f))
			}
			if limit == 0 {
				return r.arrayValue([]any{})
			}
			if isUndefined(arg(args, 0)) {
				return r.arrayValue([]any{s})
			}
			sep, err := r.toString(arg(args, 0))
			if err != nil {
				return nil, err
			}
			n := limit
			if limit > 0 {
				n = limit + 1
			}
			parts := strings.SplitN(s, sep, n)
			if limit > 0 && len(parts) > limit {
				parts = parts[:limit]
			}
			if len(parts) > r.x.maxItems {
				return nil, r.rangeError("Invalid array length")
			}
			return r.stringsValue(parts)
		},
		"localeCompare": func(r *rt, s string, args []any) (any, error) {
			that, err := r.toString(arg(args, 0))
			if err != nil {
				return nil, err
			}
			return float64(r.collator().CompareString(norm.NFD.String(s), norm.NFD.String(that))), nil
		},
		"toLocaleString":    func(r *rt, s string, _ []any) (any, error) { return s, nil },
		"toLowerCase":       func(r *rt, s string, _ []any) (any, error) { return r.stringValue(toLowerJS(s)) },
		"toLocaleLowerCase": func(r *rt, s string, _ []any) (any, error) { return r.stringValue(toLowerJS(s)) },
		"toUpperCase":       func(r *rt, s string, _ []any) (any, error) { return r.stringValue(toUpperJS(s)) },
		"toLocaleUpperCase": func(r *rt, s string, _ []any) (any, error) { return r.stringValue(toUpperJS(s)) },
		"toString":          func(r *rt, s string, _ []any) (any, error) { return s, nil },
		"valueOf":           func(r *rt, s string, _ []any) (any, error) { return s, nil },
		"trim":              func(r *rt, s string, _ []any) (any, error) { return trimJS(s), nil },
		"trimStart":         trimStart,
		"trimLeft":          trimStart,
		"trimEnd":           trimEnd,
		"trimRight":         trimEnd,
	}

	numberMethods = map[string]numberMethod{
		"toFixed": func(r *rt, f float64, args []any) (any, error) {
			d, err := r.toNumber(arg(args, 0))
			if err != nil {
				return nil, err
			}
			d = toIntegerOrInfinity(d)
			if d < 0 || d > 100 {
				return nil, r.rangeError("toFixed() precision must be between 0 and 100")
			}
			return r.stringValue(toFixed(f, int(d)))
		},
		"toString": func(r *rt, f float64, args []any) (any, error) {
			radix := 10.0
			if v := arg(args, 0); !isUndefined(v) {
				var err error
				if radix, err = r.toNumber(v); err != nil {
					return nil, err
				}
				radix = toIntegerOrInfinity(radix)
			}
			if radix < 2 || radix > 36 {
				return nil, r.rangeError("toString() radix argument must be between 2 and 36")
			}
			s, ok := numberToRadix(f, int(radix))
			if !ok {
				return nil, errRuntimeUnsupported("fractional Number.prototype.toString radix")
			}
			return r.stringValue(s)
		},
		"valueOf": func(r *rt, f float64, _ []any) (any, error) { return f, nil },
		// Goja formats numbers for every locale like toString; with locales
		// or options it is Intl.NumberFormat.
		"toLocaleString": func(r *rt, f float64, args []any) (any, error) {
			if isUndefined(arg(args, 0)) && isUndefined(arg(args, 1)) {
				return r.stringValue(numberToString(f))
			}
			v, err := r.numberToLocaleString(f, arg(args, 0), arg(args, 1))
			if err != nil {
				return nil, err
			}
			return v, r.chargeFresh(v)
		},
		"toPrecision": func(r *rt, f float64, args []any) (any, error) {
			if isUndefined(arg(args, 0)) {
				return numberToString(f), nil
			}
			p, err := r.toNumber(args[0])
			if err != nil {
				return nil, err
			}
			p = toIntegerOrInfinity(p)
			if math.IsNaN(f) || math.IsInf(f, 0) {
				return numberToString(f), nil
			}
			if p < 1 || p > 100 {
				return nil, r.rangeError("toPrecision() precision must be between 1 and 100")
			}
			var buf [128]byte
			return r.stringValue(string(ftoa.FToStr(f, ftoa.ModePrecision, int(p), buf[:0])))
		},
	}

	staticFunctions = map[string]func(r *rt, args []any) (any, error){
		"JSON.stringify": func(r *rt, args []any) (any, error) {
			return r.stringify(arg(args, 0), arg(args, 1), arg(args, 2))
		},
		"JSON.parse": func(r *rt, args []any) (any, error) {
			s, err := r.toString(arg(args, 0))
			if err != nil {
				return nil, err
			}
			return r.parseJSON(s)
		},
		"Object.keys": func(r *rt, args []any) (any, error) {
			o, err := r.toObjectArg(arg(args, 0))
			if err != nil {
				return nil, err
			}
			return r.stringsValue(ownEnumerableKeys(o))
		},
		"Object.values": func(r *rt, args []any) (any, error) {
			o, err := r.toObjectArg(arg(args, 0))
			if err != nil {
				return nil, err
			}
			keys := ownEnumerableKeys(o)
			if err := r.chargeArray(len(keys)); err != nil {
				return nil, err
			}
			out := make([]any, len(keys))
			for i, k := range keys {
				if out[i], err = r.getProp(o, k); err != nil {
					return nil, err
				}
			}
			return &array{items: out}, nil
		},
		"Object.entries": func(r *rt, args []any) (any, error) {
			o, err := r.toObjectArg(arg(args, 0))
			if err != nil {
				return nil, err
			}
			keys := ownEnumerableKeys(o)
			n := allocHeader
			for _, k := range keys {
				// Each entry is a [key, value] array holding the key string.
				n += allocSlot + allocHeader + 2*allocSlot + len(k) + allocSlot
			}
			if err := r.charge(n); err != nil {
				return nil, err
			}
			out := make([]any, len(keys))
			for i, k := range keys {
				v, err := r.getProp(o, k)
				if err != nil {
					return nil, err
				}
				out[i] = &array{items: []any{k, v}}
			}
			return &array{items: out}, nil
		},
		"Object.assign": func(r *rt, args []any) (any, error) {
			target := arg(args, 0)
			if isNullish(target) {
				return nil, r.typeError("Cannot convert undefined or null to object")
			}
			for _, src := range args[1:] {
				for _, k := range ownEnumerableKeys(src) {
					v, err := r.getProp(src, k)
					if err != nil {
						return nil, err
					}
					if err := r.setProp(target, k, v); err != nil {
						return nil, err
					}
				}
			}
			return target, nil
		},
		"Object.fromEntries": func(r *rt, args []any) (any, error) {
			items, err := r.iterableItems(arg(args, 0))
			if err != nil {
				return nil, err
			}
			o, err := r.newObj(0)
			if err != nil {
				return nil, err
			}
			for _, e := range items {
				if !isObjectValue(e) {
					s, _ := r.toString(e)
					return nil, r.typeError("Value is not an object: " + s)
				}
				k, err := r.getProp(e, "0")
				if err != nil {
					return nil, err
				}
				v, err := r.getProp(e, "1")
				if err != nil {
					return nil, err
				}
				key, err := r.toPropertyKey(k)
				if err != nil {
					return nil, err
				}
				if err := r.put(o, key, v); err != nil {
					return nil, err
				}
			}
			return o, nil
		},
		"Object.hasOwn": func(r *rt, args []any) (any, error) {
			o, err := r.toObjectArg(arg(args, 0))
			if err != nil {
				return nil, err
			}
			return r.callBuiltinMethodOwn(o, arg(args, 1))
		},
		"Array.isArray": func(r *rt, args []any) (any, error) {
			_, ok := arg(args, 0).(*array)
			return ok, nil
		},
		"Array.of": func(r *rt, args []any) (any, error) {
			return r.newArray(append([]any{}, args...))
		},
		"Array.from": func(r *rt, args []any) (any, error) {
			src := arg(args, 0)
			mapFn := arg(args, 1)
			if !isUndefined(mapFn) {
				if err := r.requireCallable(mapFn); err != nil {
					return nil, err
				}
			}
			var items []any
			switch t := src.(type) {
			case *collection, *iterator:
				// Iterables are consumed lazily: mapFn runs as each item
				// is produced, and a partially consumed iterator yields
				// only what is left.
				it, _ := defaultIterator(t)
				if err := r.chargeArray(0); err != nil {
					return nil, err
				}
				items = []any{}
				for {
					v, ok := it.next()
					if !ok {
						break
					}
					if len(items) >= r.x.maxItems {
						return nil, r.rangeError("Invalid array length")
					}
					if err := r.charge(allocSlot + it.cost); err != nil {
						return nil, err
					}
					if !isUndefined(mapFn) {
						m, err := r.callback(mapFn, v, float64(len(items)))
						if err != nil {
							return nil, err
						}
						v = m
					}
					items = append(items, v)
				}
				return &array{items: items}, nil
			case *array:
				if err := r.chargeArray(len(t.items)); err != nil {
					return nil, err
				}
				items = append([]any(nil), t.items...)
			case string:
				var err error
				if items, err = r.iterableItems(t); err != nil {
					return nil, err
				}
				if err := r.chargeArray(len(items)); err != nil {
					return nil, err
				}
			case *object:
				lv, _ := t.own("length")
				n, err := r.toNumber(lv)
				if err != nil || math.IsNaN(n) || n <= 0 {
					items = []any{}
					break
				}
				if n > float64(r.x.maxItems) {
					return nil, r.rangeError("Invalid array length")
				}
				if err := r.chargeArray(int(n)); err != nil {
					return nil, err
				}
				items = make([]any, int(n))
				for i := range items {
					if items[i], err = r.getProp(t, strconv.Itoa(i)); err != nil {
						return nil, err
					}
				}
			case nil, undefined:
				return nil, r.typeError("Cannot convert undefined or null to object")
			default:
				if err := r.chargeArray(0); err != nil {
					return nil, err
				}
				items = []any{}
			}
			if f := mapFn; !isUndefined(f) {
				for i, v := range items {
					m, err := r.callback(f, v, float64(i))
					if err != nil {
						return nil, err
					}
					items[i] = m
				}
			}
			return &array{items: nonNil(items)}, nil
		},
		"Number.isInteger": func(r *rt, args []any) (any, error) {
			f, ok := arg(args, 0).(float64)
			return ok && !math.IsInf(f, 0) && f == math.Trunc(f), nil
		},
		"Number.isSafeInteger": func(r *rt, args []any) (any, error) {
			f, ok := arg(args, 0).(float64)
			return ok && f == math.Trunc(f) && math.Abs(f) <= 9007199254740991, nil
		},
		"Number.isFinite": func(r *rt, args []any) (any, error) {
			f, ok := arg(args, 0).(float64)
			return ok && !math.IsInf(f, 0) && !math.IsNaN(f), nil
		},
		"Number.isNaN": func(r *rt, args []any) (any, error) {
			f, ok := arg(args, 0).(float64)
			return ok && math.IsNaN(f), nil
		},
		"Number.parseFloat": func(r *rt, args []any) (any, error) { return r.callGlobal("parseFloat", args) },
		"Number.parseInt":   func(r *rt, args []any) (any, error) { return r.callGlobal("parseInt", args) },
		"String.fromCharCode": func(r *rt, args []any) (any, error) {
			u := make([]uint16, len(args))
			for i, a := range args {
				f, err := r.toNumber(a)
				if err != nil {
					return nil, err
				}
				u[i] = uint16(toUint32(f))
			}
			return r.stringValue(fromUnits(u))
		},
		"Math.max": func(r *rt, args []any) (any, error) { return r.minMax(args, true) },
		"Math.min": func(r *rt, args []any) (any, error) { return r.minMax(args, false) },
		"Math.round": math1(func(f float64) float64 {
			// Ported from Goja: exact for large integers, keeps -0.
			if math.IsNaN(f) || (f == 0 && math.Signbit(f)) {
				return f
			}
			t := math.Trunc(f)
			if f >= 0 {
				if f-t >= 0.5 {
					return t + 1
				}
			} else if t-f > 0.5 {
				return t - 1
			}
			return t
		}),
		"Math.random": func(r *rt, args []any) (any, error) {
			if r.x.opts.Random != nil {
				return r.x.opts.Random(), nil
			}
			return rand.Float64(), nil
		},
		"Math.pow": func(r *rt, args []any) (any, error) {
			a, err := r.toNumber(arg(args, 0))
			if err != nil {
				return nil, err
			}
			b, err := r.toNumber(arg(args, 1))
			if err != nil {
				return nil, err
			}
			return jsPow(a, b), nil
		},
		"Math.atan2": func(r *rt, args []any) (any, error) {
			a, err := r.toNumber(arg(args, 0))
			if err != nil {
				return nil, err
			}
			b, err := r.toNumber(arg(args, 1))
			if err != nil {
				return nil, err
			}
			return math.Atan2(a, b), nil
		},
		"Math.hypot": func(r *rt, args []any) (any, error) {
			sum := 0.0
			for _, a := range args {
				f, err := r.toNumber(a)
				if err != nil {
					return nil, err
				}
				if math.IsInf(f, 0) {
					return math.Inf(1), nil
				}
				sum += f * f
			}
			return math.Sqrt(sum), nil
		},
	}
	registerDateStatics()
	for name, f := range map[string]func(float64) float64{
		"abs": math.Abs, "ceil": math.Ceil, "floor": math.Floor, "trunc": math.Trunc,
		"sqrt": math.Sqrt, "cbrt": math.Cbrt, "exp": math.Exp, "expm1": math.Expm1,
		"log": math.Log, "log2": math.Log2, "log10": math.Log10, "log1p": math.Log1p,
		"sin": math.Sin, "cos": math.Cos, "tan": math.Tan, "asin": math.Asin, "acos": math.Acos,
		"atan": math.Atan, "sinh": math.Sinh, "cosh": math.Cosh, "tanh": math.Tanh,
		"asinh": math.Asinh, "acosh": math.Acosh, "atanh": math.Atanh,
		"sign": func(f float64) float64 {
			switch {
			case f > 0:
				return 1
			case f < 0:
				return -1
			}
			return f
		},
		"fround": func(f float64) float64 { return float64(float32(f)) },
	} {
		staticFunctions["Math."+name] = math1(f)
	}
}

func math1(f func(float64) float64) func(r *rt, args []any) (any, error) {
	return func(r *rt, args []any) (any, error) {
		x, err := r.toNumber(arg(args, 0))
		if err != nil {
			return nil, err
		}
		return f(x), nil
	}
}

func isUndefined(v any) bool {
	_, ok := v.(undefined)
	return ok
}

func trimStart(r *rt, s string, _ []any) (any, error) {
	return strings.TrimLeft(s, jsWhitespace), nil
}

func trimEnd(r *rt, s string, _ []any) (any, error) {
	return strings.TrimRight(s, jsWhitespace), nil
}

func (r *rt) checkString(s string) (any, error) {
	if len(s) > r.x.maxString {
		return nil, r.rangeError("Invalid string length")
	}
	return s, nil
}

// clampPos converts a position argument to [0, length].
func (r *rt) clampPos(v any, length int) (int, error) {
	if isUndefined(v) {
		return 0, nil
	}
	f, err := r.toNumber(v)
	if err != nil {
		return 0, err
	}
	f = toIntegerOrInfinity(f)
	return int(math.Min(math.Max(f, 0), float64(length))), nil
}

func (r *rt) toObjectArg(v any) (any, error) {
	if isNullish(v) {
		return nil, r.typeError("Cannot convert undefined or null to object")
	}
	return v, nil
}

func (r *rt) callBuiltinMethodOwn(o any, key any) (any, error) {
	k, err := r.toPropertyKey(key)
	if err != nil {
		return nil, err
	}
	switch t := o.(type) {
	case *object:
		_, ok := t.own(k)
		return ok || t.hasOwnHidden(k), nil
	case *hostObject:
		_, ok := t.view.own(k)
		return ok, nil
	case *function:
		if t.props != nil {
			if _, ok := t.props.own(k); ok {
				return true, nil
			}
		}
		return k == "name" || k == "length", nil
	case *array:
		i := arrayIndex(k)
		return k == "length" || (i >= 0 && i < len(t.items)), nil
	case string:
		i := arrayIndex(k)
		return k == "length" || (i >= 0 && i < utf16Len(t)), nil
	}
	return false, nil
}

func (r *rt) minMax(args []any, max bool) (any, error) {
	best := math.Inf(1)
	if max {
		best = math.Inf(-1)
	}
	nan := false
	for _, a := range args {
		f, err := r.toNumber(a)
		if err != nil {
			return nil, err
		}
		switch {
		case math.IsNaN(f):
			nan = true
		case max && (f > best || (f == 0 && best == 0 && !math.Signbit(f))):
			best = f
		case !max && (f < best || (f == 0 && best == 0 && math.Signbit(f))):
			best = f
		}
	}
	if nan {
		return math.NaN(), nil
	}
	return best, nil
}

func iterateArray(r *rt, a *array, args []any, visit func(v, res any, i int) (any, bool), def any, skipHoles ...bool) (any, error) {
	f := arg(args, 0)
	if err := r.requireCallable(f); err != nil {
		return nil, err
	}
	n := len(a.items)
	for i := 0; i < n; i++ {
		var v any = Undefined
		if i < len(a.items) {
			if isHole(a.items[i]) && len(skipHoles) > 0 {
				continue
			}
			v = unhole(a.items[i])
		} else if len(skipHoles) > 0 {
			continue
		}
		res, err := r.callback(f, v, float64(i), a)
		if err != nil {
			return nil, err
		}
		if out, done := visit(v, res, i); done {
			return out, nil
		}
	}
	return def, nil
}

func iterateArrayReverse(r *rt, a *array, args []any, visit func(v, res any, i int) (any, bool), def any) (any, error) {
	f := arg(args, 0)
	if err := r.requireCallable(f); err != nil {
		return nil, err
	}
	for i := len(a.items) - 1; i >= 0; i-- {
		var v any = Undefined
		if i < len(a.items) {
			v = unhole(a.items[i])
		}
		res, err := r.callback(f, v, float64(i), a)
		if err != nil {
			return nil, err
		}
		if out, done := visit(v, res, i); done {
			return out, nil
		}
	}
	return def, nil
}

// flatten appends items to out, descending at most depth levels. nesting is
// how many levels have already been descended: flat(Infinity) on a deeply
// nested (or cyclic) array would otherwise recurse once per level until the
// goroutine stack overflows fatally, so it throws a RangeError past
// maxConversionNesting the way engines do.
func (r *rt) flatten(out *[]any, items []any, depth float64, nesting int) error {
	for _, v := range items {
		if isHole(v) {
			continue
		}
		if b, ok := v.(*array); ok && depth > 0 {
			if nesting >= maxConversionNesting {
				return r.rangeError("Maximum call stack size exceeded")
			}
			if err := r.flatten(out, b.items, depth-1, nesting+1); err != nil {
				return err
			}
			continue
		}
		if err := r.charge(allocSlot); err != nil {
			return err
		}
		*out = append(*out, v)
		if len(*out) > r.x.maxItems {
			return r.rangeError("Invalid array length")
		}
	}
	return nil
}

func (r *rt) reduce(a *array, args []any, right bool) (any, error) {
	f := arg(args, 0)
	if err := r.requireCallable(f); err != nil {
		return nil, err
	}
	n := len(a.items)
	i, step, end := 0, 1, n
	if right {
		i, step, end = n-1, -1, -1
	}
	var acc any
	if len(args) >= 2 {
		acc = args[1]
	} else {
		for i != end && isHole(a.items[i]) {
			i += step
		}
		if i == end {
			return nil, r.typeError("No initial value")
		}
		acc = a.items[i]
		i += step
	}
	for ; i != end; i += step {
		if i >= len(a.items) || isHole(a.items[i]) {
			continue
		}
		v, err := r.callback(f, acc, a.items[i], float64(i), a)
		if err != nil {
			return nil, err
		}
		acc = v
	}
	return acc, nil
}

// sortItems ports Goja's comparator and uses the same stable sort, so even
// inconsistent comparators produce identical orders.
func (r *rt) sortItems(items []any, cmp any) error {
	if !isUndefined(cmp) {
		if _, ok := cmp.(*function); !ok {
			return r.typeError("The comparison function must be either a function or undefined")
		}
	}
	// Holes sort after everything, undefined included.
	dense := items[:0:0]
	holes := 0
	for _, v := range items {
		if isHole(v) {
			holes++
		} else {
			dense = append(dense, v)
		}
	}
	if holes > 0 {
		defer func() {
			copy(items, dense)
			for i := len(dense); i < len(items); i++ {
				items[i] = hole
			}
		}()
		items = dense
	}
	var firstErr error
	strs := map[int]string{}
	_ = strs
	s := &sorter{items: items, less: func(x, y any) bool {
		if firstErr != nil {
			return false
		}
		c, err := r.sortCompare(x, y, cmp)
		if err != nil {
			firstErr = err
			return false
		}
		return c < 0
	}}
	sort.Stable(s)
	return firstErr
}

type sorter struct {
	items []any
	less  func(x, y any) bool
}

func (s *sorter) Len() int           { return len(s.items) }
func (s *sorter) Less(i, j int) bool { return s.less(s.items[i], s.items[j]) }
func (s *sorter) Swap(i, j int)      { s.items[i], s.items[j] = s.items[j], s.items[i] }

func (r *rt) sortCompare(x, y, cmp any) (int, error) {
	xu, yu := isUndefined(x), isUndefined(y)
	switch {
	case xu && yu:
		return 0, nil
	case xu:
		return 1, nil
	case yu:
		return -1, nil
	}
	if !isUndefined(cmp) {
		v, err := r.call(cmp, []any{x, y})
		if err != nil {
			return 0, err
		}
		f, err := r.toNumber(v)
		if err != nil {
			return 0, err
		}
		switch {
		case f > 0:
			return 1, nil
		case f < 0, math.Signbit(f):
			return -1, nil
		}
		return 0, nil
	}
	a, err := r.toString(x)
	if err != nil {
		return 0, err
	}
	b, err := r.toString(y)
	if err != nil {
		return 0, err
	}
	return compareUTF16(a, b), nil
}

func (r *rt) splice(a *array, args []any, returnRemoved bool) (any, error) {
	n := len(a.items)
	start, err := r.relIndex(arg(args, 0), n, 0)
	if err != nil {
		return nil, err
	}
	del := 0
	switch {
	case len(args) == 0:
		del = 0
	case len(args) == 1:
		del = n - start
	default:
		f, err := r.toNumber(args[1])
		if err != nil {
			return nil, err
		}
		del = int(math.Min(math.Max(toIntegerOrInfinity(f), 0), float64(n-start)))
	}
	var insert []any
	if len(args) > 2 {
		insert = args[2:]
	}
	if n-del+len(insert) > r.x.maxItems {
		return nil, r.rangeError("Invalid array length")
	}
	if returnRemoved {
		if err := r.chargeArray(del); err != nil {
			return nil, err
		}
	}
	if err := r.growArray(a, n-del+len(insert)); err != nil {
		return nil, err
	}
	removed := append([]any{}, a.items[start:start+del]...)
	out := make([]any, 0, n-del+len(insert))
	out = append(out, a.items[:start]...)
	out = append(out, insert...)
	out = append(out, a.items[start+del:]...)
	a.items = out
	return &array{items: removed}, nil
}

func (r *rt) pad(s string, args []any, start bool) (any, error) {
	f, err := r.toNumber(arg(args, 0))
	if err != nil {
		return nil, err
	}
	target := toIntegerOrInfinity(f)
	l := utf16Len(s)
	if target <= float64(l) {
		return s, nil
	}
	fill := " "
	if v := arg(args, 1); !isUndefined(v) {
		if fill, err = r.toString(v); err != nil {
			return nil, err
		}
	}
	if fill == "" {
		return s, nil
	}
	if target > float64(r.x.maxString) {
		return nil, r.rangeError("Invalid string length")
	}
	// UTF-8 can take up to 3 bytes for each UTF-16 code unit of padding.
	if err := r.charge(len(s) + 3*(int(target)-l) + allocSlot); err != nil {
		return nil, err
	}
	need := int(target) - l
	fu := toUnits(fill)
	pad := make([]uint16, 0, need)
	for len(pad) < need {
		pad = append(pad, fu[:min(len(fu), need-len(pad))]...)
	}
	if start {
		return fromUnits(pad) + s, nil
	}
	return s + fromUnits(pad), nil
}

// replace implements String.prototype.replace/replaceAll with a string
// pattern (regular expressions decline at compile time).
func (r *rt) replace(s string, args []any, all bool) (any, error) {
	pat, err := r.toString(arg(args, 0))
	if err != nil {
		return nil, err
	}
	repl := arg(args, 1)
	fn, isFn := repl.(*function)
	var replStr string
	if !isFn {
		if replStr, err = r.toString(repl); err != nil {
			return nil, err
		}
	}
	u := toUnits(s)
	p := toUnits(pat)
	var positions []int
	for i := 0; i+len(p) <= len(u); {
		if unitsEqual(u[i:i+len(p)], p) {
			positions = append(positions, i)
			if !all {
				break
			}
			if len(p) == 0 {
				i++
			} else {
				i += len(p)
			}
			continue
		}
		i++
	}
	if all && len(p) == 0 && (len(positions) == 0 || positions[len(positions)-1] != len(u)) {
		positions = append(positions, len(u))
	}
	if len(positions) == 0 {
		return s, nil
	}
	var b strings.Builder
	last := 0
	for _, pos := range positions {
		b.WriteString(fromUnits(u[last:pos]))
		if isFn {
			v, err := r.call(fn, []any{pat, float64(pos), s})
			if err != nil {
				return nil, err
			}
			str, err := r.toString(v)
			if err != nil {
				return nil, err
			}
			b.WriteString(str)
		} else {
			b.WriteString(expandReplacement(replStr, pat, u, pos, len(p)))
		}
		last = pos + len(p)
		if b.Len() > r.x.maxString {
			return nil, r.rangeError("Invalid string length")
		}
	}
	b.WriteString(fromUnits(u[last:]))
	return r.stringValue(b.String())
}

// expandReplacement applies GetSubstitution for a string pattern.
func expandReplacement(repl, matched string, u []uint16, pos, n int) string {
	if !strings.Contains(repl, "$") {
		return repl
	}
	var b strings.Builder
	for i := 0; i < len(repl); i++ {
		c := repl[i]
		if c != '$' || i+1 >= len(repl) {
			b.WriteByte(c)
			continue
		}
		switch repl[i+1] {
		case '$':
			b.WriteByte('$')
			i++
		case '&':
			b.WriteString(matched)
			i++
		case '`':
			b.WriteString(fromUnits(u[:pos]))
			i++
		case '\'':
			b.WriteString(fromUnits(u[min(pos+n, len(u)):]))
			i++
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// callGlobal implements the global conversion functions.
func (r *rt) callGlobal(name string, args []any) (any, error) {
	switch name {
	case "String":
		if len(args) == 0 {
			return "", nil
		}
		s, err := r.toString(args[0])
		if err != nil {
			return nil, err
		}
		switch args[0].(type) {
		case float64, *dateValue:
			// A fresh rendering; other conversions charge where they build
			// (join, Error.prototype.toString) or return existing text.
			return r.stringValue(s)
		}
		return s, nil
	case "Number":
		if len(args) == 0 {
			return 0.0, nil
		}
		return r.toNumber(args[0])
	case "Boolean":
		return toBoolean(arg(args, 0)), nil
	case "parseInt":
		s, err := r.toString(arg(args, 0))
		if err != nil {
			return nil, err
		}
		radix := 0.0
		if v := arg(args, 1); !isUndefined(v) {
			if radix, err = r.toNumber(v); err != nil {
				return nil, err
			}
		}
		return parseIntJS(s, int(toInt32(radix))), nil
	case "parseFloat":
		s, err := r.toString(arg(args, 0))
		if err != nil {
			return nil, err
		}
		return parseFloatJS(s), nil
	case "isNaN":
		f, err := r.toNumber(arg(args, 0))
		return math.IsNaN(f), err
	case "isFinite":
		f, err := r.toNumber(arg(args, 0))
		return !math.IsNaN(f) && !math.IsInf(f, 0), err
	case "encodeURIComponent", "encodeURI":
		s, err := r.toString(arg(args, 0))
		if err != nil {
			return nil, err
		}
		keep := ""
		if name == "encodeURI" {
			keep = uriReserved
		}
		return r.stringValue(uriEncode(s, keep))
	case "decodeURIComponent", "decodeURI":
		s, err := r.toString(arg(args, 0))
		if err != nil {
			return nil, err
		}
		preserve := ""
		if name == "decodeURI" {
			preserve = uriReserved
		}
		out, err := r.uriDecode(s, preserve)
		if err != nil {
			return nil, err
		}
		return r.stringValue(out)
	}
	return nil, errInternal
}

// console formats arguments like the Goja runner and hands the line to the host.
func (r *rt) console(level string, args []any) error {
	parts := make([]string, len(args))
	for i, a := range args {
		s, err := r.consoleArg(a)
		if err != nil {
			return err
		}
		parts[i] = s
	}
	line := strings.Join(parts, " ")
	if r.seq != nil {
		// Parallel batch item: released in item order by the sequencer, so
		// the line is held until then.
		if err := r.chargeString(line); err != nil {
			return err
		}
		r.logs = append(r.logs, consoleLine{level, line})
		return nil
	}
	r.emitConsole(level, line)
	return nil
}

func (r *rt) emitConsole(level, line string) {
	if r.x.opts.Console != nil {
		r.x.opts.Console(level, line)
	}
}

func (r *rt) consoleArg(v any) (string, error) {
	if c, ok := v.(*collection); ok && c.isMap {
		// Goja exports a Map as [][2]any, which its console prints via String().
		return "[object Map]", nil
	}
	switch v.(type) {
	case *object, *array, *regexpValue, *collection, *iterator, *intlObject:
		exported, err := exportConsole(v, r.location())
		if err == nil {
			if clean, err := MarshalExport(exported); err == nil {
				return string(clean), nil
			}
		}
		if _, isArray := v.(*array); isArray {
			// Goja falls back to String(value) when encoding fails.
			return r.toString(v)
		}
		b, err := json.Marshal(errorToStringOr(v))
		if err != nil {
			return "", err
		}
		return string(b[1 : len(b)-1]), nil
	}
	return r.toString(v)
}

func errorToStringOr(v any) string {
	if o, ok := v.(*object); ok && o.errName != "" {
		return errorToString(o)
	}
	return "[object Object]"
}

// toRegexp implements the RegExp coercion in String.prototype.match/search.
func (r *rt) toRegexp(v any) (*regexpValue, error) {
	if rx, ok := v.(*regexpValue); ok {
		return rx, nil
	}
	var args []any
	if !isUndefined(v) {
		args = []any{v}
	}
	rx, err := r.newRegExp(args)
	if err != nil {
		return nil, err
	}
	return rx.(*regexpValue), nil
}

// Unicode case mapping follows Goja: x/text full case mapping, including
// its workaround for final sigma after U+0345.
func toUpperJS(s string) string {
	if isASCII(s) {
		return strings.ToUpper(s)
	}
	return cases.Upper(language.Und).String(s)
}

func toLowerJS(s string) string {
	if isASCII(s) {
		return strings.ToLower(s)
	}
	r := []rune(cases.Lower(language.Und).String(s))
	for i := 0; i < len(r)-1; i++ {
		if (i == 0 || r[i-1] != 0x3b1) && r[i] == 0x345 && r[i+1] == 0x3c2 {
			i++
			r[i] = 0x3c3
		}
	}
	return string(r)
}

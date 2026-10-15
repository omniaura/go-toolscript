package toolscript

import (
	"time"
	"unicode/utf8"
)

// errorString implements Error.prototype.toString on the caller's runtime, so
// an Error converts under the same rules as every other value String()
// reaches:
//
//   - Each Error level takes one level of the conversion budget join and
//     toLocaleString draw on (enterNesting). A long chain of Errors, or arrays
//     linked through Errors, throws a catchable RangeError instead of
//     overflowing the goroutine stack.
//   - An Error has no cycle guard of its own, as in V8. An Error whose name or
//     message leads back to itself (e.message = e) recurses until the budget
//     runs out and throws RangeError: Maximum call stack size exceeded, which
//     is what V8 throws. A cycle that passes through an array is still cut by
//     join's guard, which renders the cyclic array as "", so e.message = [e]
//     gives "Error: Error", as in V8.
//   - Each level charges one step, so a graph whose Errors share sub-Errors
//     (e.name = e.message = previous, repeated) is stopped by MaxSteps or the
//     context deadline rather than running for exponential time.
//   - An error from converting name or message propagates, as in V8 (an own
//     toString that throws makes String(e) throw).
func (r *rt) errorString(o *object) (string, error) {
	if err := r.tick(); err != nil {
		return "", err
	}
	if err := r.enterNesting(); err != nil {
		return "", err
	}
	defer r.leaveNesting()
	ns, ms := "Error", ""
	if name, _ := o.get("name"); !isUndefined(name) {
		s, err := r.toString(name)
		if err != nil {
			return "", err
		}
		ns = s
	}
	if msg, _ := o.get("message"); !isUndefined(msg) {
		s, err := r.toString(msg)
		if err != nil {
			return "", err
		}
		ms = s
	}
	switch {
	case ns == "":
		return ms, nil
	case ms == "":
		return ns, nil
	case len(ns)+len(": ")+len(ms) > r.x.maxString:
		return "", r.rangeError("Invalid string length")
	}
	return ns + ": " + ms, nil
}

// The detached formatter renders values the way String() does for the callers
// that have no live runtime and must not fail: Throw.Error() (the host
// formatting an uncaught exception after the script has finished), an Error's
// stack property and the console's fallback text for an Error. Like V8's
// side-effect-free error formatting it never runs script code, and where the
// runtime would throw it stops instead:
//
//   - An array or Error already being rendered (a cycle), or one nested past
//     maxConversionNesting, renders as nothing.
//   - A value whose conversion needs script code (an object with its own
//     toString or valueOf) cannot be rendered. Inside an Error's name or
//     message that part renders as nothing, and an array containing such a
//     value cannot be rendered either, as before this formatter existed.
//   - The output is truncated at detachedMaxBytes, and rendering stops after
//     detachedMaxVisits values or detachedMaxWrites bytes written (counting
//     output a part that cannot be rendered discards). Every rendering is
//     therefore bounded in stack, time and memory, even for a graph whose
//     Errors share sub-Errors.
//
// The output is built in one buffer, so a long chain costs time linear in its
// output rather than copying every level's text into the level above.
const (
	detachedMaxBytes  = 16 << 20 // the default MaxStringBytes
	detachedMaxWrites = 4 * detachedMaxBytes
	detachedMaxVisits = 1 << 20
)

type detached struct {
	buf    []byte
	active map[any]bool // arrays and Errors on the current path
	visits int
	writes int
	stop   bool // a cap was reached: nothing more renders
}

// errorToString renders an Error the way Error.prototype.toString does,
// without a live runtime (see detached).
func errorToString(o *object) string {
	var d detached
	d.error(o)
	return string(d.buf)
}

// detachedString renders v the way String(v) does, without a live runtime
// (see detached). ok is false when that needs script code or v is not a
// script value.
func detachedString(v any) (s string, ok bool) {
	var d detached
	ok = d.value(v)
	return string(d.buf), ok
}

func (d *detached) write(s string) {
	if d.stop {
		return
	}
	room := min(detachedMaxBytes-len(d.buf), detachedMaxWrites-d.writes)
	if len(s) > room {
		for room > 0 && !utf8.RuneStart(s[room]) {
			room-- // cut at a rune boundary
		}
		s = s[:room]
		d.stop = true
	}
	d.writes += len(s)
	d.buf = append(d.buf, s...)
}

// enter marks v as on the current path. It reports false for a cycle or past
// maxConversionNesting; the caller then renders nothing for v.
func (d *detached) enter(v any) bool {
	if d.active == nil {
		d.active = map[any]bool{}
	}
	if d.active[v] || len(d.active) >= maxConversionNesting {
		return false
	}
	d.active[v] = true
	return true
}

// value renders v as rt.toString does. It reports false when v cannot be
// rendered without running script code.
func (d *detached) value(v any) bool {
	if d.stop {
		return true
	}
	if d.visits++; d.visits > detachedMaxVisits {
		d.stop = true
		return true
	}
	switch t := v.(type) {
	case string:
		d.write(t)
	case nil:
		d.write("null")
	case undefined:
		d.write("undefined")
	case bool:
		if t {
			d.write("true")
		} else {
			d.write("false")
		}
	case float64:
		d.write(numberToString(t))
	case *array:
		return d.array(t)
	case *object:
		switch {
		case userConversion(t):
			return false // its own toString/valueOf is script code
		case t.errName != "":
			d.error(t)
		default:
			d.write("[object Object]")
		}
	case *hostObject:
		d.write("[object Object]")
	case *regexpValue:
		d.write("/" + t.pat.source + "/" + t.pat.flags)
	case *collection:
		if t.isMap {
			d.write("[object Map]")
		} else {
			d.write("[object Set]")
		}
	case *iterator:
		d.write("[object " + t.name + " Iterator]")
	case *intlObject:
		d.write("[object Intl." + t.kind.name + "]")
	case *dateValue:
		// No runtime means no ExecuteOptions.Location: render in time.Local,
		// as this text always has been.
		if t.isSet() {
			d.write(timeFromMsec(t.msec).In(time.Local).Format(dateTimeLayout))
		} else {
			d.write("Invalid Date")
		}
	case *function:
		if t.native != nil {
			d.write("function " + t.name + "() { [native code] }")
		} else {
			d.write(t.code.source)
		}
	default:
		return false
	}
	return true
}

// array renders Array.prototype.join(","): a cycle renders as "", as join's
// guard does.
func (d *detached) array(a *array) bool {
	if !d.enter(a) {
		return true
	}
	defer delete(d.active, a)
	for i, v := range a.items {
		if d.stop {
			break
		}
		if i > 0 {
			d.write(",")
		}
		if !isNullish(v) && !d.value(v) {
			return false
		}
	}
	return true
}

// error renders Error.prototype.toString: name, ": " and message, or just the
// one of them that is not empty.
func (d *detached) error(o *object) {
	if d.stop || !d.enter(o) {
		return
	}
	defer delete(d.active, o)
	start := len(d.buf)
	if name, _ := o.get("name"); isUndefined(name) {
		d.write("Error")
	} else {
		d.part(name)
	}
	msg, _ := o.get("message")
	if len(d.buf) == start { // empty name: the message alone
		if !isUndefined(msg) {
			d.part(msg)
		}
		return
	}
	colon := len(d.buf)
	d.write(": ")
	body := len(d.buf)
	if !isUndefined(msg) {
		d.part(msg)
	}
	if len(d.buf) == body { // empty message: the name alone
		d.buf = d.buf[:colon]
	}
}

// part renders an Error's name or message; one that cannot be rendered
// renders as nothing.
func (d *detached) part(v any) {
	mark := len(d.buf)
	if !d.value(v) {
		d.buf = d.buf[:mark]
	}
}

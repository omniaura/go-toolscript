package toolscript

import (
	"math"
	"sort"
	"strconv"
	"strings"
)

// Runtime values are JSON-shaped: nil (null), Undefined, bool, float64,
// string, *object, *array, *function and *hostObject. Strings are Go (UTF-8)
// strings; every index-based operation uses JavaScript UTF-16 code units.

// object is an ordinary JavaScript object: own data properties only, with the
// ECMAScript enumeration order (array-index keys ascending, then insertion).
type object struct {
	index    map[string]int // key -> position, built past smallObject keys
	keys     []string       // insertion order
	vals     []any
	errName  string // non-empty for Error objects
	errMsg   any    // own non-enumerable message (Undefined when absent)
	errCause any    // own non-enumerable cause, when hasCause
	hasCause bool   // errCause is present (it may be null or undefined)
	indexKey bool   // at least one key is a canonical array index
}

type array struct {
	items []any
	props *object // named own properties (exec results carry index/input/groups)
}

// function is a user closure or a built-in (native != nil).
type function struct {
	code   *funcCode
	scope  *scope
	native func(r *rt, this any, args []any) (any, error)
	props  *object // own properties assigned by the script (fn.state = ...)
	name   string
	length int // .length of native functions (bound functions)
}

// hostObject exposes a host Object's property view while preserving its
// original export until the script writes to it.
type hostObject struct {
	view   *object
	export any
	dirty  bool
}

type tdzMarker struct{}

// holeMarker is an array element that does not exist (a sparse slot). Every
// read converts it to undefined; iteration methods that skip holes skip it.
type holeMarker struct{}

var hole any = holeMarker{}

func unhole(v any) any {
	if _, ok := v.(holeMarker); ok {
		return Undefined
	}
	return v
}

func isHole(v any) bool {
	_, ok := v.(holeMarker)
	return ok
}

// denseItems returns the items with holes read as undefined.
func denseItems(items []any) []any {
	for i, v := range items {
		if isHole(v) {
			out := make([]any, len(items))
			for j, w := range items {
				out[j] = unhole(w)
			}
			_ = i
			return out
		}
	}
	return items
}

// tdz marks a lexical binding that has not been initialized yet.
var tdz any = tdzMarker{}

// Objects keep parallel key/value slices; most script and tool objects are
// small, where a linear scan beats hashing and allocates far less.
const smallObject = 8

func newObject(size int) *object {
	return &object{keys: make([]string, 0, size), vals: make([]any, 0, size)}
}

func (o *object) find(key string) int {
	if o.index != nil {
		if i, ok := o.index[key]; ok {
			return i
		}
		return -1
	}
	for i, k := range o.keys {
		if k == key {
			return i
		}
	}
	return -1
}

// own reads an own enumerable data property (no Error or prototype logic).
func (o *object) own(key string) (any, bool) {
	if i := o.find(key); i >= 0 {
		return o.vals[i], true
	}
	return nil, false
}

func (o *object) size() int { return len(o.keys) }

func newError(name, msg string) *object {
	o := newObject(0)
	o.errName, o.errMsg = name, msg
	return o
}

func (o *object) get(key string) (any, bool) {
	v, ok := o.own(key)
	if !ok && o.errName != "" {
		switch key {
		case "message":
			if isUndefined(o.errMsg) {
				return "", true // Error.prototype.message
			}
			return o.errMsg, true
		case "name":
			return o.errName, true
		case "stack":
			return errorToString(o), true
		case "cause":
			if o.hasCause {
				return o.errCause, true
			}
		}
	}
	return v, ok
}

// hasOwnHidden reports an Error's own non-enumerable properties.
func (o *object) hasOwnHidden(key string) bool {
	if o.errName == "" {
		return false
	}
	switch key {
	case "stack":
		return true
	case "message":
		return !isUndefined(o.errMsg)
	case "cause":
		return o.hasCause
	}
	return false
}

func (o *object) set(key string, v any) {
	if o.errName != "" && o.hasOwnHidden(key) && o.find(key) < 0 {
		// Writing an existing non-enumerable own message/cause keeps it
		// hidden; assigning one the Error lacks creates an enumerable
		// property, as in Goja.
		switch key {
		case "message":
			o.errMsg = v
			return
		case "cause":
			o.errCause = v
			return
		}
	}
	if i := o.find(key); i >= 0 {
		o.vals[i] = v
		return
	}
	o.keys = append(o.keys, key)
	o.vals = append(o.vals, v)
	if o.index != nil {
		o.index[key] = len(o.keys) - 1
	} else if len(o.keys) > smallObject {
		o.index = make(map[string]int, len(o.keys)*2)
		for i, k := range o.keys {
			o.index[k] = i
		}
	}
	if !o.indexKey && isIndexKey(key) {
		o.indexKey = true
	}
}

func (o *object) delete(key string) {
	i := o.find(key)
	if i < 0 {
		if o.errName != "" {
			switch key {
			case "message":
				o.errMsg = Undefined
			case "cause":
				o.errCause, o.hasCause = nil, false
			}
		}
		return
	}
	o.keys = append(o.keys[:i:i], o.keys[i+1:]...)
	o.vals = append(o.vals[:i:i], o.vals[i+1:]...)
	if o.index != nil {
		delete(o.index, key)
		for j := i; j < len(o.keys); j++ {
			o.index[o.keys[j]] = j
		}
	}
}

// ownKeys returns keys in ECMAScript OrdinaryOwnPropertyKeys order.
func (o *object) ownKeys() []string {
	if !o.indexKey {
		return o.keys
	}
	var idx, rest []string
	for _, k := range o.keys {
		if isIndexKey(k) {
			idx = append(idx, k)
		} else {
			rest = append(rest, k)
		}
	}
	sort.Slice(idx, func(i, j int) bool {
		a, _ := strconv.ParseUint(idx[i], 10, 64)
		b, _ := strconv.ParseUint(idx[j], 10, 64)
		return a < b
	})
	return append(idx, rest...)
}

// isIndexKey reports whether k is a canonical array index (0 .. 2^32-2).
func isIndexKey(k string) bool {
	if k == "" || len(k) > 10 || (len(k) > 1 && k[0] == '0') {
		return false
	}
	for i := 0; i < len(k); i++ {
		if k[i] < '0' || k[i] > '9' {
			return false
		}
	}
	n, err := strconv.ParseUint(k, 10, 64)
	return err == nil && n < math.MaxUint32
}

// arrayIndex converts a property key to an array index, or -1.
func arrayIndex(k string) int {
	if !isIndexKey(k) {
		return -1
	}
	n, _ := strconv.Atoi(k)
	return n
}

func isNullish(v any) bool {
	switch v.(type) {
	case nil, undefined, holeMarker:
		return true
	}
	return false
}

func typeOf(v any) string {
	switch v.(type) {
	case undefined, tdzMarker, holeMarker:
		return "undefined"
	case nil:
		return "object"
	case bool:
		return "boolean"
	case float64:
		return "number"
	case string:
		return "string"
	case *function:
		return "function"
	}
	return "object"
}

func toBoolean(v any) bool {
	switch v := v.(type) {
	case nil, undefined:
		return false
	case bool:
		return v
	case float64:
		return v != 0 && !math.IsNaN(v)
	case string:
		return v != ""
	}
	return true
}

// toPrimitive applies OrdinaryToPrimitive for the value shapes we model.
// Objects have no user-defined valueOf/toString, so both hints yield strings.
func (r *rt) toPrimitive(v any) (any, error) { return r.toPrimitiveHint(v, "default") }

// toPrimitiveHint implements OrdinaryToPrimitive, honoring script-defined
// own toString/valueOf methods on plain objects.
func (r *rt) toPrimitiveHint(v any, hint string) (any, error) {
	switch t := v.(type) {
	case *object:
		if userConversion(t) {
			order := [2]string{"valueOf", "toString"}
			if hint == "string" {
				order = [2]string{"toString", "valueOf"}
			}
			for _, m := range order {
				fv, own := t.own(m)
				if !own {
					if m == "toString" {
						return r.defaultObjectString(t)
					}
					continue // Object.prototype.valueOf returns the object
				}
				f, callable := fv.(*function)
				if !callable {
					continue // a non-callable own member shadows the default
				}
				res, err := r.call(f, nil)
				if err != nil {
					return nil, err
				}
				if !isObjectValue(res) {
					return res, nil
				}
			}
			return nil, r.typeError("Cannot convert object to primitive value")
		}
		return r.defaultObjectString(t)
	case *dateValue:
		// Date's @@toPrimitive: "number" gives the time value, else a string.
		if hint == "number" {
			if !t.isSet() {
				return math.NaN(), nil
			}
			return float64(t.msec), nil
		}
		return r.dateString(t, dateTimeLayout, false), nil
	case *array, *function, *hostObject, *regexpValue, *collection, *iterator, *intlObject:
		s, err := r.toString(v)
		return s, err
	}
	return v, nil
}

// userConversion reports own toString/valueOf members, callable or not: a
// non-callable one (tool JSON {"toString": "text"}) shadows the default.
func userConversion(o *object) bool {
	_, s := o.own("toString")
	_, v := o.own("valueOf")
	return s || v
}

func (r *rt) defaultObjectString(o *object) (string, error) {
	if o.errName != "" {
		return r.errorString(o)
	}
	return "[object Object]", nil
}

func (r *rt) toString(v any) (string, error) {
	switch v := v.(type) {
	case string:
		return v, nil
	case nil:
		return "null", nil
	case undefined:
		return "undefined", nil
	case bool:
		if v {
			return "true", nil
		}
		return "false", nil
	case float64:
		return numberToString(v), nil
	case *array:
		return r.join(v, ",")
	case *object:
		if userConversion(v) {
			p, err := r.toPrimitiveHint(v, "string")
			if err != nil {
				return "", err
			}
			return r.toString(p)
		}
		return r.defaultObjectString(v)
	case *hostObject:
		return "[object Object]", nil
	case *regexpValue:
		return "/" + v.pat.source + "/" + v.pat.flags, nil
	case *collection:
		if v.isMap {
			return "[object Map]", nil
		}
		return "[object Set]", nil
	case *iterator:
		return "[object " + v.name + " Iterator]", nil
	case *intlObject:
		return "[object Intl." + v.kind.name + "]", nil
	case *dateValue:
		return r.dateString(v, dateTimeLayout, false), nil
	case *function:
		if v.native != nil {
			return "function " + v.name + "() { [native code] }", nil
		}
		return v.code.source, nil
	}
	return "", errInternal
}

// maxConversionNesting bounds how deeply the value-conversion builtins
// (join/toString, toLocaleString, Error.prototype.toString, flat), JSON.parse
// (decodeToken) and the host-data converter (fromGo) descend through nested
// values. Each level is a Go stack frame, and a script can nest containers
// millions deep within its step budget; past the goroutine stack limit that is
// a fatal runtime error the host process cannot recover(), not a catchable
// panic. Engines instead throw a RangeError. JSON.stringify
// (stringifier.str) and Marshal (jsonValue) enforce the same 10000 levels.
const maxConversionNesting = 10000

// enterNesting takes one level of the conversion budget that join,
// toLocaleString and errorString share: r.nesting counts the conversion levels
// active on this goroutine's stack, so nesting that alternates between arrays
// and Errors (or runs through a script toString in between) is bounded as one
// recursion. Past maxConversionNesting it throws a RangeError. On success the
// caller must call leaveNesting when that level returns.
func (r *rt) enterNesting() error {
	if r.nesting >= maxConversionNesting {
		return r.rangeError("Maximum call stack size exceeded")
	}
	r.nesting++
	return nil
}

func (r *rt) leaveNesting() { r.nesting-- }

// enterJoin marks array a as being converted and reports a cycle (which
// engines render as the empty string) or throws past maxConversionNesting.
// join and toLocaleString share r.joining and the r.nesting budget. On success
// (cycle == false, err == nil) the caller must call leaveJoin(a) when it is
// done.
func (r *rt) enterJoin(a *array) (cycle bool, err error) {
	if r.joining == nil {
		r.joining = map[*array]bool{}
	}
	if r.joining[a] {
		return true, nil
	}
	if err := r.enterNesting(); err != nil {
		return false, err
	}
	r.joining[a] = true
	return false, nil
}

func (r *rt) leaveJoin(a *array) {
	delete(r.joining, a)
	r.leaveNesting()
}

// join implements Array.prototype.join, guarding against cyclic arrays the
// same way engines do (a cycle renders as the empty string) and bounding the
// nesting depth it recurses through toString (see maxConversionNesting).
func (r *rt) join(a *array, sep string) (string, error) {
	if cycle, err := r.enterJoin(a); cycle || err != nil {
		return "", err
	}
	defer r.leaveJoin(a)
	var b strings.Builder
	for i, v := range a.items {
		if i > 0 {
			b.WriteString(sep)
		}
		if isNullish(v) {
			continue
		}
		s, err := r.toString(v)
		if err != nil {
			return "", err
		}
		b.WriteString(s)
		if b.Len() > r.x.maxString {
			return "", r.rangeError("Invalid string length")
		}
	}
	return b.String(), nil
}

func (r *rt) toNumber(v any) (float64, error) {
	switch v := v.(type) {
	case float64:
		return v, nil
	case nil:
		return 0, nil
	case undefined:
		return math.NaN(), nil
	case bool:
		if v {
			return 1, nil
		}
		return 0, nil
	case string:
		return stringToNumber(v), nil
	}
	p, err := r.toPrimitiveHint(v, "number")
	if err != nil {
		return 0, err
	}
	return r.toNumber(p)
}

func (r *rt) toPropertyKey(v any) (string, error) {
	if s, ok := v.(string); ok {
		return s, nil
	}
	if f, ok := v.(float64); ok {
		return numberToString(f), nil
	}
	return r.toString(v)
}

func toInt32(f float64) int32 {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	return int32(uint32(int64(math.Mod(math.Trunc(f), 4294967296))))
}

func toUint32(f float64) uint32 { return uint32(toInt32(f)) }

// toIntegerOrInfinity implements ToIntegerOrInfinity on a number.
func toIntegerOrInfinity(f float64) float64 {
	if math.IsNaN(f) {
		return 0
	}
	return math.Trunc(f) + 0
}

func strictEquals(a, b any) bool {
	switch a := a.(type) {
	case float64:
		b, ok := b.(float64)
		return ok && a == b
	case string:
		b, ok := b.(string)
		return ok && a == b
	case bool:
		b, ok := b.(bool)
		return ok && a == b
	case nil:
		return b == nil
	case undefined:
		_, ok := b.(undefined)
		return ok
	}
	return a == b // reference identity for objects, arrays and functions
}

// sameValueZero is strict equality where NaN equals NaN (includes()).
func sameValueZero(a, b any) bool {
	if x, ok := a.(float64); ok {
		if y, ok := b.(float64); ok && math.IsNaN(x) && math.IsNaN(y) {
			return true
		}
	}
	return strictEquals(a, b)
}

func isObjectValue(v any) bool {
	switch v.(type) {
	case *object, *array, *function, *hostObject, *regexpValue, *collection, *iterator, *dateValue, *intlObject:
		return true
	}
	return false
}

func (r *rt) looseEquals(a, b any) (bool, error) {
	if typeOf(a) == typeOf(b) && (a == nil) == (b == nil) {
		return strictEquals(a, b), nil
	}
	if isNullish(a) && isNullish(b) {
		return true, nil
	}
	if isNullish(a) || isNullish(b) {
		return false, nil
	}
	switch x := a.(type) {
	case float64:
		switch y := b.(type) {
		case string:
			return x == stringToNumber(y), nil
		case bool:
			return r.looseEquals(x, boolNumber(y))
		}
	case string:
		switch b.(type) {
		case float64:
			return r.looseEquals(b, a)
		case bool:
			return r.looseEquals(a, boolNumber(b.(bool)))
		}
	case bool:
		return r.looseEquals(boolNumber(x), b)
	}
	if _, ok := b.(bool); ok {
		return r.looseEquals(a, boolNumber(b.(bool)))
	}
	if isObjectValue(a) && !isObjectValue(b) {
		p, err := r.toPrimitive(a)
		if err != nil {
			return false, err
		}
		return r.looseEquals(p, b)
	}
	if isObjectValue(b) && !isObjectValue(a) {
		p, err := r.toPrimitive(b)
		if err != nil {
			return false, err
		}
		return r.looseEquals(a, p)
	}
	return false, nil
}

func boolNumber(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// compareValues implements IsLessThan. It returns (less, undefinedResult).
func (r *rt) compareValues(a, b any) (bool, bool, error) {
	pa, err := r.toPrimitiveHint(a, "number")
	if err != nil {
		return false, false, err
	}
	pb, err := r.toPrimitiveHint(b, "number")
	if err != nil {
		return false, false, err
	}
	if sa, ok := pa.(string); ok {
		if sb, ok := pb.(string); ok {
			return compareUTF16(sa, sb) < 0, false, nil
		}
	}
	na, err := r.toNumber(pa)
	if err != nil {
		return false, false, err
	}
	nb, err := r.toNumber(pb)
	if err != nil {
		return false, false, err
	}
	if math.IsNaN(na) || math.IsNaN(nb) {
		return false, true, nil
	}
	return na < nb, false, nil
}

// getProp reads a property with JavaScript semantics for our value shapes.
func (r *rt) getProp(v any, key string) (any, error) {
	switch v := v.(type) {
	case *object:
		if p, ok := v.get(key); ok {
			return p, nil
		}
		ctor := "Object"
		if v.errName != "" {
			ctor = v.errName
		}
		if p, ok, err := inherited(key, ctor); ok {
			return p, err
		}
		return Undefined, nil
	case *array:
		if key == "length" {
			return float64(len(v.items)), nil
		}
		if i := arrayIndex(key); i >= 0 {
			if i < len(v.items) {
				return unhole(v.items[i]), nil
			}
			return Undefined, nil
		}
		if v.props != nil {
			if p, ok := v.props.get(key); ok {
				return p, nil
			}
		}
		if f := protoFn("Array", key); f != nil {
			return f, nil
		}
		if p, ok, err := inherited(key, "Array"); ok {
			return p, err
		}
		return Undefined, nil
	case *collection:
		if p, ok := ownProp(v.props, key); ok {
			return p, nil
		}
		if p, ok := collectionMember(v, key); ok {
			return p, nil
		}
		if p, ok, err := inherited(key, "Object"); ok {
			return p, err
		}
		return Undefined, nil
	case *dateValue:
		if v.props != nil {
			if p, ok := v.props.get(key); ok {
				return p, nil
			}
		}
		if f := protoFn("Date", key); f != nil {
			return f, nil
		}
		if p, ok, err := inherited(key, "Date"); ok {
			return p, err
		}
		return Undefined, nil
	case *intlObject:
		if p, ok := r.intlMember(v, key); ok {
			return p, nil
		}
		if p, ok, err := inherited(key, "Object"); ok {
			return p, err
		}
		return Undefined, nil
	case *iterator:
		if f := protoFn("Iterator", key); f != nil {
			return f, nil
		}
		if p, ok, err := inherited(key, "Object"); ok {
			return p, err
		}
		return Undefined, nil
	case *regexpValue:
		if p, ok := v.get(key); ok {
			return p, nil
		}
		if p, ok := ownProp(v.props, key); ok {
			return p, nil
		}
		if f := protoFn("RegExp", key); f != nil {
			return f, nil
		}
		return Undefined, nil
	case string:
		if key == "length" {
			return float64(utf16Len(v)), nil
		}
		if i := arrayIndex(key); i >= 0 {
			if s, ok := utf16At(v, i); ok {
				return s, nil
			}
			return Undefined, nil
		}
		if f := protoFn("String", key); f != nil {
			return f, nil
		}
		if p, ok, err := inherited(key, "String"); ok {
			return p, err
		}
		return Undefined, nil
	case *hostObject:
		return r.getProp(v.view, key)
	case float64:
		if f := protoFn("Number", key); f != nil {
			return f, nil
		}
		if p, ok, err := inherited(key, "Number"); ok {
			return p, err
		}
		return Undefined, nil
	case bool:
		if f := protoFn("Boolean", key); f != nil {
			return f, nil
		}
		if p, ok, err := inherited(key, "Boolean"); ok {
			return p, err
		}
		return Undefined, nil
	case *function:
		if v.props != nil {
			if p, ok := v.props.get(key); ok {
				return p, nil
			}
		}
		switch key {
		case "name":
			return v.name, nil
		case "length":
			if v.code != nil {
				return float64(v.code.length), nil
			}
			return float64(v.length), nil
		}
		return Undefined, nil
	case nil, undefined:
		return nil, r.typeError("Cannot read property '" + key + "' of undefined")
	case tdzMarker:
		return nil, r.referenceError("Cannot access a variable before initialization")
	}
	return Undefined, nil
}

func (r *rt) setProp(target any, key string, v any) error {
	switch t := target.(type) {
	case *object:
		if key == "__proto__" {
			return errRuntimeUnsupported("__proto__ assignment")
		}
		t.set(key, v)
		return nil
	case *array:
		if key == "length" {
			n, err := r.toNumber(v)
			if err != nil {
				return err
			}
			if n < 0 || n != math.Trunc(n) || n > math.MaxUint32-1 {
				return r.rangeError("Invalid array length")
			}
			return r.resize(t, int(n))
		}
		if i := arrayIndex(key); i >= 0 {
			if i >= len(t.items) {
				if err := r.resize(t, i+1); err != nil {
					return err
				}
			}
			t.items[i] = v
			return nil
		}
		if t.props == nil {
			t.props = newObject(1)
		}
		t.props.set(key, v)
		return nil
	case *regexpValue:
		if key == "lastIndex" {
			t.lastIndex = v
			return nil
		}
		if _, accessor := t.get(key); accessor {
			return nil // prototype accessors without a setter ignore writes
		}
		return setOwnProp(&t.props, key, v)
	case *collection:
		if key == "size" {
			return nil // prototype accessor without a setter
		}
		return setOwnProp(&t.props, key, v)
	case *dateValue:
		if t.props == nil {
			t.props = newObject(1)
		}
		t.props.set(key, v)
		return nil
	case *intlObject:
		if t.props == nil {
			t.props = newObject(1)
		}
		t.props.set(key, v)
		return nil
	case *hostObject:
		t.dirty = true
		t.view.set(key, v)
		return nil
	case nil, undefined:
		return r.typeError("Cannot convert undefined or null to object")
	case *function:
		if t.native != nil || key == "name" || key == "length" {
			return nil // built-ins and read-only function properties ignore writes
		}
		if t.props == nil {
			t.props = newObject(1)
		}
		t.props.set(key, v)
		return nil
	}
	return nil // primitives silently ignore writes in sloppy mode
}

// maxHoles bounds sparse growth: Goja stores huge sparse arrays lazily, so a
// dense emulation of `a.length = 1e7` would cost what Goja does not.
const maxHoles = 1 << 16

func (r *rt) resize(a *array, n int) error {
	if n > r.x.maxItems {
		return r.rangeError("Invalid array length")
	}
	if n-len(a.items) > maxHoles {
		return errRuntimeUnsupported("sparse array growth")
	}
	for len(a.items) < n {
		a.items = append(a.items, hole)
	}
	a.items = a.items[:n]
	return nil
}

func (r *rt) deleteProp(target any, key string) (bool, error) {
	switch t := target.(type) {
	case *object:
		t.delete(key)
		return true, nil
	case *hostObject:
		t.dirty = true
		t.view.delete(key)
		return true, nil
	case *array:
		if i := arrayIndex(key); i >= 0 {
			if i < len(t.items) {
				t.items[i] = hole
			}
			return true, nil
		}
		if t.props != nil {
			t.props.delete(key)
		}
		return key != "length", nil
	case *collection:
		if t.props != nil {
			t.props.delete(key)
		}
	case *regexpValue:
		if t.props != nil {
			t.props.delete(key)
		}
	case nil, undefined:
		return false, r.typeError("Cannot convert undefined or null to object")
	}
	return true, nil
}

// hasProperty implements the `in` operator.
func (r *rt) hasProperty(target any, key string) (bool, error) {
	switch t := target.(type) {
	case *object:
		_, ok := t.get(key)
		return ok || objectProtoMember(key), nil
	case *hostObject:
		_, ok := t.view.get(key)
		return ok || objectProtoMember(key), nil
	case *array:
		if key == "length" || arrayMethods[key] != nil || declinedArrayMembers[key] || objectProtoMember(key) {
			return true, nil
		}
		if t.props != nil {
			if _, ok := t.props.own(key); ok {
				return true, nil
			}
		}
		i := arrayIndex(key)
		return i >= 0 && i < len(t.items) && !isHole(t.items[i]), nil
	case *regexpValue:
		_, ok := t.get(key)
		_, own := ownProp(t.props, key)
		return ok || own, nil
	case *collection:
		_, ok := collectionMember(t, key)
		_, own := ownProp(t.props, key)
		return ok || own || objectProtoMember(key), nil
	case *iterator:
		return key == "next" || objectProtoMember(key), nil
	case *intlObject:
		_, ok := r.intlMember(t, key)
		return ok || objectProtoMember(key), nil
	case *dateValue:
		if t.props != nil {
			if _, ok := t.props.own(key); ok {
				return true, nil
			}
		}
		return dateMethods[key] != nil || objectProtoMember(key), nil
	case *function:
		if t.props != nil {
			if _, ok := t.props.own(key); ok {
				return true, nil
			}
		}
		switch key {
		case "name", "length", "call", "apply", "bind":
			return true, nil
		}
		return objectProtoMember(key), nil
	}
	s, _ := r.toString(target)
	return false, r.typeError("Value is not an object: " + s)
}

func ownEnumerableKeys(v any) []string {
	switch v := v.(type) {
	case *object:
		return v.ownKeys()
	case *hostObject:
		return v.view.ownKeys()
	case *function:
		if v.props != nil {
			return v.props.ownKeys()
		}
		return nil
	case *dateValue:
		if v.props != nil {
			return v.props.ownKeys()
		}
		return nil
	case *collection:
		if v.props != nil {
			return v.props.ownKeys()
		}
		return nil
	case *regexpValue:
		if v.props != nil {
			return v.props.ownKeys()
		}
		return nil
	case *intlObject:
		if v.props != nil {
			return v.props.ownKeys()
		}
		return nil
	case *array:
		keys := make([]string, 0, len(v.items))
		for i, item := range v.items {
			if !isHole(item) {
				keys = append(keys, strconv.Itoa(i))
			}
		}
		if v.props != nil {
			keys = append(keys, v.props.ownKeys()...)
		}
		return keys
	case string:
		n := utf16Len(v)
		keys := make([]string, n)
		for i := range n {
			keys[i] = strconv.Itoa(i)
		}
		return keys
	}
	return nil
}

// objectProtoMember names Object.prototype members every object inherits.
func objectProtoMember(k string) bool {
	switch k {
	case "constructor", "toString", "toLocaleString", "valueOf", "hasOwnProperty", "isPrototypeOf", "propertyIsEnumerable", "__defineGetter__", "__defineSetter__", "__lookupGetter__", "__lookupSetter__", "__proto__":
		return true
	}
	return false
}

// protoFunctions are the shared, immutable function values read through
// inherited prototype members (o[k] where k is "toString", "constructor"...).
var protoFunctions = map[string]*function{}

func protoFunction(key, ctor string) *function {
	name := key
	if key == "constructor" {
		name = ctor
	}
	id := key + "/" + name
	if f, ok := protoFunctions[id]; ok {
		return f
	}
	return &function{name: name, native: func(r *rt, _ any, _ []any) (any, error) {
		return nil, errRuntimeUnsupported("calling an inherited " + key + " as a value")
	}}
}

func init() {
	for _, ctor := range []string{"Object", "Array", "String", "Number", "Boolean", "Function", "Error", "RegExp", "Date"} {
		for _, key := range []string{"constructor", "toString", "toLocaleString", "valueOf", "hasOwnProperty", "isPrototypeOf", "propertyIsEnumerable", "__defineGetter__", "__defineSetter__", "__lookupGetter__", "__lookupSetter__"} {
			name := key
			if key == "constructor" {
				name = ctor
			}
			protoFunctions[key+"/"+name] = &function{name: name, native: func(r *rt, this any, args []any) (any, error) {
				return nil, errRuntimeUnsupported("calling an inherited " + key + " as a value")
			}}
		}
	}
}

// inherited returns a prototype member for a missing own key, if any.
func inherited(key, ctor string) (any, bool, error) {
	if key == "__proto__" {
		// The prototype object itself is not modelled; a fresh empty object
		// answers typeof/keys/property reads the way Object.prototype would.
		return newObject(0), true, nil
	}
	if objectProtoMember(key) {
		return protoFunction(key, ctor), true, nil
	}
	return nil, false, nil
}

// Prototype members Goja has but the engine does not implement: readable as
// function values (typeof, feature checks); calling them is unsupported.
var (
	declinedArrayMembers  = map[string]bool{"copyWithin": true}
	declinedStringMembers = map[string]bool{"matchAll": true, "normalize": true}
	declinedNumberMembers = map[string]bool{"toExponential": true}
)

// ownProp reads a script-assigned own property from an optional bag.
func ownProp(props *object, key string) (any, bool) {
	if props == nil {
		return nil, false
	}
	return props.own(key)
}

// setOwnProp writes a script-assigned own property, creating the bag.
func setOwnProp(props **object, key string, v any) error {
	if key == "__proto__" {
		return errRuntimeUnsupported("__proto__ assignment")
	}
	if *props == nil {
		*props = newObject(1)
	}
	(*props).set(key, v)
	return nil
}

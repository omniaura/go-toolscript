package toolscript

import (
	"math"
	"strings"
	"unicode/utf8"
)

// collection is a Set or Map with SameValueZero keys and insertion order.
// Deleted entries leave tombstones so live iteration behaves like JS.
type collection struct {
	index map[any]int
	keys  []any
	vals  []any
	dead  []bool
	props *object // own properties assigned by the script (m.k = v)
	live  int
	isMap bool
}

type nanKey struct{}

func normKey(v any) any {
	if f, ok := v.(float64); ok {
		if math.IsNaN(f) {
			return nanKey{}
		}
		if f == 0 {
			return 0.0 // -0 and +0 are the same key
		}
	}
	return v
}

func newCollection(isMap bool) *collection {
	return &collection{index: map[any]int{}, isMap: isMap}
}

func (c *collection) set(r *rt, k, v any) error {
	nk := normKey(k)
	if i, ok := c.index[nk]; ok {
		c.vals[i] = v
		return nil
	}
	if len(c.keys) >= r.x.maxItems {
		return r.rangeError("Invalid collection size")
	}
	// Deleted entries stay as tombstones, so every new key grows the
	// collection for good.
	if err := r.charge(allocEntry); err != nil {
		return err
	}
	if f, ok := k.(float64); ok && f == 0 {
		k = 0.0 // stored keys are normalized: new Set([-0]) holds +0
	}
	c.index[nk] = len(c.keys)
	c.keys = append(c.keys, k)
	c.vals = append(c.vals, v)
	c.dead = append(c.dead, false)
	c.live++
	return nil
}

func (c *collection) get(k any) (any, bool) {
	if i, ok := c.index[normKey(k)]; ok {
		return c.vals[i], true
	}
	return Undefined, false
}

func (c *collection) delete(k any) bool {
	nk := normKey(k)
	i, ok := c.index[nk]
	if !ok {
		return false
	}
	delete(c.index, nk)
	c.dead[i] = true
	c.keys[i], c.vals[i] = Undefined, Undefined
	c.live--
	return true
}

func (c *collection) clear() {
	for i := range c.keys {
		c.dead[i] = true
		c.keys[i], c.vals[i] = Undefined, Undefined
	}
	c.index = map[any]int{}
	c.live = 0
}

// entry renders one element as iteration yields it.
func (c *collection) entry(i int, kind string) any {
	switch kind {
	case "keys":
		return c.keys[i]
	case "values":
		if c.isMap {
			return c.vals[i]
		}
		return c.keys[i]
	}
	if c.isMap {
		return &array{items: []any{c.keys[i], c.vals[i]}}
	}
	return &array{items: []any{c.keys[i], c.keys[i]}}
}

// iterator is a live, single-pass iterator over an array, Set or Map.
type iterator struct {
	next func() (any, bool)
	name string // "Array", "Set" or "Map", for String(it)
	// cost is the bytes each next() allocates for the value it yields (the
	// [key, value] array of an entries iterator); consumers charge it.
	cost int
}

// entryCost is what an entries iterator allocates per item.
const entryCost = allocHeader + 2*allocSlot

// iteratorValue charges for a new iterator and returns it.
func (r *rt) iteratorValue(it *iterator) (any, error) {
	if err := r.charge(allocHeader); err != nil {
		return nil, err
	}
	return it, nil
}

func (c *collection) iterator(kind string) *iterator {
	i := 0
	name := "Set"
	if c.isMap {
		name = "Map"
	}
	cost := 0
	if kind == "entries" {
		cost = entryCost
	}
	return &iterator{name: name, cost: cost, next: func() (any, bool) {
		for i < len(c.keys) {
			j := i
			i++
			if !c.dead[j] {
				return c.entry(j, kind), true
			}
		}
		return nil, false
	}}
}

func arrayIterator(a *array, kind string) *iterator {
	i := 0
	cost := 0
	if kind == "entries" {
		cost = entryCost
	}
	return &iterator{name: "Array", cost: cost, next: func() (any, bool) {
		if i >= len(a.items) {
			return nil, false
		}
		j := i
		i++
		switch kind {
		case "keys":
			return float64(j), true
		case "entries":
			return &array{items: []any{float64(j), unhole(a.items[j])}}, true
		}
		return unhole(a.items[j]), true
	}}
}

// defaultIterator returns what for-of and spread consume for Sets, Maps
// and iterators (arrays and strings are handled directly).
func defaultIterator(v any) (*iterator, bool) {
	switch t := v.(type) {
	case *collection:
		if t.isMap {
			return t.iterator("entries"), true
		}
		return t.iterator("values"), true
	case *iterator:
		return t, true
	}
	return nil, false
}

func (it *iterator) drain(r *rt) ([]any, error) {
	out := []any{}
	for {
		v, ok := it.next()
		if !ok {
			return out, nil
		}
		if err := r.charge(allocSlot + it.cost); err != nil {
			return nil, err
		}
		out = append(out, v)
		if len(out) > r.x.maxItems {
			return nil, r.rangeError("Invalid array length")
		}
	}
}

// newCollectionFrom implements new Set(iterable) / new Map(entries).
func (r *rt) newCollectionFrom(isMap bool, args []any) (any, error) {
	if err := r.charge(allocHeader); err != nil {
		return nil, err
	}
	c := newCollection(isMap)
	src := arg(args, 0)
	if isNullish(src) {
		return c, nil
	}
	items, err := r.iterableItems(src)
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		if !isMap {
			if err := c.set(r, item, Undefined); err != nil {
				return nil, err
			}
			continue
		}
		if !isObjectValue(item) {
			s, _ := r.toString(item)
			return nil, r.typeError("Value is not an object: " + s) // Goja's text
		}
		k, err := r.getProp(item, "0")
		if err != nil {
			return nil, err
		}
		v, err := r.getProp(item, "1")
		if err != nil {
			return nil, err
		}
		if err := c.set(r, k, v); err != nil {
			return nil, err
		}
	}
	return c, nil
}

func (r *rt) collectionMethod(c *collection, key string, args []any) (any, error) {
	switch key {
	case "has":
		_, ok := c.get(arg(args, 0))
		return ok, nil
	case "delete":
		return c.delete(arg(args, 0)), nil
	case "clear":
		c.clear()
		return Undefined, nil
	case "keys", "values", "entries":
		return r.iteratorValue(c.iterator(key))
	case "forEach":
		f := arg(args, 0)
		if err := r.requireCallable(f); err != nil {
			return nil, err
		}
		for i := 0; i < len(c.keys); i++ {
			if c.dead[i] {
				continue
			}
			v := c.vals[i]
			if !c.isMap {
				v = c.keys[i]
			}
			if _, err := r.call(f, []any{v, c.keys[i], c}); err != nil {
				return nil, err
			}
		}
		return Undefined, nil
	case "toString":
		return r.toString(c)
	}
	if c.isMap {
		switch key {
		case "get":
			v, _ := c.get(arg(args, 0))
			return v, nil
		case "set":
			return c, c.set(r, arg(args, 0), arg(args, 1))
		}
	} else if key == "add" {
		return c, c.set(r, arg(args, 0), Undefined)
	}
	return nil, r.typeError("Object has no member '" + key + "'")
}

func (r *rt) iteratorMethod(it *iterator, key string) (any, error) {
	switch key {
	case "next":
		if err := r.charge(allocHeader + 2*allocProp + it.cost); err != nil {
			return nil, err
		}
		o := newObject(2)
		v, ok := it.next()
		if !ok {
			o.set("value", Undefined)
			o.set("done", true)
			return o, nil
		}
		o.set("value", v)
		o.set("done", false)
		return o, nil
	case "toString":
		return "[object " + it.name + " Iterator]", nil
	}
	return nil, r.typeError("Object has no member '" + key + "'")
}

func collectionMember(c *collection, key string) (any, bool) {
	if key == "size" {
		return float64(c.live), true
	}
	owner := "Set"
	if c.isMap {
		owner = "Map"
	}
	if f := protoFn(owner, key); f != nil {
		return f, true
	}
	return nil, false
}

// ---- Array(n), bind/call/apply ----

func (r *rt) newArrayFromArgs(args []any) (any, error) {
	if len(args) == 1 {
		if f, ok := args[0].(float64); ok {
			if f < 0 || f != math.Trunc(f) || f > math.MaxUint32 {
				return nil, r.rangeError("Invalid array length")
			}
			if int(f) > maxHoles {
				return nil, errRuntimeUnsupported("large sparse Array(n)")
			}
			if err := r.chargeArray(int(f)); err != nil {
				return nil, err
			}
			items := make([]any, int(f))
			for i := range items {
				items[i] = hole
			}
			return &array{items: items}, nil
		}
	}
	return r.arrayValue(append([]any{}, args...))
}

func (r *rt) functionMethod(f *function, key string, args []any) (any, bool, error) {
	switch key {
	case "call":
		var rest []any
		if len(args) > 1 {
			rest = args[1:]
		}
		v, err := r.callThis(f, arg(args, 0), rest)
		return v, true, err
	case "apply":
		list := arg(args, 1)
		var items []any
		if !isNullish(list) {
			a, ok := list.(*array)
			if !ok {
				return nil, true, r.typeError("CreateListFromArrayLike called on non-object")
			}
			items = denseItems(a.items)
		}
		v, err := r.callThis(f, arg(args, 0), items)
		return v, true, err
	case "bind":
		if f.binds >= maxBindNesting {
			return nil, true, r.rangeError("Maximum call stack size exceeded")
		}
		if err := r.charge(2*allocHeader + len(args)*allocSlot); err != nil {
			return nil, true, err
		}
		var bound []any
		if len(args) > 1 {
			bound = append(bound, args[1:]...)
		}
		length := 0
		if f.code != nil {
			length = max(0, f.code.length-len(bound))
		} else {
			length = max(0, f.length-len(bound))
		}
		boundThis := arg(args, 0)
		// The name is f's name with one more "bound " (see displayName).
		return &function{name: f.name, binds: f.binds + 1, length: length, native: func(r *rt, _ any, more []any) (any, error) {
			return r.callThis(f, boundThis, append(append([]any{}, bound...), more...))
		}}, true, nil
	}
	return nil, false, nil
}

// ---- URI functions (ECMAScript Encode/Decode) ----

const uriUnreservedMarks = "-_.!~*'()"
const uriReserved = ";/?:@&=+$,#"

func uriEncode(s string, keep string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		c := s[i]
		if r < utf8.RuneSelf && (('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') || ('0' <= c && c <= '9') || strings.IndexByte(uriUnreservedMarks, c) >= 0 || strings.IndexByte(keep, c) >= 0) {
			b.WriteByte(c)
		} else {
			for j := 0; j < size; j++ {
				b.WriteByte('%')
				b.WriteByte("0123456789ABCDEF"[s[i+j]>>4])
				b.WriteByte("0123456789ABCDEF"[s[i+j]&15])
			}
		}
		i += size
	}
	return b.String()
}

func (r *rt) uriDecode(s string, preserve string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '%' {
			b.WriteByte(s[i])
			continue
		}
		start := i
		var buf []byte
		for i < len(s) && s[i] == '%' {
			if i+2 >= len(s) || !isHex(s[i+1]) || !isHex(s[i+2]) {
				return "", r.throw("URIError", "Malformed URI")
			}
			buf = append(buf, unhex(s[i+1])<<4|unhex(s[i+2]))
			i += 3
			if utf8.FullRune(buf) {
				break
			}
		}
		i--
		if len(buf) == 1 && buf[0] < utf8.RuneSelf {
			if strings.IndexByte(preserve, buf[0]) >= 0 {
				b.WriteString(s[start : start+3])
			} else {
				b.WriteByte(buf[0])
			}
			continue
		}
		if !utf8.Valid(buf) {
			return "", r.throw("URIError", "Malformed URI")
		}
		b.Write(buf)
	}
	return b.String(), nil
}

func isHex(c byte) bool {
	return ('0' <= c && c <= '9') || ('a' <= c && c <= 'f') || ('A' <= c && c <= 'F')
}

func unhex(c byte) byte {
	switch {
	case '0' <= c && c <= '9':
		return c - '0'
	case 'a' <= c && c <= 'f':
		return c - 'a' + 10
	}
	return c - 'A' + 10
}

package toolscript

// The allocation budget (ExecuteOptions.MaxAllocBytes) bounds the memory one
// run can make live. MaxStringBytes and MaxArrayLength bound each value, but
// not how many a run builds: a few hundred steps of
// `keep.push(big.slice())` over a million-element array hold gigabytes.
//
// Every operation that makes a value the script can reach charges the bytes
// that value occupies before (or as) it makes it: string bytes, array
// elements, object properties, Map/Set entries, closures and the frames they
// capture, Dates, RegExps, iterators, Intl objects, JSON.parse output and
// converted tool and host results. Charges are never credited back, even
// when the value becomes garbage, so the total bounds what the run can hold
// live at any moment whatever it retains. Growing an array in place charges
// only past the longest that array has been charged for (array.hw), so a
// queue that pushes and shifts, or a stack that pushes and pops, pays for its
// peak length once rather than on every operation.
//
// Costs approximate the Go heap a value occupies on a 64-bit platform:
// allocSlot per interface value (an array element, a captured variable, a
// boxed string header), allocProp per object property and per scope header,
// allocEntry per Map or Set entry, allocHeader per object, array, function,
// Map/Set, Date, RegExp, iterator or Intl instance, and a string's bytes plus
// allocSlot. Slice
// growth slack and allocator rounding are not charged, so the heap a run
// holds can exceed its charge by a small constant factor; size the budget
// accordingly.
//
// Not charged, because none of it is reachable by the script once the
// operation returns, and each is bounded per operation by MaxStringBytes,
// MaxArrayLength or the export limits: scratch buffers inside builtins
// (UTF-16 code-unit copies, regexp subjects and match indexes, sort and
// split scratch, strings.Builder slack), the argument slices of calls, call
// frames (a frame outlives its call only through a closure, which is charged
// for the frame it captures), tool and host call arguments exported for
// Dispatch, console lines handed to the host, and boxed numbers (8 bytes,
// held in a slot that is charged).
const (
	allocSlot   = 16
	allocProp   = 2 * allocSlot
	allocEntry  = 4 * allocSlot // key, value, tombstone flag and index entry
	allocHeader = 64
	// allocRegexpUnit approximates compiled RegExp program bytes per pattern
	// byte (one instruction per literal character).
	allocRegexpUnit = 64
	// allocBatchMax is how many bytes one goroutine charges before it
	// publishes them to the shared total (like steps; see tick).
	allocBatchMax = 64 << 10
)

// errAllocBudget is the RangeError message for an exhausted budget.
const errAllocBudget = "allocation budget exceeded"

// charge accounts n bytes against MaxAllocBytes. Callers charge before they
// make the value (or before it becomes visible to the script). Over budget it
// throws a catchable RangeError, and because charges are never returned the
// budget stays spent: every later charge in the run throws too.
func (r *rt) charge(n int) error {
	x := r.x
	if x.maxAlloc == 0 || n <= 0 {
		return nil
	}
	r.allocPending += n
	if r.allocPending < x.allocBatch && !x.allocSpent.Load() {
		return nil
	}
	return r.flushAlloc()
}

// flushAlloc publishes this goroutine's pending charges.
func (r *rt) flushAlloc() error {
	x := r.x
	total := x.alloc.Add(int64(r.allocPending))
	r.allocPending = 0
	if x.maxAlloc > 0 && total > x.maxAlloc {
		x.allocSpent.Store(true)
		return r.rangeError(errAllocBudget)
	}
	return nil
}

// chargeString charges for a new string value.
func (r *rt) chargeString(s string) error { return r.charge(len(s) + allocSlot) }

// stringValue charges for a new string and returns it as a value.
func (r *rt) stringValue(s string) (any, error) {
	if err := r.chargeString(s); err != nil {
		return nil, err
	}
	return s, nil
}

// chargeArray charges for a new array of n elements.
func (r *rt) chargeArray(n int) error { return r.charge(allocHeader + n*allocSlot) }

// arrayValue charges for a new array holding items and returns it. Every
// array the runtime builds for the script goes through here or newArray.
func (r *rt) arrayValue(items []any) (any, error) {
	if err := r.chargeArray(len(items)); err != nil {
		return nil, err
	}
	return &array{items: items}, nil
}

// stringsValue builds an array of new strings, charging for each.
func (r *rt) stringsValue(values []string) (any, error) {
	n := allocHeader
	for _, s := range values {
		n += 2*allocSlot + len(s)
	}
	if err := r.charge(n); err != nil {
		return nil, err
	}
	items := make([]any, len(values))
	for i, s := range values {
		items[i] = s
	}
	return &array{items: items}, nil
}

// growArray charges for a growing to n elements in place. Only growth past
// the longest a has been charged for is charged: its elements up to then were
// paid for when it was made or last grew.
func (r *rt) growArray(a *array, n int) error {
	if a.hw < len(a.items) {
		a.hw = len(a.items)
	}
	if n <= a.hw {
		return nil
	}
	if err := r.charge((n - a.hw) * allocSlot); err != nil {
		return err
	}
	a.hw = n
	return nil
}

// newObj returns an empty object charged for its header and n properties.
func (r *rt) newObj(n int) (*object, error) {
	if err := r.charge(allocHeader + n*allocProp); err != nil {
		return nil, err
	}
	return newObject(n), nil
}

// put sets o[key] = v, charging for the property when it is new.
func (r *rt) put(o *object, key string, v any) error {
	n := len(o.keys)
	o.set(key, v)
	if len(o.keys) > n {
		return r.charge(allocProp + len(key))
	}
	return nil
}

// closure makes a script function over scope s, charging for the function
// and for the scopes it keeps alive (see captureScope).
func (r *rt) closure(code *funcCode, s *scope, name string) (*function, error) {
	if err := r.charge(allocHeader); err != nil {
		return nil, err
	}
	if err := r.captureScope(s); err != nil {
		return nil, err
	}
	return &function{code: code, scope: s, name: name}, nil
}

// captureScope charges for s and its ancestors the first time anything can
// keep them alive past their call. Frames are not charged when they are
// made: one that no closure captures dies with its call. Each scope is
// charged once however many closures share it, and a charged scope's
// ancestors are all charged, so the walk stops at the first one.
//
// A parallel batch captures its enclosing scope before its workers start,
// so workers only read the flag on scopes they share and write it only on
// scopes of their own.
func (r *rt) captureScope(s *scope) error {
	n := 0
	for sc := s; sc != nil && !sc.charged; sc = sc.parent {
		n += allocProp + len(sc.vars)*allocSlot
	}
	if n == 0 {
		return nil
	}
	if err := r.charge(n); err != nil {
		return err
	}
	for sc := s; sc != nil && !sc.charged; sc = sc.parent {
		sc.charged = true
	}
	return nil
}

// chargeFresh charges for v, a value an operation has just made in full and
// shares with nothing else (an Intl or Date method result): strings, arrays
// and objects with their elements and property values, two levels deep.
func (r *rt) chargeFresh(v any) error { return r.charge(freshSize(v, 2)) }

func freshSize(v any, depth int) int {
	switch t := v.(type) {
	case string:
		return len(t) + allocSlot
	case *array:
		n := allocHeader + len(t.items)*allocSlot
		if depth > 0 {
			for _, e := range t.items {
				n += freshSize(e, depth-1)
			}
		}
		return n
	case *object:
		n := allocHeader
		for i, k := range t.keys {
			n += allocProp + len(k)
			if depth > 0 {
				n += freshSize(t.vals[i], depth-1)
			}
		}
		return n
	case *dateValue, *regexpValue, *iterator, *intlObject, *collection, *function:
		return allocHeader
	}
	return 0
}

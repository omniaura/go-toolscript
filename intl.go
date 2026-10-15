package toolscript

import (
	"math"
	"sort"
	"strings"
	"sync"
)

// ECMA-402 (Intl) for the English locale data Goja does not have at all.
//
// The engine ships one locale's data, CLDR "en" (US English), matching ICU
// as V8 uses it (see testdata/intl for the golden outputs generated with
// Node). Locale negotiation is the specification's: every request resolves
// against the available locales {"en", "en-US"}, so "en-GB" resolves to
// "en" and "de-DE" to the default locale "en-US", and resolvedOptions()
// reports the locale actually used. Calendars other than Gregorian and
// numbering systems other than Latin digits are declined rather than
// formatted wrongly.

// intlKind describes one Intl service constructor (Intl.DateTimeFormat, ...).
type intlKind struct {
	name        string
	construct   func(r *rt, locales, options any) (any, error) // returns the service state
	requiresNew bool                                           // calling without new throws
	boundGetter string                                         // prototype getter returning a bound method ("format")
	methods     map[string]intlMethod
}

type intlMethod struct {
	length int
	call   func(r *rt, o *intlObject, args []any) (any, error)
}

// intlObject is an instance of an Intl service.
type intlObject struct {
	kind  *intlKind
	state any
	props *object   // expando properties assigned by the script
	bound *function // cached boundGetter function
}

var intlKinds = map[string]*intlKind{}

func registerIntlKind(k *intlKind) { intlKinds[k.name] = k }

func (r *rt) newIntl(name string, args []any) (any, error) {
	k := intlKinds[name]
	if k == nil {
		return nil, errRuntimeUnsupported("Intl." + name)
	}
	// The resolved state holds a handful of option strings and tables.
	if err := r.charge(allocIntl); err != nil {
		return nil, err
	}
	state, err := k.construct(r, arg(args, 0), arg(args, 1))
	if err != nil {
		return nil, err
	}
	return &intlObject{kind: k, state: state}, nil
}

// allocIntl approximates an Intl instance with its resolved state.
const allocIntl = 8 * allocHeader

// callIntl calls an Intl method, charging for the strings, parts arrays or
// option objects it returns (each is built afresh).
func (r *rt) callIntl(m intlMethod, o *intlObject, args []any) (any, error) {
	v, err := m.call(r, o, args)
	if err != nil {
		return nil, err
	}
	return v, r.chargeFresh(v)
}

// intlMember reads a property of an Intl instance.
func (r *rt) intlMember(o *intlObject, key string) (any, bool) {
	if o.props != nil {
		if v, ok := o.props.get(key); ok {
			return v, true
		}
	}
	if key == o.kind.boundGetter && key != "" {
		if o.bound == nil {
			m := o.kind.methods[key]
			o.bound = &function{length: m.length, native: func(r *rt, _ any, args []any) (any, error) {
				return r.callIntl(m, o, args)
			}}
		}
		return o.bound, true
	}
	if f := intlProtoFn(o.kind, key); f != nil {
		return f, true
	}
	return nil, false
}

func (r *rt) intlMethodCall(o *intlObject, key string, args []any) (any, error) {
	if o.props != nil {
		if f, ok := o.props.get(key); ok {
			return r.callThis(f, o, args)
		}
	}
	if m, ok := o.kind.methods[key]; ok {
		return r.callIntl(m, o, args)
	}
	return r.objectMethod(o, key, args)
}

var (
	intlProtoMu  sync.Mutex
	intlProtoFns = map[string]*function{}
)

// intlProtoFn is the shared prototype method (identical across instances).
func intlProtoFn(k *intlKind, key string) *function {
	m, ok := k.methods[key]
	if !ok || key == k.boundGetter {
		return nil
	}
	id := k.name + "." + key
	intlProtoMu.Lock()
	defer intlProtoMu.Unlock()
	if f := intlProtoFns[id]; f != nil {
		return f
	}
	f := &function{name: key, length: m.length, native: func(r *rt, this any, args []any) (any, error) {
		o, ok := this.(*intlObject)
		if !ok || o.kind != k {
			return nil, r.typeError("Method Intl." + k.name + ".prototype." + key + " called on incompatible receiver " + receiverText(r, this))
		}
		return r.callIntl(m, o, args)
	}}
	intlProtoFns[id] = f
	return f
}

func receiverText(r *rt, v any) string {
	switch v.(type) {
	case *object, *hostObject:
		return "#<Object>"
	case *array:
		return "[object Array]"
	}
	s, _ := r.toString(v)
	return s
}

// Intl namespace statics: Intl.getCanonicalLocales, Intl.supportedValuesOf,
// Intl.X(...) without new, and Intl.X.supportedLocalesOf.
func init() {
	staticFunctions["Intl.getCanonicalLocales"] = func(r *rt, args []any) (any, error) {
		tags, err := r.canonicalizeLocaleList(arg(args, 0))
		if err != nil {
			return nil, err
		}
		return r.stringsValue(tags)
	}
	staticFunctions["Intl.supportedValuesOf"] = func(r *rt, args []any) (any, error) {
		key, err := r.toString(arg(args, 0))
		if err != nil {
			return nil, err
		}
		values, ok := intlSupportedValues(key)
		if !ok {
			return nil, r.rangeError("Invalid key : " + key)
		}
		return r.stringsValue(values)
	}
}

// intlStatic implements Intl.<name> and Intl.<name>.<member> calls.
func intlStatic(full string) (func(r *rt, args []any) (any, error), bool) {
	parts := strings.Split(full, ".")
	if len(parts) < 2 || parts[0] != "Intl" {
		return nil, false
	}
	k := intlKinds[parts[1]]
	if k == nil {
		return nil, false
	}
	switch {
	case len(parts) == 2:
		return func(r *rt, args []any) (any, error) {
			if k.requiresNew {
				return nil, r.typeError("Constructor Intl." + k.name + " requires 'new'")
			}
			return r.newIntl(k.name, args)
		}, true
	case len(parts) == 3 && parts[2] == "supportedLocalesOf":
		return func(r *rt, args []any) (any, error) {
			v, err := r.supportedLocalesOf(k.name, arg(args, 0), arg(args, 1))
			if err != nil {
				return nil, err
			}
			return v, r.chargeFresh(v)
		}, true
	}
	return nil, false
}

func stringArray(values []string) *array {
	items := make([]any, len(values))
	for i, v := range values {
		items[i] = v
	}
	return &array{items: items}
}

// intlParts builds the [{type, value}, ...] array of formatToParts.
func intlParts(parts []intlPart) *array {
	items := make([]any, len(parts))
	for i, p := range parts {
		o := newObject(3)
		o.set("type", p.typ)
		o.set("value", p.value)
		if p.source != "" {
			o.set("source", p.source)
		}
		items[i] = o
	}
	return &array{items: items}
}

type intlPart struct{ typ, value, source string }

func joinParts(parts []intlPart) string {
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.value)
	}
	return b.String()
}

// --- options (ECMA-402 GetOption and friends, with V8's messages) ---

// intlOptions is a CoerceOptionsToObject result: nil when options were
// undefined (every read yields undefined).
type intlOptions struct {
	v       any
	service string
}

func (r *rt) coerceOptions(v any, service string) (*intlOptions, error) {
	switch v.(type) {
	case undefined:
		return &intlOptions{service: service}, nil
	case nil:
		return nil, r.typeError(service + " called on null or undefined")
	}
	return &intlOptions{v: v, service: service}, nil
}

func (r *rt) optionValue(o *intlOptions, prop string) (any, error) {
	if o == nil || o.v == nil {
		return Undefined, nil
	}
	return r.getProp(o.v, prop)
}

// stringOption returns the option's string value, or "" when undefined.
func (r *rt) stringOption(o *intlOptions, prop string, allowed ...string) (string, error) {
	s, _, err := r.stringOpt(o, prop, allowed...)
	return s, err
}

// stringOpt is GetOption(..., "string", allowed, undefined) reporting presence.
func (r *rt) stringOpt(o *intlOptions, prop string, allowed ...string) (string, bool, error) {
	v, err := r.optionValue(o, prop)
	if err != nil || isUndefined(v) {
		return "", false, err
	}
	s, err := r.toString(v)
	if err != nil {
		return "", false, err
	}
	if len(allowed) > 0 && !contains(allowed, s) {
		return "", false, r.rangeError("Value " + s + " out of range for " + o.service + " options property " + prop)
	}
	return s, true, nil
}

// boolOption returns (value, present).
func (r *rt) boolOption(o *intlOptions, prop string) (bool, bool, error) {
	v, err := r.optionValue(o, prop)
	if err != nil || isUndefined(v) {
		return false, false, err
	}
	return toBoolean(v), true, nil
}

// numberOption implements GetNumberOption: undefined gives fallback.
func (r *rt) numberOption(o *intlOptions, prop string, lo, hi, fallback int) (int, error) {
	v, err := r.optionValue(o, prop)
	if err != nil || isUndefined(v) {
		return fallback, err
	}
	return r.defaultNumberOption(v, prop, lo, hi)
}

func (r *rt) defaultNumberOption(v any, prop string, lo, hi int) (int, error) {
	f, err := r.toNumber(v)
	if err != nil {
		return 0, err
	}
	if math.IsNaN(f) || f < float64(lo) || f > float64(hi) {
		return 0, r.rangeError(prop + " value is out of range.")
	}
	return int(math.Floor(f)), nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// --- locales ---

// intlDefaultLocale is DefaultLocale(); the data of "en" and "en-US" is the
// same CLDR US English.
const intlDefaultLocale = "en-US"

var intlAvailableLocales = []string{"en", "en-US"}

// canonicalizeLocaleList implements CanonicalizeLocaleList.
func (r *rt) canonicalizeLocaleList(v any) ([]string, error) {
	var raw []any
	switch t := v.(type) {
	case undefined:
		return nil, nil
	case nil:
		return nil, r.typeError("Cannot convert undefined or null to object")
	case string:
		raw = []any{t}
	case *array:
		for _, item := range t.items {
			if !isHole(item) {
				raw = append(raw, item)
			}
		}
	case *object, *hostObject:
		n, err := r.getProp(t, "length")
		if err != nil {
			return nil, err
		}
		f, err := r.toNumber(n)
		if err != nil {
			return nil, err
		}
		for i := 0; i < int(math.Min(f, 1<<16)); i++ {
			key := numberToString(float64(i))
			if ok, err := r.hasProperty(t, key); err != nil {
				return nil, err
			} else if !ok {
				continue
			}
			item, err := r.getProp(t, key)
			if err != nil {
				return nil, err
			}
			raw = append(raw, item)
		}
	default:
		return nil, nil // ToObject of a number/boolean has no length
	}
	var out []string
	for _, item := range raw {
		s, ok := item.(string)
		if !ok {
			if !isObjectValue(item) {
				return nil, r.typeError("Language ID should be string or object.")
			}
			var err error
			if s, err = r.toString(item); err != nil {
				return nil, err
			}
		}
		tag, ok := canonicalizeLanguageTag(s)
		if !ok {
			return nil, r.rangeError("Invalid language tag: " + s)
		}
		if !contains(out, tag) {
			out = append(out, tag)
		}
	}
	return out, nil
}

// languageTag is a parsed, canonicalized unicode_locale_id.
type languageTag struct {
	base string            // language[-script][-region][-variants]
	ext  map[string]string // -u- keywords
}

func (t languageTag) String() string {
	if len(t.ext) == 0 {
		return t.base
	}
	keys := make([]string, 0, len(t.ext))
	for k := range t.ext {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(t.base)
	b.WriteString("-u")
	for _, k := range keys {
		b.WriteString("-" + k)
		if v := t.ext[k]; v != "" && v != "true" {
			b.WriteString("-" + v)
		}
	}
	return b.String()
}

// canonicalizeLanguageTag validates a BCP 47 language tag (the
// unicode_locale_id subset ECMA-402 accepts) and canonicalizes its case.
// Other extensions and private use are dropped from the canonical form's
// matching but kept in the string.
func canonicalizeLanguageTag(s string) (string, bool) {
	t, rest, ok := parseLanguageTag(s)
	if !ok {
		return "", false
	}
	out := t.String()
	if rest != "" {
		out += "-" + rest
	}
	return out, true
}

var languageAliases = map[string]string{"iw": "he", "in": "id", "ji": "yi", "jw": "jv", "mo": "ro", "tl": "fil", "no": "nb"}

func parseLanguageTag(s string) (languageTag, string, bool) {
	if s == "" {
		return languageTag{}, "", false
	}
	subtags := strings.Split(strings.ToLower(s), "-")
	for _, st := range subtags {
		if st == "" || len(st) > 8 || !isAlnum(st) {
			return languageTag{}, "", false
		}
	}
	i := 0
	lang := subtags[i]
	if !isAlpha(lang) || len(lang) < 2 || len(lang) == 4 || len(lang) > 8 {
		return languageTag{}, "", false
	}
	if a, ok := languageAliases[lang]; ok {
		lang = a
	}
	parts := []string{lang}
	i++
	if i < len(subtags) && len(subtags[i]) == 4 && isAlpha(subtags[i]) {
		parts = append(parts, strings.ToUpper(subtags[i][:1])+subtags[i][1:])
		i++
	}
	if i < len(subtags) && (len(subtags[i]) == 2 && isAlpha(subtags[i]) || len(subtags[i]) == 3 && isDigits(subtags[i])) {
		parts = append(parts, strings.ToUpper(subtags[i]))
		i++
	}
	var variants []string
	for i < len(subtags) && (len(subtags[i]) >= 5 || len(subtags[i]) == 4 && subtags[i][0] >= '0' && subtags[i][0] <= '9') {
		if contains(variants, subtags[i]) {
			return languageTag{}, "", false
		}
		variants = append(variants, subtags[i])
		i++
	}
	sort.Strings(variants)
	parts = append(parts, variants...)
	t := languageTag{base: strings.Join(parts, "-")}
	var rest []string
	seen := map[string]bool{}
	for i < len(subtags) {
		singleton := subtags[i]
		if len(singleton) != 1 {
			return languageTag{}, "", false
		}
		if singleton == "x" {
			if i+1 >= len(subtags) {
				return languageTag{}, "", false
			}
			rest = append(rest, subtags[i:]...)
			break
		}
		if seen[singleton] {
			return languageTag{}, "", false
		}
		seen[singleton] = true
		i++
		start := i
		for i < len(subtags) && len(subtags[i]) > 1 {
			i++
		}
		if i == start {
			return languageTag{}, "", false
		}
		if singleton != "u" {
			rest = append(rest, singleton)
			rest = append(rest, subtags[start:i]...)
			continue
		}
		// -u-: attributes then key[-type...] keywords.
		j := start
		for j < i && len(subtags[j]) >= 3 {
			rest = append(rest, "u", subtags[j]) // attributes are kept verbatim
			j++
		}
		for j < i {
			key := subtags[j]
			if len(key) != 2 || !(isAlpha(key[1:]) && isAlnum(key[:1])) {
				return languageTag{}, "", false
			}
			j++
			var types []string
			for j < i && len(subtags[j]) >= 3 {
				types = append(types, subtags[j])
				j++
			}
			if t.ext == nil {
				t.ext = map[string]string{}
			}
			if _, dup := t.ext[key]; !dup {
				t.ext[key] = strings.Join(types, "-")
			}
		}
	}
	return t, strings.Join(rest, "-"), true
}

func isAlpha(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i] | 0x20; c < 'a' || c > 'z' {
			return false
		}
	}
	return s != ""
}

func isDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}

func isAlnum(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c|0x20 >= 'a' && c|0x20 <= 'z') {
			return false
		}
	}
	return true
}

// resolvedLocale is ResolveLocale's result.
type resolvedLocale struct {
	locale    string            // the resolved locale string (with kept extensions)
	extension map[string]string // relevant -u- keywords that were applied
}

// lookupLocale finds the best available locale for one requested tag
// (BestAvailableLocale over its truncations).
func lookupLocale(base string) (string, bool) {
	candidate := base
	for {
		if contains(intlAvailableLocales, candidate) {
			return candidate, true
		}
		i := strings.LastIndexByte(candidate, '-')
		if i < 0 {
			return "", false
		}
		if i >= 2 && candidate[i-2] == '-' {
			i -= 2
		}
		candidate = candidate[:i]
	}
}

// resolveLocale implements ResolveLocale with the lookup matcher over the
// available locales. relevant lists the -u- keys the service honors with
// their supported values; unsupported values are ignored as the spec says.
func (r *rt) resolveLocale(locales any, relevant map[string][]string) (resolvedLocale, error) {
	requested, err := r.canonicalizeLocaleList(locales)
	if err != nil {
		return resolvedLocale{}, err
	}
	return r.resolveLocaleList(requested, relevant), nil
}

// resolveLocaleList is ResolveLocale over a canonicalized locale list.
func (r *rt) resolveLocaleList(requested []string, relevant map[string][]string) resolvedLocale {
	for _, tag := range requested {
		t, _, _ := parseLanguageTag(tag)
		found, ok := lookupLocale(t.base)
		if !ok {
			continue
		}
		res := resolvedLocale{extension: map[string]string{}}
		kept := languageTag{base: found}
		for key, values := range relevant {
			v, present := t.ext[key]
			if !present {
				continue
			}
			if v == "" {
				v = "true"
			}
			if contains(values, v) {
				res.extension[key] = v
				if kept.ext == nil {
					kept.ext = map[string]string{}
				}
				kept.ext[key] = v
			}
		}
		res.locale = kept.String()
		return res
	}
	return resolvedLocale{locale: intlDefaultLocale, extension: map[string]string{}}
}

// supportedLocalesOf implements Intl.X.supportedLocalesOf.
func (r *rt) supportedLocalesOf(service string, locales, options any) (any, error) {
	requested, err := r.canonicalizeLocaleList(locales)
	if err != nil {
		return nil, err
	}
	opts, err := r.coerceOptions(options, "Intl."+service+".supportedLocalesOf")
	if err != nil {
		return nil, err
	}
	if _, err := r.stringOption(opts, "localeMatcher", "lookup", "best fit"); err != nil {
		return nil, err
	}
	var out []string
	for _, tag := range requested {
		t, _, _ := parseLanguageTag(tag)
		if _, ok := lookupLocale(t.base); ok {
			out = append(out, tag)
		}
	}
	return stringArray(out), nil
}

// localeMatcherOption validates the localeMatcher option every service reads.
func (r *rt) localeMatcherOption(o *intlOptions) error {
	_, err := r.stringOption(o, "localeMatcher", "lookup", "best fit")
	return err
}

// isWellFormedUnicodeType checks a -u- type value (calendar, numberingSystem).
func isWellFormedUnicodeType(s string) bool {
	for _, part := range strings.Split(s, "-") {
		if len(part) < 3 || len(part) > 8 || !isAlnum(part) {
			return false
		}
	}
	return s != ""
}

var intlSupportedValueLists = map[string][]string{
	"calendar":        {"gregory", "iso8601"},
	"collation":       {"default"},
	"numberingSystem": {"latn"},
}

func intlSupportedValues(key string) ([]string, bool) {
	switch key {
	case "timeZone":
		return tzCanonicalIDs(), true
	case "currency":
		return intlCurrencyCodes(), true
	case "unit":
		return intlSanctionedUnits(), true
	}
	v, ok := intlSupportedValueLists[key]
	return v, ok
}

// hooks filled in by the service implementations.
var (
	intlCurrencyCodes   = func() []string { return nil }
	intlSanctionedUnits = func() []string { return nil }
)

// intlArgs reports whether a toLocale*String call passes locales or options.
func intlArgs(args []any) bool {
	return !isUndefined(arg(args, 0)) || !isUndefined(arg(args, 1))
}

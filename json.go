package toolscript

import (
	"encoding/json"
	"errors"
	"io"
	"math"
	"reflect"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/dop251/goja/ast"
	"github.com/dop251/goja/parser"
)

const jsWhitespace = parser.WhitespaceChars

type stringifier struct {
	r        *rt
	replacer *function
	props    []string
	stack    []any
	gap      string
	indent   string
	buf      strings.Builder
}

// stringify implements JSON.stringify following Goja's implementation,
// including its handling of indentation after empty containers.
func (r *rt) stringify(v, replacer, space any) (any, error) {
	ctx := &stringifier{r: r}
	switch rp := replacer.(type) {
	case *array:
		seen := map[string]bool{}
		ctx.props = []string{}
		for _, item := range rp.items {
			var name string
			switch item := item.(type) {
			case float64, string:
				name, _ = r.toString(item)
			default:
				continue
			}
			if !seen[name] {
				seen[name] = true
				ctx.props = append(ctx.props, name)
			}
		}
	case *function:
		ctx.replacer = rp
	}
	switch sp := space.(type) {
	case float64:
		n := int64(sp)
		if n > 0 {
			ctx.gap = strings.Repeat(" ", int(min(n, 10)))
		}
	case string:
		if len(sp) > 10 {
			ctx.gap = sp[:10]
		} else {
			ctx.gap = sp
		}
	}
	holder := newObject(1)
	holder.set("", v)
	ok, err := ctx.str("", holder)
	if err != nil {
		return nil, err
	}
	if !ok {
		return Undefined, nil
	}
	return r.stringValue(ctx.buf.String())
}

func (ctx *stringifier) str(key string, holder any) (bool, error) {
	r := ctx.r
	value, err := r.getProp(holder, key)
	if err != nil {
		return false, err
	}
	if d, ok := value.(*dateValue); ok {
		if value, err = r.dateToJSON(d); err != nil {
			return false, err
		}
	}
	if o, ok := value.(*object); ok {
		tj, _ := o.own("toJSON")
		if f, ok := tj.(*function); ok {
			if value, err = r.call(f, []any{key}); err != nil {
				return false, err
			}
		}
	}
	if ctx.replacer != nil {
		if value, err = r.call(ctx.replacer, []any{key, value}); err != nil {
			return false, err
		}
	}
	if ctx.buf.Len() > r.x.maxString {
		return false, r.rangeError("Invalid string length")
	}
	switch v := value.(type) {
	case bool:
		if v {
			ctx.buf.WriteString("true")
		} else {
			ctx.buf.WriteString("false")
		}
	case string:
		quoteJSON(&ctx.buf, v)
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			ctx.buf.WriteString("null")
		} else {
			ctx.buf.WriteString(numberToString(v))
		}
	case nil:
		ctx.buf.WriteString("null")
	case *object, *array, *hostObject, *regexpValue, *collection, *iterator, *dateValue, *intlObject:
		for _, o := range ctx.stack {
			if o == value {
				return false, r.typeError("Converting circular structure to JSON")
			}
		}
		if len(ctx.stack) > 10000 {
			return false, r.rangeError("Maximum call stack size exceeded")
		}
		ctx.stack = append(ctx.stack, value)
		defer func() { ctx.stack = ctx.stack[:len(ctx.stack)-1] }()
		if a, ok := v.(*array); ok {
			return true, ctx.ja(a)
		}
		return true, ctx.jo(value)
	default:
		return false, nil
	}
	return true, nil
}

func (ctx *stringifier) ja(a *array) error {
	var stepback string
	if ctx.gap != "" {
		stepback = ctx.indent
		ctx.indent += ctx.gap
	}
	if len(a.items) == 0 {
		ctx.buf.WriteString("[]")
		return nil // Goja leaves the indent level raised here.
	}
	ctx.buf.WriteByte('[')
	separator := ","
	if ctx.gap != "" {
		ctx.buf.WriteByte('\n')
		ctx.buf.WriteString(ctx.indent)
		separator = ",\n" + ctx.indent
	}
	for i := range a.items {
		ok, err := ctx.str(strconv.Itoa(i), a)
		if err != nil {
			return err
		}
		if !ok {
			ctx.buf.WriteString("null")
		}
		if i < len(a.items)-1 {
			ctx.buf.WriteString(separator)
		}
	}
	if ctx.gap != "" {
		ctx.buf.WriteByte('\n')
		ctx.buf.WriteString(stepback)
		ctx.indent = stepback
	}
	ctx.buf.WriteByte(']')
	return nil
}

func (ctx *stringifier) jo(o any) error {
	var stepback string
	if ctx.gap != "" {
		stepback = ctx.indent
		ctx.indent += ctx.gap
	}
	ctx.buf.WriteByte('{')
	mark := ctx.buf.Len()
	separator := ","
	if ctx.gap != "" {
		ctx.buf.WriteByte('\n')
		ctx.buf.WriteString(ctx.indent)
		separator = ",\n" + ctx.indent
	}
	props := ctx.props
	if props == nil {
		props = ownEnumerableKeys(o)
	}
	empty := true
	for _, name := range props {
		off := ctx.buf.Len()
		if !empty {
			ctx.buf.WriteString(separator)
		}
		quoteJSON(&ctx.buf, name)
		if ctx.gap != "" {
			ctx.buf.WriteString(": ")
		} else {
			ctx.buf.WriteByte(':')
		}
		ok, err := ctx.str(name, o)
		if err != nil {
			return err
		}
		if ok {
			empty = false
		} else {
			truncate(&ctx.buf, off)
		}
	}
	if empty {
		truncate(&ctx.buf, mark)
	} else if ctx.gap != "" {
		ctx.buf.WriteByte('\n')
		ctx.buf.WriteString(stepback)
		ctx.indent = stepback
	}
	ctx.buf.WriteByte('}')
	return nil
}

func truncate(b *strings.Builder, n int) {
	s := b.String()[:n]
	b.Reset()
	b.WriteString(s)
}

const hexDigits = "0123456789abcdef"

func quoteJSON(b *strings.Builder, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteByte(byte(r))
		case 0x08:
			b.WriteString(`\b`)
		case 0x09:
			b.WriteString(`\t`)
		case 0x0A:
			b.WriteString(`\n`)
		case 0x0C:
			b.WriteString(`\f`)
		case 0x0D:
			b.WriteString(`\r`)
		default:
			if r < 0x20 {
				b.WriteString(`\u00`)
				b.WriteByte(hexDigits[r>>4])
				b.WriteByte(hexDigits[r&0xF])
			} else if utf16.IsSurrogate(r) {
				b.WriteString(`\u`)
				b.WriteByte(hexDigits[r>>12])
				b.WriteByte(hexDigits[(r>>8)&0xF])
				b.WriteByte(hexDigits[(r>>4)&0xF])
				b.WriteByte(hexDigits[r&0xF])
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}

// parseJSON ports Goja's token-based JSON.parse (same errors and order).
func (r *rt) parseJSON(s string) (any, error) {
	d := json.NewDecoder(strings.NewReader(s))
	v, err := r.decodeJSON(d, 0)
	if errors.Is(err, io.EOF) {
		return nil, r.syntaxError("Unexpected end of JSON input (" + err.Error() + ")")
	}
	if err != nil {
		var thrown *Throw
		if errors.As(err, &thrown) {
			return nil, err
		}
		return nil, r.syntaxError(err.Error())
	}
	if tok, err := d.Token(); err != io.EOF {
		return nil, r.syntaxError("Unexpected token at the end: " + tokenString(tok))
	}
	return v, nil
}

func tokenString(tok json.Token) string {
	switch t := tok.(type) {
	case json.Delim:
		return string(t)
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	case nil:
		return "<nil>"
	}
	return reflect.TypeOf(tok).String()
}

func (r *rt) decodeJSON(d *json.Decoder, depth int) (any, error) {
	tok, err := d.Token()
	if err != nil {
		return nil, err
	}
	return r.decodeToken(d, tok, depth)
}

// decodeToken recurses once per level of nesting, so it is bounded by
// maxConversionNesting itself rather than relying on the standard library:
// json.Decoder's streaming Token() caps depth only on some Go releases.
func (r *rt) decodeToken(d *json.Decoder, tok json.Token, depth int) (any, error) {
	switch t := tok.(type) {
	case json.Delim:
		if depth >= maxConversionNesting {
			return nil, errors.New("exceeded max depth")
		}
		switch t {
		case '{':
			o, err := r.newObj(0)
			if err != nil {
				return nil, err
			}
			for {
				key, err := d.Token()
				if err != nil {
					return nil, err
				}
				if key == json.Delim('}') {
					return o, nil
				}
				k, ok := key.(string)
				if !ok {
					return nil, errors.New("Unexpected token")
				}
				v, err := r.decodeJSON(d, depth+1)
				if err != nil {
					return nil, err
				}
				if err := r.put(o, k, v); err != nil {
					return nil, err
				}
			}
		case '[':
			if err := r.chargeArray(0); err != nil {
				return nil, err
			}
			items := []any{}
			for {
				tok, err := d.Token()
				if err != nil {
					return nil, err
				}
				if tok == json.Delim(']') {
					return &array{items: items}, nil
				}
				v, err := r.decodeToken(d, tok, depth+1)
				if err != nil {
					return nil, err
				}
				if err := r.charge(allocSlot); err != nil {
					return nil, err
				}
				items = append(items, v)
				if len(items) > r.x.maxItems {
					return nil, r.rangeError("Invalid array length")
				}
			}
		}
	case string:
		return r.stringValue(t)
	case nil, bool, float64:
		return t, nil
	}
	return nil, errors.New("Unexpected token")
}

// walkAST visits nodes depth-first; visit returns false to skip children.
func walkAST(n ast.Node, visit func(ast.Node) bool) {
	walkValue(reflect.ValueOf(n), visit)
}

var nodeType = reflect.TypeOf((*ast.Node)(nil)).Elem()

func walkValue(v reflect.Value, visit func(ast.Node) bool) {
	switch v.Kind() {
	case reflect.Interface, reflect.Pointer:
		if v.IsNil() {
			return
		}
		if v.Type().Implements(nodeType) {
			if n, ok := v.Interface().(ast.Node); ok && !visit(n) {
				return
			}
		}
		walkValue(v.Elem(), visit)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).IsExported() {
				walkValue(v.Field(i), visit)
			}
		}
	case reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			walkValue(v.Index(i), visit)
		}
	}
}

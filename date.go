package toolscript

import (
	"fmt"
	"math"
	"time"
)

// Date follows Goja's model (ported from its date.go and builtin_date.go,
// MIT License, Copyright (c) 2016 Dmitry Panov): a time value is int64
// milliseconds, local-time fields come from Go's time package in the host's
// location, and every string form uses Goja's layouts.

const (
	dateTimeLayout       = "Mon Jan 02 2006 15:04:05 GMT-0700 (MST)"
	utcDateTimeLayout    = "Mon, 02 Jan 2006 15:04:05 GMT"
	isoDateTimeLayout    = "2006-01-02T15:04:05.000Z"
	dateLayout           = "Mon Jan 02 2006"
	timeLayout           = "15:04:05 GMT-0700 (MST)"
	datetimeLayout_en_GB = "01/02/2006, 15:04:05"
	dateLayout_en_GB     = "01/02/2006"
	timeLayout_en_GB     = "15:04:05"

	maxTime   = 8.64e15
	timeUnset = math.MinInt64
)

type dateValue struct {
	props *object // expando properties assigned by the script
	msec  int64   // timeUnset for an invalid date
}

func (d *dateValue) isSet() bool { return d.msec != timeUnset }

func (r *rt) location() *time.Location {
	if r.x.opts.Location != nil {
		return r.x.opts.Location
	}
	return time.Local
}

func (r *rt) now() time.Time {
	if r.x.opts.Now != nil {
		return r.x.opts.Now()
	}
	return time.Now()
}

func (r *rt) dateTime(d *dateValue) time.Time { return timeFromMsec(d.msec).In(r.location()) }

func dateUTC(d *dateValue) time.Time { return timeFromMsec(d.msec).In(time.UTC) }

func timeFromMsec(msec int64) time.Time {
	return time.Unix(msec/1000, (msec%1000)*1e6)
}

func timeToMsec(t time.Time) int64 {
	return t.Unix()*1000 + int64(t.Nanosecond())/1e6
}

func (d *dateValue) setTimeMs(ms int64) any {
	if ms >= 0 && ms <= maxTime || ms < 0 && ms >= -maxTime {
		d.msec = ms
		return float64(ms)
	}
	d.msec = timeUnset
	return math.NaN()
}

func newDate(t time.Time, valid bool) *dateValue {
	if !valid {
		return &dateValue{msec: timeUnset}
	}
	return &dateValue{msec: timeToMsec(t)}
}

func (r *rt) dateParse(s string) (time.Time, bool) {
	d, ok := parseDateISOString(s)
	if !ok {
		d, ok = parseDateOtherString(s)
	}
	if !ok {
		return time.Time{}, false
	}
	if d.month > 12 || d.day > 31 || d.hour > 24 || d.min > 59 || d.sec > 59 ||
		// special case 24:00:00.000
		(d.hour == 24 && (d.min != 0 || d.sec != 0 || d.msec != 0)) {
		return time.Time{}, false
	}
	loc := r.location()
	if !d.isLocal {
		loc = time.FixedZone("", d.timeZoneOffset*60)
	}
	t := time.Date(d.year, time.Month(d.month), d.day, d.hour, d.min, d.sec, d.msec*1e6, loc)
	unixMilli := t.UnixMilli()
	return t, unixMilli >= -maxTime && unixMilli <= maxTime
}

// makeDate implements the Date constructor's argument handling.
func (r *rt) makeDate(args []any, utc bool) (time.Time, bool, error) {
	var t time.Time
	valid := false
	switch {
	case len(args) >= 2:
		t = time.Date(1970, time.January, 1, 0, 0, 0, 0, r.location())
		var err error
		t, valid, err = r.dateSetYear(t, args, 0, utc)
		if err != nil {
			return t, false, err
		}
	case len(args) == 0:
		t, valid = r.now(), true
	default:
		if d, ok := args[0].(*dateValue); ok {
			if !d.isSet() {
				return t, false, nil
			}
			t, valid = timeFromMsec(d.msec), true
			break
		}
		p, err := r.toPrimitive(args[0])
		if err != nil {
			return t, false, err
		}
		if s, ok := p.(string); ok {
			t, valid = r.dateParse(s)
			return t, valid, nil
		}
		f, err := r.toNumber(p)
		if err != nil {
			return t, false, err
		}
		if math.IsNaN(f) || math.IsInf(f, 0) || math.Abs(f) > maxTime {
			return t, false, nil
		}
		t, valid = timeFromMsec(int64(f)), true
	}
	if valid {
		msec := t.Unix()*1000 + int64(t.Nanosecond()/1e6)
		if msec < 0 {
			msec = -msec
		}
		if msec > maxTime {
			valid = false
		}
	}
	return t, valid, nil
}

// _norm returns nhi, nlo such that hi*base+lo == nhi*base+nlo, 0 <= nlo < base.
func dateNorm(hi, lo, base int64) (int64, int64, bool) {
	if lo < 0 {
		if hi == math.MinInt64 && lo <= -base {
			return 0, 0, false
		}
		n := (-lo-1)/base + 1
		hi -= n
		lo += n * base
	}
	if lo >= base {
		if hi == math.MaxInt64 {
			return 0, 0, false
		}
		n := lo / base
		hi += n
		lo -= n * base
	}
	return hi, lo, true
}

func mkTime(year, m, day, hour, min, sec, nsec int64, loc *time.Location) (time.Time, bool) {
	var ok bool
	if year, m, ok = dateNorm(year, m, 12); !ok {
		return time.Time{}, false
	}
	if sec, nsec, ok = dateNorm(sec, nsec, 1e9); !ok {
		return time.Time{}, false
	}
	if min, sec, ok = dateNorm(min, sec, 60); !ok {
		return time.Time{}, false
	}
	if hour, min, ok = dateNorm(hour, min, 60); !ok {
		return time.Time{}, false
	}
	if day, hour, ok = dateNorm(day, hour, 24); !ok {
		return time.Time{}, false
	}
	if year > math.MaxInt32 || year < math.MinInt32 || day > math.MaxInt32 || day < math.MinInt32 || m >= math.MaxInt32 || m < math.MinInt32-1 {
		return time.Time{}, false
	}
	return time.Date(int(year), time.Month(m)+1, int(day), int(hour), int(min), int(sec), int(nsec), loc), true
}

func floatToIntClip(n float64) int64 {
	switch {
	case math.IsNaN(n):
		return 0
	case n >= math.MaxInt64:
		return math.MaxInt64
	case n <= math.MinInt64:
		return math.MinInt64
	}
	return int64(n)
}

// dateIntArg reads argument i as Goja's _intArg does (NaN is not ok).
func (r *rt) dateIntArg(args []any, i int) (int64, bool, error) {
	f, err := r.toNumber(arg(args, i))
	if err != nil || math.IsNaN(f) {
		return 0, false, err
	}
	return floatToIntClip(f), true, nil
}

// The setter chain: argNum is the position of this component in args;
// negative positions keep the current value (Goja's scheme).
func present(args []any, argNum int) bool {
	return argNum == 0 || argNum > 0 && argNum < len(args)
}

type dateParts struct{ year, mon, day, hours, min, sec int64 }

func (r *rt) dateSetYear(t time.Time, args []any, argNum int, utc bool) (time.Time, bool, error) {
	var p dateParts
	if present(args, argNum) {
		v, ok, err := r.dateIntArg(args, argNum)
		if !ok || err != nil {
			return time.Time{}, false, err
		}
		if v >= 0 && v <= 99 {
			v += 1900
		}
		p.year = v
	} else {
		p.year = int64(t.Year())
	}
	return r.dateSetFrom(p, 1, t, args, argNum+1, utc)
}

func (r *rt) dateSetFullYear(t time.Time, args []any, argNum int, utc bool) (time.Time, bool, error) {
	var p dateParts
	if present(args, argNum) {
		v, ok, err := r.dateIntArg(args, argNum)
		if !ok || err != nil {
			return time.Time{}, false, err
		}
		p.year = v
	} else {
		p.year = int64(t.Year())
	}
	return r.dateSetFrom(p, 1, t, args, argNum+1, utc)
}

// dateSetFrom fills components from field onward (1 month, 2 day, 3 hours,
// 4 minutes, 5 seconds, 6 milliseconds) and builds the time.
func (r *rt) dateSetFrom(p dateParts, field int, t time.Time, args []any, argNum int, utc bool) (time.Time, bool, error) {
	current := [7]int64{0, int64(t.Month()) - 1, int64(t.Day()), int64(t.Hour()), int64(t.Minute()), int64(t.Second()), int64(t.Nanosecond() / 1e6)}
	vals := [7]int64{p.year}
	for f := 1; f <= 6; f++ {
		if f < field {
			continue
		}
		if present(args, argNum) {
			v, ok, err := r.dateIntArg(args, argNum)
			if !ok || err != nil {
				return time.Time{}, false, err
			}
			vals[f] = v
		} else {
			vals[f] = current[f]
		}
		argNum++
	}
	sec, msec, ok := dateNorm(vals[5], vals[6], 1e3)
	if !ok {
		return time.Time{}, false, nil
	}
	loc := r.location()
	if utc {
		loc = time.UTC
	}
	res, ok := mkTime(vals[0], vals[1], vals[2], vals[3], vals[4], sec, msec*1e6, loc)
	if !ok {
		return time.Time{}, false, nil
	}
	if utc {
		return res.In(r.location()), true, nil
	}
	return res, true, nil
}

func limitArgs(args []any, n int) []any {
	if len(args) > n {
		return args[:n]
	}
	return args
}

func (r *rt) dateToISO(d *dateValue) (string, error) {
	if !d.isSet() {
		return "", r.rangeError("Invalid time value")
	}
	utc := dateUTC(d)
	if year := utc.Year(); year < -9999 || year > 9999 {
		return fmt.Sprintf("%+06d-", year) + utc.Format(isoDateTimeLayout[5:]), nil
	}
	return utc.Format(isoDateTimeLayout), nil
}

func (r *rt) dateString(d *dateValue, layout string, utc bool) string {
	if !d.isSet() {
		return "Invalid Date"
	}
	if utc {
		return dateUTC(d).Format(layout)
	}
	return r.dateTime(d).Format(layout)
}

// dateToJSON implements Date.prototype.toJSON: null for an invalid date,
// otherwise this.toISOString() (an own override wins, as in Goja).
func (r *rt) dateToJSON(d *dateValue) (any, error) {
	if !d.isSet() {
		return nil, nil
	}
	if d.props != nil {
		if f, ok := d.props.get("toISOString"); ok {
			return r.callThis(f, d, nil)
		}
	}
	return r.dateToISO(d)
}

type dateMethod func(r *rt, d *dateValue, args []any) (any, error)

var dateMethods map[string]dateMethod

func dateGetter(get func(t time.Time) float64, utc bool) dateMethod {
	return func(r *rt, d *dateValue, _ []any) (any, error) {
		if !d.isSet() {
			return math.NaN(), nil
		}
		t := r.dateTime(d)
		if utc {
			t = dateUTC(d)
		}
		return get(t), nil
	}
}

// dateSetter covers setSeconds...setMonth: first is the (negative) argNum
// passed to Goja's _dateSetFullYear, limit the argument cap (0 = none).
func dateSetter(first, limit int, utc bool) dateMethod {
	return func(r *rt, d *dateValue, args []any) (any, error) {
		if limit > 0 {
			args = limitArgs(args, limit)
		}
		tv := d.msec
		t := r.dateTime(d)
		if utc {
			t = dateUTC(d)
		}
		res, ok, err := r.dateSetFullYear(t, args, first, utc)
		if err != nil {
			return nil, err
		}
		if !ok {
			d.msec = timeUnset
			return math.NaN(), nil
		}
		if tv == timeUnset {
			return math.NaN(), nil
		}
		return d.setTimeMs(timeToMsec(res)), nil
	}
}

func setMilliseconds(r *rt, d *dateValue, args []any) (any, error) {
	tv := d.msec
	n, err := r.toNumber(arg(args, 0))
	if err != nil {
		return nil, err
	}
	if tv == timeUnset {
		return math.NaN(), nil
	}
	if math.IsNaN(n) {
		d.msec = timeUnset
		return math.NaN(), nil
	}
	sec, msec, ok := dateNorm(tv/1e3, floatToIntClip(n), 1e3)
	if !ok {
		d.msec = timeUnset
		return math.NaN(), nil
	}
	return d.setTimeMs(sec*1e3 + msec), nil
}

func setFullYear(utc bool) dateMethod {
	return func(r *rt, d *dateValue, args []any) (any, error) {
		var t time.Time
		switch {
		case d.isSet() && utc:
			t = dateUTC(d)
		case d.isSet():
			t = r.dateTime(d)
		case utc:
			t = time.Date(1970, time.January, 1, 0, 0, 0, 0, time.UTC)
		default:
			t = time.Date(1970, time.January, 1, 0, 0, 0, 0, r.location())
		}
		res, ok, err := r.dateSetFullYear(t, limitArgs(args, 3), 0, utc)
		if err != nil {
			return nil, err
		}
		if !ok {
			d.msec = timeUnset
			return math.NaN(), nil
		}
		return d.setTimeMs(timeToMsec(res)), nil
	}
}

func init() {
	num := func(n int) float64 { return float64(n) }
	dateMethods = map[string]dateMethod{
		"getTime": dateGetter(func(t time.Time) float64 { return float64(timeToMsec(t)) }, false),
		"valueOf": dateGetter(func(t time.Time) float64 { return float64(timeToMsec(t)) }, false),
		"getTimezoneOffset": dateGetter(func(t time.Time) float64 {
			_, offset := t.Zone()
			return float64(-offset) / 60
		}, false),
		"setTime": func(r *rt, d *dateValue, args []any) (any, error) {
			n, err := r.toNumber(arg(args, 0))
			if err != nil {
				return nil, err
			}
			if math.IsNaN(n) {
				d.msec = timeUnset
				return math.NaN(), nil
			}
			return d.setTimeMs(floatToIntClip(n)), nil
		},
		"setMilliseconds":    setMilliseconds,
		"setUTCMilliseconds": setMilliseconds,
		"setSeconds":         dateSetter(-5, 0, false),
		"setUTCSeconds":      dateSetter(-5, 0, true),
		"setMinutes":         dateSetter(-4, 0, false),
		"setUTCMinutes":      dateSetter(-4, 0, true),
		"setHours":           dateSetter(-3, 0, false),
		"setUTCHours":        dateSetter(-3, 0, true),
		"setDate":            dateSetter(-2, 1, false),
		"setUTCDate":         dateSetter(-2, 1, true),
		"setMonth":           dateSetter(-1, 2, false),
		"setUTCMonth":        dateSetter(-1, 2, true),
		"setFullYear":        setFullYear(false),
		"setUTCFullYear":     setFullYear(true),
		"toISOString": func(r *rt, d *dateValue, _ []any) (any, error) {
			return r.dateToISO(d)
		},
		"toJSON": func(r *rt, d *dateValue, _ []any) (any, error) { return r.dateToJSON(d) },
		"toString": func(r *rt, d *dateValue, _ []any) (any, error) {
			return r.dateString(d, dateTimeLayout, false), nil
		},
		"toUTCString": func(r *rt, d *dateValue, _ []any) (any, error) {
			return r.dateString(d, utcDateTimeLayout, true), nil
		},
		"toDateString": func(r *rt, d *dateValue, _ []any) (any, error) {
			return r.dateString(d, dateLayout, false), nil
		},
		"toTimeString": func(r *rt, d *dateValue, _ []any) (any, error) {
			return r.dateString(d, timeLayout, false), nil
		},
		// Without arguments these keep Goja's fixed layouts; locales or
		// options format through Intl.DateTimeFormat as ECMA-402 specifies.
		"toLocaleString": func(r *rt, d *dateValue, args []any) (any, error) {
			if intlArgs(args) {
				return r.dateToLocale(d, args, dtfRequiredAny, dtfDefaultsAll, "Date.prototype.toLocaleString")
			}
			return r.dateString(d, datetimeLayout_en_GB, false), nil
		},
		"toLocaleDateString": func(r *rt, d *dateValue, args []any) (any, error) {
			if intlArgs(args) {
				return r.dateToLocale(d, args, dtfRequiredDate, dtfDefaultsDate, "Date.prototype.toLocaleDateString")
			}
			return r.dateString(d, dateLayout_en_GB, false), nil
		},
		"toLocaleTimeString": func(r *rt, d *dateValue, args []any) (any, error) {
			if intlArgs(args) {
				return r.dateToLocale(d, args, dtfRequiredTime, dtfDefaultsTime, "Date.prototype.toLocaleTimeString")
			}
			return r.dateString(d, timeLayout_en_GB, false), nil
		},
	}
	fields := map[string]func(t time.Time) float64{
		"FullYear":     func(t time.Time) float64 { return num(t.Year()) },
		"Month":        func(t time.Time) float64 { return num(int(t.Month()) - 1) },
		"Date":         func(t time.Time) float64 { return num(t.Day()) },
		"Day":          func(t time.Time) float64 { return num(int(t.Weekday())) },
		"Hours":        func(t time.Time) float64 { return num(t.Hour()) },
		"Minutes":      func(t time.Time) float64 { return num(t.Minute()) },
		"Seconds":      func(t time.Time) float64 { return num(t.Second()) },
		"Milliseconds": func(t time.Time) float64 { return num(t.Nanosecond() / 1e6) },
	}
	for name, get := range fields {
		dateMethods["get"+name] = dateGetter(get, false)
		dateMethods["getUTC"+name] = dateGetter(get, true)
	}
	// Every Date method that returns a string (toISOString, toString,
	// toLocaleString, ...) renders it afresh: charge for it.
	for name, m := range dateMethods {
		dateMethods[name] = func(r *rt, d *dateValue, args []any) (any, error) {
			v, err := m(r, d, args)
			if err != nil {
				return nil, err
			}
			return v, r.chargeFresh(v)
		}
	}
}

// Date statics: Date(), Date.now(), Date.parse(), Date.UTC().
func (r *rt) dateCall() (any, error) {
	return r.stringValue(r.now().In(r.location()).Format(dateTimeLayout))
}

// registerDateStatics is called by the built-ins initializer, after
// staticFunctions exists.
func registerDateStatics() {
	staticFunctions["Date.now"] = func(r *rt, _ []any) (any, error) {
		return float64(timeToMsec(r.now())), nil
	}
	staticFunctions["Date.parse"] = func(r *rt, args []any) (any, error) {
		s, err := r.toString(arg(args, 0))
		if err != nil {
			return nil, err
		}
		if t, ok := r.dateParse(s); ok {
			return float64(timeToMsec(t)), nil
		}
		return math.NaN(), nil
	}
	staticFunctions["Date.UTC"] = func(r *rt, args []any) (any, error) {
		if len(args) < 2 {
			args = []any{arg(args, 0), 0.0}
		}
		t, valid, err := r.makeDate(args, true)
		if err != nil || !valid {
			return math.NaN(), err
		}
		return float64(timeToMsec(t)), nil
	}
}

func (r *rt) newDateFromArgs(args []any) (any, error) {
	t, valid, err := r.makeDate(args, false)
	if err != nil {
		return nil, err
	}
	if err := r.charge(allocHeader); err != nil {
		return nil, err
	}
	return newDate(t, valid), nil
}

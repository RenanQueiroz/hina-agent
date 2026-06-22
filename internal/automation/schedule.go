package automation

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// minInterval is the floor for an interval trigger — anything shorter risks a hot
// fire loop that never lets a run finish. Cron's finest granularity is one minute.
const minInterval = 30 * time.Second

// maxCronSearchDays bounds the next-fire search for a cron expression so an
// impossible spec (e.g. "0 0 30 2 *") returns an error instead of spinning.
const maxCronSearchDays = 366

// Location resolves the automation's timezone to a *time.Location, defaulting to
// UTC when empty. An unknown zone is an error (caught by validation).
func (d Definition) Location() (*time.Location, error) {
	if d.Timezone == "" {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(d.Timezone)
	if err != nil {
		return nil, fmt.Errorf("unknown timezone %q", d.Timezone)
	}
	return loc, nil
}

// Next returns the next fire strictly after `after`, evaluated in loc. A manual
// trigger never fires on a schedule and returns the zero time. An invalid trigger
// returns an error.
func (t Trigger) Next(loc *time.Location, after time.Time) (time.Time, error) {
	switch t.Type {
	case TriggerManual:
		return time.Time{}, nil
	case TriggerInterval:
		every, err := t.interval()
		if err != nil {
			return time.Time{}, err
		}
		return after.Add(every), nil
	case TriggerCron:
		sched, err := parseCron(t.Cron)
		if err != nil {
			return time.Time{}, err
		}
		return sched.next(after.In(loc))
	default:
		return time.Time{}, fmt.Errorf("unknown trigger type %q", t.Type)
	}
}

// NextAfterNow returns the next fire strictly after now, skipping any fires missed
// while the server was down (the default missed-run policy). It advances from a
// stored anchor (the previous next-run, or now for a fresh automation) until the
// result is in the future, so a long downtime collapses to a single next fire. The
// search is bounded.
func (d Definition) NextAfterNow(anchor, now time.Time) (time.Time, error) {
	loc, err := d.Location()
	if err != nil {
		return time.Time{}, err
	}
	from := anchor
	if from.IsZero() || from.After(now) {
		from = now
	}
	// Interval triggers: jump ARITHMETICALLY to the first fire strictly after now, so a long
	// downtime (which would otherwise exhaust the bounded one-fire-at-a-time loop below and
	// clear the schedule) collapses to a single next fire in O(1).
	if d.Trigger.Type == TriggerInterval {
		every, err := d.Trigger.interval()
		if err != nil {
			return time.Time{}, err
		}
		next := from.Add(every)
		if !next.After(now) {
			missed := int64(now.Sub(next) / every) // whole periods already past
			next = next.Add(every * time.Duration(missed+1))
			for !next.After(now) { // guard integer-division rounding -> strictly after now
				next = next.Add(every)
			}
		}
		return next, nil
	}
	for i := 0; i < 100000; i++ {
		next, err := d.Trigger.Next(loc, from)
		if err != nil {
			return time.Time{}, err
		}
		if next.IsZero() { // manual
			return time.Time{}, nil
		}
		if next.After(now) {
			return next, nil
		}
		if !next.After(from) { // no forward progress -> stop (defensive)
			return time.Time{}, fmt.Errorf("schedule did not advance")
		}
		from = next
	}
	return time.Time{}, fmt.Errorf("could not compute a future fire time")
}

func (t Trigger) interval() (time.Duration, error) {
	d, err := time.ParseDuration(t.Every)
	if err != nil {
		return 0, fmt.Errorf("invalid interval %q", t.Every)
	}
	if d < minInterval {
		return 0, fmt.Errorf("interval %q is shorter than the %s minimum", t.Every, minInterval)
	}
	return d, nil
}

// cronSchedule is a parsed 5-field cron expression (minute hour day-of-month month
// day-of-week). Each field is a set of permitted values.
type cronSchedule struct {
	minute  map[int]bool
	hour    map[int]bool
	dom     map[int]bool
	month   map[int]bool
	dow     map[int]bool
	domStar bool // day-of-month is "*"
	dowStar bool // day-of-week is "*"
}

// parseCron parses a standard 5-field cron expression supporting *, n, a-b, a,b,
// and step (*/n, a-b/n). Day-of-week is 0-6 with 0=Sunday (7 also accepted as
// Sunday). It does NOT support named months/days or the @-shortcuts in v1.
func parseCron(expr string) (cronSchedule, error) {
	fields := strings.Fields(strings.TrimSpace(expr))
	if len(fields) != 5 {
		return cronSchedule{}, fmt.Errorf("cron must have 5 fields (got %d): %q", len(fields), expr)
	}
	min, err := parseCronField(fields[0], 0, 59)
	if err != nil {
		return cronSchedule{}, fmt.Errorf("cron minute: %w", err)
	}
	hour, err := parseCronField(fields[1], 0, 23)
	if err != nil {
		return cronSchedule{}, fmt.Errorf("cron hour: %w", err)
	}
	dom, err := parseCronField(fields[2], 1, 31)
	if err != nil {
		return cronSchedule{}, fmt.Errorf("cron day-of-month: %w", err)
	}
	month, err := parseCronField(fields[3], 1, 12)
	if err != nil {
		return cronSchedule{}, fmt.Errorf("cron month: %w", err)
	}
	// Day-of-week allows 7 as an alias for Sunday. Parse with hi=7 so 7 is in range, then
	// collapse it to 0 AFTER range/step expansion — a textual 7->0 replace would corrupt a
	// valid Sunday-inclusive range/step like 5-7 / 1-7 / */7 into 5-0 / 1-0 / */0.
	dow, err := parseCronField(fields[4], 0, 7)
	if err != nil {
		return cronSchedule{}, fmt.Errorf("cron day-of-week: %w", err)
	}
	if dow[7] {
		dow[0] = true
		delete(dow, 7)
	}
	return cronSchedule{
		minute: min, hour: hour, dom: dom, month: month, dow: dow,
		domStar: fields[2] == "*", dowStar: fields[4] == "*",
	}, nil
}

// parseCronField parses one cron field into the set of values it permits within
// [lo,hi].
func parseCronField(field string, lo, hi int) (map[int]bool, error) {
	out := map[int]bool{}
	for _, part := range strings.Split(field, ",") {
		if part == "" {
			return nil, fmt.Errorf("empty term in %q", field)
		}
		rangePart, step := part, 1
		if i := strings.IndexByte(part, '/'); i >= 0 {
			s, err := strconv.Atoi(part[i+1:])
			if err != nil || s < 1 {
				return nil, fmt.Errorf("invalid step in %q", part)
			}
			step = s
			rangePart = part[:i]
		}
		start, end := lo, hi
		switch {
		case rangePart == "*":
			// full range
		case strings.IndexByte(rangePart, '-') >= 0:
			bounds := strings.SplitN(rangePart, "-", 2)
			a, err1 := strconv.Atoi(bounds[0])
			b, err2 := strconv.Atoi(bounds[1])
			if err1 != nil || err2 != nil {
				return nil, fmt.Errorf("invalid range %q", rangePart)
			}
			start, end = a, b
		default:
			v, err := strconv.Atoi(rangePart)
			if err != nil {
				return nil, fmt.Errorf("invalid value %q", rangePart)
			}
			start, end = v, v
		}
		if start < lo || end > hi || start > end {
			return nil, fmt.Errorf("term %q out of range [%d,%d]", part, lo, hi)
		}
		for v := start; v <= end; v += step {
			out[v] = true
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("field %q matched no values", field)
	}
	return out, nil
}

// next returns the next minute strictly after `after` (already in the target
// location) that matches the schedule. It steps minute-by-minute up to a bounded
// horizon so an impossible spec errors rather than loops forever.
func (s cronSchedule) next(after time.Time) (time.Time, error) {
	// Start at the next whole minute.
	t := after.Truncate(time.Minute).Add(time.Minute)
	horizon := t.AddDate(0, 0, maxCronSearchDays)
	for t.Before(horizon) {
		if s.matches(t) {
			return t, nil
		}
		t = t.Add(time.Minute)
	}
	return time.Time{}, fmt.Errorf("cron expression has no fire time within %d days", maxCronSearchDays)
}

// matches reports whether t satisfies every field. Day-of-month and day-of-week
// follow standard cron: if BOTH are restricted (neither is "*"), a day matches when
// EITHER matches; otherwise both must match.
func (s cronSchedule) matches(t time.Time) bool {
	if !s.minute[t.Minute()] || !s.hour[t.Hour()] || !s.month[int(t.Month())] {
		return false
	}
	domOK := s.dom[t.Day()]
	dowOK := s.dow[int(t.Weekday())]
	if !s.domStar && !s.dowStar {
		return domOK || dowOK
	}
	return domOK && dowOK
}

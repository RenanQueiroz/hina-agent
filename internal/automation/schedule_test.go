package automation

import (
	"testing"
	"time"
)

// A long downtime (more missed interval periods than the one-fire-at-a-time loop could
// iterate) must NOT error — interval catch-up jumps arithmetically to the first future
// fire, so the schedule isn't cleared and left dormant (round-36 finding).
func TestNextAfterNowIntervalLongDowntime(t *testing.T) {
	def := Definition{Trigger: Trigger{Type: TriggerInterval, Every: "30s"}}
	now := time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC)
	// Anchor 200000 periods in the past (well over the 100000-iteration bound).
	anchor := now.Add(-200000 * 30 * time.Second)
	next, err := def.NextAfterNow(anchor, now)
	if err != nil {
		t.Fatalf("long downtime must not error: %v", err)
	}
	if !next.After(now) {
		t.Fatalf("next (%v) must be after now (%v)", next, now)
	}
	// It collapses to the FIRST fire after now (within one interval), not a backlog.
	if next.Sub(now) > 30*time.Second {
		t.Fatalf("next should be the first fire after now, got %v ahead", next.Sub(now))
	}
}

func TestIntervalNext(t *testing.T) {
	tr := Trigger{Type: TriggerInterval, Every: "5m"}
	loc := time.UTC
	after := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	next, err := tr.Next(loc, after)
	if err != nil {
		t.Fatal(err)
	}
	if !next.Equal(after.Add(5 * time.Minute)) {
		t.Errorf("next = %v, want +5m", next)
	}
}

func TestIntervalFloor(t *testing.T) {
	tr := Trigger{Type: TriggerInterval, Every: "1s"}
	if _, err := tr.Next(time.UTC, time.Now()); err == nil {
		t.Fatal("interval below the floor should error")
	}
}

func TestManualNeverFires(t *testing.T) {
	tr := Trigger{Type: TriggerManual}
	next, err := tr.Next(time.UTC, time.Now())
	if err != nil || !next.IsZero() {
		t.Fatalf("manual next = %v, err=%v (want zero,nil)", next, err)
	}
}

func TestCronNextDaily(t *testing.T) {
	// 0 9 * * * -> 09:00 every day.
	tr := Trigger{Type: TriggerCron, Cron: "0 9 * * *"}
	after := time.Date(2026, 6, 21, 10, 0, 0, 0, time.UTC)
	next, err := tr.Next(time.UTC, after)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 6, 22, 9, 0, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Errorf("next = %v, want %v", next, want)
	}
}

func TestCronStepAndRange(t *testing.T) {
	// every 15 minutes between 9-10.
	tr := Trigger{Type: TriggerCron, Cron: "*/15 9-10 * * *"}
	after := time.Date(2026, 6, 21, 9, 5, 0, 0, time.UTC)
	next, _ := tr.Next(time.UTC, after)
	want := time.Date(2026, 6, 21, 9, 15, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Errorf("next = %v, want %v", next, want)
	}
}

func TestCronDayOfWeek(t *testing.T) {
	// Mondays at 00:00. 2026-06-21 is a Sunday; next Monday is 2026-06-22.
	tr := Trigger{Type: TriggerCron, Cron: "0 0 * * 1"}
	after := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	next, _ := tr.Next(time.UTC, after)
	want := time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Errorf("next = %v, want %v (weekday %v)", next, want, next.Weekday())
	}
}

// 7 is a Sunday alias in day-of-week — it must collapse to 0 AFTER range/step expansion, so
// Sunday-inclusive ranges + steps (5-7, 1-7, */7) parse correctly, not get text-mangled (round-54).
func TestCronDayOfWeekSundayAlias(t *testing.T) {
	cases := []struct {
		field string
		want  []int
	}{
		{"7", []int{0}},
		{"0", []int{0}},
		{"1,7", []int{0, 1}},
		{"5-7", []int{0, 5, 6}},             // Fri, Sat, Sun
		{"1-7", []int{0, 1, 2, 3, 4, 5, 6}}, // every day
		{"6-7", []int{0, 6}},                // Sat, Sun
		{"*/7", []int{0}},                   // step 7 over [0,7] -> {0,7} -> {0}
	}
	for _, c := range cases {
		sched, err := parseCron("0 0 * * " + c.field)
		if err != nil {
			t.Errorf("parseCron dow %q: unexpected error %v", c.field, err)
			continue
		}
		if sched.dow[7] {
			t.Errorf("dow %q: 7 must be collapsed to 0, got %v", c.field, sched.dow)
		}
		if len(sched.dow) != len(c.want) {
			t.Errorf("dow %q: got %v (%d), want %v", c.field, sched.dow, len(sched.dow), c.want)
		}
		for _, d := range c.want {
			if !sched.dow[d] {
				t.Errorf("dow %q: expected day %d in %v", c.field, d, sched.dow)
			}
		}
	}
}

func TestCronTimezone(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("tz db unavailable: %v", err)
	}
	tr := Trigger{Type: TriggerCron, Cron: "30 8 * * *"} // 08:30 local
	after := time.Date(2026, 6, 21, 0, 0, 0, 0, loc)
	next, _ := tr.Next(loc, after)
	if next.Hour() != 8 || next.Minute() != 30 {
		t.Errorf("next local = %v, want 08:30", next)
	}
}

func TestNextAfterNowSkipsMissed(t *testing.T) {
	// A 5m interval anchored far in the past collapses to a single next fire.
	d := Definition{Trigger: Trigger{Type: TriggerInterval, Every: "5m"}}
	anchor := time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 6, 21, 12, 3, 0, 0, time.UTC)
	next, err := d.NextAfterNow(anchor, now)
	if err != nil {
		t.Fatal(err)
	}
	if !next.After(now) {
		t.Errorf("next %v should be after now %v", next, now)
	}
	if next.Sub(now) > 5*time.Minute {
		t.Errorf("next %v should be within one interval of now", next)
	}
}

func TestBadCron(t *testing.T) {
	for _, expr := range []string{"", "* * *", "60 * * * *", "* 25 * * *", "a b c d e", "*/0 * * * *"} {
		if _, err := parseCron(expr); err == nil {
			t.Errorf("parseCron(%q) should error", expr)
		}
	}
}

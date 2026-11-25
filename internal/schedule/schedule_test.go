package schedule

import (
	"testing"
	"time"
)

func mustParse(t *testing.T, s string) Schedule {
	t.Helper()
	sc, err := Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return sc
}

func TestParse(t *testing.T) {
	for _, ok := range []string{"30 2 * * *", "*/10 * * * *", "@daily", "@hourly", "@every 2h", "CRON_TZ=Europe/Berlin 0 3 * * 1-5", "off", "OFF", "  0  4 * * * "} {
		if _, err := Parse(ok); err != nil {
			t.Errorf("Parse(%q): %v", ok, err)
		}
	}
	for _, bad := range []string{"", "* * *", "61 * * * *", "0 0 * * * *", "@every 10s", "CRON_TZ=Nowhere/City 0 3 * * *", "daily"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) should fail", bad)
		}
	}
}

func TestDue(t *testing.T) {
	s := mustParse(t, "CRON_TZ=UTC 30 2 * * *")
	at := func(h, m int) time.Time { return time.Date(2026, 11, 24, h, m, 0, 0, time.UTC) }
	if !s.Due(time.Time{}, at(1, 0)) {
		t.Error("no history must be due immediately")
	}
	last := at(2, 30).Add(-24 * time.Hour).Add(5 * time.Second)
	if s.Due(last, at(2, 25)) {
		t.Error("before the slot must not be due")
	}
	if !s.Due(last, at(2, 30)) || !s.Due(last, at(9, 0)) {
		t.Error("at or after the slot must be due")
	}
	// Host off for three days: due once, then not again until the next slot.
	if !s.Due(last.Add(-72*time.Hour), at(9, 0)) {
		t.Error("missed slots must catch up")
	}
	if s.Due(at(9, 0), at(9, 5)) {
		t.Error("after catching up, not due until the next slot")
	}
	if off := mustParse(t, "off"); off.Due(time.Time{}, at(9, 0)) || !off.Next(at(9, 0)).IsZero() {
		t.Error("off is never due")
	}
}

func TestDST(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skip("no tzdata")
	}
	s := mustParse(t, "CRON_TZ=Europe/Berlin 30 2 * * *")
	// 2026-03-29: 02:00 -> 03:00 in Berlin; 02:30 does not exist that day.
	last := time.Date(2026, 3, 28, 2, 30, 0, 0, berlin)
	next := s.Next(last)
	if next.Before(last) || next.Sub(last) > 48*time.Hour {
		t.Fatalf("next after spring-forward: %v", next)
	}
	if !s.Due(last, time.Date(2026, 3, 30, 3, 0, 0, 0, berlin)) {
		t.Error("must be due again by the next day")
	}
}

func TestNextUsesHostLocalTimeForUTCInput(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skip("no tzdata")
	}
	old := time.Local
	time.Local = berlin
	defer func() { time.Local = old }()
	sc, err := Parse("30 2 * * *")
	if err != nil {
		t.Fatal(err)
	}
	// 2026-11-24 20:19 UTC (21:19 CET); state files hold UTC times.
	got := sc.Next(time.Date(2026, 11, 24, 20, 19, 0, 0, time.UTC))
	if want := time.Date(2026, 11, 25, 2, 30, 0, 0, berlin); !got.Equal(want) {
		t.Fatalf("next = %s, want %s (host local time)", got, want)
	}
	tz, _ := Parse("CRON_TZ=UTC 30 2 * * *")
	if got := tz.Next(time.Date(2026, 11, 24, 20, 19, 0, 0, time.UTC)); !got.Equal(time.Date(2026, 11, 25, 2, 30, 0, 0, time.UTC)) {
		t.Fatalf("CRON_TZ must still win: %s", got)
	}
}

// Package schedule parses backup schedules (cron expressions) and decides
// when a target is due.
package schedule

import (
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

// Off disables scheduled backups of a target.
const Off = "off"

// Schedule is a parsed cron expression. The zero value is "off".
type Schedule struct {
	Expr  string
	sched cron.Schedule
}

var parser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// Parse accepts a standard 5-field cron expression, a descriptor (@hourly,
// @daily, @weekly, @monthly, @yearly, @every <duration>), an optional
// CRON_TZ=<zone> prefix, or "off". Times are in the host's local zone
// unless CRON_TZ says otherwise.
func Parse(expr string) (Schedule, error) {
	expr = strings.Join(strings.Fields(expr), " ")
	if expr == "" {
		return Schedule{}, fmt.Errorf("empty schedule")
	}
	if strings.EqualFold(expr, Off) {
		return Schedule{Expr: Off}, nil
	}
	s, err := parser.Parse(expr)
	if err != nil {
		return Schedule{}, fmt.Errorf("invalid schedule %q: %w", expr, err)
	}
	if ce, ok := s.(cron.ConstantDelaySchedule); ok && ce.Delay < time.Minute {
		return Schedule{}, fmt.Errorf("invalid schedule %q: interval must be at least 1m", expr)
	}
	return Schedule{Expr: expr, sched: s}, nil
}

// IsOff reports whether the schedule disables scheduled backups.
func (s Schedule) IsOff() bool { return s.sched == nil }

// Next returns the first activation strictly after t (zero time when off).
func (s Schedule) Next(t time.Time) time.Time {
	if s.IsOff() {
		return time.Time{}
	}
	// cron evaluates a schedule without CRON_TZ in the zone of t. Run times
	// are stored in UTC, so without this "30 2 * * *" would fire at 02:30
	// UTC instead of 02:30 host local time.
	return s.sched.Next(t.In(time.Local))
}

// Due reports whether a target whose last attempt started at lastAttempt
// must be backed up now. A target without history is due immediately;
// missed activations (host off, lock held) fire once, not once per slot.
func (s Schedule) Due(lastAttempt, now time.Time) bool {
	if s.IsOff() {
		return false
	}
	if lastAttempt.IsZero() {
		return true
	}
	return !s.Next(lastAttempt).After(now)
}

// String returns the expression.
func (s Schedule) String() string { return s.Expr }

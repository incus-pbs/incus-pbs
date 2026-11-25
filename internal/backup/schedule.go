package backup

import (
	"fmt"
	"time"

	"github.com/incus-pbs/incus-pbs/internal/incus"
	"github.com/incus-pbs/incus-pbs/internal/schedule"
)

// TargetKey identifies a target in the state file.
func TargetKey(kind Kind, project, name string) string {
	return string(kind) + "/" + project + "/" + name
}

// targetSchedule parses a target's schedule; Discover already validated it,
// so a failure falls back to the default.
func targetSchedule(t Target, def schedule.Schedule) schedule.Schedule {
	sc, err := schedule.Parse(t.Schedule)
	if err != nil {
		return def
	}
	return sc
}

// Due is the outcome of SelectDue.
type Due struct {
	Plan *Plan
	// Host: the host index (metadata, host config) and the volumes that
	// live in it (shared and standalone) are due.
	Host bool
	// Deferred counts targets that are not due yet.
	Deferred int
	// Known lists the state keys of every scheduled (not off) target.
	Known []string
}

// Empty reports whether nothing at all is due.
func (d Due) Empty() bool { return len(d.Plan.Targets) == 0 && !d.Host }

// SelectDue narrows a full plan to what a scheduled run must back up now.
// Instances follow their own schedule, volumes owned by one instance follow
// that instance, and everything stored in the host index follows
// backup.schedule (or runs at once when a host volume has no history yet).
func SelectDue(plan *Plan, st *State, defExpr string, now time.Time) (Due, error) {
	def, err := schedule.Parse(defExpr)
	if err != nil {
		return Due{}, fmt.Errorf("backup.schedule: %w", err)
	}
	out := &Plan{Excluded: append([]Excluded(nil), plan.Excluded...), Warnings: plan.Warnings}
	d := Due{Plan: out, Host: def.Due(st.HostLastAttempt, now)}
	for _, t := range plan.Targets {
		if t.Kind == KindVolume && t.Owner == "" && st.Targets[TargetKey(t.Kind, t.Project, t.Name)].LastAttempt.IsZero() {
			d.Host = true
		}
	}
	due := map[string]bool{} // instance "project/name" -> due
	for _, t := range plan.Targets {
		if t.Kind == KindVolume {
			continue
		}
		sc := targetSchedule(t, def)
		if sc.IsOff() {
			out.Excluded = append(out.Excluded, Excluded{What: fmt.Sprintf("instance %s/%s", t.Project, t.Name),
				Reason: "schedule off (" + incus.ScheduleKey + ")"})
			continue
		}
		due[t.Project+"/"+t.Name] = sc.Due(st.Targets[TargetKey(t.Kind, t.Project, t.Name)].LastAttempt, now)
		d.Known = append(d.Known, TargetKey(t.Kind, t.Project, t.Name))
	}
	for _, t := range plan.Targets {
		var include bool
		switch {
		case t.Kind != KindVolume:
			include = due[t.Project+"/"+t.Name]
		case t.Owner != "":
			ownerDue, known := due[t.Owner]
			if !known { // owner is off
				out.Excluded = append(out.Excluded, Excluded{What: t.ID(), Reason: "follows instance " + t.Owner + " (schedule off)"})
				continue
			}
			include = ownerDue
		default:
			include = d.Host
		}
		if t.Kind == KindVolume {
			d.Known = append(d.Known, TargetKey(t.Kind, t.Project, t.Name))
		}
		if include {
			out.Targets = append(out.Targets, t)
		} else {
			d.Deferred++
		}
	}
	return d, nil
}

// NextRun is when `backup --scheduled` will next include t (now when it is
// due already); ok is false for "off".
func NextRun(t Target, st *State, now time.Time) (next time.Time, ok bool) {
	sc, err := schedule.Parse(t.Schedule)
	if err != nil || sc.IsOff() {
		return time.Time{}, false
	}
	last := st.Targets[TargetKey(t.Kind, t.Project, t.Name)].LastAttempt
	if !last.IsZero() && t.Kind == KindVolume && t.Owner == "" {
		last = st.HostLastAttempt // stored in the run index, which has its own slot
	}
	if last.IsZero() {
		return now, true
	}
	if n := sc.Next(last); n.After(now) {
		return n, true
	}
	return now, true
}

// ExpectedBy is when a target's next successful backup is due: the first
// slot after its last success, or after it was first tried when it has never
// succeeded (zero when unknown or off).
func (ts TargetState) ExpectedBy() time.Time {
	sc, err := schedule.Parse(ts.Schedule)
	if err != nil || sc.IsOff() {
		return time.Time{}
	}
	base := ts.LastSuccess
	if base.IsZero() {
		base = ts.FirstSeen
	}
	if base.IsZero() {
		return time.Time{}
	}
	return sc.Next(base)
}

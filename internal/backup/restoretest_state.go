package backup

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// RestoreTestState is the history of `restore test` drills for one source
// instance, kept in the state file next to the backup history.
type RestoreTestState struct {
	Kind    Kind   `json:"kind"`
	Project string `json:"project"`
	Name    string `json:"name"`
	// LastRun is the end of the last drill, successful or not.
	LastRun     time.Time `json:"last_run"`
	LastOK      bool      `json:"last_ok"`
	LastSuccess time.Time `json:"last_success,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
	// Snapshot is the guest snapshot the last drill restored, if it got that far.
	Snapshot        string  `json:"snapshot,omitempty"`
	DurationSeconds float64 `json:"duration_seconds"`
	BootSeconds     float64 `json:"boot_seconds,omitempty"`
}

// maxRestoreTestError keeps a long multi-line failure from bloating the state.
const maxRestoreTestError = 1024

// RecordRestoreTest merges a finished drill into the state. A failed drill
// keeps the time of the previous success.
//
// When the drill failed before the backup was located, Kind and Project may be
// unknown: the result then updates the one existing entry for that name (and
// project, if given), so a drill that keeps failing early stays visible.
// Without such an entry it is not recorded.
func (s *State) RecordRestoreTest(r RestoreTestState) {
	if s.RestoreTests == nil {
		s.RestoreTests = map[string]RestoreTestState{}
	}
	key := ""
	if r.Kind != "" && r.Project != "" {
		key = TargetKey(r.Kind, r.Project, r.Name)
	} else {
		var matches []string
		for k, e := range s.RestoreTests {
			if e.Name == r.Name && (r.Project == "" || e.Project == r.Project) {
				matches = append(matches, k)
			}
		}
		if len(matches) != 1 {
			// No history to attach it to (e.g. a mistyped name): the
			// drill's exit code reports it, the state stays clean.
			return
		}
		key = matches[0]
		prev := s.RestoreTests[key]
		r.Kind, r.Project = prev.Kind, prev.Project
	}
	prev := s.RestoreTests[key]
	if len(r.LastError) > maxRestoreTestError {
		r.LastError = r.LastError[:maxRestoreTestError] + "…"
	}
	if r.LastOK {
		r.LastSuccess, r.LastError = r.LastRun, ""
	} else {
		r.LastSuccess = prev.LastSuccess
	}
	s.RestoreTests[key] = r
}

// forgetRestoreTests drops drills of instances that a complete plan no longer
// contains (deleted, excluded or set to off), like Record does for targets.
func (s *State) forgetRestoreTests(known map[string]bool) {
	for k := range s.RestoreTests {
		if !known[k] {
			delete(s.RestoreTests, k)
		}
	}
}

// RestoreTestKeys returns the keys of RestoreTests in a stable order.
func (s *State) RestoreTestKeys() []string {
	keys := make([]string, 0, len(s.RestoreTests))
	for k := range s.RestoreTests {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (s *State) restoreTestMetrics(b *strings.Builder) {
	keys := s.RestoreTestKeys()
	ts := func(t time.Time) float64 {
		if t.IsZero() {
			return 0
		}
		return float64(t.Unix())
	}
	for _, m := range []struct {
		name, help string
		v          func(RestoreTestState) float64
	}{
		{"incus_pbs_restore_test_last_run_timestamp_seconds", "End time of the last restore drill of each instance.",
			func(r RestoreTestState) float64 { return ts(r.LastRun) }},
		{"incus_pbs_restore_test_last_success_timestamp_seconds", "End time of the last successful restore drill (0 if none succeeded).",
			func(r RestoreTestState) float64 { return ts(r.LastSuccess) }},
		{"incus_pbs_restore_test_last_ok", "1 if the last restore drill restored, started and found all files.",
			func(r RestoreTestState) float64 { return b2f(r.LastOK) }},
		{"incus_pbs_restore_test_last_duration_seconds", "Duration of the last restore drill.",
			func(r RestoreTestState) float64 { return r.DurationSeconds }},
		{"incus_pbs_restore_test_last_boot_seconds", "Time from start until the first file check answered in the last drill.",
			func(r RestoreTestState) float64 { return r.BootSeconds }},
	} {
		fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s gauge\n", m.name, m.help, m.name)
		for _, k := range keys {
			r := s.RestoreTests[k]
			fmt.Fprintf(b, "%s{kind=%q,project=%q,name=%q} %s\n", m.name, string(r.Kind), r.Project, r.Name, fmtFloat(m.v(r)))
		}
	}
}

func fmtFloat(v float64) string { return strings.TrimSuffix(fmt.Sprintf("%.3f", v), ".000") }

package backup

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/incus-pbs/incus-pbs/internal/schedule"
)

// State persists run history needed by `status` and monitoring.
type State struct {
	LastRun             *Report   `json:"last_run,omitempty"`
	LastSuccess         time.Time `json:"last_success,omitempty"`
	LastSuccessSnapshot string    `json:"last_success_snapshot,omitempty"`
	// HostLastAttempt is the start of the last run that included the host
	// index content (host config, shared and standalone volumes).
	HostLastAttempt time.Time              `json:"host_last_attempt,omitempty"`
	Targets         map[string]TargetState `json:"targets"`
	// RestoreTests holds the last `restore test` drill per source instance,
	// keyed like Targets.
	RestoreTests map[string]RestoreTestState `json:"restore_tests,omitempty"`
}

// TargetState is the per-target history.
type TargetState struct {
	Kind        Kind      `json:"kind"`
	Project     string    `json:"project"`
	Name        string    `json:"name"`
	LastStatus  string    `json:"last_status"`
	LastSuccess time.Time `json:"last_success,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
	// LastAttempt is the start of the last run that tried this target
	// (successful or not); scheduled runs measure due time from it.
	LastAttempt time.Time `json:"last_attempt,omitempty"`
	Schedule    string    `json:"schedule,omitempty"`
	// NextDue is the next scheduled run after LastAttempt.
	NextDue time.Time `json:"next_due,omitempty"`
	// FirstSeen is the start of the first run that tried this target, so a
	// target that has never succeeded still becomes overdue.
	FirstSeen time.Time `json:"first_seen,omitempty"`
}

// StateFile is the file name inside the state directory.
const StateFile = "state.json"

// LoadState reads the state; a missing file yields an empty state.
func LoadState(dir string) (*State, error) {
	s := &State{Targets: map[string]TargetState{}}
	b, err := os.ReadFile(filepath.Join(dir, StateFile))
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, s); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", StateFile, err)
	}
	if s.Targets == nil {
		s.Targets = map[string]TargetState{}
	}
	return s, nil
}

// Record merges a finished run into the state.
func (s *State) Record(rep *Report) {
	s.LastRun = rep
	end := rep.Start.Add(time.Duration(rep.DurationSeconds * float64(time.Second)))
	if rep.ExitCode == ExitOK {
		s.LastSuccess, s.LastSuccessSnapshot = end, rep.PBSSnapshot
	}
	if rep.HostContent {
		s.HostLastAttempt = rep.Start
	}
	if rep.Known != nil { // forget deleted, excluded and "off" targets
		known := map[string]bool{}
		for _, k := range rep.Known {
			known[k] = true
		}
		for k := range s.Targets {
			if !known[k] {
				delete(s.Targets, k)
			}
		}
		s.forgetRestoreTests(known)
	}
	for _, it := range rep.Items {
		key := TargetKey(it.Kind, it.Project, it.Name)
		ts := s.Targets[key]
		ts.Kind, ts.Project, ts.Name, ts.LastStatus, ts.LastError = it.Kind, it.Project, it.Name, it.Status, it.Error
		ts.LastAttempt = rep.Start
		if ts.FirstSeen.IsZero() {
			ts.FirstSeen = rep.Start
		}
		if it.Schedule != "" {
			ts.Schedule = it.Schedule
		}
		if sc, err := schedule.Parse(ts.Schedule); err == nil {
			ts.NextDue = sc.Next(rep.Start)
		}
		if it.Status == "ok" {
			ts.LastSuccess = end
		}
		s.Targets[key] = ts
	}
}

// Save writes the state atomically.
func (s *State) Save(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, StateFile), b, 0o600)
}

func writeAtomic(path string, b []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	// Flush before the rename, or a power loss can leave an empty file
	// under the final name.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

// syncDir makes a rename in dir durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil && !errors.Is(err, os.ErrInvalid) {
		return err
	}
	return nil
}

// Metrics renders Prometheus text exposition format for the textfile collector.
func (s *State) Metrics() string {
	var b strings.Builder
	gauge := func(name, help string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
	}
	ts := func(t time.Time) float64 {
		if t.IsZero() {
			return 0
		}
		return float64(t.Unix())
	}
	gauge("incus_pbs_last_success_timestamp_seconds", "End time of the last fully successful backup run.")
	fmt.Fprintf(&b, "incus_pbs_last_success_timestamp_seconds %.0f\n", ts(s.LastSuccess))
	if r := s.LastRun; r != nil {
		end := r.Start.Add(time.Duration(r.DurationSeconds * float64(time.Second)))
		for _, m := range []struct {
			name, help string
			v          float64
		}{
			{"incus_pbs_last_run_timestamp_seconds", "End time of the last backup run.", ts(end)},
			{"incus_pbs_last_run_exit_code", "Exit code of the last run (0 ok, 1 failed, 2 partial).", float64(r.ExitCode)},
			{"incus_pbs_last_run_complete", "1 if the last run produced a complete PBS snapshot.", b2f(r.Complete)},
			{"incus_pbs_last_run_duration_seconds", "Duration of the last run.", r.DurationSeconds},
			{"incus_pbs_last_run_upload_seconds", "Upload duration of the last run.", r.UploadSeconds},
			{"incus_pbs_last_run_logical_bytes", "Logical size of all archives of the last run.", float64(r.LogicalBytes)},
			{"incus_pbs_last_run_uploaded_bytes", "Bytes actually uploaded by the last run.", float64(r.UploadedBytes)},
		} {
			gauge(m.name, m.help)
			fmt.Fprintf(&b, "%s %s\n", m.name, strconv.FormatFloat(m.v, 'f', -1, 64))
		}
		gauge("incus_pbs_last_run_targets", "Targets of the last run by kind and status.")
		counts := map[[2]string]int{}
		for _, it := range r.Items {
			counts[[2]string{string(it.Kind), it.Status}]++
		}
		keys := make([][2]string, 0, len(counts))
		for k := range counts {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return keys[i][0]+keys[i][1] < keys[j][0]+keys[j][1] })
		for _, k := range keys {
			fmt.Fprintf(&b, "incus_pbs_last_run_targets{kind=%q,status=%q} %d\n", k[0], k[1], counts[k])
		}
	}
	keys := make([]string, 0, len(s.Targets))
	for k := range s.Targets {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, m := range []struct {
		name, help string
		v          func(TargetState) time.Time
	}{
		{"incus_pbs_target_last_success_timestamp_seconds", "Last time each target was included in a successful upload.",
			func(t TargetState) time.Time { return t.LastSuccess }},
		{"incus_pbs_target_next_due_timestamp_seconds", "Next scheduled run of each target after its last attempt.",
			func(t TargetState) time.Time { return t.NextDue }},
		{"incus_pbs_target_expected_by_timestamp_seconds", "First schedule slot after the last success: the target is overdue once time() passes it (plus a grace period).",
			TargetState.ExpectedBy},
	} {
		gauge(m.name, m.help)
		for _, k := range keys {
			t := s.Targets[k]
			fmt.Fprintf(&b, "%s{kind=%q,project=%q,name=%q} %.0f\n", m.name, string(t.Kind), t.Project, t.Name, ts(m.v(t)))
		}
	}
	s.restoreTestMetrics(&b)
	return b.String()
}

// WriteMetrics writes the textfile atomically (node_exporter must never read a partial file).
func (s *State) WriteMetrics(path string) error {
	return writeAtomic(path, []byte(s.Metrics()), 0o644)
}

func b2f(v bool) float64 {
	if v {
		return 1
	}
	return 0
}

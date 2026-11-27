package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/urfave/cli/v3"

	"github.com/incus-pbs/incus-pbs/internal/backup"
	"github.com/incus-pbs/incus-pbs/internal/config"
)

const (
	defaultMaxAge = 26 * time.Hour
	defaultGrace  = 2 * time.Hour
)

func emitJSON(w io.Writer, v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Fprintln(w, string(b))
}

func bytesStr(b int64) string { return humanize.IBytes(uint64(max(b, 0))) }

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func cmdCheck(ctx context.Context, c *cli.Command, rt *runtime) error {
	res := rt.eng.Check(ctx)
	if c.Bool("json") {
		_ = json.NewEncoder(rt.stdout).Encode(res)
	} else {
		t := newTable(rt.stdout, "STATUS", "CHECK", "DETAIL")
		for _, r := range res {
			t.AppendRow([]any{strings.ToUpper(string(r.Status)), r.Name, r.Detail})
		}
		t.Render()
	}
	if backup.CheckFailed(res) {
		return cli.Exit("", exitFailed)
	}
	return nil
}

func cmdList(_ context.Context, c *cli.Command, rt *runtime) error {
	plan, err := backup.Discover(rt.eng.Incus, rt.cfg, backup.Selection{Projects: c.StringSlice("project")})
	if err != nil {
		return cli.Exit("error: "+err.Error(), exitFailed)
	}
	st, err := backup.LoadState(rt.cfg.Storage.StateDirectory)
	if err != nil {
		rt.log.Warn("state_load_failed", "event", "state_load_failed", "error", err)
		st, _ = backup.LoadState(os.DevNull)
	}
	now := time.Now()
	type listed struct {
		backup.Target
		NextRun *time.Time `json:"next_run,omitempty"` // for `backup --scheduled`; absent when off
	}
	rows := make([]listed, 0, len(plan.Targets))
	for _, t := range plan.Targets {
		l := listed{Target: t}
		if n, ok := backup.NextRun(t, st, now); ok {
			l.NextRun = &n
		}
		rows = append(rows, l)
	}
	if c.Bool("json") {
		_ = json.NewEncoder(rt.stdout).Encode(map[string]any{"targets": rows, "excluded": plan.Excluded, "warnings": plan.Warnings})
		return nil
	}
	tbl := newTable(rt.stdout, "KIND", "PROJECT", "NAME", "STATUS", "POOL", "SCHEDULE", "NEXT", "ARCHIVES", "PROBLEM")
	for _, r := range rows {
		t := r.Target
		status := t.Status
		if t.Kind == backup.KindVolume {
			status = t.ContentType
		}
		next := "off"
		if r.NextRun != nil {
			next = r.NextRun.Local().Format("2006-01-02 15:04")
			if !r.NextRun.After(now) {
				next = "due"
			}
		}
		sched := t.Schedule
		switch t.ScheduleSource {
		case "owner":
			sched += " (" + t.Owner + ")"
		case "invalid":
			sched += " (invalid key)"
		}
		tbl.AppendRow([]any{t.Kind, t.Project, t.Name, status, t.Pool.Name, sched, next,
			strings.Join(t.Archives(), ","), dash(t.Problem)})
	}
	tbl.Render()
	for _, x := range plan.Excluded {
		fmt.Fprintf(rt.stdout, "excluded: %s (%s)\n", x.What, x.Reason)
	}
	for _, w := range plan.Warnings {
		fmt.Fprintf(rt.stdout, "warning: %s\n", w)
	}
	fmt.Fprintf(rt.stdout, "\n%s\n", plan.Summary())
	return nil
}

func cmdBackup(ctx context.Context, c *cli.Command, rt *runtime) error {
	rep, err := rt.eng.Run(ctx, backup.Selection{Projects: c.StringSlice("project"), Instances: c.StringSlice("instance"),
		Scheduled: c.Bool("scheduled")})
	if rep.NothingDue && err == nil {
		return nil // nothing ran: no state change, no report
	}
	recordRun(rt.cfg, rt.log, rep)
	emitJSON(rt.stdout, rep)
	code := rep.ExitCode
	if err != nil && code == backup.ExitOK {
		code = backup.ExitFailed
	}
	if code != exitOK {
		return cli.Exit("", code)
	}
	return nil
}

// recordRun persists the report in the state directory and refreshes the
// Prometheus textfile. Failures here never change the backup exit code.
func recordRun(cfg config.Config, log *slog.Logger, rep *backup.Report) {
	updateState(cfg, log, func(st *backup.State) { st.Record(rep) })
}

// recordRestoreTest persists a drill result like recordRun; it never changes
// the drill's exit code either. The caller holds the run lock.
func recordRestoreTest(cfg config.Config, log *slog.Logger, r backup.RestoreTestState) {
	updateState(cfg, log, func(st *backup.State) { st.RecordRestoreTest(r) })
}

func updateState(cfg config.Config, log *slog.Logger, change func(*backup.State)) {
	st, err := backup.LoadState(cfg.Storage.StateDirectory)
	if err != nil {
		log.Error("state_load_failed", "event", "state_load_failed", "error", err)
		st, _ = backup.LoadState(os.DevNull)
	}
	change(st)
	if err := st.Save(cfg.Storage.StateDirectory); err != nil {
		log.Error("state_save_failed", "event", "state_save_failed", "error", err)
	}
	if p := cfg.Monitoring.TextfilePath; p != "" {
		if err := st.WriteMetrics(p); err != nil {
			log.Error("metrics_write_failed", "event", "metrics_write_failed", "path", p, "error", err)
		}
	}
}

func cmdCleanup(ctx context.Context, c *cli.Command, rt *runtime) error {
	dry := c.Bool("dry-run")
	rep, err := rt.eng.Cleanup(ctx, backup.Selection{}, dry)
	if err == nil && c.Bool("release-bookmarks") {
		rep.ReleasedBookmarks, err = rt.eng.ReleaseBookmarks(ctx, dry)
	}
	emitJSON(rt.stdout, rep)
	if err != nil {
		rt.log.Error("cleanup_failed", "event", "cleanup_failed", "error", err)
		return cli.Exit("", exitPartial)
	}
	return nil
}

func cmdStatus(_ context.Context, c *cli.Command, rt *runtime) error {
	st, err := backup.LoadState(rt.cfg.Storage.StateDirectory)
	if err != nil {
		return cli.Exit("error: "+err.Error(), exitFailed)
	}
	maxAge := c.Duration("max-age")
	var problems []string
	if st.LastRun == nil {
		problems = append(problems, "no backup run recorded yet")
	} else if st.LastRun.ExitCode != backup.ExitOK {
		problems = append(problems, fmt.Sprintf("last run %s exited with %d: %s", st.LastRun.RunID, st.LastRun.ExitCode, st.LastRun.Error))
	}
	grace := c.Duration("grace")
	keys := make([]string, 0, len(st.Targets))
	for k := range st.Targets {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		t := st.Targets[k]
		if by := t.ExpectedBy(); !by.IsZero() && time.Now().After(by.Add(grace)) {
			last := "never"
			if !t.LastSuccess.IsZero() {
				last = t.LastSuccess.Local().Format(time.RFC3339)
			}
			problems = append(problems, fmt.Sprintf("%s %s/%s is overdue: last success %s, schedule %q expected one by %s",
				t.Kind, t.Project, t.Name, last, t.Schedule, by.Local().Format(time.RFC3339)))
		}
	}
	if !st.LastSuccess.IsZero() && time.Since(st.LastSuccess) > maxAge {
		problems = append(problems, fmt.Sprintf("last successful backup is %s old (max %s)", time.Since(st.LastSuccess).Round(time.Minute), maxAge))
	} else if st.LastSuccess.IsZero() && st.LastRun != nil {
		problems = append(problems, "no successful backup recorded")
	}
	done := func() error {
		if len(problems) > 0 {
			return cli.Exit("", exitFailed)
		}
		return nil
	}
	if c.Bool("json") {
		_ = json.NewEncoder(rt.stdout).Encode(map[string]any{"ok": len(problems) == 0, "problems": problems, "state": st})
		return done()
	}
	out := rt.stdout
	if r := st.LastRun; r != nil {
		fmt.Fprintf(out, "last run:      %s  exit=%d  complete=%v  %.0fs  uploaded %s of %s\n", r.Start.Local().Format(time.RFC3339),
			r.ExitCode, r.Complete, r.DurationSeconds, bytesStr(r.UploadedBytes), bytesStr(r.LogicalBytes))
	}
	if !st.LastSuccess.IsZero() {
		fmt.Fprintf(out, "last success:  %s  (%s)  %s\n", st.LastSuccess.Local().Format(time.RFC3339),
			humanize.Time(st.LastSuccess), st.LastSuccessSnapshot)
	}
	fmt.Fprintln(out)
	tbl := newTable(out, "KIND", "PROJECT", "NAME", "SCHEDULE", "LAST STATUS", "LAST SUCCESS", "NEXT", "ERROR")
	stamp := func(t time.Time, zero string) string {
		if t.IsZero() {
			return zero
		}
		return t.Local().Format("2006-01-02 15:04")
	}
	for _, k := range keys {
		t := st.Targets[k]
		tbl.AppendRow([]any{t.Kind, t.Project, t.Name, dash(t.Schedule), t.LastStatus, stamp(t.LastSuccess, "never"), stamp(t.NextDue, "-"), dash(t.LastError)})
	}
	tbl.Render()
	if len(st.RestoreTests) > 0 {
		fmt.Fprintln(out)
		rtbl := newTable(out, "RESTORE DRILL", "LAST RUN", "RESULT", "LAST SUCCESS", "SNAPSHOT", "ERROR")
		for _, k := range st.RestoreTestKeys() {
			r := st.RestoreTests[k]
			result := "ok"
			if !r.LastOK {
				result = "FAILED"
			}
			errLine, _, _ := strings.Cut(r.LastError, "\n")
			rtbl.AppendRow([]any{fmt.Sprintf("%s %s/%s", r.Kind, r.Project, r.Name), stamp(r.LastRun, "-"), result,
				stamp(r.LastSuccess, "never"), dash(r.Snapshot), dash(errLine)})
		}
		rtbl.Render()
	}
	for _, p := range problems {
		fmt.Fprintln(out, "PROBLEM:", p)
	}
	return done()
}

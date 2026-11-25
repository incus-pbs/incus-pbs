package backup

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/incus-pbs/incus-pbs/internal/config"
	"github.com/incus-pbs/incus-pbs/internal/incus"
)

func setSchedule(f *fakeHost, name, expr string) {
	for i := range f.instances["default"] {
		if in := &f.instances["default"][i]; in.Name == name {
			in.Config = map[string]string{incus.ScheduleKey: expr}
		}
	}
}

func utcDefault(c *config.Config) { c.Backup.Schedule = "CRON_TZ=UTC 30 2 * * *" }

func TestDiscoverSchedules(t *testing.T) {
	f := newFakeHost(t)
	f.volumes["default"][1].UsedBy = []string{"/1.0/instances/v1"} // rawdisk: only v1
	setSchedule(f, "c1", "*/10 * * * *")
	setSchedule(f, "c2", "not a cron")
	setSchedule(f, "v1", "@hourly")
	e := newEngine(t, f, utcDefault)
	plan, err := Discover(f, e.Cfg, Selection{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, tg := range plan.Targets {
		got[tg.Name] = tg.ScheduleSource + " " + tg.Schedule
	}
	want := map[string]string{
		"c1": "instance */10 * * * *", "c2": "invalid CRON_TZ=UTC 30 2 * * *", "v1": "instance @hourly",
		"rawdisk": "owner @hourly", "shared": "config CRON_TZ=UTC 30 2 * * *", "standalone": "config CRON_TZ=UTC 30 2 * * *",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: got %q want %q", k, got[k], v)
		}
	}
	if !slices.ContainsFunc(plan.Warnings, func(w string) bool { return strings.Contains(w, "c2") && strings.Contains(w, incus.ScheduleKey) }) {
		t.Errorf("invalid schedule must warn: %v", plan.Warnings)
	}
}

func TestSelectDue(t *testing.T) {
	f := newFakeHost(t)
	f.volumes["default"][1].UsedBy = []string{"/1.0/instances/v1"}
	setSchedule(f, "c1", "*/10 * * * *")
	setSchedule(f, "c2", "off")
	e := newEngine(t, f, utcDefault)
	plan, err := Discover(f, e.Cfg, Selection{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 24, 12, 5, 0, 0, time.UTC)
	st := &State{Targets: map[string]TargetState{}, HostLastAttempt: now.Add(-time.Hour)}
	for _, tg := range plan.Targets {
		st.Targets[TargetKey(tg.Kind, tg.Project, tg.Name)] = TargetState{LastAttempt: now.Add(-time.Hour)}
	}
	names := func(d Due) []string {
		var out []string
		for _, tg := range d.Plan.Targets {
			out = append(out, tg.Name)
		}
		slices.Sort(out)
		return out
	}

	d, err := SelectDue(plan, st, e.Cfg.Backup.Schedule, now)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(names(d), []string{"c1"}) || d.Host {
		t.Fatalf("only c1 is due: %v host=%v", names(d), d.Host)
	}
	if !slices.ContainsFunc(d.Plan.Excluded, func(x Excluded) bool { return strings.Contains(x.What, "c2") && strings.Contains(x.Reason, "off") }) {
		t.Errorf("off must be listed as excluded: %+v", d.Plan.Excluded)
	}

	// v1 never attempted: due at once, its only-attached volume with it.
	delete(st.Targets, TargetKey(KindVM, "default", "v1"))
	d, _ = SelectDue(plan, st, e.Cfg.Backup.Schedule, now)
	if !slices.Equal(names(d), []string{"c1", "rawdisk", "v1"}) || d.Host {
		t.Fatalf("new v1 with its volume: %v host=%v", names(d), d.Host)
	}

	// A new standalone volume pulls the host content forward.
	delete(st.Targets, TargetKey(KindVolume, "default", "standalone"))
	d, _ = SelectDue(plan, st, e.Cfg.Backup.Schedule, now)
	if !d.Host || !slices.Contains(names(d), "shared") || !slices.Contains(names(d), "standalone") {
		t.Fatalf("host content due: %v host=%v", names(d), d.Host)
	}

	// Nothing due.
	for k, v := range st.Targets {
		v.LastAttempt = now
		st.Targets[k] = v
	}
	st.Targets[TargetKey(KindVM, "default", "v1")] = TargetState{LastAttempt: now}
	st.Targets[TargetKey(KindVolume, "default", "standalone")] = TargetState{LastAttempt: now}
	st.HostLastAttempt = now
	if d, _ = SelectDue(plan, st, e.Cfg.Backup.Schedule, now.Add(time.Minute)); !d.Empty() || d.Deferred == 0 {
		t.Fatalf("nothing due: %v host=%v deferred=%d", names(d), d.Host, d.Deferred)
	}
}

func TestScheduledRuns(t *testing.T) {
	f := newFakeHost(t)
	setSchedule(f, "c1", "*/10 * * * *")
	e := newEngine(t, f, utcDefault)
	now := time.Date(2026, 11, 24, 12, 1, 0, 0, time.UTC)
	e.Now = func() time.Time { return now }
	run := func() *Report {
		t.Helper()
		f.calls, f.guest = nil, nil
		rep, err := e.Run(context.Background(), Selection{Scheduled: true})
		if err != nil {
			t.Fatalf("run at %v: %v", now, err)
		}
		if !rep.NothingDue {
			st, _ := LoadState(e.Cfg.Storage.StateDirectory)
			st.Record(rep)
			if err := st.Save(e.Cfg.Storage.StateDirectory); err != nil {
				t.Fatal(err)
			}
		}
		return rep
	}
	groups := func() []string {
		var g []string
		for _, c := range f.calls {
			g = append(g, c.group)
		}
		return g
	}

	// First tick: no history, everything is due.
	if rep := run(); rep.NothingDue || !rep.HostContent || !slices.Equal(groups(), []string{"ct/c1", "ct/c2", "vm/v1", "host/h1"}) {
		t.Fatalf("first run: %v %+v", groups(), rep)
	}
	// Five minutes later: nothing.
	now = now.Add(5 * time.Minute)
	if rep := run(); !rep.NothingDue || len(f.calls) != 0 {
		t.Fatalf("second run must do nothing: %v", groups())
	}
	// 12:10 slot passed: only c1, without a host index.
	now = now.Add(5 * time.Minute)
	rep := run()
	if rep.NothingDue || rep.HostContent || rep.PBSSnapshot != "" || rep.ExitCode != ExitOK || !slices.Equal(groups(), []string{"ct/c1"}) {
		t.Fatalf("c1 only: %v %+v", groups(), rep)
	}
	if !strings.Contains(f.guest["ct/c1"], `"scheduled": true`) {
		t.Errorf("guest manifest of a scheduled run: %s", f.guest["ct/c1"])
	}
	// Host off until 03:00 the next day: every missed slot fires once.
	now = time.Date(2026, 11, 25, 3, 0, 0, 0, time.UTC)
	run()
	if !slices.Equal(groups(), []string{"ct/c1", "ct/c2", "vm/v1", "host/h1"}) {
		t.Fatalf("catch-up: %v", groups())
	}
	now = now.Add(5 * time.Minute)
	if rep := run(); !rep.NothingDue {
		t.Fatalf("caught up, nothing due: %v", groups())
	}
	st, _ := LoadState(e.Cfg.Storage.StateDirectory)
	if ts := st.Targets[TargetKey(KindContainer, "default", "c1")]; ts.Schedule != "*/10 * * * *" ||
		!ts.NextDue.Equal(time.Date(2026, 11, 25, 3, 10, 0, 0, time.UTC)) {
		t.Errorf("state for c1: %+v", ts)
	}
	f.assertClean(t)
}

func TestScheduledRunSkipsHooksOfOtherInstances(t *testing.T) {
	f := newFakeHost(t)
	setSchedule(f, "c1", "*/10 * * * *")
	e := newEngine(t, f, func(c *config.Config) {
		utcDefault(c)
		c.Hooks.PreBackup = []config.Hook{
			{Name: "c1", Instance: "c1", Command: []string{"true"}},
			{Name: "v1", Instance: "v1", Command: []string{"true"}},
			{Name: "host", Command: []string{"/hooks/ok"}},
		}
	})
	now := time.Date(2026, 11, 24, 12, 11, 0, 0, time.UTC)
	e.Now = func() time.Time { return now }
	st := &State{Targets: map[string]TargetState{}, HostLastAttempt: now.Add(-time.Minute)}
	for _, n := range []string{"c2", "v1"} {
		k := KindContainer
		if n == "v1" {
			k = KindVM
		}
		st.Targets[TargetKey(k, "default", n)] = TargetState{LastAttempt: now.Add(-time.Minute)}
	}
	for _, v := range []string{"shared", "rawdisk", "standalone"} {
		st.Targets[TargetKey(KindVolume, "default", v)] = TargetState{LastAttempt: now.Add(-time.Minute)}
	}
	if err := st.Save(e.Cfg.Storage.StateDirectory); err != nil {
		t.Fatal(err)
	}
	rep, err := e.Run(context.Background(), Selection{Scheduled: true})
	if err != nil {
		t.Fatal(err)
	}
	status := map[string]string{}
	for _, h := range rep.Hooks {
		status[h.Name] = h.Status
	}
	if status["c1"] != "ok" || status["v1"] != "skipped" || status["host"] != "ok" {
		t.Fatalf("hooks: %+v", rep.Hooks)
	}
	if f.indexOf("exec default/v1") >= 0 {
		t.Error("hook of an instance outside the run must not execute")
	}
	if f.count("hook ") != 1 {
		t.Errorf("host hook must run once: %v", f.events)
	}
}

func TestStateRecordsSchedulesAndForgetsGoneTargets(t *testing.T) {
	start := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	st := &State{Targets: map[string]TargetState{"container/default/gone": {Name: "gone"}}}
	st.Record(&Report{Start: start, ExitCode: ExitOK, Known: []string{"container/default/web"},
		Items: []ItemResult{{Kind: KindContainer, Project: "default", Name: "web", Status: "ok", Schedule: "CRON_TZ=UTC 0 */6 * * *"}}})
	if _, ok := st.Targets["container/default/gone"]; ok {
		t.Error("targets missing from a complete plan must be forgotten")
	}
	web := st.Targets["container/default/web"]
	if !web.LastAttempt.Equal(start) || !web.NextDue.Equal(start.Add(6*time.Hour)) || !web.ExpectedBy().Equal(start.Add(6*time.Hour)) {
		t.Fatalf("web: %+v expected by %v", web, web.ExpectedBy())
	}
	if !st.HostLastAttempt.IsZero() {
		t.Error("a run without host content must not move the host slot")
	}
	m := st.Metrics()
	for _, want := range []string{
		`incus_pbs_target_next_due_timestamp_seconds{kind="container",project="default",name="web"} 1795543200`,
		`incus_pbs_target_expected_by_timestamp_seconds{kind="container",project="default",name="web"} 1795543200`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("metrics miss %s:\n%s", want, m)
		}
	}
}

func TestScheduledHostOnlyRun(t *testing.T) {
	f := newFakeHost(t)
	e := newEngine(t, f, func(c *config.Config) { utcDefault(c); c.Backup.IncludeCustomVolumes = false })
	now := time.Date(2026, 11, 24, 2, 31, 0, 0, time.UTC)
	e.Now = func() time.Time { return now }
	st := &State{Targets: map[string]TargetState{}, HostLastAttempt: now.Add(-24 * time.Hour)}
	for _, in := range f.instances["default"] {
		k := KindContainer
		if in.IsVM() {
			k = KindVM
		}
		setSchedule(f, in.Name, "@weekly")
		st.Targets[TargetKey(k, "default", in.Name)] = TargetState{LastAttempt: now.Add(-time.Hour)}
	}
	if err := st.Save(e.Cfg.Storage.StateDirectory); err != nil {
		t.Fatal(err)
	}
	rep, err := e.Run(context.Background(), Selection{Scheduled: true})
	if err != nil || rep.ExitCode != ExitOK || !rep.HostContent || rep.PBSSnapshot != "host/h1/2026-11-24T02:30:00Z" || len(rep.Items) != 0 {
		t.Fatalf("host-only run: %v %+v", err, rep)
	}
	if len(f.calls) != 1 || f.calls[0].group != "host/h1" || f.count("snapshot ") != 0 {
		t.Fatalf("only the run index: %+v %v", f.calls, f.events)
	}
}

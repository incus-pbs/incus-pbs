package backup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/incus-pbs/incus-pbs/internal/config"
)

func hook(name string, cmd ...string) config.Hook {
	return config.Hook{Name: name, Command: cmd}
}

func validated(t *testing.T, e *Engine) {
	t.Helper()
	if err := e.Cfg.Validate(); err != nil { // applies hook defaults
		t.Fatal(err)
	}
}

func TestCrashRecoveryAutoCleanup(t *testing.T) {
	f := newFakeHost(t)
	e := newEngine(t, f, nil)
	f.addSnap("i:default/c1", "pbs-backup-20261123-023000")
	f.addSnap("i:default/c1", "user-keepme")
	f.clones["tank/incus-pbs-staging/vm_default_v1"] = true
	f.mounts[e.Cfg.Storage.StagingDirectory+"/mnt/ct_default_c1"] = true

	rep, err := e.Run(context.Background(), Selection{})
	if err != nil || rep.ExitCode != ExitOK {
		t.Fatalf("run with crash recovery failed: %v", err)
	}
	if rep.CrashRecovery == nil || len(rep.CrashRecovery.DestroyedClones) != 1 || len(rep.CrashRecovery.Unmounted) != 1 {
		t.Fatalf("crash recovery not recorded: %+v", rep.CrashRecovery)
	}
	// user snapshot survives
	if got := f.snapshots["i:default/c1"]; len(got) != 1 || got[0] != "user-keepme" {
		t.Fatalf("user snapshot touched: %v", got)
	}
}

func TestHooksOrderAndEnvironment(t *testing.T) {
	f := newFakeHost(t)
	e := newEngine(t, f, nil)
	e.Cfg.Hooks = config.Hooks{
		PreBackup:    []config.Hook{hook("freeze", "/hooks/ok", "freeze"), {Name: "flush-db", Instance: "c1", Command: []string{"mysql", "-e", "FLUSH TABLES"}}},
		PostSnapshot: []config.Hook{hook("thaw", "/hooks/ok", "thaw")},
		PostBackup:   []config.Hook{hook("notify", "/hooks/ok", "notify")},
	}
	validated(t, e)
	rep, err := e.Run(context.Background(), Selection{})
	if err != nil {
		t.Fatal(err)
	}
	iFreeze, iExec, iThaw := f.indexOf("hook freeze"), f.indexOf("exec default/c1 mysql"), f.indexOf("hook thaw")
	iSnapFirst, iSnapLast := f.indexOf("snapshot "), -1
	for i, ev := range f.events {
		if strings.HasPrefix(ev, "snapshot ") {
			iSnapLast = i
		}
	}
	iUpload, iNotify := f.indexOf("pbc backup"), f.indexOf("hook notify")
	if !(iFreeze < iExec && iExec < iSnapFirst && iSnapLast < iThaw && iThaw < iUpload && iUpload < iNotify) {
		t.Fatalf("hook order wrong: %v", f.events)
	}
	if !strings.Contains(f.events[iNotify], "phase=post_backup") || !strings.Contains(f.events[iExec], "phase=pre_backup") {
		t.Fatalf("phase env missing: %v", f.events)
	}
	if len(rep.Hooks) != 4 {
		t.Fatalf("hook results: %+v", rep.Hooks)
	}
	// post_backup runs after cleanup
	if f.indexOf("delete-snapshot") > iNotify {
		t.Fatalf("post_backup must run after cleanup: %v", f.events)
	}
}

func TestPreBackupHookFailureAbortsButThaws(t *testing.T) {
	f := newFakeHost(t)
	e := newEngine(t, f, nil)
	e.Cfg.Hooks = config.Hooks{
		PreBackup:    []config.Hook{hook("freeze", "/hooks/fail")},
		PostSnapshot: []config.Hook{hook("thaw", "/hooks/ok", "thaw")},
		PostBackup:   []config.Hook{hook("notify", "/hooks/ok", "notify")},
	}
	validated(t, e)
	rep, err := e.Run(context.Background(), Selection{})
	if err == nil || rep.ExitCode != ExitFailed {
		t.Fatalf("pre hook failure must abort: %v", err)
	}
	if f.count("snapshot ") != 0 {
		t.Fatal("no snapshots after failed pre hook")
	}
	if f.indexOf("hook thaw") < 0 {
		t.Fatal("post_snapshot (thaw) must run even when pre_backup failed")
	}
	if !strings.Contains(f.events[f.indexOf("hook notify")], "phase=post_backup") {
		t.Fatal("post_backup must run on failure too")
	}
	f.assertClean(t)
}

func TestHookContinueAndStoppedInstanceSkipped(t *testing.T) {
	f := newFakeHost(t)
	e := newEngine(t, f, nil)
	e.Cfg.Hooks = config.Hooks{PreBackup: []config.Hook{
		{Name: "soft", Command: []string{"/hooks/fail"}, OnFailure: "continue"},
		{Name: "stopped", Instance: "c2", Command: []string{"true"}},
	}}
	validated(t, e)
	rep, err := e.Run(context.Background(), Selection{})
	if err != nil || rep.ExitCode != ExitOK {
		t.Fatalf("continue hook must not fail the run: %v", err)
	}
	if rep.Hooks[0].Status != "failed" || rep.Hooks[1].Status != "skipped" || f.indexOf("exec default/c2") >= 0 {
		t.Fatalf("hooks: %+v events=%v", rep.Hooks, f.events)
	}
}

func TestInterruptStillThaws(t *testing.T) {
	f := newFakeHost(t)
	e := newEngine(t, f, nil)
	e.Cfg.Hooks = config.Hooks{PostSnapshot: []config.Hook{hook("thaw", "/hooks/ok", "thaw")}}
	validated(t, e)
	ctx, cancel := context.WithCancel(context.Background())
	f.failOn["snapshot default/v1"] = true
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	f.pbcHang = true
	rep, err := e.Run(ctx, Selection{})
	if err == nil || rep.ExitCode != ExitFailed {
		t.Fatalf("expected failure, got %v", err)
	}
	if f.indexOf("hook thaw") < 0 {
		t.Fatalf("thaw must run: %v", f.events)
	}
	delete(f.failOn, "snapshot default/v1")
	f.assertClean(t)
}

func TestExcludeAndRatePassedToClient(t *testing.T) {
	f := newFakeHost(t)
	e := newEngine(t, f, func(c *config.Config) {
		c.PBS.ExcludePatterns = []string{"/rootfs/tmp/*", "/rootfs/var/cache/apt/archives/*.deb"}
		c.PBS.RateLimit = "50MiB"
	})
	if _, err := e.Run(context.Background(), Selection{}); err != nil {
		t.Fatal(err)
	}
	args := strings.Join(f.pbcArgs, " ")
	if !strings.Contains(args, "--exclude /rootfs/tmp/*") || !strings.Contains(args, "--rate 50MiB") {
		t.Fatalf("args: %s", args)
	}
}

func TestStateAndMetrics(t *testing.T) {
	f := newFakeHost(t)
	e := newEngine(t, f, nil)
	dir := t.TempDir()
	rep, err := e.Run(context.Background(), Selection{})
	if err != nil {
		t.Fatal(err)
	}
	st, _ := LoadState(dir)
	st.Record(rep)
	if err := st.Save(dir); err != nil {
		t.Fatal(err)
	}
	st2, err := LoadState(dir)
	if err != nil || st2.LastSuccess.IsZero() || st2.LastSuccessSnapshot != rep.PBSSnapshot || len(st2.Targets) != 6 {
		t.Fatalf("state roundtrip: %+v %v", st2, err)
	}
	// A failed run keeps the previous success time and target success times.
	failed := &Report{RunID: "x", Start: time.Now(), ExitCode: ExitFailed,
		Items: []ItemResult{{Kind: KindContainer, Project: "default", Name: "c1", Status: "failed", Error: "boom"}}}
	st2.Record(failed)
	c1 := st2.Targets["container/default/c1"]
	if st2.LastSuccess.IsZero() || c1.LastSuccess.IsZero() || c1.LastStatus != "failed" || c1.LastError != "boom" {
		t.Fatalf("failure handling: %+v", c1)
	}
	prom := filepath.Join(dir, "incus_pbs.prom")
	if err := st2.WriteMetrics(prom); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(prom)
	for _, want := range []string{
		"incus_pbs_last_run_exit_code 1",
		"incus_pbs_last_run_complete 0",
		`incus_pbs_last_run_targets{kind="container",status="failed"} 1`,
		`incus_pbs_target_last_success_timestamp_seconds{kind="custom-volume",project="default",name="shared"}`,
		"# TYPE incus_pbs_last_success_timestamp_seconds gauge",
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("metrics missing %q:\n%s", want, b)
		}
	}
}

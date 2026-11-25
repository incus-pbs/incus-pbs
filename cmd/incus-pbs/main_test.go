package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/incus-pbs/incus-pbs/internal/backup"
	"github.com/incus-pbs/incus-pbs/internal/lock"
)

func invoke(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(context.Background(), append([]string{"incus-pbs"}, args...), &out, &errb)
	return code, out.String(), errb.String()
}

// testConfig writes a valid config whose Incus socket does not exist.
func testConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	cfg := `incus:
  socket: ` + filepath.Join(dir, "missing.sock") + `
pbs:
  repository: "backup@pbs!t@pbs:ds"
  password_file: ` + filepath.Join(dir, "token") + `
storage:
  staging_directory: ` + filepath.Join(dir, "cache", "staging") + `
  state_directory: ` + filepath.Join(dir, "state") + `
backup:
  lock_file: ` + filepath.Join(dir, "lock") + `
`
	if err := os.WriteFile(p, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestHelpAndVersionNeedNoConfig(t *testing.T) {
	code, out, _ := invoke(t, "--config", "/nonexistent", "--help")
	if code != 0 || !strings.Contains(out, "backup") || !strings.Contains(out, "restore") || !strings.Contains(out, "Exit codes") {
		t.Fatalf("help: %d %q", code, out)
	}
	code, out, _ = invoke(t, "--config", "/nonexistent", "version")
	if code != 0 || !strings.HasPrefix(out, "incus-pbs dev") {
		t.Fatalf("version: %d %q", code, out)
	}
	code, out, _ = invoke(t, "restore", "mount", "--help")
	if code != 0 || !strings.Contains(out, "--partition") || !strings.Contains(out, "<snapshot> <instance|volume>") {
		t.Fatalf("subcommand help: %d %q", code, out)
	}
}

func TestRestoreTestUsage(t *testing.T) {
	code, out, _ := invoke(t, "restore", "test", "--help")
	if code != 0 || !strings.Contains(out, "--contains") || !strings.Contains(out, "--keep") || !strings.Contains(out, "<snapshot> <instance>") {
		t.Fatalf("help: %d %q", code, out)
	}
	cfg := testConfig(t)
	for _, args := range [][]string{
		{"restore", "test", "latest", "web"},
		{"restore", "test", "latest", "--file", "/a"},
		{"restore", "test", "latest", "web", "--file", "rel/path"},
		{"restore", "test", "latest", "web", "--file", "/a", "--file", "/b", "--contains", "x"},
	} {
		if code, _, errs := invoke(t, append([]string{"-c", cfg}, args...)...); code != exitUsage {
			t.Errorf("%v: want usage error, got %d %q", args, code, errs)
		}
	}
}

func TestUsageErrors(t *testing.T) {
	if code, _, errs := invoke(t, "frobnicate"); code != exitUsage || !strings.Contains(errs, "unknown command") {
		t.Fatalf("unknown command: %d %q", code, errs)
	}
	if code, _, _ := invoke(t); code != exitUsage {
		t.Fatalf("no command: %d", code)
	}
	cfg := testConfig(t)
	// required flag missing -> parser error before anything runs
	if code, _, errs := invoke(t, "-c", cfg, "restore", "mount", "latest", "web"); code != exitUsage || !strings.Contains(errs, "dir") {
		t.Fatalf("missing --dir: %d %q", code, errs)
	}
	// wrong number of positional arguments, checked after config load but before Incus
	if code, _, errs := invoke(t, "-c", cfg, "restore", "instance", "latest"); code != exitUsage || !strings.Contains(errs, "expected arguments") {
		t.Fatalf("missing instance arg must fail: %d %q", code, errs)
	}
	if code, _, errs := invoke(t, "-c", cfg, "backup", "--bogus"); code != exitUsage || !strings.Contains(errs, "bogus") {
		t.Fatalf("unknown flag: %d %q", code, errs)
	}
}

func TestConfigErrors(t *testing.T) {
	code, _, errs := invoke(t, "--config", "/nonexistent/config.yaml", "check")
	if code != exitConfig || !strings.Contains(errs, "reading config") {
		t.Fatalf("missing config: %d %q", code, errs)
	}
	t.Setenv("INCUS_PBS_CONFIG", "/also/nonexistent.yaml")
	code, _, errs = invoke(t, "status")
	if code != exitConfig || !strings.Contains(errs, "/also/nonexistent.yaml") {
		t.Fatalf("env var config: %d %q", code, errs)
	}
	cfg := testConfig(t)
	if code, _, errs := invoke(t, "-c", cfg, "--log-level", "loud", "status"); code != exitConfig || !strings.Contains(errs, "logging.level") {
		t.Fatalf("invalid log level override: %d %q", code, errs)
	}
}

func TestFlagsAfterArgumentsAndIncusErrors(t *testing.T) {
	cfg := testConfig(t)
	// Flags after positional args parse fine; the run then fails at the Incus connection (exit 1).
	code, _, errs := invoke(t, "restore", "instance", "latest", "web", "--name", "web2", "--identity", "new", "-c", cfg)
	if code != exitFailed || !strings.Contains(errs, "incus_connect_failed") {
		t.Fatalf("interleaved flags: %d %q", code, errs)
	}
}

func TestStatusWithoutHistory(t *testing.T) {
	cfg := testConfig(t)
	code, out, _ := invoke(t, "-c", cfg, "status")
	if code != exitFailed || !strings.Contains(out, "no backup run recorded yet") {
		t.Fatalf("status: %d %q", code, out)
	}
	code, out, _ = invoke(t, "-c", cfg, "status", "--json", "--max-age", "1h")
	if code != exitFailed || !strings.Contains(out, `"ok":false`) {
		t.Fatalf("status json: %d %q", code, out)
	}
}

func TestScheduledBackupUsage(t *testing.T) {
	cfg := testConfig(t)
	if code, _, errs := invoke(t, "-c", cfg, "backup", "--scheduled", "--instance", "web"); code != exitUsage || !strings.Contains(errs, "cannot be combined") {
		t.Fatalf("--scheduled with --instance: %d %q", code, errs)
	}
	if code, out, _ := invoke(t, "backup", "--help"); code != 0 || !strings.Contains(out, "--scheduled") {
		t.Fatalf("help: %d %q", code, out)
	}
}

func TestScheduledBackupLockHeldIsQuiet(t *testing.T) {
	dir := t.TempDir()
	lockFile := filepath.Join(dir, "lock")
	held, err := lock.Acquire(lockFile)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	var logs bytes.Buffer
	rt := &runtime{log: slog.New(slog.NewJSONHandler(&logs, nil))}
	rt.cfg.Backup.LockFile = lockFile
	ran := false
	act := scheduledLock(locked(func(context.Context, *cli.Command, *runtime) error { ran = true; return nil }))
	for _, tc := range []struct {
		args []string
		code int
	}{{[]string{"x", "--scheduled"}, exitOK}, {[]string{"x"}, exitLocked}} {
		cmd := &cli.Command{Name: "x", Flags: []cli.Flag{&cli.BoolFlag{Name: "scheduled"}},
			ExitErrHandler: func(context.Context, *cli.Command, error) {},
			Action:         func(ctx context.Context, c *cli.Command) error { return act(ctx, c, rt) }}
		err := cmd.Run(context.Background(), tc.args)
		code := exitOK
		var ec cli.ExitCoder
		if errors.As(err, &ec) {
			code = ec.ExitCode()
		}
		if code != tc.code || ran {
			t.Errorf("%v: exit %d (want %d), ran=%v", tc.args, code, tc.code, ran)
		}
	}
	if !strings.Contains(logs.String(), "scheduled_run_skipped") {
		t.Errorf("logs: %s", logs.String())
	}
}

func TestStatusReportsOverdueTargets(t *testing.T) {
	cfg := testConfig(t)
	stateDir := filepath.Join(filepath.Dir(cfg), "state")
	now := time.Now()
	st := &backup.State{LastRun: &backup.Report{Start: now.Add(-10 * time.Minute)}, LastSuccess: now.Add(-10 * time.Minute),
		Targets: map[string]backup.TargetState{
			"container/default/web": {Kind: backup.KindContainer, Project: "default", Name: "web", Schedule: "@hourly",
				LastStatus: "failed", LastSuccess: now.Add(-5 * time.Hour)},
			"container/default/db": {Kind: backup.KindContainer, Project: "default", Name: "db", Schedule: "@daily",
				LastStatus: "ok", LastSuccess: now.Add(-10 * time.Minute)},
			// Never succeeded: overdue from the first attempt on.
			"container/default/new": {Kind: backup.KindContainer, Project: "default", Name: "new", Schedule: "@hourly",
				LastStatus: "failed", FirstSeen: now.Add(-3 * time.Hour)},
		}}
	if err := st.Save(stateDir); err != nil {
		t.Fatal(err)
	}
	code, out, _ := invoke(t, "-c", cfg, "status")
	if code != exitFailed || !strings.Contains(out, "container default/web is overdue") || strings.Contains(out, "default/db is overdue") ||
		!strings.Contains(out, "container default/new is overdue: last success never") {
		t.Fatalf("status: %d %q", code, out)
	}
	if code, out, _ := invoke(t, "-c", cfg, "status", "--grace", "24h"); code != exitOK {
		t.Fatalf("within grace: %d %q", code, out)
	}
}

func TestShellCompletion(t *testing.T) {
	for _, shell := range []string{"bash", "zsh"} {
		code, out, errOut := invoke(t, "--config", "/nonexistent", "completion", shell)
		if code != 0 || errOut != "" || !strings.Contains(out, "incus-pbs") || !strings.Contains(out, "--generate-shell-completion") {
			t.Fatalf("completion %s: %d %q %q", shell, code, out, errOut)
		}
	}
	code, out, _ := invoke(t, "--help")
	if code != 0 || !strings.Contains(out, "completion") {
		t.Fatalf("completion command not listed in help: %d %q", code, out)
	}

	cases := []struct {
		args []string
		want []string
	}{
		{nil, []string{"backup", "restore", "status"}},
		{[]string{"restore"}, []string{"instance", "mount"}},
		{[]string{"backup", "-"}, []string{"--scheduled", "--instance"}},
		// Partial positional input must not trip the argument validators.
		{[]string{"restore", "instance", "latest"}, nil},
		{[]string{"restore", "test", "latest"}, nil},
		{[]string{"restore", "list", "a", "b"}, nil},
	}
	// urfave/cli reads os.Args (not the Run args) when completing at the root.
	oldArgs := os.Args
	t.Cleanup(func() { os.Args = oldArgs })
	for _, tc := range cases {
		args := append(append([]string{"--config", "/nonexistent"}, tc.args...), completionFlag)
		os.Args = append([]string{"incus-pbs"}, args...)
		code, out, errOut := invoke(t, args...)
		if code != 0 || errOut != "" {
			t.Fatalf("%v: code %d stderr %q", tc.args, code, errOut)
		}
		for _, w := range tc.want {
			if !strings.Contains(out, w) {
				t.Errorf("%v: %q missing from %q", tc.args, w, out)
			}
		}
	}
}

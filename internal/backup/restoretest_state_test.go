package backup

import (
	"strings"
	"testing"
	"time"
)

func TestRecordRestoreTest(t *testing.T) {
	t1 := time.Date(2026, 11, 22, 6, 0, 0, 0, time.UTC)
	t2 := t1.Add(7 * 24 * time.Hour)
	t3 := t2.Add(7 * 24 * time.Hour)
	st := &State{Targets: map[string]TargetState{}}

	st.RecordRestoreTest(RestoreTestState{Kind: KindContainer, Project: "default", Name: "web", LastRun: t1, LastOK: true,
		Snapshot: "ct/web/2026-11-22T02:30:00Z", DurationSeconds: 41.5, BootSeconds: 3.25})
	web := st.RestoreTests["container/default/web"]
	if !web.LastSuccess.Equal(t1) || !web.LastOK {
		t.Fatalf("success: %+v", web)
	}

	// A failure keeps the last success and the error.
	st.RecordRestoreTest(RestoreTestState{Kind: KindContainer, Project: "default", Name: "web", LastRun: t2,
		LastError: "file checks failed:\n  - /etc/hostname: does not exist"})
	web = st.RestoreTests["container/default/web"]
	if web.LastOK || !web.LastSuccess.Equal(t1) || !web.LastRun.Equal(t2) || !strings.Contains(web.LastError, "does not exist") {
		t.Fatalf("failure: %+v", web)
	}

	// A drill that failed before the backup was found (no kind/project)
	// updates the existing entry instead of creating an orphan.
	st.RecordRestoreTest(RestoreTestState{Name: "web", LastRun: t3, LastError: strings.Repeat("x", 5000)})
	if len(st.RestoreTests) != 1 {
		t.Fatalf("early failure must update the existing entry: %+v", st.RestoreTests)
	}
	web = st.RestoreTests["container/default/web"]
	if web.Kind != KindContainer || web.Project != "default" || !web.LastRun.Equal(t3) || !web.LastSuccess.Equal(t1) || len(web.LastError) > 1100 {
		t.Fatalf("early failure: %+v", web)
	}

	// An unknown name without history is not recorded at all.
	st.RecordRestoreTest(RestoreTestState{Name: "typo", LastRun: t3, LastError: "no backups"})
	if len(st.RestoreTests) != 1 {
		t.Fatalf("an unresolvable drill must not create an entry: %+v", st.RestoreTests)
	}

	m := st.Metrics()
	for _, want := range []string{
		"# TYPE incus_pbs_restore_test_last_ok gauge",
		`incus_pbs_restore_test_last_ok{kind="container",project="default",name="web"} 0`,
		`incus_pbs_restore_test_last_success_timestamp_seconds{kind="container",project="default",name="web"} 1795327200`,
		`incus_pbs_restore_test_last_run_timestamp_seconds{kind="container",project="default",name="web"} 1796536800`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("metrics miss %s:\n%s", want, m)
		}
	}

	// Success clears the error again.
	st.RecordRestoreTest(RestoreTestState{Kind: KindContainer, Project: "default", Name: "web", LastRun: t3.Add(time.Hour), LastOK: true})
	if web = st.RestoreTests["container/default/web"]; web.LastError != "" || !web.LastSuccess.Equal(t3.Add(time.Hour)) {
		t.Fatalf("recovery: %+v", web)
	}
}

func TestRecordForgetsRestoreTestsOfGoneInstances(t *testing.T) {
	st := &State{Targets: map[string]TargetState{}}
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	for _, n := range []string{"web", "gone"} {
		st.RecordRestoreTest(RestoreTestState{Kind: KindContainer, Project: "default", Name: n, LastRun: now, LastOK: true})
	}
	// A partial run (Known == nil) forgets nothing.
	st.Record(&Report{Start: now, ExitCode: ExitOK})
	if len(st.RestoreTests) != 2 {
		t.Fatalf("partial run must keep drills: %+v", st.RestoreTests)
	}
	st.Record(&Report{Start: now, ExitCode: ExitOK, Known: []string{"container/default/web"}})
	if _, ok := st.RestoreTests["container/default/gone"]; ok || len(st.RestoreTests) != 1 {
		t.Fatalf("drills of instances missing from a complete plan must be forgotten: %+v", st.RestoreTests)
	}
}

func TestRestoreTestStateRoundtrip(t *testing.T) {
	dir := t.TempDir()
	st, _ := LoadState(dir)
	st.RecordRestoreTest(RestoreTestState{Kind: KindVM, Project: "p", Name: "db", LastRun: time.Now().UTC().Truncate(time.Second), LastOK: true})
	if err := st.Save(dir); err != nil {
		t.Fatal(err)
	}
	st2, err := LoadState(dir)
	if err != nil || st2.RestoreTests["virtual-machine/p/db"].LastSuccess.IsZero() {
		t.Fatalf("roundtrip: %+v %v", st2.RestoreTests, err)
	}
}

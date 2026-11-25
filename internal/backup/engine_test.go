package backup

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/incus-pbs/incus-pbs/internal/config"
	"github.com/incus-pbs/incus-pbs/internal/incus"
)

func archiveNames(f *fakeHost) []string {
	var out []string
	for n := range f.archives {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

func TestDiscoverPlan(t *testing.T) {
	f := newFakeHost(t)
	e := newEngine(t, f, nil)
	plan, err := Discover(f, e.Cfg, Selection{})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, tg := range plan.Targets {
		ids = append(ids, tg.ID())
	}
	want := []string{
		"container default/c1", "container default/c2", "virtual-machine default/v1",
		"volume default/default/rawdisk", "volume default/default/shared", "volume default/default/standalone",
	}
	if !slices.Equal(ids, want) {
		t.Fatalf("targets:\n got %q\nwant %q", ids, want)
	}
	if len(plan.Excluded) != 1 || !strings.Contains(plan.Excluded[0].What, "installer") {
		t.Errorf("iso volume must be excluded explicitly: %+v", plan.Excluded)
	}
	if len(plan.Warnings) != 1 || !strings.Contains(plan.Warnings[0], "/srv/host") {
		t.Errorf("host path disk must produce a warning: %v", plan.Warnings)
	}
}

func TestDiscoverInstanceFilterIncludesOnlyAttachedVolumes(t *testing.T) {
	f := newFakeHost(t)
	e := newEngine(t, f, nil)
	plan, err := Discover(f, e.Cfg, Selection{Instances: []string{"c1"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Targets) != 2 || plan.Targets[1].Name != "shared" {
		t.Fatalf("got %+v", plan.Targets)
	}
	if _, err := Discover(f, e.Cfg, Selection{Instances: []string{"nope"}}); err == nil {
		t.Fatal("unknown instance must be an error")
	}
}

func TestDiscoverVolumesDisabledAreListedAsExcluded(t *testing.T) {
	f := newFakeHost(t)
	e := newEngine(t, f, func(c *config.Config) { c.Backup.IncludeCustomVolumes = false })
	plan, _ := Discover(f, e.Cfg, Selection{})
	if len(plan.Targets) != 3 {
		t.Fatalf("expected only instances, got %d", len(plan.Targets))
	}
	n := 0
	for _, x := range plan.Excluded {
		if strings.Contains(x.Reason, "include_custom_volumes") {
			n++
		}
	}
	if n != 3 { // c1:data, v1:data, v1:blk
		t.Fatalf("attached volumes must never be silently omitted, excluded=%+v", plan.Excluded)
	}
}

func TestDiscoverSkipsRestoreTestInstances(t *testing.T) {
	f := newFakeHost(t)
	f.instances["default"] = append(f.instances["default"], incus.Instance{Name: "restoretest-c1", Project: "default", Type: "container",
		RootPool: "default", Config: map[string]string{incus.RestoreTestKey: "2026-11-24T00:00:00Z"}})
	e := newEngine(t, f, nil)
	plan, err := Discover(f, e.Cfg, Selection{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tg := range plan.Targets {
		if tg.Name == "restoretest-c1" {
			t.Fatal("restore-test instance must not be backed up")
		}
	}
	found := false
	for _, x := range plan.Excluded {
		found = found || (strings.Contains(x.What, "restoretest-c1") && strings.Contains(x.Reason, "restore-test"))
	}
	if !found {
		t.Fatalf("exclusion must be reported: %+v", plan.Excluded)
	}
}

func TestDiscoverUnsupportedPoolAndExclude(t *testing.T) {
	f := newFakeHost(t)
	f.instances["default"] = append(f.instances["default"], incus.Instance{Name: "onlvm", Project: "default", Type: "container", RootPool: "lvm"})
	e := newEngine(t, f, func(c *config.Config) { c.Backup.Exclude = []string{"c2"} })
	plan, _ := Discover(f, e.Cfg, Selection{})
	probs := plan.Problems()
	if len(probs) != 1 || probs[0].Name != "onlvm" || !strings.Contains(probs[0].Problem, "lvm") {
		t.Fatalf("problems: %+v", probs)
	}
	for _, tg := range plan.Targets {
		if tg.Name == "c2" {
			t.Fatal("excluded instance planned")
		}
	}
}

func TestRunSuccess(t *testing.T) {
	f := newFakeHost(t)
	e := newEngine(t, f, nil)
	rep, err := e.Run(context.Background(), Selection{})
	if err != nil || rep.ExitCode != ExitOK || !rep.Complete {
		t.Fatalf("err=%v rep=%+v events=%v", err, rep, f.events)
	}
	if f.count("pbc backup") != 4 { // ct/c1, ct/c2, vm/v1, host/h1
		t.Fatalf("one PBS upload per group expected: %v", f.events)
	}
	want := []string{
		"ct_default_c1.pxar", "ct_default_c2.pxar", "host-config.pxar", "incus-guest.conf", "incus-metadata.pxar",
		"vm_default_v1-config.pxar", "vm_default_v1.img",
		"vol_default_default_rawdisk.img", "vol_default_default_shared.pxar", "vol_default_default_standalone.pxar",
	}
	if got := archiveNames(f); !slices.Equal(got, want) {
		t.Fatalf("archives:\n got %q\nwant %q", got, want)
	}
	// Shared volume snapshotted exactly once.
	if f.count("snapshot volume shared") != 1 {
		t.Fatalf("shared volume must be snapshotted once: %v", f.events)
	}
	// All snapshots happen before any exposure, uploads after exposure, cleanup after upload.
	lastSnap, firstExpose := -1, len(f.events)
	for i, ev := range f.events {
		if strings.HasPrefix(ev, "snapshot ") {
			lastSnap = i
		}
		if (strings.HasPrefix(ev, "mount") || strings.HasPrefix(ev, "clone")) && i < firstExpose {
			firstExpose = i
		}
	}
	if lastSnap > firstExpose {
		t.Errorf("snapshots must all be taken before exposing: %v", f.events)
	}
	if f.indexOf("delete-snapshot") < f.indexOf("pbc backup") || f.indexOf("destroy vm_default_v1") > f.indexOf("delete-snapshot default/v1") {
		t.Errorf("cleanup order wrong: %v", f.events)
	}
	if rep.InstancesOK != 3 || rep.VolumesOK != 3 || rep.PBSSnapshot != "host/h1/2026-11-24T02:30:00Z" || rep.UploadedBytes != 4<<20 { // 1 MiB per group upload
		t.Errorf("report: %+v", rep)
	}
	// Arguments: namespace, encryption, change detection; no secrets anywhere on argv.
	args := strings.Join(f.pbcArgs, " ")
	for _, w := range []string{"--ns incus", "--backup-type host", "--backup-id h1", "--keyfile", "--change-detection-mode metadata"} {
		if !strings.Contains(args, w) {
			t.Errorf("missing %q in %s", w, args)
		}
	}
	if strings.Contains(args+strings.Join(f.pbcEnv, " "), "TOKENSECRET") {
		t.Error("token secret leaked to argv/env")
	}
	f.assertClean(t)
	if _, err := os.Stat(e.Cfg.Storage.StagingDirectory + "/metadata"); !os.IsNotExist(err) {
		t.Error("metadata staging dir not removed")
	}
}

func TestManifestContentAndNoSecrets(t *testing.T) {
	f := newFakeHost(t)
	e := newEngine(t, f, nil)
	if _, err := e.Run(context.Background(), Selection{}); err != nil {
		t.Fatal(err)
	}
	var m Manifest
	if err := json.Unmarshal([]byte(f.metadata["incus-metadata.pxar/manifest.json"]), &m); err != nil {
		t.Fatalf("manifest: %v (%v)", err, f.metadata)
	}
	if !m.Complete || m.Hostname != "incus-host" || m.ManifestVersion != ManifestVersion || len(m.Targets) != 6 || m.PBS["encrypted"] != "true" {
		t.Errorf("manifest: %+v", m)
	}
	if _, ok := f.metadata["incus-metadata.pxar/incus-project-default.json"]; !ok {
		t.Error("incus project metadata missing")
	}
	if f.metadata["host-config.pxar/files"+e.Cfg.Backup.HostConfigFiles[0]] != "incus-host\n" {
		t.Errorf("host config file not copied: %v", f.metadata)
	}
	if !strings.Contains(f.metadata["host-config.pxar/missing-files.txt"], "does-not-exist") {
		t.Error("missing host files must be recorded")
	}
	for name, content := range f.metadata {
		for _, secret := range []string{"TOKENSECRET", "KEYMATERIAL", "SECRET\""} {
			if strings.Contains(content, secret) {
				t.Errorf("%s contains secret %q", name, secret)
			}
		}
	}
}

func TestRunStoppedAndRunningInstancesOnly(t *testing.T) {
	f := newFakeHost(t)
	e := newEngine(t, f, nil)
	rep, err := e.Run(context.Background(), Selection{Instances: []string{"c2"}})
	if err != nil || rep.InstancesOK != 1 || rep.VolumesOK != 0 {
		t.Fatalf("stopped instance backup: err=%v rep=%+v", err, rep)
	}
	if got := archiveNames(f); !slices.Equal(got, []string{"ct_default_c2.pxar", "host-config.pxar", "incus-guest.conf", "incus-metadata.pxar"}) {
		t.Fatalf("archives %q", got)
	}
	f.assertClean(t)
}

func TestRunFailuresAbortCleanly(t *testing.T) {
	cases := map[string]string{
		"snapshot fails":        "snapshot default/v1",
		"volume snapshot fails": "snapshot volume rawdisk",
		"vm clone fails":        "clone vm_default_v1",
		"container mount fails": "mount ct_default_c2",
		"volume mount fails":    "mount vol_default_default_shared",
		"pbs upload fails":      "pbc backup",
		"pbs unreachable":       "pbs-unreachable",
	}
	for name, fail := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeHost(t)
			f.failOn[fail] = true
			e := newEngine(t, f, nil)
			rep, err := e.Run(context.Background(), Selection{})
			if err == nil || rep.ExitCode != ExitFailed || rep.PBSSnapshot != "" || rep.Complete {
				t.Fatalf("expected failed run, err=%v rep=%+v", err, rep)
			}
			if fail != "pbc backup" && f.count("pbc backup") != 0 {
				t.Fatal("must not upload after a preparation failure in abort mode")
			}
			delete(f.failOn, fail)
			f.assertClean(t)
		})
	}
}

func TestRunSkipModeCreatesIncompleteSnapshot(t *testing.T) {
	f := newFakeHost(t)
	f.failOn["clone vm_default_v1"] = true
	e := newEngine(t, f, func(c *config.Config) { c.Backup.OnInstanceError = "skip" })
	rep, err := e.Run(context.Background(), Selection{})
	if err == nil || rep.ExitCode != ExitPartial || rep.Complete || rep.PBSSnapshot == "" {
		t.Fatalf("expected partial run, err=%v rep=%+v", err, rep)
	}
	if rep.InstancesFailed != 1 || rep.InstancesOK != 2 {
		t.Fatalf("counts: %+v", rep)
	}
	if _, ok := f.archives["vm_default_v1.img"]; ok {
		t.Fatal("failed VM must not be uploaded")
	}
	var m Manifest
	json.Unmarshal([]byte(f.metadata["incus-metadata.pxar/manifest.json"]), &m)
	if m.Complete {
		t.Fatal("manifest must mark the snapshot incomplete")
	}
	failed := false
	for _, it := range m.Items {
		if it.Name == "v1" && it.Status == "failed" && strings.Contains(it.Error, "injected") {
			failed = true
		}
	}
	if !failed {
		t.Fatalf("manifest must record the failed instance: %+v", m.Items)
	}
	delete(f.failOn, "clone vm_default_v1")
	f.assertClean(t)
}

func TestRunUnsupportedTargetAbortsBeforeTouchingAnything(t *testing.T) {
	f := newFakeHost(t)
	f.instances["default"] = append(f.instances["default"], incus.Instance{Name: "onlvm", Project: "default", Type: "container", RootPool: "lvm"})
	e := newEngine(t, f, nil)
	rep, err := e.Run(context.Background(), Selection{})
	if err == nil || rep.ExitCode != ExitFailed || len(f.events) != 0 {
		t.Fatalf("err=%v events=%v", err, f.events)
	}
}

func TestRunInterruptedUpload(t *testing.T) {
	f := newFakeHost(t)
	f.pbcHang = true
	e := newEngine(t, f, nil)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }() // SIGTERM equivalent
	rep, err := e.Run(ctx, Selection{})
	if !errors.Is(err, context.Canceled) || rep.ExitCode != ExitFailed || rep.PBSSnapshot != "" {
		t.Fatalf("err=%v rep=%+v", err, rep)
	}
	f.assertClean(t)
}

func TestRunUploadTimeout(t *testing.T) {
	f := newFakeHost(t)
	f.pbcHang = true
	e := newEngine(t, f, func(c *config.Config) { c.PBS.Timeout = config.Duration(100 * time.Millisecond) })
	rep, err := e.Run(context.Background(), Selection{})
	if !errors.Is(err, context.DeadlineExceeded) || rep.ExitCode != ExitFailed {
		t.Fatalf("err=%v", err)
	}
	f.assertClean(t)
}

func TestRunMissingCredentials(t *testing.T) {
	f := newFakeHost(t)
	e := newEngine(t, f, nil)
	os.Remove(e.Cfg.PBS.PasswordFile)
	rep, err := e.Run(context.Background(), Selection{})
	if err == nil || rep.ExitCode != ExitFailed || f.count("pbc backup") != 0 {
		t.Fatalf("err=%v", err)
	}
	f.assertClean(t)
}

func TestRunRefusesLeftoversThenCleanupFixes(t *testing.T) {
	f := newFakeHost(t)
	e := newEngine(t, f, func(c *config.Config) { c.Backup.AutoCleanup = false })
	// Simulate a crashed run: snapshots, clone and mount left behind.
	f.addSnap("i:default/v1", "pbs-backup-20261123-023000")
	f.addSnap("v:default/default/shared", "pbs-backup-20261123-023000")
	f.clones["tank/incus-pbs-staging/vm_default_v1"] = true
	f.mounts[e.Cfg.Storage.StagingDirectory+"/mnt/ct_default_c1"] = true

	if _, err := e.Run(context.Background(), Selection{}); err == nil || !strings.Contains(err.Error(), "cleanup") {
		t.Fatalf("expected refusal, got %v", err)
	}
	if f.count("snapshot ") != 0 {
		t.Fatal("must not snapshot over leftovers")
	}
	dry, err := e.Cleanup(context.Background(), Selection{}, true)
	if err != nil || len(dry.DeletedSnapshots) != 2 || len(dry.DestroyedClones) != 1 || len(dry.Unmounted) != 1 || len(f.clones) != 1 {
		t.Fatalf("dry-run: %+v %v", dry, err)
	}
	if _, err := e.Cleanup(context.Background(), Selection{}, false); err != nil {
		t.Fatal(err)
	}
	f.assertClean(t)
	if f.indexOf("umount") > f.indexOf("destroy") || f.indexOf("destroy") > f.indexOf("delete-snapshot") {
		t.Fatalf("cleanup order: %v", f.events)
	}
	if rep, err := e.Run(context.Background(), Selection{}); err != nil || rep.ExitCode != ExitOK {
		t.Fatalf("run after cleanup: %v", err)
	}
}

func TestCheck(t *testing.T) {
	f := newFakeHost(t)
	e := newEngine(t, f, nil)
	res := e.Check(context.Background())
	byName := map[string]CheckResult{}
	for _, r := range res {
		byName[r.Name] = r
	}
	if byName["pbs password file"].Status != CheckOK || byName["encryption key"].Status != CheckOK ||
		byName["incus api"].Status != CheckOK || byName["discovery"].Status != CheckOK ||
		byName["staging dataset tank/incus-pbs-staging"].Status != CheckOK {
		t.Fatalf("unexpected check results: %+v", res)
	}
	if byName["warning"].Status != CheckWarn {
		t.Error("host-path warning expected")
	}

	os.Chmod(e.Cfg.PBS.PasswordFile, 0o644)
	f.failOn["pbs-unreachable"] = true
	res = e.Check(context.Background())
	if !CheckFailed(res) {
		t.Fatal("insecure password file must fail check")
	}
	for _, r := range res {
		if r.Name == "pbs connection" && (r.Status != CheckFail || !strings.Contains(r.Detail, "authentication")) {
			t.Errorf("pbs connection: %+v", r)
		}
	}
}

func TestHostConfigFollowsSymlinksAndToleratesErrors(t *testing.T) {
	f := newFakeHost(t)
	e := newEngine(t, f, nil)
	dir := t.TempDir()
	os.WriteFile(dir+"/real", []byte("ID=debian\n"), 0o644)
	os.Symlink(dir+"/real", dir+"/link")
	os.Mkdir(dir+"/adir", 0o755)
	e.Cfg.Backup.HostConfigFiles = []string{dir + "/link", dir + "/adir"}
	if rep, err := e.Run(context.Background(), Selection{}); err != nil || rep.ExitCode != ExitOK {
		t.Fatalf("host config problems must not fail the backup: %v", err)
	}
	if f.metadata["host-config.pxar/files"+dir+"/link"] != "ID=debian\n" {
		t.Errorf("symlink not followed: %v", f.metadata)
	}
	if !strings.Contains(f.metadata["host-config.pxar/missing-files.txt"], "adir") {
		t.Error("non-regular file must be recorded")
	}
}

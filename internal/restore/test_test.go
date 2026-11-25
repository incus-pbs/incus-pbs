package restore

import (
	"context"
	"strings"
	"testing"
	"time"
)

func testOpts(o TestOptions) TestOptions {
	o.Snapshot, o.PollInterval = "latest", time.Millisecond
	if o.Instance == "" {
		o.Instance = "web"
	}
	return o
}

func TestRestoreTestSuccessIsolatesAndDeletes(t *testing.T) {
	f := newFake(t)
	f.files = map[string]string{"/etc/os-release": "ID=debian\n", "/etc/hostname": "web\n"}
	r := newRestorer(t, f)
	res, err := r.TestInstance(context.Background(), testOpts(TestOptions{Files: []string{"/etc/os-release", "/etc/hostname"}}))
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || !res.Started || !res.Deleted || len(res.Files) != 2 || !res.Files[0].OK || res.Files[0].Size != 10 {
		t.Fatalf("result: %+v", res)
	}
	want := "start default/restoretest-web,stop default/restoretest-web,delete default/restoretest-web"
	if strings.Join(f.lifecycle, ",") != want {
		t.Fatalf("lifecycle: %v", f.lifecycle)
	}
	inst := index(t, f)["config"].(map[string]any)["container"].(map[string]any)
	devs := inst["devices"].(map[string]any)
	for _, n := range []string{"data", "lan"} {
		if d, _ := devs[n].(map[string]any); d["type"] != "none" {
			t.Errorf("device %s must be masked: %v", n, devs)
		}
	}
	cfg := inst["config"].(map[string]any)
	if cfg["boot.autostart"] != "false" || cfg["user.incus-pbs.restore-test"] == nil {
		t.Errorf("test marker/autostart: %v", cfg)
	}
	if _, ok := cfg["volatile.eth0.hwaddr"]; ok {
		t.Error("identity must always be new")
	}
	if strings.Join(res.Restore.DroppedDevices, ",") != "data,lan" {
		t.Errorf("dropped: %v", res.Restore.DroppedDevices)
	}
}

func TestRestoreTestIsolationIgnoresMissingDevices(t *testing.T) {
	f := newFake(t)
	delete(f.volumes, "default/default/webdata")
	delete(f.networks, "default/lanbr")
	f.files = map[string]string{"/a": "x"}
	if _, err := newRestorer(t, f).TestInstance(context.Background(), testOpts(TestOptions{Files: []string{"/a"}})); err != nil {
		t.Fatalf("masked devices must not need their volume/network: %v", err)
	}
	f2 := newFake(t)
	f2.profiles = map[string]bool{}
	if _, err := newRestorer(t, f2).TestInstance(context.Background(), testOpts(TestOptions{Files: []string{"/a"}})); err == nil || !strings.Contains(err.Error(), "profile") {
		t.Fatalf("profiles are still required: %v", err)
	}
}

func TestRestoreTestFailuresStillCleanUp(t *testing.T) {
	f := newFake(t)
	f.files = map[string]string{"/etc/os-release": "ID=debian\n"}
	r := newRestorer(t, f)
	res, err := r.TestInstance(context.Background(), testOpts(TestOptions{Files: []string{"/etc/os-release", "/missing", "/etc"}}))
	if err == nil || res.OK || !strings.Contains(err.Error(), "/missing: does not exist") || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("missing file must fail: %v", err)
	}
	if !res.Deleted || !res.Files[0].OK || res.Files[1].OK {
		t.Fatalf("result: %+v", res)
	}

	f2 := newFake(t)
	f2.files = map[string]string{"/etc/os-release": "ID=alpine\n"}
	res, err = newRestorer(t, f2).TestInstance(context.Background(), testOpts(TestOptions{Files: []string{"/etc/os-release"}, Contains: "ID=debian"}))
	if err == nil || !strings.Contains(err.Error(), `does not contain "ID=debian"`) || !res.Deleted {
		t.Fatalf("content mismatch: %v %+v", err, res)
	}

	f3 := newFake(t)
	f3.failStart = true
	res, err = newRestorer(t, f3).TestInstance(context.Background(), testOpts(TestOptions{Files: []string{"/a"}}))
	if err == nil || !strings.Contains(err.Error(), "boot failed") || !res.Deleted || !strings.Contains(strings.Join(f3.lifecycle, ","), "stop") {
		t.Fatalf("start failure: %v %+v %v", err, res, f3.lifecycle)
	}

	f4 := newFake(t)
	f4.files, f4.failDelete = map[string]string{"/a": "x"}, true
	res, err = newRestorer(t, f4).TestInstance(context.Background(), testOpts(TestOptions{Files: []string{"/a"}}))
	if err == nil || res.OK || res.Deleted || !strings.Contains(err.Error(), "remove it manually") {
		t.Fatalf("cleanup failure must fail the test: %v %+v", err, res)
	}
}

func TestRestoreTestSuccessAfterPollingHasNoError(t *testing.T) {
	f := newFake(t)
	f.notReady = 2
	f.files = map[string]string{"/etc/hostname": "web\n"}
	res, err := newRestorer(t, f).TestInstance(context.Background(), testOpts(TestOptions{Files: []string{"/etc/hostname"}}))
	if err != nil || !res.Files[0].OK || res.Files[0].Error != "" {
		t.Fatalf("a successful check must not carry polling errors: %+v %v", res.Files, err)
	}
}

func TestRestoreTestWaitsForAgentAndTimesOut(t *testing.T) {
	f := newFake(t)
	f.files, f.notReady = map[string]string{"/etc/hostname": "db"}, 3
	res, err := newRestorer(t, f).TestInstance(context.Background(), testOpts(TestOptions{Instance: "db", Files: []string{"/etc/hostname"}}))
	if err != nil || !res.Files[0].OK || f.lifecycle[0] != "start default/restoretest-db" {
		t.Fatalf("must retry until the agent answers: %v %+v", err, res)
	}
	f2 := newFake(t)
	f2.notReady = 1 << 30
	res, err = newRestorer(t, f2).TestInstance(context.Background(), testOpts(TestOptions{Instance: "db", Files: []string{"/x"}, Timeout: 20 * time.Millisecond}))
	if err == nil || !strings.Contains(err.Error(), "gave up") || !res.Deleted {
		t.Fatalf("timeout: %v %+v", err, res)
	}
}

func TestRestoreTestKeepAndOptions(t *testing.T) {
	f := newFake(t)
	f.files = map[string]string{"/a": "x"}
	res, err := newRestorer(t, f).TestInstance(context.Background(), testOpts(TestOptions{Files: []string{"/a"}, Keep: true, NewName: "drill"}))
	if err != nil || res.Deleted || strings.Join(f.lifecycle, ",") != "start default/drill,stop default/drill" {
		t.Fatalf("keep: %v %+v %v", err, res, f.lifecycle)
	}
	f2 := newFake(t)
	f2.instances["default/restoretest-web"] = true
	if _, err := newRestorer(t, f2).TestInstance(context.Background(), testOpts(TestOptions{Files: []string{"/a"}})); err == nil || !strings.Contains(err.Error(), "already exists") || len(f2.lifecycle) != 0 {
		t.Fatalf("must never overwrite: %v", err)
	}
	for _, o := range []TestOptions{{}, {Files: []string{"rel"}}, {Files: []string{"/a", "/b"}, Contains: "x"}} {
		if _, err := newRestorer(t, newFake(t)).TestInstance(context.Background(), testOpts(o)); err == nil {
			t.Errorf("invalid options accepted: %+v", o)
		}
	}
	if n := TestName(strings.Repeat("a", 60)); len(n) != 63 {
		t.Errorf("name length %d", len(n))
	}
}

func TestIsolateKeepsRootAndMasksProfileDevices(t *testing.T) {
	cfg := map[string]any{"container": map[string]any{
		"devices": map[string]any{"data": map[string]any{"type": "disk", "pool": "p", "source": "v", "path": "/data"}},
		"expanded_devices": map[string]any{
			"root":   map[string]any{"type": "disk", "pool": "p", "path": "/"},
			"eth0":   map[string]any{"type": "nic", "network": "br0"},
			"ci":     map[string]any{"type": "disk", "source": "cloud-init:config"},
			"gpu":    map[string]any{"type": "gpu"},
			"hostfs": map[string]any{"type": "disk", "source": "/srv", "path": "/srv"},
		},
	}}
	masked := isolate(cfg, time.Unix(0, 0))
	if strings.Join(masked, ",") != "data,eth0,gpu,hostfs" {
		t.Fatalf("masked: %v", masked)
	}
	inst := cfg["container"].(map[string]any)
	devs, exp := inst["devices"].(map[string]any), inst["expanded_devices"].(map[string]any)
	if _, ok := devs["root"]; ok {
		t.Error("root must not get a local override")
	}
	if len(exp) != 2 || exp["root"] == nil || exp["ci"] == nil || len(devs) != 4 {
		t.Errorf("devices %v expanded %v", devs, exp)
	}
}

func TestRestoreTestFollowsSymlinks(t *testing.T) {
	f := newFake(t)
	// Debian: /etc/os-release -> ../usr/lib/os-release; plus an absolute link and a loop.
	f.links = map[string]string{"/etc/os-release": "../usr/lib/os-release", "/etc/abs": "/usr/lib/os-release", "/a": "/b", "/b": "/a"}
	f.files = map[string]string{"/usr/lib/os-release": "ID=debian\n"}
	r := newRestorer(t, f)
	res, err := r.TestInstance(context.Background(), testOpts(TestOptions{Files: []string{"/etc/os-release"}, Contains: "ID=debian"}))
	if err != nil || !res.Files[0].OK || res.Files[0].Resolved != "/usr/lib/os-release" {
		t.Fatalf("relative symlink: %+v %v", res, err)
	}
	f = newFake(t)
	f.links = map[string]string{"/etc/abs": "/usr/lib/os-release", "/a": "/b", "/b": "/a", "/dangling": "missing"}
	f.files = map[string]string{"/usr/lib/os-release": "ID=debian\n"}
	res, err = newRestorer(t, f).TestInstance(context.Background(), testOpts(TestOptions{Files: []string{"/etc/abs", "/a", "/dangling"}}))
	if err == nil || !res.Files[0].OK || res.Files[1].OK || !strings.Contains(res.Files[1].Error, "too many") ||
		res.Files[2].OK || res.Files[2].Error != "does not exist" || !res.Deleted {
		t.Fatalf("absolute/loop/dangling: %+v", res.Files)
	}
}

func TestRestoreTestReplacesOnlyMarkedStaleCopies(t *testing.T) {
	f := newFake(t)
	f.files = map[string]string{"/etc/hostname": "web\n"}
	f.instances["default/restoretest-web"] = true
	f.instConfig = map[string]map[string]string{"default/restoretest-web": {"user.incus-pbs.restore-test": "2026-11-24T10:00:00Z"}}
	r := newRestorer(t, f)
	res, err := r.TestInstance(context.Background(), testOpts(TestOptions{Files: []string{"/etc/hostname"}}))
	if err != nil || !res.OK || !res.Restore.ReplacedStale {
		t.Fatalf("stale marked copy must be replaced: %+v %v", res, err)
	}
	want := "stop default/restoretest-web,delete default/restoretest-web,start default/restoretest-web"
	if !strings.HasPrefix(strings.Join(f.lifecycle, ","), want) {
		t.Fatalf("lifecycle: %v", f.lifecycle)
	}

	// An unmarked instance with that name is the user's: never touched.
	f = newFake(t)
	f.instances["default/restoretest-web"] = true
	f.instConfig = map[string]map[string]string{"default/restoretest-web": {"limits.cpu": "2"}}
	_, err = newRestorer(t, f).TestInstance(context.Background(), testOpts(TestOptions{Files: []string{"/etc/hostname"}}))
	if err == nil || !strings.Contains(err.Error(), "already exists") || len(f.lifecycle) != 0 {
		t.Fatalf("unmarked instance must be refused untouched: %v %v", err, f.lifecycle)
	}

	// A normal (non-isolated) restore never replaces anything, marked or not.
	f = newFake(t)
	f.instances["default/w2"] = true
	f.instConfig = map[string]map[string]string{"default/w2": {"user.incus-pbs.restore-test": "x"}}
	if _, err := newRestorer(t, f).RestoreInstance(context.Background(), InstanceOptions{Snapshot: snap, Instance: "web", NewName: "w2"}); err == nil || len(f.lifecycle) != 0 {
		t.Fatalf("restore instance must not replace: %v %v", err, f.lifecycle)
	}
}

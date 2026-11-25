package backup

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/incus-pbs/incus-pbs/internal/config"
	"github.com/incus-pbs/incus-pbs/internal/incus"
)

func callFor(f *fakeHost, group string) *pbcCall {
	for i := range f.calls {
		if f.calls[i].group == group {
			return &f.calls[i]
		}
	}
	return nil
}

func TestPerGuestGroups(t *testing.T) {
	f := newFakeHost(t)
	f.volumes["default"][1].UsedBy = []string{"/1.0/instances/v1"} // rawdisk: only v1
	e := newEngine(t, f, nil)
	rep, err := e.Run(context.Background(), Selection{})
	if err != nil || rep.ExitCode != ExitOK || !rep.Complete {
		t.Fatalf("run: %v %+v", err, rep)
	}
	var groups []string
	for _, c := range f.calls {
		groups = append(groups, c.group)
	}
	if !slices.Equal(groups, []string{"ct/c1", "ct/c2", "vm/v1", "host/h1"}) {
		t.Fatalf("groups/order (host index must be last): %v", groups)
	}
	if c := callFor(f, "vm/v1"); !slices.Equal(c.archives, []string{"incus-guest.conf", "vm_default_v1-config.pxar", "vm_default_v1.img", "vol_default_default_rawdisk.img"}) {
		t.Fatalf("vm/v1 must hold its disk, config, its only-attached volume and its manifest: %v", c.archives)
	}
	var gm GuestManifest
	if err := json.Unmarshal([]byte(f.guest["vm/v1"]), &gm); err != nil {
		t.Fatal(err)
	}
	if gm.Group != "vm/v1" || gm.HostBackupID != "h1" || len(gm.Targets) != 2 || gm.Targets[0].Name != "v1" ||
		gm.Targets[1].Name != "rawdisk" || gm.Targets[1].Owner != "default/v1" {
		t.Fatalf("guest manifest: %+v", gm)
	}
	if _, ok := f.guest["host/h1"]; ok {
		t.Error("the host index carries no guest manifest")
	}
	host := callFor(f, "host/h1")
	for _, want := range []string{"incus-metadata.pxar", "host-config.pxar", "vol_default_default_shared.pxar", "vol_default_default_standalone.pxar"} {
		if !slices.Contains(host.archives, want) {
			t.Errorf("host group misses %s: %v", want, host.archives)
		}
	}
	var m Manifest
	if err := json.Unmarshal([]byte(f.metadata["incus-metadata.pxar/manifest.json"]), &m); err != nil {
		t.Fatal(err)
	}
	if m.ArchiveSnapshots["vm_default_v1.img"] != "vm/v1/2026-11-24T02:30:00Z" ||
		m.ArchiveSnapshots["ct_default_c1.pxar"] != "ct/c1/2026-11-24T02:30:00Z" {
		t.Fatalf("manifest archive map: %+v", m.ArchiveSnapshots)
	}
	for _, it := range m.Items {
		if it.Status != "included" && it.Status != "ok" {
			t.Errorf("item %s status %s", it.Name, it.Status)
		}
	}
	if rep.PBSSnapshot != "host/h1/2026-11-24T02:30:00Z" {
		t.Fatalf("run snapshot is the host index: %s", rep.PBSSnapshot)
	}
	f.assertClean(t)
}

func TestPerGuestVMThroughLibraryWithConfigBlob(t *testing.T) {
	f := newFakeHost(t)
	f.volumes["default"][1].UsedBy = []string{"/1.0/instances/v1"}
	lib := newFakeLib()
	e := blockEngine(t, f, lib, nil)
	rep, err := e.Run(context.Background(), Selection{})
	if err != nil || rep.ExitCode != ExitOK {
		t.Fatalf("run: %v", err)
	}
	if !slices.Equal(lib.backupIDs, []string{"v1"}) {
		t.Fatalf("one library session per VM, backup id = VM name: %v", lib.backupIDs)
	}
	if callFor(f, "vm/v1") != nil {
		t.Fatal("the VM group must not get a second snapshot from the client")
	}
	if _, ok := lib.blobs["v1/vm_default_v1-config.tar"]; !ok {
		t.Fatalf("VM config must go into the same snapshot as a blob: %v", lib.blobs)
	}
	if len(lib.writes["vm_default_v1"]) == 0 || len(lib.writes["vol_default_default_rawdisk"]) == 0 {
		t.Fatalf("root disk and attached block volume via library: %v", lib.writes)
	}
	var v1 ItemResult
	for _, it := range rep.Items {
		if it.Name == "v1" {
			v1 = it
		}
	}
	if !slices.Contains(v1.Archives, "vm_default_v1-config.tar.blob") || v1.Snapshot == "" || !strings.HasPrefix(v1.Snapshot, "vm/v1/") {
		t.Fatalf("v1 item: %+v", v1)
	}
	if rep.ArchiveSnapshots["vm_default_v1-config.tar.blob"] != v1.Snapshot {
		t.Fatalf("archive map: %v", rep.ArchiveSnapshots)
	}
}

func TestPerGuestOneGroupFailsIsPartial(t *testing.T) {
	f := newFakeHost(t)
	f.failOn["pbc backup ct/c2"] = true
	e := newEngine(t, f, nil)
	rep, err := e.Run(context.Background(), Selection{})
	if err == nil || rep.ExitCode != ExitPartial || rep.Complete {
		t.Fatalf("expected partial: %v %+v", err, rep)
	}
	if callFor(f, "host/h1") == nil {
		t.Fatal("the run index must still be written")
	}
	var m Manifest
	json.Unmarshal([]byte(f.metadata["incus-metadata.pxar/manifest.json"]), &m)
	if m.Complete {
		t.Fatal("manifest must say incomplete")
	}
	for _, it := range m.Items {
		if it.Name == "c2" && (it.Status != "failed" || !strings.Contains(it.Error, "ct/c2")) {
			t.Fatalf("c2: %+v", it)
		}
		if it.Name == "c1" && it.Status != "ok" {
			t.Fatalf("c1 must be ok: %+v", it)
		}
	}
	if _, ok := m.ArchiveSnapshots["ct_default_c2.pxar"]; ok {
		t.Fatal("failed group must not be indexed")
	}
	delete(f.failOn, "pbc backup ct/c2")
	f.assertClean(t)
}

func TestPerGuestIndexFailureFailsRun(t *testing.T) {
	f := newFakeHost(t)
	f.failOn["pbc backup host/h1"] = true
	e := newEngine(t, f, nil)
	rep, err := e.Run(context.Background(), Selection{})
	if err == nil || rep.ExitCode != ExitFailed || !strings.Contains(err.Error(), "restorable on their own") {
		t.Fatalf("expected failure: %v %+v", err, rep.ExitCode)
	}
	delete(f.failOn, "pbc backup host/h1")
	f.assertClean(t)
}

func TestGroupIDsAndCollisions(t *testing.T) {
	if GroupID("default", "web") != "web" || GroupID("", "web") != "web" || GroupID("prod", "web") != "prod_web" || LegacyGroupID("prod", "web") != "prod-web" {
		t.Fatal("GroupID")
	}
	f := newFakeHost(t)
	f.instances["a"] = []incus.Instance{{Name: "b", Project: "a", Type: "container", RootPool: "default",
		Devices: map[string]map[string]string{"root": {"type": "disk", "path": "/", "pool": "default"}}}}
	f.instances["default"] = append(f.instances["default"], incus.Instance{Name: "a-b", Project: "default", Type: "container", RootPool: "default",
		Devices: map[string]map[string]string{"root": {"type": "disk", "path": "/", "pool": "default"}}})
	e := newEngine(t, f, func(c *config.Config) { c.Incus.Projects = []string{"default", "a"} })
	// default/a-b and a/b collided under the old "-" separator; "_" cannot
	// appear in Incus project or instance names, so they now get their own groups.
	if _, err := e.Run(context.Background(), Selection{}); err != nil {
		t.Fatal(err)
	}
	if callFor(f, "ct/a-b") == nil || callFor(f, "ct/a_b") == nil {
		t.Fatal("default/a-b and a/b must be in separate groups")
	}
	f.assertClean(t)
}

func TestSoleOwner(t *testing.T) {
	cases := []struct {
		used []string
		want string
		ok   bool
	}{
		{[]string{"/1.0/instances/v1"}, "default/v1", true},
		{[]string{"/1.0/instances/v1", "/1.0/instances/v1/snapshots/s"}, "default/v1", true},
		{[]string{"/1.0/instances/c3?project=prod"}, "prod/c3", true},
		{[]string{"/1.0/instances/a", "/1.0/instances/b"}, "", false},
		{[]string{"/1.0/profiles/default"}, "", false},
		{nil, "", false},
	}
	for _, c := range cases {
		got, ok := soleOwner(Target{Project: "default", UsedBy: c.used})
		if got != c.want || ok != c.ok {
			t.Errorf("%v: got %q %v", c.used, got, ok)
		}
	}
}

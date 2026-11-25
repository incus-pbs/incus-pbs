package backup

import (
	"context"
	"strings"
	"testing"

	"github.com/incus-pbs/incus-pbs/internal/incus"
)

// Incus refuses snapshots of the volumes it uses itself (storage.backups_volume,
// storage.images_volume, storage.logs_volume); they must be excluded, not fail the run.
func TestDaemonVolumesExcluded(t *testing.T) {
	f := newFakeHost(t)
	f.volumes["default"] = append(f.volumes["default"], incus.Volume{Pool: "default", Project: "default", Name: "backups", ContentType: "filesystem"})
	e := newEngine(t, f, nil)
	plan, err := Discover(f, e.Cfg, Selection{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tg := range plan.Targets {
		if tg.Name == "backups" {
			t.Fatal("Incus daemon volume must not be a backup target")
		}
	}
	found := false
	for _, x := range plan.Excluded {
		if strings.Contains(x.What, "backups") && strings.Contains(x.Reason, "storage.backups_volume") {
			found = true
		}
	}
	if !found {
		t.Fatalf("daemon volume must be listed as excluded: %+v", plan.Excluded)
	}
	if rep, err := e.Run(context.Background(), Selection{}); err != nil || rep.ExitCode != ExitOK {
		t.Fatalf("run must succeed with a daemon volume present: %v", err)
	}
}

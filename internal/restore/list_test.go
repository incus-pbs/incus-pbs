package restore

import (
	"context"
	"testing"
	"time"

	"github.com/incus-pbs/incus-pbs/internal/pbc"
)

func at(h, m int) int64 { return time.Date(2026, 11, 24, h, m, 0, 0, time.UTC).Unix() }

func listFake(t *testing.T) *fake {
	f := newFake(t)
	f.snaps = []pbc.Snapshot{
		// run 1 (index 02:30), run 2 (index 03:30), run 3 in progress
		{BackupType: "ct", BackupID: "web", BackupTime: at(2, 21)},
		{BackupType: "vm", BackupID: "db", BackupTime: at(2, 25)},
		{BackupType: "host", BackupID: "h1", BackupTime: at(2, 30)},
		{BackupType: "ct", BackupID: "web", BackupTime: at(3, 5)}, // leftover of a run whose index failed
		{BackupType: "ct", BackupID: "web", BackupTime: at(3, 21)},
		{BackupType: "vm", BackupID: "db", BackupTime: at(3, 25)},
		{BackupType: "host", BackupID: "h1", BackupTime: at(3, 30)},
		{BackupType: "host", BackupID: "other", BackupTime: at(3, 31)}, // another host
		{BackupType: "ct", BackupID: "web", BackupTime: at(4, 21)},
	}
	return f
}

func TestListGroups(t *testing.T) {
	l, err := newRestorer(t, listFake(t)).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(l.HostIndexes) != 2 || l.HostIndexes[0].Path() != "host/h1/2026-11-24T03:30:00Z" {
		t.Fatalf("run indexes newest first, other hosts excluded: %+v", l.HostIndexes)
	}
	if len(l.Guests) != 2 || l.Guests[0].Group != "ct/web" || l.Guests[1].Group != "vm/db" {
		t.Fatalf("groups: %+v", l.Guests)
	}
	if len(l.Guests[0].Snapshots) != 4 || l.Guests[0].Snapshots[0].Path() != "ct/web/2026-11-24T04:21:00Z" {
		t.Fatalf("web snapshots newest first: %+v", l.Guests[0].Snapshots)
	}
	snaps := l.InstanceSnapshots("default", "web")
	if len(snaps) != 4 || snaps[3].Path() != "ct/web/2026-11-24T02:21:00Z" {
		t.Fatalf("instance snapshots: %+v", snaps)
	}
	if len(l.InstanceSnapshots("prod", "web")) != 0 {
		t.Fatal("project prefix must be part of the group id")
	}
}

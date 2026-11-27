package main

import (
	"errors"
	"testing"
	"time"

	"github.com/incus-pbs/incus-pbs/internal/backup"
	"github.com/incus-pbs/incus-pbs/internal/restore"
)

func TestRestoreTestState(t *testing.T) {
	end := time.Date(2026, 11, 29, 6, 5, 0, 0, time.UTC)
	ok := &restore.TestResult{OK: true, Seconds: 40, BootSeconds: 3,
		Restore: &restore.Result{Kind: backup.KindContainer, Source: "media/web", Snapshot: "ct/media_web/2026-11-29T02:30:00Z"}}
	r := restoreTestState("", "web", ok, nil, end)
	if r.Kind != backup.KindContainer || r.Project != "media" || r.Name != "web" || !r.LastOK || r.Snapshot == "" || r.DurationSeconds != 40 {
		t.Fatalf("ok drill: %+v", r)
	}

	// Failed before the backup was found: only the command line is known.
	r = restoreTestState("", "web", nil, errors.New("no backups of instance"), end)
	if r.LastOK || r.Kind != "" || r.Name != "web" || r.LastError != "no backups of instance" {
		t.Fatalf("early failure: %+v", r)
	}

	// A result that claims OK but comes with an error (e.g. cleanup failed) is a failure.
	r = restoreTestState("default", "web", &restore.TestResult{OK: true}, errors.New("cleanup"), end)
	if r.LastOK {
		t.Fatalf("error must win: %+v", r)
	}
}

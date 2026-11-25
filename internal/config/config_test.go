package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const minimal = `
pbs:
  repository: "backup@pbs!incus@pbs.example.net:datastore"
  password_file: /etc/incus-pbs/pbs-token
`

func TestExampleConfigIsValid(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "examples", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PBS.Namespace != "incus" || cfg.Backup.Parallelism != 2 || cfg.Backup.SnapshotExpiry.D() != 24*time.Hour {
		t.Fatalf("unexpected values: %+v", cfg)
	}
}

func TestDefaultsApplied(t *testing.T) {
	cfg, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Incus.Projects[0] != "default" || cfg.PBS.ChangeDetectionMode != "metadata" ||
		cfg.Storage.StagingDirectory != "/var/cache/incus-pbs" || cfg.Backup.OnInstanceError != "abort" ||
		cfg.Logging.Format != "json" || len(cfg.Backup.HostConfigFiles) == 0 {
		t.Fatalf("defaults missing: %+v", cfg)
	}
}

func TestUnknownKeysRejected(t *testing.T) {
	_, err := Parse([]byte(minimal + "\nbackup:\n  optimized_storage: true\n"))
	if err == nil || !strings.Contains(err.Error(), "optimized_storage") {
		t.Fatalf("expected unknown-key error, got %v", err)
	}
}

func TestSecretNeverInline(t *testing.T) {
	_, err := Parse([]byte(minimal + "  password: hunter2\n"))
	if err == nil {
		t.Fatal("inline password must be rejected as unknown key")
	}
}

func TestValidationCollectsAllProblems(t *testing.T) {
	_, err := Parse([]byte(`
incus:
  socket: relative.sock
  projects: ["bad project"]
pbs:
  repository: "not a repo!"
  namespace: "a//b"
  password_file: relative
  fingerprint: nope
  change_detection_mode: fast
storage:
  staging_directory: /
  staging_dataset_name: a/b
backup:
  parallelism: 0
  on_instance_error: ignore
  exclude: ["a/b/c"]
  snapshot_expiry: 5m
  lock_file: lock
logging:
  level: loud
  format: xml
`))
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected ValidationError, got %v", err)
	}
	for _, want := range []string{"incus.socket", "incus.projects", "pbs.repository", "pbs.namespace",
		"pbs.password_file", "pbs.fingerprint", "change_detection_mode", "staging_directory", "staging_dataset_name",
		"parallelism", "on_instance_error", "backup.exclude", "snapshot_expiry", "lock_file", "logging.level", "logging.format"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing problem about %s", want)
		}
	}
}

func TestMissingRequired(t *testing.T) {
	_, err := Parse([]byte("{}"))
	if err == nil || !strings.Contains(err.Error(), "pbs.repository is required") || !strings.Contains(err.Error(), "pbs.password_file is required") {
		t.Fatalf("got %v", err)
	}
}

func TestRepositoryForms(t *testing.T) {
	for _, r := range []string{"datastore", "host:ds", "pbs.example.net:8007:ds", "user@pbs@host:ds", "backup@pbs!incus@10.0.0.1:ds", "root@pam@[fd00::1]:ds"} {
		cfg := Default()
		cfg.PBS.Repository, cfg.PBS.PasswordFile = r, "/x"
		if err := cfg.Validate(); err != nil {
			t.Errorf("%s: %v", r, err)
		}
	}
}

func TestEmptyFileAndMissingFile(t *testing.T) {
	if _, err := Load("/nonexistent/config.yaml"); err == nil {
		t.Fatal("expected error for missing file")
	}
	p := filepath.Join(t.TempDir(), "c.yaml")
	os.WriteFile(p, nil, 0o600)
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "repository") {
		t.Fatalf("empty file should fail validation, got %v", err)
	}
}

func TestExcluded(t *testing.T) {
	cfg := Default()
	cfg.Backup.Exclude = []string{"scratch", "prod/tmp"}
	if !cfg.Excluded("default", "scratch") || !cfg.Excluded("prod", "tmp") || cfg.Excluded("default", "tmp") {
		t.Fatal("exclude matching wrong")
	}
}

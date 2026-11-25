// Package config loads and validates /etc/incus-pbs/config.yaml.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/incus-pbs/incus-pbs/internal/schedule"
)

// DefaultPath is the default configuration file location.
const DefaultPath = "/etc/incus-pbs/config.yaml"

// Config is the complete configuration.
type Config struct {
	Incus      Incus      `yaml:"incus"`
	PBS        PBS        `yaml:"pbs"`
	Storage    Storage    `yaml:"storage"`
	Backup     Backup     `yaml:"backup"`
	Hooks      Hooks      `yaml:"hooks"`
	Monitoring Monitoring `yaml:"monitoring"`
	Logging    Logging    `yaml:"logging"`
}

// Hooks run around a backup. Commands are argv lists, never shell strings.
//
//	pre_backup:    before snapshots are taken (flush/freeze applications)
//	post_snapshot: right after all snapshots exist (thaw) - also runs when a
//	               pre_backup hook or the snapshot step failed
//	post_backup:   after upload and cleanup, with the result in the environment
type Hooks struct {
	PreBackup    []Hook `yaml:"pre_backup"`
	PostSnapshot []Hook `yaml:"post_snapshot"`
	PostBackup   []Hook `yaml:"post_backup"`
}

// Hook is a single hook command.
type Hook struct {
	Name string `yaml:"name"`
	// Command is the argv. Host commands need an absolute path.
	Command []string `yaml:"command"`
	// Instance runs the command inside an instance via the Incus API
	// ("name" in the first configured project, or "project/name").
	Instance string `yaml:"instance"`
	// Timeout per hook (default 5m).
	Timeout Duration `yaml:"timeout"`
	// OnFailure: "abort" fails the backup (default for pre_backup and
	// post_snapshot), "continue" only logs (default and only option for post_backup).
	OnFailure string `yaml:"on_failure"`
}

// Monitoring outputs.
type Monitoring struct {
	// TextfilePath for the node_exporter textfile collector, e.g.
	// /var/lib/prometheus/node-exporter/incus_pbs.prom ("" disables).
	TextfilePath string `yaml:"textfile_path"`
}

// Incus connection and scope.
type Incus struct {
	Socket   string   `yaml:"socket"`
	Projects []string `yaml:"projects"`
}

// PBS repository and credentials. Secrets are only referenced by file path.
type PBS struct {
	Repository          string `yaml:"repository"`
	Namespace           string `yaml:"namespace"`
	BackupID            string `yaml:"backup_id"`
	PasswordFile        string `yaml:"password_file"`
	EncryptionKeyFile   string `yaml:"encryption_key_file"`
	Fingerprint         string `yaml:"fingerprint"`
	ChangeDetectionMode string `yaml:"change_detection_mode"`
	ClientBinary        string `yaml:"client_binary"`
	// Timeout for the whole upload; 0 = no timeout.
	Timeout Duration `yaml:"timeout"`
	// ExcludePatterns are passed as --exclude to every pxar archive
	// (paths relative to the archive root, e.g. "/rootfs/tmp/*").
	ExcludePatterns []string `yaml:"exclude_patterns"`
	// RateLimit passed as --rate (e.g. "100MiB"); "" = unlimited.
	RateLimit string `yaml:"rate_limit"`
}

// Storage controls local staging.
type Storage struct {
	StagingDirectory string `yaml:"staging_directory"`
	// StagingDataset is the ZFS dataset for temporary clones,
	// "<pool dataset>/<name>" in every Incus ZFS pool (default incus-pbs-staging).
	StagingDatasetName string `yaml:"staging_dataset_name"`
	// MinFreeBytes required in the staging directory (check command).
	MinFreeBytes int64 `yaml:"min_free_bytes"`
	// StateDirectory keeps the last run report and per-target success times.
	StateDirectory string `yaml:"state_directory"`
}

// Backup behaviour.
type Backup struct {
	// Schedule is the cron expression used by `backup --scheduled` for
	// instances without user.incus-pbs.schedule, for shared and standalone
	// volumes and for the host index (default "30 2 * * *", host local time).
	Schedule             string `yaml:"schedule"`
	IncludeCustomVolumes bool   `yaml:"include_custom_volumes"`
	IncludeHostConfig    bool   `yaml:"include_host_config"`
	// Parallelism bounds concurrent snapshot/expose operations.
	Parallelism int `yaml:"parallelism"`
	// OnInstanceError: "abort" (default) fails the whole run without creating
	// a PBS snapshot; "skip" backs up the rest and records skipped instances in
	// the manifest (the run still exits non-zero).
	OnInstanceError string `yaml:"on_instance_error"`
	// Exclude instances by name ("name" or "project/name").
	Exclude []string `yaml:"exclude"`
	// SnapshotExpiry is set on temporary Incus snapshots as a safety net.
	SnapshotExpiry Duration `yaml:"snapshot_expiry"`
	// LockFile prevents concurrent runs.
	LockFile string `yaml:"lock_file"`
	// HostConfigFiles are copied into host-config.pxar (allowlist).
	HostConfigFiles []string `yaml:"host_config_files"`
	// AutoCleanup removes leftovers of a crashed previous run (own snapshots,
	// clones and mounts only) before starting. Safe because the run lock
	// guarantees no other run owns them.
	AutoCleanup bool `yaml:"auto_cleanup"`
	// BlockIncremental controls changed-block tracking for VM disks and block
	// volumes (ZFS bookmarks + libproxmox-backup-qemu):
	//   auto   - only for datasets without other ZFS snapshots, so that
	//            `incus snapshot restore` is never blocked (default)
	//   always - also with user snapshots (older snapshots cannot be restored
	//            until `incus-pbs cleanup --release-bookmarks`)
	//   off    - always read block devices in full via proxmox-backup-client
	BlockIncremental string `yaml:"block_incremental"`
	// BlockFullInterval forces a full read of each block device at least this often.
	BlockFullInterval Duration `yaml:"block_full_interval"`
}

// Logging options.
type Logging struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// Duration is a time.Duration that unmarshals from strings like "6h".
type Duration time.Duration

// UnmarshalYAML parses a Go duration string.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("line %d: invalid duration %q", n.Line, s)
	}
	*d = Duration(v)
	return nil
}

// D returns the time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// DefaultHostConfigFiles never contains secrets (no keys, no shadow, no certs).
var DefaultHostConfigFiles = []string{
	"/etc/hostname", "/etc/hosts", "/etc/subuid", "/etc/subgid",
	"/etc/os-release", "/etc/network/interfaces", "/etc/default/incus",
	"/etc/modules", "/etc/sysctl.conf",
}

// Default returns a config populated with defaults.
func Default() Config {
	host, _ := os.Hostname()
	return Config{
		Incus: Incus{Socket: "/var/lib/incus/unix.socket", Projects: []string{"default"}},
		PBS: PBS{
			Namespace: "", BackupID: host,
			ChangeDetectionMode: "metadata", ClientBinary: "proxmox-backup-client",
		},
		Storage: Storage{
			StagingDirectory:   "/var/cache/incus-pbs",
			StagingDatasetName: "incus-pbs-staging",
			MinFreeBytes:       1 << 30,
			StateDirectory:     "/var/lib/incus-pbs",
		},
		Backup: Backup{
			Schedule:             "30 2 * * *",
			IncludeCustomVolumes: true, IncludeHostConfig: true, Parallelism: 2,
			OnInstanceError: "abort", SnapshotExpiry: Duration(24 * time.Hour),
			LockFile: "/run/incus-pbs.lock", HostConfigFiles: DefaultHostConfigFiles,
			AutoCleanup: true, BlockIncremental: "auto", BlockFullInterval: Duration(7 * 24 * time.Hour),
		},
		Logging: Logging{Level: "info", Format: "json"},
	}
}

// Load reads, defaults and validates a config file.
func Load(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("reading config: %w", err)
	}
	return Parse(b)
}

// Parse decodes YAML strictly (unknown keys are errors) on top of defaults.
func Parse(b []byte) (Config, error) {
	cfg := Default()
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return Config{}, fmt.Errorf("parsing config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

var (
	// user@realm!token@host[:port]:datastore, or host:datastore, or datastore.
	repoRE     = regexp.MustCompile(`^(?:(?:[^@\s:]+@[^@!\s:]+(?:![^@\s:]+)?@)?(?:\[[0-9a-fA-F:]+\]|[A-Za-z0-9.-]+)(?::\d+)?:)?[A-Za-z0-9][A-Za-z0-9_.-]*$`)
	idRE       = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*$`)
	nsRE       = regexp.MustCompile(`^([A-Za-z0-9_][A-Za-z0-9._-]*)(/[A-Za-z0-9_][A-Za-z0-9._-]*)*$`)
	datasetRE  = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.:-]*$`)
	fpRE       = regexp.MustCompile(`^([0-9a-fA-F]{2}:){31}[0-9a-fA-F]{2}$`)
	projectRE  = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	excludeRE  = regexp.MustCompile(`^([A-Za-z0-9_-]+/)?[A-Za-z0-9-]+$`)
	rateRE     = regexp.MustCompile(`^\d+(\.\d+)?\s?(B|KB|MB|GB|KiB|MiB|GiB)$`)
	logLevels  = map[string]bool{"debug": true, "info": true, "warn": true, "error": true}
	logFormats = map[string]bool{"json": true, "text": true}
	cdModes    = map[string]bool{"legacy": true, "data": true, "metadata": true}
)

// ValidationError lists all problems found in a config.
type ValidationError struct{ Problems []string }

func (e *ValidationError) Error() string {
	return "invalid configuration:\n  - " + strings.Join(e.Problems, "\n  - ")
}

// Validate checks values (not filesystem state; see the check command).
func (c *Config) Validate() error {
	var p []string
	add := func(f string, a ...any) { p = append(p, fmt.Sprintf(f, a...)) }

	if c.Incus.Socket != "" && !filepath.IsAbs(c.Incus.Socket) {
		add("incus.socket must be an absolute path")
	}
	if len(c.Incus.Projects) == 0 {
		add("incus.projects must list at least one project")
	}
	for _, pr := range c.Incus.Projects {
		if !projectRE.MatchString(pr) {
			add("incus.projects: invalid project name %q", pr)
		}
	}

	switch {
	case c.PBS.Repository == "":
		add("pbs.repository is required (e.g. \"backup@pbs!incus@pbs.example.net:datastore\")")
	case !repoRE.MatchString(c.PBS.Repository):
		add("pbs.repository %q is not of the form [[user@realm[!token]@]host[:port]:]datastore", c.PBS.Repository)
	}
	if c.PBS.Namespace != "" && (!nsRE.MatchString(c.PBS.Namespace) || strings.Count(c.PBS.Namespace, "/") > 7) {
		add("pbs.namespace %q is invalid (max depth 8, [A-Za-z0-9_.-] per level)", c.PBS.Namespace)
	}
	if !idRE.MatchString(c.PBS.BackupID) {
		add("pbs.backup_id %q is invalid", c.PBS.BackupID)
	}
	if c.PBS.PasswordFile == "" {
		add("pbs.password_file is required (never put the secret itself in the config)")
	} else if !filepath.IsAbs(c.PBS.PasswordFile) {
		add("pbs.password_file must be an absolute path")
	}
	if c.PBS.EncryptionKeyFile != "" && !filepath.IsAbs(c.PBS.EncryptionKeyFile) {
		add("pbs.encryption_key_file must be an absolute path")
	}
	if c.PBS.Fingerprint != "" && !fpRE.MatchString(c.PBS.Fingerprint) {
		add("pbs.fingerprint must be a sha256 fingerprint (32 hex bytes separated by ':')")
	}
	if !cdModes[c.PBS.ChangeDetectionMode] {
		add("pbs.change_detection_mode must be legacy, data or metadata, got %q", c.PBS.ChangeDetectionMode)
	}
	if c.PBS.ClientBinary == "" {
		add("pbs.client_binary must not be empty")
	}
	if c.PBS.Timeout < 0 {
		add("pbs.timeout must not be negative")
	}

	sd := c.Storage.StagingDirectory
	switch {
	case !filepath.IsAbs(sd):
		add("storage.staging_directory must be an absolute path")
	case filepath.Clean(sd) != sd || strings.Count(sd, "/") < 2:
		add("storage.staging_directory %q must be a clean path at least two levels deep (e.g. /var/cache/incus-pbs)", sd)
	}
	if !datasetRE.MatchString(c.Storage.StagingDatasetName) {
		add("storage.staging_dataset_name %q must be a single dataset name component", c.Storage.StagingDatasetName)
	}
	if c.Storage.MinFreeBytes < 0 {
		add("storage.min_free_bytes must not be negative")
	}

	if sc, err := schedule.Parse(c.Backup.Schedule); err != nil {
		add("backup.schedule: %v", err)
	} else if sc.IsOff() {
		add("backup.schedule must not be off (it covers the host index and shared volumes)")
	}
	if c.Backup.Parallelism < 1 || c.Backup.Parallelism > 32 {
		add("backup.parallelism must be between 1 and 32, got %d", c.Backup.Parallelism)
	}
	if c.Backup.OnInstanceError != "abort" && c.Backup.OnInstanceError != "skip" {
		add("backup.on_instance_error must be abort or skip, got %q", c.Backup.OnInstanceError)
	}
	for _, e := range c.Backup.Exclude {
		if !excludeRE.MatchString(e) {
			add("backup.exclude: invalid entry %q (use name or project/name)", e)
		}
	}
	if c.Backup.SnapshotExpiry.D() < time.Hour {
		add("backup.snapshot_expiry must be at least 1h (it must outlive a backup run)")
	}
	if !filepath.IsAbs(c.Backup.LockFile) {
		add("backup.lock_file must be an absolute path")
	}
	for _, f := range c.Backup.HostConfigFiles {
		if !filepath.IsAbs(f) {
			add("backup.host_config_files: %q must be absolute", f)
		}
	}

	switch c.Backup.BlockIncremental {
	case "auto", "always", "off":
	default:
		add("backup.block_incremental must be auto, always or off, got %q", c.Backup.BlockIncremental)
	}
	if c.Backup.BlockFullInterval.D() < time.Hour {
		add("backup.block_full_interval must be at least 1h")
	}
	if !filepath.IsAbs(c.Storage.StateDirectory) {
		add("storage.state_directory must be an absolute path")
	}
	for _, pat := range c.PBS.ExcludePatterns {
		if strings.TrimSpace(pat) == "" || strings.ContainsAny(pat, "\x00\n") {
			add("pbs.exclude_patterns: invalid pattern %q", pat)
		}
	}
	if c.PBS.RateLimit != "" && !rateRE.MatchString(c.PBS.RateLimit) {
		add("pbs.rate_limit %q must look like 100MiB, 1GiB or 50MB", c.PBS.RateLimit)
	}
	if p := c.Monitoring.TextfilePath; p != "" && (!filepath.IsAbs(p) || !strings.HasSuffix(p, ".prom")) {
		add("monitoring.textfile_path must be an absolute path ending in .prom")
	}
	for phase, hooks := range map[string][]Hook{"pre_backup": c.Hooks.PreBackup, "post_snapshot": c.Hooks.PostSnapshot, "post_backup": c.Hooks.PostBackup} {
		for i := range hooks {
			h := &hooks[i]
			where := fmt.Sprintf("hooks.%s[%d]", phase, i)
			if h.Name == "" {
				h.Name = fmt.Sprintf("%s-%d", phase, i+1)
			}
			if len(h.Command) == 0 || h.Command[0] == "" {
				add("%s: command is required (argv list)", where)
			} else if h.Instance == "" && !filepath.IsAbs(h.Command[0]) {
				add("%s: host command %q must be an absolute path", where, h.Command[0])
			}
			if h.Instance != "" && !excludeRE.MatchString(h.Instance) {
				add("%s: invalid instance %q (use name or project/name)", where, h.Instance)
			}
			if h.Timeout == 0 {
				h.Timeout = Duration(5 * time.Minute)
			}
			if h.Timeout < 0 {
				add("%s: timeout must be positive", where)
			}
			switch {
			case h.OnFailure == "" && phase == "post_backup":
				h.OnFailure = "continue"
			case h.OnFailure == "":
				h.OnFailure = "abort"
			case h.OnFailure == "abort" && phase == "post_backup":
				add("%s: on_failure abort is not possible after the backup finished", where)
			case h.OnFailure != "abort" && h.OnFailure != "continue":
				add("%s: on_failure must be abort or continue", where)
			}
		}
	}

	if !logLevels[c.Logging.Level] {
		add("logging.level must be debug, info, warn or error, got %q", c.Logging.Level)
	}
	if !logFormats[c.Logging.Format] {
		add("logging.format must be json or text, got %q", c.Logging.Format)
	}
	if len(p) > 0 {
		return &ValidationError{Problems: p}
	}
	return nil
}

// Excluded reports whether project/name is excluded.
func (c *Config) Excluded(project, name string) bool {
	for _, e := range c.Backup.Exclude {
		if e == name || e == project+"/"+name {
			return true
		}
	}
	return false
}

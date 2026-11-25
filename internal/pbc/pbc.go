// Package pbc drives the official proxmox-backup-client binary.
//
// Credentials are never placed on the command line: the repository and the
// password file are passed via PBS_REPOSITORY / PBS_PASSWORD_FILE, and the
// certificate fingerprint via PBS_FINGERPRINT.
package pbc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/incus-pbs/incus-pbs/internal/execx"
)

// Archive is one archive within a backup snapshot ("name.pxar:/path", "name.img:/dev/...").
type Archive struct {
	Name   string `json:"name"`
	Source string `json:"source"`
}

var archiveNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*\.(pxar|img|conf|log|blob)$`)

// BackupOptions configures one backup snapshot.
type BackupOptions struct {
	Namespace       string
	BackupType      string // host | vm | ct
	BackupID        string
	KeyFile         string
	ChangeDetection string // legacy | data | metadata ("" = client default)
	Archives        []Archive
	Exclude         []string // --exclude patterns for pxar archives
	RateLimit       string   // --rate
}

// ArchiveStats are the per-archive upload statistics printed by the client.
type ArchiveStats struct {
	Archive         string  `json:"archive"`
	UploadedBytes   int64   `json:"uploaded_bytes"`
	TotalBytes      int64   `json:"total_bytes"`
	CompressedBytes int64   `json:"compressed_bytes"`
	Seconds         float64 `json:"seconds"`
	ReusedBytes     int64   `json:"reused_bytes"`
	ReusedPercent   float64 `json:"reused_percent"`
}

// BackupResult is the outcome of a backup run.
type BackupResult struct {
	Snapshot string         `json:"snapshot"`
	Stats    []ArchiveStats `json:"stats"`
	Duration time.Duration  `json:"duration_ns"`
	ExitCode int            `json:"exit_code"`
	Output   string         `json:"output"`
}

// Client wraps proxmox-backup-client.
type Client struct {
	Bin          string // default "proxmox-backup-client"
	Repository   string
	PasswordFile string
	Fingerprint  string
	X            execx.Runner
}

// Binary returns the client executable name or path.
func (c *Client) Binary() string {
	if c.Bin == "" {
		return "proxmox-backup-client"
	}
	return c.Bin
}

// Env returns the environment for the client. No secret values, only file paths.
func (c *Client) Env() []string {
	env := []string{"PBS_REPOSITORY=" + c.Repository}
	if c.PasswordFile != "" {
		env = append(env, "PBS_PASSWORD_FILE="+c.PasswordFile)
	}
	if c.Fingerprint != "" {
		env = append(env, "PBS_FINGERPRINT="+c.Fingerprint)
	}
	return env
}

// Validate checks configuration and secret file permissions.
func (c *Client) Validate() error {
	if c.Repository == "" {
		return fmt.Errorf("pbs repository is not configured")
	}
	if c.PasswordFile == "" {
		return fmt.Errorf("pbs password_file is not configured")
	}
	return CheckSecretFile(c.PasswordFile)
}

// CheckSecretFile ensures a secret file exists, is a regular file and is not group/world accessible.
func CheckSecretFile(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("secret file: %w", err)
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("secret file %s is not a regular file", path)
	}
	if st.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("secret file %s has mode %o, must not be accessible by group/others", path, st.Mode().Perm())
	}
	return nil
}

// BackupArgs builds the argument list for `proxmox-backup-client backup`.
func BackupArgs(o BackupOptions) ([]string, error) {
	if len(o.Archives) == 0 {
		return nil, fmt.Errorf("no archives to back up")
	}
	if o.BackupID == "" || o.BackupType == "" {
		return nil, fmt.Errorf("backup id and type are required")
	}
	args := []string{"backup"}
	seen := map[string]bool{}
	for _, a := range o.Archives {
		if !archiveNameRE.MatchString(a.Name) {
			return nil, fmt.Errorf("invalid archive name %q", a.Name)
		}
		if seen[a.Name] {
			return nil, fmt.Errorf("duplicate archive name %q", a.Name)
		}
		seen[a.Name] = true
		if !strings.HasPrefix(a.Source, "/") {
			return nil, fmt.Errorf("archive %s: source must be absolute, got %q", a.Name, a.Source)
		}
		args = append(args, a.Name+":"+a.Source)
	}
	args = append(args, "--backup-type", o.BackupType, "--backup-id", o.BackupID)
	if o.Namespace != "" {
		args = append(args, "--ns", o.Namespace)
	}
	if o.KeyFile != "" {
		args = append(args, "--keyfile", o.KeyFile)
	} else {
		args = append(args, "--crypt-mode", "none")
	}
	if o.ChangeDetection != "" {
		args = append(args, "--change-detection-mode", o.ChangeDetection)
	}
	for _, x := range o.Exclude {
		args = append(args, "--exclude", x)
	}
	if o.RateLimit != "" {
		args = append(args, "--rate", o.RateLimit)
	}
	return args, nil
}

// Backup creates one backup snapshot containing all archives. PBS only
// finalizes the snapshot when the client finishes successfully, so a failed
// or aborted run never shows up as a complete backup.
func (c *Client) Backup(ctx context.Context, o BackupOptions) (BackupResult, error) {
	if err := c.Validate(); err != nil {
		return BackupResult{}, err
	}
	if o.KeyFile != "" {
		if err := CheckSecretFile(o.KeyFile); err != nil {
			return BackupResult{}, err
		}
	}
	args, err := BackupArgs(o)
	if err != nil {
		return BackupResult{}, err
	}
	res, runErr := c.X.Run(ctx, execx.Cmd{Name: c.Binary(), Args: args, Env: c.Env()})
	out := res.Stdout + res.Stderr
	br := BackupResult{
		Stats:    ParseStats(out),
		Duration: res.Duration,
		ExitCode: res.ExitCode,
		Output:   out,
		Snapshot: parseSnapshot(out),
	}
	return br, runErr
}

// Snapshot is an entry of `snapshot list`.
type Snapshot struct {
	BackupType string `json:"backup-type"`
	BackupID   string `json:"backup-id"`
	BackupTime int64  `json:"backup-time"`
	Size       int64  `json:"size,omitempty"`
	Files      []any  `json:"files,omitempty"`
}

// Finished reports whether the snapshot is complete. A snapshot that is
// still being written is listed with no files (not even index.json.blob)
// and cannot be read yet.
func (s Snapshot) Finished() bool { return len(s.Files) > 0 }

// HasFile reports whether the snapshot lists a file (e.g. "incus-guest.conf.blob").
// PBS lists files as objects with a "filename"; plain strings are accepted too.
func (s Snapshot) HasFile(name string) bool {
	for _, f := range s.Files {
		switch v := f.(type) {
		case string:
			if v == name {
				return true
			}
		case map[string]any:
			if v["filename"] == name {
				return true
			}
		}
	}
	return false
}

// ListSnapshots lists snapshots in a namespace.
func (c *Client) ListSnapshots(ctx context.Context, ns string) ([]Snapshot, error) {
	args := []string{"snapshot", "list", "--output-format", "json"}
	if ns != "" {
		args = append(args, "--ns", ns)
	}
	res, err := c.X.Run(ctx, execx.Cmd{Name: c.Binary(), Args: args, Env: c.Env()})
	if err != nil {
		return nil, err
	}
	var out []Snapshot
	if err := json.Unmarshal([]byte(res.Stdout), &out); err != nil {
		return nil, fmt.Errorf("parsing snapshot list: %w", err)
	}
	return out, nil
}

// Version returns the client version string.
func (c *Client) Version(ctx context.Context) string {
	res, err := c.X.Run(ctx, execx.Cmd{Name: c.Binary(), Args: []string{"version"}, Env: c.Env()})
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(res.Stdout + res.Stderr)
}

var (
	statsRE    = regexp.MustCompile(`(?m)^(\S+): had to backup (.+?) of (.+?) \(compressed (.+?)\) in ([\d.]+) ?s`)
	reusedRE   = regexp.MustCompile(`(?m)^(\S+): backup was done incrementally, reused (.+?) \(([\d.]+)%\)`)
	snapshotRE = regexp.MustCompile(`(?m)^Starting backup: (?:\[[^\]]*\]:)?(\S+)`)
)

func parseSnapshot(out string) string {
	if m := snapshotRE.FindStringSubmatch(out); m != nil {
		return m[1]
	}
	return ""
}

// ParseStats extracts per-archive statistics from client output.
func ParseStats(out string) []ArchiveStats {
	idx := map[string]int{}
	var stats []ArchiveStats
	for _, m := range statsRE.FindAllStringSubmatch(out, -1) {
		s := ArchiveStats{Archive: m[1]}
		s.UploadedBytes, _ = ParseSize(m[2])
		s.TotalBytes, _ = ParseSize(m[3])
		s.CompressedBytes, _ = ParseSize(m[4])
		s.Seconds, _ = strconv.ParseFloat(m[5], 64)
		idx[s.Archive] = len(stats)
		stats = append(stats, s)
	}
	for _, m := range reusedRE.FindAllStringSubmatch(out, -1) {
		i, ok := idx[m[1]]
		if !ok {
			continue
		}
		stats[i].ReusedBytes, _ = ParseSize(m[2])
		stats[i].ReusedPercent, _ = strconv.ParseFloat(m[3], 64)
	}
	return stats
}

var units = map[string]float64{
	"B": 1, "KiB": 1 << 10, "MiB": 1 << 20, "GiB": 1 << 30, "TiB": 1 << 40,
	"KB": 1e3, "MB": 1e6, "GB": 1e9, "TB": 1e12,
}

// ParseSize parses sizes such as "1.5 GiB", "512 B" or "10MiB".
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	i := strings.IndexFunc(s, func(r rune) bool { return (r < '0' || r > '9') && r != '.' })
	if i <= 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	v, err := strconv.ParseFloat(s[:i], 64)
	if err != nil {
		return 0, err
	}
	u, ok := units[strings.TrimSpace(s[i:])]
	if !ok {
		return 0, fmt.Errorf("unknown unit in %q", s)
	}
	return int64(v * u), nil
}

// Path formats "type/id/time" as used by the client.
func (s Snapshot) Path() string {
	return s.BackupType + "/" + s.BackupID + "/" + time.Unix(s.BackupTime, 0).UTC().Format("2006-01-02T15:04:05Z")
}

// File is an entry of `snapshot files`.
type File struct {
	Filename  string `json:"filename"`
	Size      int64  `json:"size"`
	CryptMode string `json:"crypt-mode"`
}

var snapshotPathRE = regexp.MustCompile(`^(host|vm|ct)/[A-Za-z0-9_][A-Za-z0-9._-]*/\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$`)

// ValidSnapshotPath reports whether p looks like "host/id/2026-11-24T02:30:00Z".
func ValidSnapshotPath(p string) bool { return snapshotPathRE.MatchString(p) }

// Files lists the archives of a snapshot.
func (c *Client) Files(ctx context.Context, ns, snapshot string) ([]File, error) {
	if !ValidSnapshotPath(snapshot) {
		return nil, fmt.Errorf("invalid snapshot %q", snapshot)
	}
	args := []string{"snapshot", "files", snapshot, "--output-format", "json"}
	if ns != "" {
		args = append(args, "--ns", ns)
	}
	res, err := c.X.Run(ctx, execx.Cmd{Name: c.Binary(), Args: args, Env: c.Env()})
	if err != nil {
		return nil, err
	}
	var out []File
	if err := json.Unmarshal([]byte(res.Stdout), &out); err != nil {
		return nil, fmt.Errorf("parsing snapshot files: %w", err)
	}
	return out, nil
}

// Restore extracts one archive of a snapshot to target (a directory for
// .pxar archives, a file for .img archives).
func (c *Client) Restore(ctx context.Context, ns, snapshot, archive, target, keyFile string) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if !ValidSnapshotPath(snapshot) {
		return fmt.Errorf("invalid snapshot %q", snapshot)
	}
	if !archiveNameRE.MatchString(archive) {
		return fmt.Errorf("invalid archive name %q", archive)
	}
	if !strings.HasPrefix(target, "/") {
		return fmt.Errorf("restore target must be absolute, got %q", target)
	}
	args := []string{"restore", snapshot, archive, target}
	if ns != "" {
		args = append(args, "--ns", ns)
	}
	if keyFile != "" {
		if err := CheckSecretFile(keyFile); err != nil {
			return err
		}
		args = append(args, "--keyfile", keyFile)
	}
	_, err := c.X.Run(ctx, execx.Cmd{Name: c.Binary(), Args: args, Env: c.Env()})
	return err
}

// RestoreStream streams one archive (typically a .img) to the returned reader
// ("restore <snapshot> <archive> -"). wait must be called after reading.
func (c *Client) RestoreStream(ctx context.Context, ns, snapshot, archive, keyFile string) (io.ReadCloser, func() error, error) {
	if err := c.Validate(); err != nil {
		return nil, nil, err
	}
	if !ValidSnapshotPath(snapshot) || !archiveNameRE.MatchString(archive) {
		return nil, nil, fmt.Errorf("invalid snapshot %q or archive %q", snapshot, archive)
	}
	args := []string{"restore", snapshot, archive, "-"}
	if ns != "" {
		args = append(args, "--ns", ns)
	}
	if keyFile != "" {
		if err := CheckSecretFile(keyFile); err != nil {
			return nil, nil, err
		}
		args = append(args, "--keyfile", keyFile)
	}
	cmd := exec.CommandContext(ctx, c.Binary(), args...)
	cmd.Env = append(os.Environ(), c.Env()...)
	// Like execx: SIGINT first so the client can close its PBS session, then
	// SIGKILL after a grace period.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGINT) }
	cmd.WaitDelay = 30 * time.Second
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	return out, func() error {
		if err := cmd.Wait(); err != nil {
			return fmt.Errorf("%s restore: %w: %s", c.Binary(), err, strings.TrimSpace(stderr.String()))
		}
		return nil
	}, nil
}

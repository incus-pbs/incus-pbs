// Package restore restores instances, custom volumes and host metadata from
// PBS snapshots created by incus-pbs.
//
// Archives are first restored into a private directory below the staging
// directory, laid out exactly like an Incus backup tarball
// (backup/index.yaml + backup/container|virtual-machine|volume[.img]), and
// then streamed into Incus' official import API. That restores config,
// devices and profiles the same way `incus import` does, onto any storage
// driver, and never touches existing instances or volumes.
package restore

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/incus-pbs/incus-pbs/internal/backup"
	"github.com/incus-pbs/incus-pbs/internal/config"
	"github.com/incus-pbs/incus-pbs/internal/incus"
	"github.com/incus-pbs/incus-pbs/internal/pbc"
)

// TarFunc streams dir/backup as an uncompressed tar.
type TarFunc func(ctx context.Context, dir string) (io.ReadCloser, func() error, error)

// Restorer performs restores.
type Restorer struct {
	Cfg   config.Config
	Incus incus.Importer
	PBC   *pbc.Client
	Log   *slog.Logger

	Tar       TarFunc
	FreeBytes func(path string) uint64
	// BackupsDir is Incus' upload buffer (default IncusBackupsDir).
	BackupsDir string
	// StreamArchive streams one archive from PBS (nil = proxmox-backup-client restore ... -).
	StreamArchive func(ctx context.Context, snapshot, archive string) (io.ReadCloser, func() error, error)
	Now           func() time.Time
}

func (r *Restorer) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Snapshots lists the finished run indexes (host/<backup_id>), newest first.
func (r *Restorer) Snapshots(ctx context.Context) ([]pbc.Snapshot, error) {
	all, err := r.PBC.ListSnapshots(ctx, r.Cfg.PBS.Namespace)
	if err != nil {
		return nil, err
	}
	var out []pbc.Snapshot
	for _, s := range all {
		if s.BackupType == "host" && s.BackupID == r.Cfg.PBS.BackupID && s.Finished() {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].BackupTime > out[j].BackupTime })
	return out, nil
}

var timeOnlyRE = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$`)

// Resolve turns "latest", a timestamp or a run index path into a run index
// snapshot path (host/<backup_id>/<time>). Instances and volumes are found
// with locate instead: their newest backup is usually not in the newest index.
func (r *Restorer) Resolve(ctx context.Context, ref string) (string, error) {
	switch {
	case ref == "latest":
		snaps, err := r.Snapshots(ctx)
		if err != nil {
			return "", err
		}
		if len(snaps) == 0 {
			return "", fmt.Errorf("no snapshots for host/%s in namespace %q", r.Cfg.PBS.BackupID, r.Cfg.PBS.Namespace)
		}
		return snaps[0].Path(), nil
	case timeOnlyRE.MatchString(ref):
		return "host/" + r.Cfg.PBS.BackupID + "/" + ref, nil
	case pbc.ValidSnapshotPath(ref) && IsGuestPath(ref):
		return "", fmt.Errorf("%s is a guest snapshot, not a run index", ref)
	case pbc.ValidSnapshotPath(ref):
		return ref, nil
	}
	return "", fmt.Errorf("invalid snapshot %q (use latest, 2026-11-24T02:30:00Z or host/<id>/<time>)", ref)
}

// workDir creates a private restore directory below the staging directory.
func (r *Restorer) workDir(label string) (string, error) {
	base := filepath.Join(r.Cfg.Storage.StagingDirectory, "restore")
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", err
	}
	return os.MkdirTemp(base, label+"-"+r.now().UTC().Format("20060102-150405")+"-")
}

// Manifest downloads and parses the manifest of a run index.
func (r *Restorer) Manifest(ctx context.Context, snapshot string) (*backup.Manifest, error) {
	m, dir, err := r.fetchMetadata(ctx, snapshot)
	if dir != "" {
		defer os.RemoveAll(dir)
	}
	return m, err
}

func (r *Restorer) fetchMetadata(ctx context.Context, snapshot string) (*backup.Manifest, string, error) {
	dir, err := r.workDir("manifest")
	if err != nil {
		return nil, "", err
	}
	target := filepath.Join(dir, "md")
	if err := r.PBC.Restore(ctx, r.Cfg.PBS.Namespace, snapshot, "incus-metadata.pxar", target, r.Cfg.PBS.EncryptionKeyFile); err != nil {
		return nil, dir, fmt.Errorf("restoring incus-metadata.pxar: %w", err)
	}
	b, err := os.ReadFile(filepath.Join(target, "manifest.json"))
	if err != nil {
		return nil, dir, fmt.Errorf("snapshot has no manifest (not created by incus-pbs?): %w", err)
	}
	var m backup.Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, dir, fmt.Errorf("parsing manifest: %w", err)
	}
	if m.ManifestVersion < backup.ManifestVersion {
		return nil, dir, fmt.Errorf("manifest version %d was written by an older incus-pbs and is not supported (per-instance schedules changed the layout)", m.ManifestVersion)
	}
	if m.ManifestVersion > backup.ManifestVersion {
		return nil, dir, fmt.Errorf("manifest version %d is newer than supported (%d); upgrade incus-pbs", m.ManifestVersion, backup.ManifestVersion)
	}
	return &m, dir, nil
}

// findTarget locates a target in the manifest and verifies it was uploaded.
var (
	errNotInSnapshot = errors.New("not part of this snapshot")
	errNotBackedUp   = errors.New("not backed up in this snapshot")
)

func findTarget(m *backup.Manifest, kinds []backup.Kind, project, name, pool string) (backup.Target, error) {
	var matches []backup.Target
	for _, t := range m.Targets {
		if t.Name != name || (project != "" && t.Project != project) || (pool != "" && t.Pool.Name != pool) {
			continue
		}
		for _, k := range kinds {
			if t.Kind == k {
				matches = append(matches, t)
			}
		}
	}
	switch {
	case len(matches) == 0:
		return backup.Target{}, fmt.Errorf("%q is %w", name, errNotInSnapshot)
	case len(matches) > 1:
		return backup.Target{}, fmt.Errorf("%q is ambiguous (%d matches); specify --project/--pool", name, len(matches))
	}
	t := matches[0]
	for _, it := range m.Items {
		if it.Kind == t.Kind && it.Project == t.Project && it.Name == t.Name {
			if it.Status != "ok" && it.Status != "included" {
				return t, fmt.Errorf("%s was %w (%s: %s)", t.ID(), errNotBackedUp, it.Status, it.Error)
			}
			return t, nil
		}
	}
	return t, fmt.Errorf("%s has no result entry in the manifest", t.ID())
}

// InstanceOptions controls an instance restore.
type InstanceOptions struct {
	Snapshot      string
	Instance      string
	SourceProject string // project at backup time ("" = any)
	TargetProject string // default: source project
	NewName       string // default: original name
	Pool          string // default: original pool
	// Identity: "auto" (default) keeps MAC addresses, UUIDs and the cloud-init
	// instance-id only when the original instance no longer exists and the
	// name is unchanged (disaster recovery); "new" always generates new ones;
	// "keep" always keeps them (refused while the original exists).
	Identity           string
	DropMissingDevices bool
	StagingOnly        bool // restore into the staging directory and stop
	KeepStaging        bool
	// Isolate restores a throwaway copy for restore tests: every device except
	// the root disk is masked (no network, no custom volumes), new identity,
	// no autostart, marked with incus.RestoreTestKey.
	Isolate bool
}

// Result describes a restore.
type Result struct {
	Kind           backup.Kind `json:"kind"`
	Source         string      `json:"source"`
	Project        string      `json:"project"`
	Name           string      `json:"name"`
	Pool           string      `json:"pool"`
	Snapshot       string      `json:"snapshot"`
	Imported       bool        `json:"imported"`
	StagingDir     string      `json:"staging_dir,omitempty"`
	DroppedDevices []string    `json:"dropped_devices,omitempty"`
	StrippedKeys   []string    `json:"stripped_keys,omitempty"`
	// AutostartDisabled: the copy runs next to its original, so it must not
	// come up on its own after a host reboot.
	AutostartDisabled bool    `json:"autostart_disabled,omitempty"`
	ReplacedStale     bool    `json:"replaced_stale_test_copy,omitempty"`
	Seconds           float64 `json:"seconds"`
}

// identityKeyRE matches keys that identify an instance on the network or to
// guests. idmap/last_state keys are deliberately kept: Incus needs them to
// know how the restored files are mapped on disk, and
// volatile.<nic>.last_state.hwaddr is the host NIC's own MAC that Incus puts
// back when a phys/sriov NIC is released (see stripIdentity).
var identityKeyRE = regexp.MustCompile(`^volatile\.(.+\.hwaddr|uuid|uuid\.generation|cloud-init\.instance-id|vsock_id|.+\.host_name)$`)

// RestoreInstance restores a container or VM as a new instance.
func (r *Restorer) RestoreInstance(ctx context.Context, o InstanceOptions) (res *Result, err error) {
	start := r.now()
	loc, err := r.locate(ctx, o.Snapshot, instanceKinds, o.SourceProject, "", o.Instance)
	if err != nil {
		return nil, err
	}
	t, snap := loc.Target, loc.Snapshot
	res = &Result{Kind: t.Kind, Source: t.Project + "/" + t.Name, Snapshot: snap,
		Project: or(o.TargetProject, t.Project), Name: or(o.NewName, t.Name), Pool: or(o.Pool, t.Pool.Name)}
	log := r.Log.With("snapshot", snap, "source", res.Source, "target", res.Project+"/"+res.Name)

	// Never overwrite. The only exception: a restore-test copy left behind by
	// a crashed drill (it carries incus.RestoreTestKey) is removed by the next drill.
	if cfg, ok, err := r.Incus.InstanceConfig(res.Project, res.Name); err != nil {
		return nil, err
	} else if ok {
		if !o.Isolate || cfg[incus.RestoreTestKey] == "" {
			return nil, fmt.Errorf("instance %s/%s already exists; choose --name or delete it yourself first", res.Project, res.Name)
		}
		if err := r.removeStaleTestCopy(ctx, res.Project, res.Name, cfg[incus.RestoreTestKey]); err != nil {
			return nil, err
		}
		res.ReplacedStale = true
	}
	if _, err := r.Incus.GetPool(res.Pool); err != nil {
		return nil, fmt.Errorf("target pool %q: %w", res.Pool, err)
	}
	originalExists, err := r.Incus.InstanceExists(t.Project, t.Name)
	if err != nil {
		return nil, err
	}
	keepIdentity := false
	identity := o.Identity
	if o.Isolate {
		identity = "new"
	}
	switch identity {
	case "", "auto":
		keepIdentity = !originalExists && res.Name == t.Name
	case "keep":
		if originalExists {
			return nil, fmt.Errorf("--identity keep refused: the original %s still exists (duplicate MAC addresses)", res.Source)
		}
		keepIdentity = true
	case "new":
	default:
		return nil, fmt.Errorf("invalid identity mode %q (auto, keep, new)", o.Identity)
	}

	// Validate dependencies from the metadata before downloading anything large;
	// backup.yaml from the archive is checked again afterwards (authoritative).
	if early := loc.Instance; early != nil && !o.DropMissingDevices && !o.Isolate {
		if _, err := r.checkDependencies(map[string]any{"container": early}, res.Project, res.Pool, false); err != nil {
			return nil, err
		}
	}

	dir, err := r.workDir("instance")
	if err != nil {
		return nil, err
	}
	defer r.finish(dir, &o.KeepStaging, o.StagingOnly, &err, log)
	res.StagingDir = dir

	sub := "container"
	archives := map[string]string{t.ArchiveBase + ".pxar": filepath.Join(dir, "backup", "container")}
	if t.Kind == backup.KindVM {
		sub = "virtual-machine"
		conf, err := r.vmConfigArchive(ctx, loc)
		if err != nil {
			return res, err
		}
		archives = map[string]string{
			conf:                   filepath.Join(dir, "backup", "virtual-machine"),
			t.ArchiveBase + ".img": filepath.Join(dir, "backup", "virtual-machine.img"),
		}
	}
	img, err := r.restoreArchives(ctx, loc, dir, archives, !o.StagingOnly, log)
	if err != nil {
		return res, err
	}

	cfg, err := readBackupYAML(filepath.Join(dir, "backup", sub, "backup.yaml"))
	if err != nil {
		return res, err
	}
	if o.Isolate {
		res.DroppedDevices = isolate(cfg, r.now())
	}
	if dropped, err := r.checkDependencies(cfg, res.Project, res.Pool, o.DropMissingDevices); err != nil {
		return res, err
	} else if !o.Isolate {
		res.DroppedDevices = dropped
	}
	if !keepIdentity {
		res.StrippedKeys = stripIdentity(cfg)
		if originalExists && !o.Isolate {
			disableAutostart(cfg)
			res.AutostartDisabled = true
		}
	}
	// Backups contain the current state only, never the instance's own
	// snapshots; Incus rejects an import whose config lists snapshots that
	// are not in the tarball.
	delete(cfg, "snapshots")
	delete(cfg, "volume_snapshots")
	// Incus validates the backup.yaml inside the instance directory as well,
	// so it must carry the same sanitized config as index.yaml.
	byPath := filepath.Join(dir, "backup", sub, "backup.yaml")
	if err := os.Chmod(byPath, 0o600); err != nil {
		return res, err
	}
	if err := writeYAML(byPath, cfg); err != nil {
		return res, err
	}
	index := map[string]any{
		"name": t.Name, "backend": t.Pool.Driver, "pool": t.Pool.Name,
		"optimized": false, "optimized_header": false, "type": string(t.Kind), "config": cfg,
	}
	if err := writeYAML(filepath.Join(dir, "backup", "index.yaml"), index); err != nil {
		return res, err
	}
	if o.StagingOnly {
		log.Info("restore_staged", "event", "restore_staged", "dir", dir)
		return res, nil
	}
	if err := r.importStream(ctx, dir, img, func(rd io.Reader) error {
		return r.Incus.ImportInstance(ctx, res.Project, res.Pool, res.Name, rd)
	}); err != nil {
		return res, err
	}
	res.Imported = true
	res.Seconds = r.now().Sub(start).Seconds()
	log.Info("instance_restored", "event", "instance_restored", "kind", t.Kind, "pool", res.Pool,
		"dropped_devices", res.DroppedDevices, "stripped_keys", res.StrippedKeys, "autostart_disabled", res.AutostartDisabled,
		"seconds", res.Seconds)
	return res, nil
}

// VolumeOptions controls a custom volume restore.
type VolumeOptions struct {
	Snapshot      string
	Volume        string
	SourceProject string
	SourcePool    string
	TargetProject string
	NewName       string
	Pool          string
	StagingOnly   bool
	KeepStaging   bool
}

// RestoreVolume restores a custom volume as a new volume.
func (r *Restorer) RestoreVolume(ctx context.Context, o VolumeOptions) (res *Result, err error) {
	start := r.now()
	loc, err := r.locate(ctx, o.Snapshot, volumeKinds, o.SourceProject, o.SourcePool, o.Volume)
	if err != nil {
		return nil, err
	}
	t, snap := loc.Target, loc.Snapshot
	res = &Result{Kind: t.Kind, Source: t.Pool.Name + "/" + t.Project + "/" + t.Name, Snapshot: snap,
		Project: or(o.TargetProject, t.Project), Name: or(o.NewName, t.Name), Pool: or(o.Pool, t.Pool.Name)}
	log := r.Log.With("snapshot", snap, "source", res.Source, "target", res.Pool+"/"+res.Project+"/"+res.Name)
	if ok, err := r.Incus.VolumeExists(res.Project, res.Pool, res.Name); err != nil {
		return nil, err
	} else if ok {
		return nil, fmt.Errorf("volume %s/%s/%s already exists; choose --name or delete it yourself first", res.Pool, res.Project, res.Name)
	}
	if _, err := r.Incus.GetPool(res.Pool); err != nil {
		return nil, fmt.Errorf("target pool %q: %w", res.Pool, err)
	}

	dir, err := r.workDir("volume")
	if err != nil {
		return nil, err
	}
	defer r.finish(dir, &o.KeepStaging, o.StagingOnly, &err, log)
	res.StagingDir = dir

	contentType := t.ContentType
	archives := map[string]string{t.ArchiveBase + ".pxar": filepath.Join(dir, "backup", "volume")}
	if contentType == "block" {
		archives = map[string]string{t.ArchiveBase + ".img": filepath.Join(dir, "backup", "volume.img")}
	}
	img, err := r.restoreArchives(ctx, loc, dir, archives, !o.StagingOnly, log)
	if err != nil {
		return res, err
	}
	vol := map[string]any{"name": t.Name, "type": "custom", "content_type": contentType, "project": t.Project, "config": map[string]string{}}
	if t.Volume != nil {
		vol["description"] = t.Volume.Description
		cfg := map[string]string{}
		for k, v := range t.Volume.Config {
			if !strings.HasPrefix(k, "volatile.") && v != "<redacted>" {
				cfg[k] = v
			}
		}
		vol["config"] = cfg
	}
	index := map[string]any{
		"name": t.Name, "backend": t.Pool.Driver, "pool": t.Pool.Name,
		"optimized": false, "optimized_header": false, "type": "custom",
		"config": map[string]any{"volume": vol},
	}
	if err := writeYAML(filepath.Join(dir, "backup", "index.yaml"), index); err != nil {
		return res, err
	}
	if o.StagingOnly {
		log.Info("restore_staged", "event", "restore_staged", "dir", dir)
		return res, nil
	}
	if err := r.importStream(ctx, dir, img, func(rd io.Reader) error {
		return r.Incus.ImportVolume(ctx, res.Project, res.Pool, res.Name, rd)
	}); err != nil {
		return res, err
	}
	res.Imported = true
	res.Seconds = r.now().Sub(start).Seconds()
	log.Info("volume_restored", "event", "volume_restored", "seconds", res.Seconds)
	return res, nil
}

// finish removes the work directory unless it should be kept.
func (r *Restorer) finish(dir string, keep *bool, stagingOnly bool, err *error, log *slog.Logger) {
	if stagingOnly || *keep {
		return
	}
	if rmErr := removeAll(dir); rmErr != nil {
		log.Warn("restore_staging_not_removed", "event", "restore_staging_not_removed", "dir", dir, "error", rmErr)
	}
	if *err != nil {
		log.Error("restore_failed", "event", "restore_failed", "error", *err)
	}
}

// imageStream is a .img archive streamed straight from PBS into the import
// (no staging copy).
type imageStream struct {
	snapshot string
	archive  string
	tarName  string // e.g. backup/virtual-machine.img
	size     int64
}

// archiveSnapshot returns the PBS snapshot holding an archive: guest
// archives live in their ct/ or vm/ group, everything else in the run index.
func archiveSnapshot(m *backup.Manifest, hostSnap, archive string) string {
	if m != nil && m.ArchiveSnapshots[archive] != "" {
		return m.ArchiveSnapshots[archive]
	}
	return hostSnap
}

// restoreArchives restores pxar archives into the staging layout. With
// stream set, .img archives are not downloaded but returned for streaming.
func (r *Restorer) restoreArchives(ctx context.Context, loc *located, dir string, archives map[string]string,
	stream bool, log *slog.Logger) (*imageStream, error) {
	filesBySnap := map[string][]pbc.File{}
	var need int64
	var img *imageStream
	for name, target := range archives {
		src := loc.source(name)
		files, ok := filesBySnap[src]
		if !ok {
			var err error
			if files, err = r.PBC.Files(ctx, r.Cfg.PBS.Namespace, src); err != nil {
				return nil, err
			}
			filesBySnap[src] = files
		}
		found := false
		for _, f := range files {
			// pxar archives may be stored split (.mpxar/.ppxar) or whole (.pxar).
			base := strings.TrimSuffix(name, ".pxar")
			if f.Filename == name || f.Filename == name+".didx" || f.Filename == name+".fidx" || f.Filename == base+".ppxar.didx" || f.Filename == base+".mpxar.didx" {
				found = true
				if stream && strings.HasSuffix(name, ".img") {
					rel, _ := filepath.Rel(dir, target)
					img = &imageStream{snapshot: src, archive: name, tarName: filepath.ToSlash(rel), size: f.Size}
					continue
				}
				need += f.Size
			}
		}
		if !found {
			return nil, fmt.Errorf("archive %s missing from snapshot %s", name, src)
		}
	}
	free := r.freeBytes(dir)
	if uint64(need) > free {
		return nil, fmt.Errorf("not enough space in %s: need %d bytes, %d free", dir, need, free)
	}
	if stream {
		// Incus writes the whole uploaded tarball to its backups directory
		// before unpacking it. Running out of space there would also hit
		// /var/lib/incus (the database) unless storage.backups_volume is set.
		total := uint64(need)
		if img != nil {
			total += uint64(img.size)
		}
		bdir := r.incusBackupsDir()
		if free, known := r.freeBytesKnown(bdir); known && total > free {
			return nil, fmt.Errorf("not enough space in %s for Incus to receive the import: need about %d bytes, %d free "+
				"(set storage.backups_volume to a pool volume with enough space)", bdir, total, free)
		}
	}
	names := make([]string, 0, len(archives))
	for n := range archives {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		if img != nil && name == img.archive {
			continue
		}
		target := archives[name]
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return nil, err
		}
		src := loc.source(name)
		t0 := r.now()
		if strings.HasSuffix(name, ".tar.blob") {
			// Per-guest VMs: the config directory is stored as a tar blob.
			if err := r.restoreTarBlob(ctx, src, name, target); err != nil {
				return nil, fmt.Errorf("restoring %s: %w", name, err)
			}
			log.Info("archive_restored", "event", "archive_restored", "archive", name, "snapshot", src, "seconds", r.now().Sub(t0).Seconds())
			continue
		}
		if err := r.PBC.Restore(ctx, r.Cfg.PBS.Namespace, src, name, target, r.Cfg.PBS.EncryptionKeyFile); err != nil {
			return nil, fmt.Errorf("restoring %s: %w", name, err)
		}
		log.Info("archive_restored", "event", "archive_restored", "archive", name, "snapshot", src, "seconds", r.now().Sub(t0).Seconds())
	}
	return img, nil
}

// IncusBackupsDir is where Incus buffers an uploaded backup before import.
const IncusBackupsDir = "/var/lib/incus/backups"

func (r *Restorer) incusBackupsDir() string {
	if r.BackupsDir != "" {
		return r.BackupsDir
	}
	return IncusBackupsDir
}

// freeBytesKnown is freeBytes, but reports whether the path could be checked.
func (r *Restorer) freeBytesKnown(path string) (uint64, bool) {
	if r.FreeBytes != nil {
		return r.FreeBytes(path), true
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, false
	}
	return uint64(st.Bavail) * uint64(st.Bsize), true
}

func (r *Restorer) freeBytes(path string) uint64 {
	if r.FreeBytes != nil {
		return r.FreeBytes(path)
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0
	}
	return uint64(st.Bavail) * uint64(st.Bsize)
}

func (r *Restorer) importStream(ctx context.Context, dir string, img *imageStream, imp func(io.Reader) error) error {
	var rc io.ReadCloser
	var wait func() error
	var err error
	switch {
	case img != nil:
		rc, wait, err = r.tarWithImage(ctx, dir, img)
	case r.Tar != nil:
		rc, wait, err = r.Tar(ctx, dir)
	default:
		rc, wait, err = GNUTar(ctx, dir)
	}
	if err != nil {
		return err
	}
	impErr := imp(rc)
	rc.Close()
	tarErr := wait()
	if impErr != nil {
		return impErr
	}
	if tarErr != nil {
		return fmt.Errorf("building backup stream: %w", tarErr)
	}
	return nil
}

// tarWithImage writes the staged files (index.yaml, VM config) and then the
// disk image streamed directly from PBS, so the image never touches the
// staging directory and download overlaps the import.
func (r *Restorer) tarWithImage(ctx context.Context, dir string, img *imageStream) (io.ReadCloser, func() error, error) {
	open := r.StreamArchive
	if open == nil {
		open = func(ctx context.Context, snap, archive string) (io.ReadCloser, func() error, error) {
			return r.PBC.RestoreStream(ctx, r.Cfg.PBS.Namespace, snap, archive, r.Cfg.PBS.EncryptionKeyFile)
		}
	}
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := func() error {
			tw := tar.NewWriter(pw)
			if err := addTree(tw, dir, "backup"); err != nil {
				return err
			}
			src, wait, err := open(ctx, img.snapshot, img.archive)
			if err != nil {
				return err
			}
			hdr := &tar.Header{Name: img.tarName, Mode: 0o600, Size: img.size, Typeflag: tar.TypeReg, ModTime: r.now(), Format: tar.FormatPAX}
			if err := tw.WriteHeader(hdr); err != nil {
				src.Close()
				return err
			}
			n, cerr := io.Copy(tw, src)
			src.Close()
			if werr := wait(); werr != nil {
				return fmt.Errorf("streaming %s: %w", img.archive, werr)
			}
			if cerr != nil {
				return cerr
			}
			if n != img.size {
				return fmt.Errorf("streaming %s: got %d bytes, expected %d", img.archive, n, img.size)
			}
			return tw.Close()
		}()
		pw.CloseWithError(err)
		done <- err
	}()
	return pr, func() error { return <-done }, nil
}

// addTree adds dir/name recursively (dirs, regular files, symlinks) with
// numeric ownership and modes.
func addTree(tw *tar.Writer, dir, name string) error {
	return filepath.Walk(filepath.Join(dir, name), func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		link := ""
		if fi.Mode()&os.ModeSymlink != 0 {
			if link, err = os.Readlink(p); err != nil {
				return err
			}
		}
		hdr, err := tar.FileInfoHeader(fi, link)
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if fi.IsDir() {
			hdr.Name += "/"
		}
		hdr.Uname, hdr.Gname = "", ""
		hdr.Format = tar.FormatPAX
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if fi.Mode().IsRegular() {
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			_, err = io.Copy(tw, f)
			f.Close()
			return err
		}
		return nil
	})
}

// GNUTar streams <dir>/backup with numeric owners, xattrs and ACLs, the same
// properties Incus itself preserves in exports.
func GNUTar(ctx context.Context, dir string) (io.ReadCloser, func() error, error) {
	cmd := exec.CommandContext(ctx, "tar", "-C", dir, "--numeric-owner", "--xattrs", "--xattrs-include=*",
		"--acls", "-cf", "-", "backup")
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
			return fmt.Errorf("tar: %w: %s", err, strings.TrimSpace(stderr.String()))
		}
		return nil
	}, nil
}

func readBackupYAML(path string) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading backup.yaml from archive: %w", err)
	}
	var cfg map[string]any
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("parsing backup.yaml: %w", err)
	}
	if _, ok := cfg["container"].(map[string]any); !ok {
		return nil, errors.New("backup.yaml has no instance section")
	}
	return cfg, nil
}

func writeYAML(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := yaml.Marshal(v)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

func stripIdentity(cfg map[string]any) []string {
	inst := cfg["container"].(map[string]any)
	var stripped []string
	for _, key := range []string{"config", "expanded_config"} {
		c, _ := inst[key].(map[string]any)
		for k := range c {
			if identityKeyRE.MatchString(k) && !strings.Contains(k, ".last_state.") {
				delete(c, k)
				if key == "config" {
					stripped = append(stripped, k)
				}
			}
		}
	}
	// A MAC set on the NIC device itself (e.g. for a DHCP reservation) would
	// clash with the original on the same network; Incus generates a new one.
	for _, key := range []string{"devices", "expanded_devices"} {
		devs, _ := inst[key].(map[string]any)
		for n, d := range devs {
			dm, _ := d.(map[string]any)
			if str(dm["type"]) == "nic" && dm["hwaddr"] != nil {
				delete(dm, "hwaddr")
				if key == "devices" {
					stripped = append(stripped, "devices."+n+".hwaddr")
				}
			}
		}
	}
	sort.Strings(stripped)
	return stripped
}

// disableAutostart keeps a copy that runs next to its original from starting
// on its own (boot.autostart unset + last state RUNNING would start it).
func disableAutostart(cfg map[string]any) {
	inst := cfg["container"].(map[string]any)
	for _, key := range []string{"config", "expanded_config"} {
		c, _ := inst[key].(map[string]any)
		if c == nil {
			c = map[string]any{}
			inst[key] = c
		}
		c["boot.autostart"] = "false"
		delete(c, "volatile.last_state.power")
	}
}

// checkDependencies verifies profiles, networks and custom volumes referenced
// by the instance exist in the target project. Missing local devices can be
// dropped (explicitly) - missing profiles are always an error.
func (r *Restorer) checkDependencies(cfg map[string]any, project, pool string, drop bool) ([]string, error) {
	inst := cfg["container"].(map[string]any)
	var problems, dropped []string
	if profiles, ok := inst["profiles"].([]any); ok {
		for _, p := range profiles {
			name := fmt.Sprint(p)
			if ok, err := r.Incus.ProfileExists(project, name); err != nil {
				return nil, err
			} else if !ok {
				problems = append(problems, fmt.Sprintf("profile %q does not exist in project %s (recreate it from incus-metadata, see `restore host`)", name, project))
			}
		}
	}
	devices, _ := inst["devices"].(map[string]any)
	names := make([]string, 0, len(devices))
	for n := range devices {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		d, _ := devices[n].(map[string]any)
		typ, devPool, source, network := str(d["type"]), str(d["pool"]), str(d["source"]), str(d["network"])
		var missing string
		switch {
		case typ == "disk" && devPool != "" && source != "" && str(d["path"]) != "/":
			vi, err := r.Incus.VolumeInfo(project, devPool, source)
			if err != nil {
				return nil, err
			}
			switch {
			case !vi.Exists:
				missing = fmt.Sprintf("device %q needs custom volume %s/%s/%s (restore it first with `restore volume`)", n, devPool, project, source)
			case vi.ContentType == "block" && !vi.Shared && len(vi.UsedBy) > 0:
				missing = fmt.Sprintf("device %q: block volume %s/%s/%s is already attached to %v and is not shared (restore a copy with `restore volume --name`, or drop the device)", n, devPool, project, source, vi.UsedBy)
			}
		case typ == "nic" && network != "":
			ok, err := r.Incus.NetworkExists(project, network)
			if err != nil {
				return nil, err
			}
			if !ok {
				missing = fmt.Sprintf("device %q needs network %q", n, network)
			}
		}
		if missing == "" {
			continue
		}
		if drop {
			delete(devices, n)
			if ed, ok := inst["expanded_devices"].(map[string]any); ok {
				delete(ed, n)
			}
			dropped = append(dropped, n)
			continue
		}
		problems = append(problems, missing)
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("cannot restore into project %s:\n  - %s\n(use --drop-missing-devices to restore without missing devices)", project, strings.Join(problems, "\n  - "))
	}
	return dropped, nil
}

// isolate masks every device except the root disk and the cloud-init/agent
// config drives with a local "none" device (this also masks devices that
// come from profiles), disables autostart and marks the instance.
func isolate(cfg map[string]any, now time.Time) []string {
	inst := cfg["container"].(map[string]any)
	devices, _ := inst["devices"].(map[string]any)
	if devices == nil {
		devices = map[string]any{}
		inst["devices"] = devices
	}
	expanded, _ := inst["expanded_devices"].(map[string]any)
	names := map[string]map[string]any{}
	for _, src := range []map[string]any{expanded, devices} {
		for n, d := range src {
			dm, _ := d.(map[string]any)
			names[n] = dm
		}
	}
	var masked []string
	for n, d := range names {
		if keepIsolated(d) {
			continue
		}
		devices[n] = map[string]any{"type": "none"}
		delete(expanded, n)
		masked = append(masked, n)
	}
	sort.Strings(masked)
	for _, key := range []string{"config", "expanded_config"} {
		c, _ := inst[key].(map[string]any)
		if c == nil {
			c = map[string]any{}
			inst[key] = c
		}
		c["boot.autostart"] = "false"
		c[incus.RestoreTestKey] = now.UTC().Format(time.RFC3339)
	}
	return masked
}

func keepIsolated(d map[string]any) bool {
	if str(d["type"]) != "disk" {
		return false
	}
	src := str(d["source"])
	return str(d["path"]) == "/" || strings.HasPrefix(src, "cloud-init:") || strings.HasPrefix(src, "agent:")
}

func str(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// restoreTarBlob downloads a tar blob and unpacks it into dir.
func (r *Restorer) restoreTarBlob(ctx context.Context, snap, archive, dir string) error {
	tmp := dir + ".tar"
	if err := r.PBC.Restore(ctx, r.Cfg.PBS.Namespace, snap, archive, tmp, r.Cfg.PBS.EncryptionKeyFile); err != nil {
		return err
	}
	defer os.Remove(tmp)
	f, err := os.Open(tmp)
	if err != nil {
		return err
	}
	defer f.Close()
	return untarInto(f, dir)
}

// untarInto extracts a tar stream into dir (regular files, dirs, symlinks)
// with modes and numeric owners. Every operation goes through an os.Root, so
// neither "../" names nor symlinks in the archive can reach outside dir.
func untarInto(rd io.Reader, dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	tr := tar.NewReader(rd)
	type dirMode struct {
		path string
		mode os.FileMode
	}
	var dirs []dirMode
	for {
		h, err := tr.Next()
		if err == io.EOF {
			// Directory modes last, so restrictive dirs (e.g. dr-x------) don't block their content.
			for i := len(dirs) - 1; i >= 0; i-- {
				_ = root.Chmod(dirs[i].path, dirs[i].mode)
			}
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(h.Name)
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			return fmt.Errorf("unsafe path in archive: %q", h.Name)
		}
		mode := os.FileMode(h.Mode).Perm()
		switch h.Typeflag {
		case tar.TypeDir:
			if name != "." {
				if err := root.MkdirAll(name, 0o700); err != nil {
					return err
				}
			}
			dirs = append(dirs, dirMode{name, mode})
		case tar.TypeReg:
			if d := filepath.Dir(name); d != "." {
				if err := root.MkdirAll(d, 0o700); err != nil {
					return err
				}
			}
			out, err := root.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				return fmt.Errorf("extracting %q: %w", h.Name, err)
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if d := filepath.Dir(name); d != "." {
				if err := root.MkdirAll(d, 0o700); err != nil {
					return err
				}
			}
			if err := root.Symlink(h.Linkname, name); err != nil {
				return err
			}
		default:
			continue
		}
		if h.Typeflag != tar.TypeSymlink {
			_ = root.Lchown(name, h.Uid, h.Gid) // numeric owners; best effort when not root (tests)
		}
		if h.Typeflag == tar.TypeReg {
			_ = root.Chmod(name, mode)
		}
	}
}

// removeAll deletes a staging tree even if restored directories are
// read-only (e.g. a VM's config/ is dr-x------).
func removeAll(dir string) error {
	_ = filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.IsDir() && fi.Mode().Perm()&0o700 != 0o700 {
			_ = os.Chmod(p, fi.Mode().Perm()|0o700)
		}
		return nil
	})
	return os.RemoveAll(dir)
}

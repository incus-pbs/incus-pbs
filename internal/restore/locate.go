package restore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/incus-pbs/incus-pbs/internal/backup"
	"github.com/incus-pbs/incus-pbs/internal/pbc"
)

var (
	instanceKinds = []backup.Kind{backup.KindContainer, backup.KindVM}
	volumeKinds   = []backup.Kind{backup.KindVolume}
	anyKinds      = []backup.Kind{backup.KindContainer, backup.KindVM, backup.KindVolume}
)

// located is a target found in a backup.
type located struct {
	// Snapshot is the guest snapshot holding the target, or the run index
	// (host/<id>/<time>) whose manifest lists it.
	Snapshot string
	Time     int64
	Target   backup.Target
	Host     *backup.Manifest      // found through a run index
	Guest    *backup.GuestManifest // found in a guest snapshot
	// Instance is the instance as the Incus API returned it at backup time
	// (nil if unknown); used to check dependencies before downloading.
	Instance map[string]any
}

// source returns the snapshot that holds archive.
func (l *located) source(archive string) string {
	if l.Host != nil {
		return archiveSnapshot(l.Host, l.Snapshot, archive)
	}
	return l.Snapshot
}

func (l *located) identity() string {
	return string(l.Target.Kind) + "/" + l.Target.Project + "/" + l.Target.Pool.Name + "/" + l.Target.Name
}

// GuestManifest downloads and parses incus-guest.conf of a guest snapshot.
func (r *Restorer) GuestManifest(ctx context.Context, snapshot string) (*backup.GuestManifest, error) {
	dir, err := r.workDir("guest")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "incus-guest.json")
	if err := r.PBC.Restore(ctx, r.Cfg.PBS.Namespace, snapshot, backup.GuestManifestArchive, path, r.Cfg.PBS.EncryptionKeyFile); err != nil {
		return nil, fmt.Errorf("%s: restoring %s (not created by this incus-pbs version?): %w", snapshot, backup.GuestManifestArchive, err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m backup.GuestManifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s: parsing guest manifest: %w", snapshot, err)
	}
	if m.GuestManifestVersion != backup.GuestManifestVersion {
		return nil, fmt.Errorf("%s: guest manifest version %d is not supported (this incus-pbs reads version %d): %w",
			snapshot, m.GuestManifestVersion, backup.GuestManifestVersion, errManifestVersion)
	}
	return &m, nil
}

var errManifestVersion = errors.New("unsupported manifest version")

// findGuestTarget locates a target in a guest manifest.
func findGuestTarget(m *backup.GuestManifest, kinds []backup.Kind, project, name, pool string) (backup.Target, bool) {
	for _, t := range m.Targets {
		if t.Name == name && (project == "" || t.Project == project) && (pool == "" || t.Pool.Name == pool) && slices.Contains(kinds, t.Kind) {
			return t, true
		}
	}
	return backup.Target{}, false
}

// groupMayHold reports whether a guest group can contain the target
// without reading its manifest: instance groups are named after the
// instance, volumes may live in any instance's group.
func groupMayHold(s pbc.Snapshot, kinds []backup.Kind, project, name string) bool {
	if slices.Contains(kinds, backup.KindVolume) {
		return true
	}
	if project != "" {
		return s.BackupID == backup.GroupID(project, name) || s.BackupID == backup.LegacyGroupID(project, name)
	}
	return s.BackupID == name || strings.HasSuffix(s.BackupID, "_"+name) || strings.HasSuffix(s.BackupID, "-"+name)
}

// groupsNewestFirst returns the finished guest snapshots taken at or before
// cutoff (unix seconds), grouped by PBS group, each group newest first, the
// groups ordered by their newest snapshot. Snapshots still being written (a
// backup in progress) are skipped.
func groupsNewestFirst(all []pbc.Snapshot, cutoff int64) [][]pbc.Snapshot {
	by := map[string][]pbc.Snapshot{}
	for _, s := range all {
		if (s.BackupType != "ct" && s.BackupType != "vm") || s.BackupTime > cutoff || !s.Finished() {
			continue
		}
		key := s.BackupType + "/" + s.BackupID
		by[key] = append(by[key], s)
	}
	out := make([][]pbc.Snapshot, 0, len(by))
	for _, g := range by {
		newestFirst(g)
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i][0].BackupTime != out[j][0].BackupTime {
			return out[i][0].BackupTime > out[j][0].BackupTime
		}
		return out[i][0].Path() < out[j][0].Path()
	})
	return out
}

// maxGroupWalk bounds how many snapshots of one group are read looking for
// one that belongs to this host.
const maxGroupWalk = 32

// ownGuestSnapshots returns, per PBS group, the newest guest snapshot at or
// before cutoff that this host wrote, together with its manifest. Groups are
// only read when keep accepts them. Snapshots without an incus-pbs guest
// manifest (e.g. Proxmox VE backups in the same namespace) and manifests of
// an unsupported version are skipped with a warning instead of failing the
// whole lookup; other errors (PBS unreachable, ...) are returned.
func (r *Restorer) ownGuestSnapshots(ctx context.Context, cutoff int64, keep func(pbc.Snapshot) bool) ([]guestBackup, error) {
	all, err := r.PBC.ListSnapshots(ctx, r.Cfg.PBS.Namespace)
	if err != nil {
		return nil, err
	}
	var out []guestBackup
	for _, group := range groupsNewestFirst(all, cutoff) {
		if !keep(group[0]) {
			continue
		}
		for i, s := range group {
			if i == maxGroupWalk {
				r.warn("guest_group_skipped", "group", s.BackupType+"/"+s.BackupID,
					"reason", fmt.Sprintf("none of the newest %d snapshots belong to host %s", maxGroupWalk, r.Cfg.PBS.BackupID))
				break
			}
			if !s.HasFile(backup.GuestManifestArchive + ".blob") {
				r.warn("guest_snapshot_skipped", "snapshot", s.Path(), "reason", "no incus-pbs guest manifest")
				continue
			}
			gm, err := r.GuestManifest(ctx, s.Path())
			if errors.Is(err, errManifestVersion) {
				r.warn("guest_snapshot_skipped", "snapshot", s.Path(), "reason", err.Error())
				continue
			}
			if err != nil {
				return nil, err
			}
			if gm.HostBackupID != r.Cfg.PBS.BackupID {
				continue // another host sharing the namespace and group
			}
			out = append(out, guestBackup{Snapshot: s.Path(), Time: s.BackupTime, Manifest: gm})
			break
		}
	}
	return out, nil
}

func (r *Restorer) warn(event string, args ...any) {
	if r.Log != nil {
		r.Log.Warn(event, append([]any{"event", event}, args...)...)
	}
}

// cutoffOf turns "latest" or a time into the newest acceptable backup time.
func cutoffOf(ref string) (int64, bool) {
	if ref == "latest" {
		return math.MaxInt64, true
	}
	if timeOnlyRE.MatchString(ref) {
		if t, err := time.Parse(time.RFC3339, ref); err == nil {
			return t.Unix(), true
		}
	}
	return 0, false
}

// locate finds a target for a restore.
//
//   - ct|vm/<id>/<time>: that guest snapshot.
//   - host/<id>/<time>: that run index (volumes stored in it).
//   - latest or a time: the newest backup at or before it. Instances come
//     from their own group; volumes from the newest guest snapshot or run
//     index that holds them.
func (r *Restorer) locate(ctx context.Context, ref string, kinds []backup.Kind, project, pool, name string) (*located, error) {
	switch {
	case pbc.ValidSnapshotPath(ref) && IsGuestPath(ref):
		gm, err := r.GuestManifest(ctx, ref)
		if err != nil {
			return nil, err
		}
		t, ok := findGuestTarget(gm, kinds, project, name, pool)
		if !ok {
			return nil, fmt.Errorf("%q is not part of guest snapshot %s", name, ref)
		}
		return &located{Snapshot: ref, Target: t, Guest: gm, Instance: gm.Instance}, nil
	case pbc.ValidSnapshotPath(ref):
		if !slices.Contains(kinds, backup.KindVolume) {
			return nil, fmt.Errorf("%s is a run index; instances are restored from their own snapshots: use latest, a time or ct|vm/<id>/<time> (see `restore list %s`)", ref, name)
		}
		return r.locateInIndex(ctx, ref, project, pool, name)
	}
	cutoff, ok := cutoffOf(ref)
	if !ok {
		return nil, fmt.Errorf("invalid snapshot %q (use latest, 2026-11-24T02:30:00Z, host/<id>/<time> or ct|vm/<id>/<time>)", ref)
	}
	guests, err := r.ownGuestSnapshots(ctx, cutoff, func(s pbc.Snapshot) bool { return groupMayHold(s, kinds, project, name) })
	if err != nil {
		return nil, err
	}
	var found []*located
	for _, g := range guests {
		if t, ok := findGuestTarget(g.Manifest, kinds, project, name, pool); ok {
			found = append(found, &located{Snapshot: g.Snapshot, Time: g.Time, Target: t, Guest: g.Manifest, Instance: g.Manifest.Instance})
		}
	}
	if slices.Contains(kinds, backup.KindVolume) {
		idx, err := r.Snapshots(ctx)
		if err != nil {
			return nil, err
		}
		for _, s := range idx {
			if s.BackupTime > cutoff {
				continue
			}
			l, err := r.locateInIndex(ctx, s.Path(), project, pool, name)
			if errors.Is(err, errNotBackedUp) {
				continue // failed in that run: an older index may hold it
			}
			if errors.Is(err, errNotInSnapshot) {
				break // not stored in run indexes (it lives in a guest group)
			}
			if err != nil {
				return nil, err // ambiguous, PBS unreachable, unreadable manifest: never fall back silently
			}
			l.Time = s.BackupTime
			found = append(found, l)
			break // the newest index at or before the cutoff that holds it
		}
	}
	if len(found) == 0 {
		when := "any backup"
		if ref != "latest" {
			when = "a backup at or before " + ref
		}
		return nil, fmt.Errorf("%q is not part of %s of host/%s", name, when, r.Cfg.PBS.BackupID)
	}
	ids := map[string]bool{}
	for _, l := range found {
		ids[l.identity()] = true
	}
	if len(ids) > 1 {
		names := make([]string, 0, len(ids))
		for id := range ids {
			names = append(names, id)
		}
		sort.Strings(names)
		return nil, fmt.Errorf("%q is ambiguous (%s); specify --project/--source-pool", name, strings.Join(names, ", "))
	}
	sort.SliceStable(found, func(i, j int) bool { return found[i].Time > found[j].Time })
	return found[0], nil
}

// locateInIndex finds a volume in a run index.
func (r *Restorer) locateInIndex(ctx context.Context, snap, project, pool, name string) (*located, error) {
	m, err := r.Manifest(ctx, snap)
	if err != nil {
		return nil, err
	}
	t, err := findTarget(m, volumeKinds, project, name, pool)
	if err != nil {
		return nil, err
	}
	return &located{Snapshot: snap, Target: t, Host: m}, nil
}

// vmConfigArchive returns the archive holding a VM's config directory: a
// tar blob when the VM went through the block library, a pxar otherwise.
func (r *Restorer) vmConfigArchive(ctx context.Context, l *located) (string, error) {
	blob := l.Target.ArchiveBase + "-config.tar.blob"
	files, err := r.PBC.Files(ctx, r.Cfg.PBS.Namespace, l.source(blob))
	if err != nil {
		return "", err
	}
	for _, f := range files {
		if f.Filename == blob {
			return blob, nil
		}
	}
	return l.Target.ArchiveBase + "-config.pxar", nil
}

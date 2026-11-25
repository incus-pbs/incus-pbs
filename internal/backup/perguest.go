package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/incus-pbs/incus-pbs/internal/pbc"
)

// Per-guest layout: every instance gets its own PBS backup group
// (ct/<id> or vm/<id>), volumes attached to exactly one instance go into
// that instance's group, and host/<backup_id> holds the run index
// (manifest with archive -> snapshot map), metadata, host config and
// shared/standalone volumes. Guest groups are uploaded first; the host
// index last, so it only references snapshots that exist.

// GroupID is the PBS backup id of an instance: the name in the default
// project, "<project>_<name>" elsewhere. Incus forbids "_" in both project
// and instance names, so the mapping is unambiguous.
func GroupID(project, name string) string {
	if project == "" || project == "default" {
		return name
	}
	return project + "_" + name
}

// LegacyGroupID is the group id used before 0.2 ("<project>-<name>"). It
// could collide (project a + b-c vs project a-b + c), so it is only read
// for restores of older backups, never written.
func LegacyGroupID(project, name string) string {
	if project == "" || project == "default" {
		return name
	}
	return project + "-" + name
}

type guestGroup struct {
	Type     string // ct | vm | host
	ID       string
	results  []*prepared
	archives []pbc.Archive
}

func (g *guestGroup) key() string { return g.Type + "/" + g.ID }

// soleOwner returns "project/name" when a volume is used by exactly one
// instance (snapshots of that instance count as the same instance).
func soleOwner(v Target) (string, bool) {
	owners := map[string]bool{}
	for _, u := range v.UsedBy {
		path, query, _ := strings.Cut(u, "?")
		if !strings.HasPrefix(path, "/1.0/instances/") {
			return "", false // profiles or other users: shared
		}
		name := strings.SplitN(strings.TrimPrefix(path, "/1.0/instances/"), "/", 2)[0]
		project := v.Project
		if strings.HasPrefix(query, "project=") {
			project = strings.TrimPrefix(query, "project=")
		}
		owners[project+"/"+name] = true
	}
	if len(owners) != 1 {
		return "", false
	}
	for o := range owners {
		return o, true
	}
	return "", false
}

// assignGroups distributes prepared targets over PBS groups.
func (e *Engine) assignGroups(results []prepared) ([]*guestGroup, *guestGroup, error) {
	host := &guestGroup{Type: "host", ID: e.Cfg.PBS.BackupID}
	byInstance := map[string]*guestGroup{}
	byKey := map[string]string{}
	var groups []*guestGroup
	for i := range results {
		r := &results[i]
		if r.err != nil || r.t.Kind == KindVolume {
			continue
		}
		typ := "ct"
		if r.t.Kind == KindVM {
			typ = "vm"
		}
		g := &guestGroup{Type: typ, ID: GroupID(r.t.Project, r.t.Name)}
		if other, dup := byKey[g.key()]; dup {
			return nil, nil, fmt.Errorf("PBS group %s would be shared by %s and %s; rename one instance", g.key(), other, r.t.ID())
		}
		byKey[g.key()] = r.t.ID()
		g.results = append(g.results, r)
		g.archives = append(g.archives, r.archives...)
		byInstance[r.t.Project+"/"+r.t.Name] = g
		groups = append(groups, g)
	}
	for i := range results {
		r := &results[i]
		if r.err != nil || r.t.Kind != KindVolume {
			continue
		}
		g := host
		if owner, ok := soleOwner(r.t); ok && byInstance[owner] != nil {
			g = byInstance[owner]
		}
		g.results = append(g.results, r)
		g.archives = append(g.archives, r.archives...)
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].key() < groups[j].key() })
	return groups, host, nil
}

// runPerGuest uploads the prepared targets group by group, then the host
// index. complete reports whether every target was prepared successfully.
//
// Every guest group carries its own manifest (incus-guest.conf), so guest
// snapshots restore without the host index. writeIndex=false skips the host
// index when a scheduled run covers only instances; it is still written
// whenever a volume ends up in the host group.
func (e *Engine) runPerGuest(ctx context.Context, runID string, start time.Time, sel Selection, plan *Plan,
	results []prepared, complete, writeIndex bool, rep *Report, cleanup *cleanupStack, log *slog.Logger) error {
	groups, host, err := e.assignGroups(results)
	if err != nil {
		return err
	}
	rep.ArchiveSnapshots = map[string]string{}
	itemOf := func(r *prepared) *ItemResult {
		for i := range rep.Items {
			it := &rep.Items[i]
			if it.Kind == r.t.Kind && it.Project == r.t.Project && it.Name == r.t.Name {
				return it
			}
		}
		return nil
	}
	backend, st, blockOK := e.blockAvailable(rep, log)
	failedGroups := 0
	dumps := &projectDumps{e: e, cache: map[string]*projectDump{}}

	for _, g := range groups {
		if ctx.Err() != nil {
			return fmt.Errorf("interrupted: %w", ctx.Err())
		}
		mf, err := e.writeGuestManifest(runID, start, sel, g, dumps, cleanup)
		var snap string
		if err == nil {
			g.archives = append(g.archives, pbc.Archive{Name: GuestManifestArchive, Source: mf})
			snap, err = e.uploadGroup(ctx, runID, g, backend, st, blockOK, rep, log)
		}
		for _, r := range g.results {
			it := itemOf(r)
			if it == nil {
				continue
			}
			if err != nil {
				it.Status, it.Error = "failed", "upload to "+g.key()+": "+err.Error()
				continue
			}
			it.Status, it.Snapshot = "ok", snap
			for _, a := range it.Archives {
				rep.ArchiveSnapshots[a] = snap
			}
			event := "instance_backup_completed"
			if it.Kind == KindVolume {
				event = "volume_backup_completed"
			}
			log.Info(event, "event", event, "kind", it.Kind, "project", it.Project, "name", it.Name,
				"archives", it.Archives, "pbs_snapshot", snap)
		}
		if err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("interrupted: %w", ctx.Err())
			}
			failedGroups++
			complete = false
			log.Error("group_upload_failed", "event", "group_upload_failed", "group", g.key(), "error", err)
		}
	}

	if !writeIndex && len(host.results) == 0 {
		rep.Complete = complete
		if complete {
			rep.ExitCode = ExitOK
			return nil
		}
		rep.ExitCode = ExitPartial
		return fmt.Errorf("run %s is incomplete (%d group(s) failed to upload)", runID, failedGroups)
	}

	// Host index last: it must only reference finished guest snapshots.
	extra, err := e.writeMetadata(ctx, runID, start, sel, plan, rep, complete, cleanup)
	if err != nil {
		return fmt.Errorf("metadata: %w", err)
	}
	host.archives = append(host.archives, extra...)
	snap, err := e.clientUpload(ctx, host, rep, log)
	if err != nil {
		return fmt.Errorf("uploading the run index (%s) failed; the guest snapshots of this run are complete and restorable on their own: %w", host.key(), err)
	}
	rep.PBSSnapshot = snap
	for _, r := range host.results {
		if it := itemOf(r); it != nil {
			it.Status, it.Snapshot = "ok", snap
			for _, a := range it.Archives {
				rep.ArchiveSnapshots[a] = snap
			}
		}
	}
	rep.Complete = complete
	if complete {
		rep.ExitCode = ExitOK
		return nil
	}
	rep.ExitCode = ExitPartial
	return fmt.Errorf("run %s is incomplete (%d group(s) failed to upload; see manifest)", snap, failedGroups)
}

// uploadGroup writes one guest group. VMs with only block images (plus
// their small config directory) go through the incremental library as a
// single snapshot; everything else (and any library failure) through
// proxmox-backup-client.
func (e *Engine) uploadGroup(ctx context.Context, runID string, g *guestGroup, backend BlockBackend, st *BlockState,
	blockOK bool, rep *Report, log *slog.Logger) (string, error) {
	if g.Type == "vm" && blockOK {
		var imgs []*blockImage
		var configDir, configArchive, guestManifest string
		libOnly := true
		for _, a := range g.archives {
			switch {
			case a.Name == GuestManifestArchive:
				guestManifest = a.Source
			case strings.HasSuffix(a.Name, ".img"):
			case strings.HasSuffix(a.Name, "-config.pxar") && configDir == "":
				configDir, configArchive = a.Source, a.Name
			default:
				libOnly = false // e.g. filesystem volumes attached to the VM
			}
		}
		if libOnly {
			sub := make([]prepared, 0, len(g.results))
			for _, r := range g.results {
				sub = append(sub, *r)
			}
			imgs = collectBlockImages(runID, sub, rep)
			now := e.now()
			// Anything going wrong before the library session is not a reason
			// to fail the group: proxmox-backup-client reads it in full.
			fallback := func(what string, err error) {
				log.Warn("block_prepare_failed", "event", "block_prepare_failed", "group", g.key(), "step", what,
					"error", err, "fallback", "proxmox-backup-client full read")
			}
			prepErr := e.planBlockImages(ctx, imgs, st, now, log)
			if prepErr != nil {
				fallback("plan", prepErr)
			}
			blobs := map[string][]byte{}
			if guestManifest != "" && prepErr == nil {
				data, err := os.ReadFile(guestManifest)
				if err != nil {
					prepErr = err
					fallback("guest manifest", err)
				}
				blobs[GuestManifestArchive] = data
			}
			blobName := ""
			if configDir != "" && prepErr == nil {
				data, err := tarDir(configDir)
				if err != nil {
					prepErr = err
					fallback("packing VM config", err)
				}
				blobName = strings.TrimSuffix(configArchive, ".pxar") + ".tar"
				blobs[blobName] = data
			}
			snap, ok := "", false
			if prepErr == nil {
				var err error
				snap, ok, err = e.blockSession(ctx, backend, st, g.ID, imgs, blobs, runID, now, rep, log)
				if err != nil {
					return "", err
				}
			}
			if ctx.Err() != nil {
				return "", fmt.Errorf("interrupted: %w", ctx.Err())
			}
			if ok {
				// The config directory went in as a blob: record the real archive name.
				for _, r := range g.results {
					for i := range rep.Items {
						it := &rep.Items[i]
						if it.Kind != r.t.Kind || it.Project != r.t.Project || it.Name != r.t.Name {
							continue
						}
						for j, a := range it.Archives {
							if a == configArchive {
								it.Archives[j] = blobName + ".blob"
							}
						}
					}
				}
				return snap, nil
			}
			// fall through: library failed, use the client for this group
		}
	}
	return e.clientUpload(ctx, g, rep, log)
}

// clientUpload writes a group with proxmox-backup-client (one snapshot).
func (e *Engine) clientUpload(ctx context.Context, g *guestGroup, rep *Report, log *slog.Logger) (string, error) {
	upCtx := ctx
	if d := e.Cfg.PBS.Timeout.D(); d > 0 {
		var cancel context.CancelFunc
		upCtx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}
	log.Info("pbs_upload_started", "event", "pbs_upload_started", "group", g.key(), "archives", len(g.archives))
	br, err := e.PBC.Backup(upCtx, pbc.BackupOptions{
		Namespace: e.Cfg.PBS.Namespace, BackupType: g.Type, BackupID: g.ID,
		KeyFile: e.Cfg.PBS.EncryptionKeyFile, ChangeDetection: e.Cfg.PBS.ChangeDetectionMode, Archives: g.archives,
		Exclude: e.Cfg.PBS.ExcludePatterns, RateLimit: e.Cfg.PBS.RateLimit,
	})
	rep.UploadSeconds += br.Duration.Seconds()
	rep.ArchiveStats = append(rep.ArchiveStats, br.Stats...)
	if err != nil {
		log.Error("pbs_upload_failed", "event", "pbs_upload_failed", "group", g.key(), "exit_code", br.ExitCode, "error", err,
			"output_tail", tail(br.Output, 1500))
		return "", err
	}
	snap := br.Snapshot
	if snap == "" {
		snap = g.key()
	}
	return snap, nil
}

const maxConfigBlob = 64 << 20

// tarDir packs a small directory (a VM's config dataset: backup.yaml,
// NVRAM, agent certificates) with numeric owners and modes.
func tarDir(dir string) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	err := filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if rel == "." {
			return nil
		}
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
		hdr.Uname, hdr.Gname, hdr.Format = "", "", tar.FormatPAX
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
			if err != nil {
				return err
			}
		}
		if buf.Len() > maxConfigBlob {
			return fmt.Errorf("config directory exceeds %d bytes", maxConfigBlob)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

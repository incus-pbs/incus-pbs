package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/incus-pbs/incus-pbs/internal/execx"
	"github.com/incus-pbs/incus-pbs/internal/pbc"
)

// ManifestVersion is bumped on incompatible manifest changes. Version 3:
// guest snapshots carry their own manifest (incus-guest.conf) and run
// indexes only cover what their run backed up. Older versions are not supported.
const ManifestVersion = 3

// Manifest describes a backup snapshot; stored as manifest.json in incus-metadata.pxar.
type Manifest struct {
	ManifestVersion int               `json:"manifest_version"`
	Tool            string            `json:"tool"`
	ToolVersion     string            `json:"tool_version"`
	Strategy        string            `json:"strategy"`
	RunID           string            `json:"run_id"`
	Hostname        string            `json:"hostname"`
	CreatedAt       time.Time         `json:"created_at"`
	Complete        bool              `json:"complete"`
	Projects        []string          `json:"projects"`
	Selection       Selection         `json:"selection"`
	PBS             map[string]string `json:"pbs"`
	Versions        map[string]string `json:"versions"`
	Items           []ItemResult      `json:"items"`
	Targets         []Target          `json:"targets"`
	Excluded        []Excluded        `json:"excluded,omitempty"`
	Warnings        []string          `json:"warnings,omitempty"`
	// ArchiveSnapshots maps every guest archive to the PBS snapshot
	// (ct/<id> or vm/<id>) holding it. Archives not listed are in this
	// host snapshot (the run index) itself.
	ArchiveSnapshots map[string]string `json:"archive_snapshots,omitempty"`
}

func (e *Engine) writeMetadata(ctx context.Context, runID string, start time.Time, sel Selection, plan *Plan,
	rep *Report, complete bool, cl *cleanupStack) ([]pbc.Archive, error) {
	base := e.Cfg.Storage.StagingDirectory
	mdDir := filepath.Join(base, "metadata")
	if err := freshDir(mdDir); err != nil {
		return nil, err
	}
	cl.push("remove "+mdDir, func(context.Context) error { return os.RemoveAll(mdDir) })

	projects := sel.Projects
	if len(projects) == 0 {
		projects = e.Cfg.Incus.Projects
	}
	m := Manifest{
		ManifestVersion: ManifestVersion, Tool: "incus-pbs", ToolVersion: e.Version,
		Strategy: "zfs-snapshot-tree", RunID: runID, Hostname: e.hostname(), CreatedAt: start.UTC(),
		Complete: complete, Projects: projects, Selection: sel,
		PBS: map[string]string{
			"namespace": e.Cfg.PBS.Namespace, "backup_id": e.Cfg.PBS.BackupID,
			"change_detection_mode": e.Cfg.PBS.ChangeDetectionMode, "encrypted": fmt.Sprint(e.Cfg.PBS.EncryptionKeyFile != ""),
		},
		Versions: rep.Versions, Items: rep.Items, Targets: plan.Targets, Excluded: plan.Excluded, Warnings: plan.Warnings,
		ArchiveSnapshots: rep.ArchiveSnapshots,
	}
	if err := writeJSON(filepath.Join(mdDir, "manifest.json"), m); err != nil {
		return nil, err
	}
	for _, p := range projects {
		md, err := e.Incus.Metadata(p)
		if err != nil {
			return nil, fmt.Errorf("collecting incus metadata for project %s: %w", p, err)
		}
		if err := writeJSON(filepath.Join(mdDir, "incus-project-"+p+".json"), md); err != nil {
			return nil, err
		}
	}
	archives := []pbc.Archive{{Name: "incus-metadata.pxar", Source: mdDir}}

	if e.Cfg.Backup.IncludeHostConfig {
		hcDir := filepath.Join(base, "host-config")
		if err := freshDir(hcDir); err != nil {
			return nil, err
		}
		cl.push("remove "+hcDir, func(context.Context) error { return os.RemoveAll(hcDir) })
		if err := e.writeHostConfig(ctx, hcDir); err != nil {
			return nil, err
		}
		archives = append(archives, pbc.Archive{Name: "host-config.pxar", Source: hcDir})
	}
	return archives, nil
}

// writeHostConfig copies allowlisted files and records pool layout.
func (e *Engine) writeHostConfig(ctx context.Context, dir string) error {
	// Host config is best effort: a missing or unreadable file is recorded,
	// never a reason to fail the instance backup.
	var missing []string
	for _, f := range e.Cfg.Backup.HostConfigFiles {
		if err := copyFile(f, filepath.Join(dir, "files", filepath.Clean(f))); err != nil {
			if !os.IsNotExist(err) {
				e.Log.Warn("host_config_file_skipped", "event", "host_config_file_skipped", "file", f, "error", err)
			}
			missing = append(missing, f+": "+err.Error())
		}
	}
	cmds := map[string]execx.Cmd{
		"zpool-list.txt": {Name: "zpool", Args: []string{"list", "-v"}},
		"zfs-list.txt":   {Name: "zfs", Args: []string{"list", "-o", "name,used,avail,refer,mountpoint,volsize"}},
		"uname.txt":      {Name: "uname", Args: []string{"-a"}},
	}
	for name, c := range cmds {
		res, err := e.X.Run(ctx, c)
		out := res.Stdout
		if err != nil {
			out = "error: " + err.Error() + "\n" + res.Stderr
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(out), 0o600); err != nil {
			return err
		}
	}
	if len(missing) > 0 {
		return os.WriteFile(filepath.Join(dir, "missing-files.txt"), []byte(strings.Join(missing, "\n")+"\n"), 0o600)
	}
	return nil
}

// freshDir recreates dir (inside the staging directory) with mode 0700.
func freshDir(dir string) error {
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return os.MkdirAll(dir, 0o700)
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

func copyFile(src, dst string) error {
	st, err := os.Stat(src) // follows symlinks, e.g. /etc/os-release
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", src)
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, st.Mode().Perm()&0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// GuestManifestVersion is bumped on incompatible guest manifest changes.
const GuestManifestVersion = 1

// GuestManifestArchive is the blob in every guest snapshot (ct/<id>,
// vm/<id>) that makes it restorable on its own, without the host index.
// PBS stores it as incus-guest.conf.blob.
const GuestManifestArchive = "incus-guest.conf"

// GuestManifest describes one guest snapshot: the instance and the volumes
// stored with it. It is written before the upload; PBS only finalizes a
// snapshot whose upload completed, so its presence means the archives are there.
type GuestManifest struct {
	GuestManifestVersion int       `json:"guest_manifest_version"`
	Tool                 string    `json:"tool"`
	ToolVersion          string    `json:"tool_version"`
	RunID                string    `json:"run_id"`
	Hostname             string    `json:"hostname"`
	HostBackupID         string    `json:"host_backup_id"`
	CreatedAt            time.Time `json:"created_at"`
	Group                string    `json:"group"` // ct/<id> or vm/<id>
	Scheduled            bool      `json:"scheduled,omitempty"`
	// Targets are the instance (first) and the volumes only it uses.
	Targets []Target `json:"targets"`
	// Instance is the instance as the Incus API returned it (secrets
	// redacted); Profiles are the profiles it uses. Used to check a restore
	// target before anything large is downloaded.
	Instance map[string]any   `json:"instance,omitempty"`
	Profiles []map[string]any `json:"profiles,omitempty"`
}

// projectDump is the part of the Incus metadata a guest manifest needs.
type projectDump struct {
	Instances []map[string]any `json:"instances"`
	Profiles  []map[string]any `json:"profiles"`
}

// projectDumps loads Incus metadata once per project and run.
type projectDumps struct {
	e     *Engine
	cache map[string]*projectDump
}

func (p *projectDumps) get(project string) *projectDump {
	if d, ok := p.cache[project]; ok {
		return d
	}
	d := &projectDump{}
	md, err := p.e.Incus.Metadata(project)
	if err != nil {
		p.e.Log.Warn("guest_metadata_incomplete", "event", "guest_metadata_incomplete", "project", project, "error", err)
	}
	if md != nil {
		if b, merr := json.Marshal(md); merr == nil {
			_ = json.Unmarshal(b, d)
		}
	}
	p.cache[project] = d
	return d
}

// writeGuestManifest writes incus-guest.conf for one group into the staging
// directory and returns its path.
func (e *Engine) writeGuestManifest(runID string, start time.Time, sel Selection, g *guestGroup, dumps *projectDumps, cl *cleanupStack) (string, error) {
	m := GuestManifest{
		GuestManifestVersion: GuestManifestVersion, Tool: "incus-pbs", ToolVersion: e.Version, RunID: runID,
		Hostname: e.hostname(), HostBackupID: e.Cfg.PBS.BackupID, CreatedAt: start.UTC(), Group: g.key(), Scheduled: sel.Scheduled,
	}
	for _, r := range g.results {
		m.Targets = append(m.Targets, r.t)
	}
	if len(m.Targets) > 0 && m.Targets[0].Kind != KindVolume {
		t := m.Targets[0]
		d := dumps.get(t.Project)
		for _, in := range d.Instances {
			if fmt.Sprint(in["name"]) == t.Name {
				m.Instance = in
			}
		}
		used := map[string]bool{}
		if ps, ok := m.Instance["profiles"].([]any); ok {
			for _, p := range ps {
				used[fmt.Sprint(p)] = true
			}
		}
		for _, p := range d.Profiles {
			if used[fmt.Sprint(p["name"])] {
				m.Profiles = append(m.Profiles, p)
			}
		}
	}
	dir := filepath.Join(e.Cfg.Storage.StagingDirectory, "guest", g.Type+"_"+g.ID)
	if err := freshDir(dir); err != nil {
		return "", err
	}
	cl.push("remove "+dir, func(context.Context) error { return os.RemoveAll(dir) })
	path := filepath.Join(dir, GuestManifestArchive)
	return path, writeJSON(path, m)
}

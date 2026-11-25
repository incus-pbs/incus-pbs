package restore

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/incus-pbs/incus-pbs/internal/pbc"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/incus-pbs/incus-pbs/internal/backup"
)

// HostResult describes a prepared host recovery kit.
type HostResult struct {
	Snapshot string   `json:"snapshot"`
	Dir      string   `json:"dir"`
	Files    []string `json:"files"`
}

// RestoreHost prepares a recovery kit in dir. It does not change the host:
// it restores the metadata and host-config archives and writes a suggested
// preseed (pools, networks, profiles, projects) plus an ordered RECOVERY.md
// with the restore commands. The old Incus database is never copied.
func (r *Restorer) RestoreHost(ctx context.Context, snapshotRef, dir string) (*HostResult, error) {
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("target directory must be absolute")
	}
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		return nil, fmt.Errorf("target directory %s is not empty", dir)
	}
	snap, err := r.Resolve(ctx, snapshotRef)
	if err != nil {
		return nil, err
	}
	m, err := r.Manifest(ctx, snap)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	res := &HostResult{Snapshot: snap, Dir: dir}
	mdDir := filepath.Join(dir, "incus-metadata")
	if err := r.PBC.Restore(ctx, r.Cfg.PBS.Namespace, snap, "incus-metadata.pxar", mdDir, r.Cfg.PBS.EncryptionKeyFile); err != nil {
		return nil, err
	}
	res.Files = append(res.Files, "incus-metadata/")
	files, err := r.PBC.Files(ctx, r.Cfg.PBS.Namespace, snap)
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		if strings.HasPrefix(f.Filename, "host-config.") {
			if err := r.PBC.Restore(ctx, r.Cfg.PBS.Namespace, snap, "host-config.pxar", filepath.Join(dir, "host-config"), r.Cfg.PBS.EncryptionKeyFile); err != nil {
				return nil, err
			}
			res.Files = append(res.Files, "host-config/")
			break
		}
	}

	preseed, notes, err := buildPreseed(mdDir, m.Projects)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "preseed.yaml"), preseed, 0o600); err != nil {
		return nil, err
	}
	res.Files = append(res.Files, "preseed.yaml")
	guests, err := r.latestGuests(ctx, snapshotRef, snap)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "RECOVERY.md"), []byte(recoveryPlan(m, snap, guests, notes)), 0o600); err != nil {
		return nil, err
	}
	res.Files = append(res.Files, "RECOVERY.md")
	r.Log.Info("host_recovery_kit_written", "event", "host_recovery_kit_written", "snapshot", snap, "dir", dir)
	return res, nil
}

// buildPreseed derives an `incus admin init --preseed` document from the
// API dump. It is a starting point to review, never applied automatically.
func buildPreseed(mdDir string, projects []string) ([]byte, []string, error) {
	var notes []string
	pools := map[string]map[string]any{}
	networks := map[string]map[string]any{}
	var profiles, projs []map[string]any
	for _, p := range projects {
		b, err := os.ReadFile(filepath.Join(mdDir, "incus-project-"+p+".json"))
		if err != nil {
			return nil, nil, fmt.Errorf("metadata for project %s: %w", p, err)
		}
		var md map[string]json.RawMessage
		if err := json.Unmarshal(b, &md); err != nil {
			return nil, nil, err
		}
		var sp, nw, pr, pj []map[string]any
		_ = json.Unmarshal(md["storage_pools"], &sp)
		_ = json.Unmarshal(md["networks"], &nw)
		_ = json.Unmarshal(md["profiles"], &pr)
		_ = json.Unmarshal(md["projects"], &pj)
		for _, x := range sp {
			name := fmt.Sprint(x["name"])
			cfg := cleanConfig(x["config"])
			if src, ok := cfg["source"]; ok {
				notes = append(notes, fmt.Sprintf("storage pool %s: verify `source: %v` matches a disk/pool on the NEW host (use /dev/disk/by-id paths)", name, src))
			}
			pools[name] = map[string]any{"name": name, "driver": x["driver"], "description": x["description"], "config": cfg}
		}
		for _, x := range nw {
			if managed, _ := x["managed"].(bool); !managed {
				continue
			}
			name := fmt.Sprint(x["name"])
			if _, seen := networks[name]; seen {
				// Projects without features.networks list the default
				// project's networks; the first (owning) project wins.
				continue
			}
			owner := p
			if np, ok := x["project"].(string); ok && np != "" {
				owner = np
			}
			networks[name] = map[string]any{"name": name, "type": x["type"], "description": x["description"], "config": cleanConfig(x["config"]), "project": owner}
		}
		for _, x := range pr {
			profiles = append(profiles, map[string]any{"name": x["name"], "description": x["description"],
				"config": cleanConfig(x["config"]), "devices": x["devices"], "project": p})
		}
		if len(projs) == 0 {
			for _, x := range pj {
				if x["name"] != "default" {
					projs = append(projs, map[string]any{"name": x["name"], "description": x["description"], "config": cleanConfig(x["config"])})
				}
			}
		}
	}
	doc := map[string]any{}
	if len(projs) > 0 {
		doc["projects"] = projs
	}
	if len(pools) > 0 {
		doc["storage_pools"] = sortedValues(pools)
	}
	if len(networks) > 0 {
		doc["networks"] = sortedValues(networks)
	}
	if len(profiles) > 0 {
		doc["profiles"] = profiles
	}
	b, err := yaml.Marshal(doc)
	if err != nil {
		return nil, nil, err
	}
	header := "# Suggested Incus preseed reconstructed by incus-pbs from a backup.\n" +
		"# REVIEW BEFORE USE: disks, addresses and redacted values must match the new host.\n" +
		"# Apply on a fresh installation with: incus admin init --preseed < preseed.yaml\n"
	return append([]byte(header), b...), notes, nil
}

func cleanConfig(v any) map[string]any {
	out := map[string]any{}
	if m, ok := v.(map[string]any); ok {
		for k, val := range m {
			if strings.HasPrefix(k, "volatile.") || val == "<redacted>" {
				continue
			}
			out[k] = val
		}
	}
	return out
}

func sortedValues(m map[string]map[string]any) []map[string]any {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		out = append(out, m[k])
	}
	return out
}

// guestBackup is the newest guest snapshot of one instance.
type guestBackup struct {
	Snapshot string
	Time     int64
	Manifest *backup.GuestManifest
}

// latestGuests returns this host's newest guest snapshot per instance: for
// "latest" the newest overall, otherwise the newest at or before the run
// index, so the kit describes one point in time.
func (r *Restorer) latestGuests(ctx context.Context, ref, index string) ([]guestBackup, error) {
	cutoff := int64(math.MaxInt64)
	if ref != "latest" {
		ts := index[strings.LastIndex(index, "/")+1:]
		t, err := time.Parse(time.RFC3339, ts)
		if err != nil {
			return nil, fmt.Errorf("run index time %q: %w", ts, err)
		}
		cutoff = t.Unix()
	}
	out, err := r.ownGuestSnapshots(ctx, cutoff, func(pbc.Snapshot) bool { return true })
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Snapshot < out[j].Snapshot })
	return out, nil
}

func recoveryPlan(m *backup.Manifest, snap string, guests []guestBackup, notes []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Host recovery plan\n\nSnapshot `%s` of host `%s` (created %s, incus-pbs %s, complete: %v).\n\n",
		snap, m.Hostname, m.CreatedAt.Format("2006-01-02 15:04 MST"), m.ToolVersion, m.Complete)
	if !m.Complete {
		b.WriteString("> **Warning:** this snapshot is incomplete. Check `incus-pbs restore inspect` for missing targets.\n\n")
	}
	b.WriteString("Never copy the old Incus database onto the new host. Recreate the environment, then import each instance and volume.\n\n")
	b.WriteString("## 1. Base system\n\n")
	b.WriteString("- Install the OS, ZFS, Incus and proxmox-backup-client.\n")
	b.WriteString("- Compare `host-config/files/etc/subuid`, `subgid`, networking and `host-config/zpool-list.txt` with the new host.\n\n")
	b.WriteString("## 2. Incus environment\n\n```bash\n$EDITOR preseed.yaml           # review disks, addresses, redacted values\nincus admin init --preseed < preseed.yaml\n```\n\n")
	for _, n := range notes {
		fmt.Fprintf(&b, "- %s\n", n)
	}
	b.WriteString("\n## 3. Custom volumes (before instances that use them)\n\n```bash\n")
	ok := map[string]bool{}
	for _, it := range m.Items {
		if it.Status == "ok" || it.Status == "included" {
			ok[backup.TargetKey(it.Kind, it.Project, it.Name)] = true
		}
	}
	// Volumes in the run index (shared and standalone), then volumes stored
	// with the only instance that uses them.
	for _, t := range m.Targets {
		if t.Kind == backup.KindVolume && t.Owner == "" && t.Problem == "" && ok[backup.TargetKey(t.Kind, t.Project, t.Name)] {
			fmt.Fprintf(&b, "incus-pbs restore volume %s %s --project %s --source-pool %s\n", snap, t.Name, t.Project, t.Pool.Name)
		}
	}
	for _, g := range guests {
		for _, t := range g.Manifest.Targets {
			if t.Kind == backup.KindVolume {
				fmt.Fprintf(&b, "incus-pbs restore volume %s %s --project %s --source-pool %s\n", g.Snapshot, t.Name, t.Project, t.Pool.Name)
			}
		}
	}
	b.WriteString("```\n\n## 4. Instances\n\nEach instance comes from its newest backup (instances can have their own schedules).\n\n```bash\n")
	for _, g := range guests {
		for _, t := range g.Manifest.Targets {
			if t.Kind != backup.KindVolume {
				fmt.Fprintf(&b, "incus-pbs restore instance %s %s --project %s\n", g.Snapshot, t.Name, t.Project)
			}
		}
	}
	b.WriteString("```\n\nStart instances one by one and verify them. VM backups are crash-consistent, so expect a filesystem journal replay on first boot.\n\n")
	b.WriteString("## 5. Backups again\n\nInstall the incus-pbs config and the encryption key, run `incus-pbs check`, then enable `incus-pbs.timer`.\n")
	if len(m.Excluded) > 0 || len(m.Warnings) > 0 {
		b.WriteString("\n## Not contained in this backup\n\n")
		for _, x := range m.Excluded {
			fmt.Fprintf(&b, "- %s: %s\n", x.What, x.Reason)
		}
		for _, w := range m.Warnings {
			fmt.Fprintf(&b, "- %s\n", w)
		}
	}
	return b.String()
}

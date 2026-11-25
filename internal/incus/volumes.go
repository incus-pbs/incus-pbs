package incus

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/lxc/incus/v7/shared/api"
)

// Volume is a custom storage volume.
type Volume struct {
	Pool        string            `json:"pool"`
	Project     string            `json:"project"` // effective project owning the volume
	Name        string            `json:"name"`
	ContentType string            `json:"content_type"` // filesystem | block | iso
	UsedBy      []string          `json:"used_by,omitempty"`
	Description string            `json:"description,omitempty"`
	Config      map[string]string `json:"config,omitempty"` // volume config (size, ...) for restore
}

// Key uniquely identifies a volume.
func (v Volume) Key() string { return v.Pool + "/" + v.Project + "/" + v.Name }

// AttachedVolume is a disk device referencing a custom volume.
type AttachedVolume struct {
	Device string
	Pool   string
	Source string
	Path   string
}

// AttachedVolumes returns non-root disk devices backed by a storage pool volume.
func AttachedVolumes(devices map[string]map[string]string) []AttachedVolume {
	var out []AttachedVolume
	for name, d := range devices {
		if d["type"] != "disk" || d["path"] == "/" || d["pool"] == "" || d["source"] == "" {
			continue
		}
		out = append(out, AttachedVolume{Device: name, Pool: d["pool"], Source: d["source"], Path: d["path"]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Device < out[j].Device })
	return out
}

// HostPathDisks returns disk devices that bind-mount host paths (not backed up).
func HostPathDisks(devices map[string]map[string]string) []string {
	var out []string
	for name, d := range devices {
		if d["type"] == "disk" && d["pool"] == "" && strings.HasPrefix(d["source"], "/") {
			out = append(out, name+"="+d["source"])
		}
	}
	sort.Strings(out)
	return out
}

// VolumeDatasets returns the ZFS dataset of a custom volume. Unlike instances,
// custom volume datasets always carry the project prefix (also "default_"),
// and block volumes are plain zvols without a ".block" suffix
// (verified against Incus 7.6 / ZFS 2.3).
func VolumeDatasets(poolDataset string, v Volume) string {
	project := v.Project
	if project == "" {
		project = "default"
	}
	return poolDataset + "/custom/" + project + "_" + v.Name
}

// ListPools lists all storage pools.
func (c *Client) ListPools() ([]Pool, error) {
	pools, err := c.srv.GetStoragePools()
	if err != nil {
		return nil, err
	}
	out := make([]Pool, 0, len(pools))
	for _, p := range pools {
		out = append(out, Pool{Name: p.Name, Driver: p.Driver, Dataset: p.Config["zfs.pool_name"]})
	}
	return out, nil
}

// ListCustomVolumes lists custom volumes visible in a project across all pools.
func (c *Client) ListCustomVolumes(project string) ([]Volume, error) {
	pools, err := c.srv.GetStoragePools()
	if err != nil {
		return nil, err
	}
	var out []Volume
	for _, p := range pools {
		vols, err := c.p(project).GetStoragePoolVolumes(p.Name)
		if err != nil {
			return nil, fmt.Errorf("pool %s: %w", p.Name, err)
		}
		for _, v := range vols {
			if v.Type != "custom" || strings.Contains(v.Name, "/") { // skip snapshots
				continue
			}
			proj := v.Project
			if proj == "" {
				proj = project
			}
			out = append(out, Volume{Pool: p.Name, Project: proj, Name: v.Name, ContentType: v.ContentType,
				UsedBy: v.UsedBy, Description: v.Description, Config: Redact(v.Config)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key() < out[j].Key() })
	return out, nil
}

// ListVolumeSnapshots lists snapshot names of a custom volume.
func (c *Client) ListVolumeSnapshots(v Volume) ([]string, error) {
	snaps, err := c.p(v.Project).GetStoragePoolVolumeSnapshots(v.Pool, "custom", v.Name)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(snaps))
	for _, s := range snaps {
		out = append(out, s.Name[strings.LastIndex(s.Name, "/")+1:])
	}
	return out, nil
}

// CreateVolumeSnapshot snapshots a custom volume (own names only).
func (c *Client) CreateVolumeSnapshot(v Volume, name string, expires time.Time) error {
	if !IsOwnSnapshot(name) {
		return fmt.Errorf("refusing to create snapshot with foreign name %q", name)
	}
	op, err := c.p(v.Project).CreateStoragePoolVolumeSnapshot(v.Pool, "custom", v.Name,
		api.StorageVolumeSnapshotsPost{Name: name, ExpiresAt: &expires})
	if err != nil {
		return err
	}
	return op.Wait()
}

// DeleteVolumeSnapshot deletes a snapshot created by this tool (refuses all others).
func (c *Client) DeleteVolumeSnapshot(v Volume, name string) error {
	if !IsOwnSnapshot(name) {
		return fmt.Errorf("refusing to delete snapshot %q: not created by incus-pbs", name)
	}
	op, err := c.p(v.Project).DeleteStoragePoolVolumeSnapshot(v.Pool, "custom", v.Name, name)
	if err != nil {
		return err
	}
	return op.Wait()
}

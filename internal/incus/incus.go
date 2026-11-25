// Package incus is the Incus provider: discovery, temporary snapshots and
// metadata collection through the official Go client (no CLI parsing).
package incus

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	incusclient "github.com/lxc/incus/v7/client"
	"github.com/lxc/incus/v7/shared/api"
)

// SnapshotPrefix marks snapshots created by this tool. Nothing else is ever deleted.
const SnapshotPrefix = "pbs-backup-"

var ownSnapshotRE = regexp.MustCompile(`^pbs-backup-\d{8}-\d{6}$`)

// NewSnapshotName returns a unique, recognizable temporary snapshot name.
func NewSnapshotName(t time.Time) string {
	return SnapshotPrefix + t.UTC().Format("20060102-150405")
}

// IsOwnSnapshot reports whether name was created by this tool.
func IsOwnSnapshot(name string) bool { return ownSnapshotRE.MatchString(name) }

// Instance is the subset of instance data the backup engine needs.
type Instance struct {
	Name     string
	Project  string
	Type     string // "container" or "virtual-machine"
	Status   string
	RootPool string
	// Devices are the expanded devices (used to find attached volumes).
	Devices map[string]map[string]string
	// Config is the instance's local config (user.* markers).
	Config map[string]string
}

// RestoreTestKey marks throwaway instances created by `restore test`; they
// are never backed up.
const RestoreTestKey = "user.incus-pbs.restore-test"

// ScheduleKey holds an instance's own backup schedule for `backup
// --scheduled`: a cron expression, a descriptor such as @daily, optionally
// prefixed with CRON_TZ=<zone>, or "off". Without it backup.schedule applies.
const ScheduleKey = "user.incus-pbs.schedule"

// IsVM reports whether the instance is a virtual machine.
func (i Instance) IsVM() bool { return i.Type == string(api.InstanceTypeVM) }

// Pool describes a storage pool.
type Pool struct {
	Name    string
	Driver  string
	Dataset string // zfs.pool_name for ZFS pools
}

// Provider is the interface the backup engine uses (mocked in tests).
type Provider interface {
	ListInstances(project string) ([]Instance, error)
	GetPool(name string) (Pool, error)
	ListSnapshots(project, instance string) ([]string, error)
	CreateSnapshot(project, instance, name string, expires time.Time) error
	DeleteSnapshot(project, instance, name string) error
	ServerVersion() (string, error)
	// DaemonVolumes returns "pool/volume" for volumes Incus uses itself
	// (storage.backups_volume, storage.images_volume, storage.logs_volume).
	DaemonVolumes() (map[string]string, error)
	Metadata(project string) (map[string]any, error)

	ListPools() ([]Pool, error)
	ListCustomVolumes(project string) ([]Volume, error)
	ListVolumeSnapshots(v Volume) ([]string, error)
	CreateVolumeSnapshot(v Volume, name string, expires time.Time) error
	DeleteVolumeSnapshot(v Volume, name string) error

	// Exec runs a command inside an instance (hooks).
	Exec(ctx context.Context, project, instance string, cmd []string, env map[string]string) (ExecResult, error)
}

// ExecResult is the outcome of Exec.
type ExecResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

// Client implements Provider with the Incus Go client.
type Client struct{ srv incusclient.InstanceServer }

// Connect opens the local unix socket ("" means the default socket).
func Connect(socket string) (*Client, error) {
	srv, err := incusclient.ConnectIncusUnix(socket, nil)
	if err != nil {
		return nil, fmt.Errorf("connecting to incus: %w", err)
	}
	return &Client{srv: srv}, nil
}

func (c *Client) p(project string) incusclient.InstanceServer { return c.srv.UseProject(project) }

// ListInstances lists containers and VMs in a project, sorted by name.
func (c *Client) ListInstances(project string) ([]Instance, error) {
	list, err := c.p(project).GetInstances(api.InstanceTypeAny)
	if err != nil {
		return nil, err
	}
	out := make([]Instance, 0, len(list))
	for _, i := range list {
		out = append(out, Instance{
			Name: i.Name, Project: project, Type: i.Type, Status: i.Status,
			RootPool: RootPool(i.ExpandedDevices), Devices: i.ExpandedDevices, Config: i.Config,
		})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out, nil
}

// RootPool finds the storage pool of the root disk device.
func RootPool(devices map[string]map[string]string) string {
	for _, d := range devices {
		if d["type"] == "disk" && d["path"] == "/" {
			return d["pool"]
		}
	}
	return ""
}

// GetPool returns pool driver and backing dataset.
func (c *Client) GetPool(name string) (Pool, error) {
	pool, _, err := c.srv.GetStoragePool(name)
	if err != nil {
		return Pool{}, err
	}
	return Pool{Name: pool.Name, Driver: pool.Driver, Dataset: pool.Config["zfs.pool_name"]}, nil
}

// ListSnapshots lists snapshot names of an instance.
func (c *Client) ListSnapshots(project, instance string) ([]string, error) {
	snaps, err := c.p(project).GetInstanceSnapshots(instance)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(snaps))
	for _, s := range snaps {
		// Name may be "instance/snap" depending on API version.
		out = append(out, s.Name[strings.LastIndex(s.Name, "/")+1:])
	}
	return out, nil
}

// CreateSnapshot creates a stateless snapshot with an expiry as safety net.
func (c *Client) CreateSnapshot(project, instance, name string, expires time.Time) error {
	if !IsOwnSnapshot(name) {
		return fmt.Errorf("refusing to create snapshot with foreign name %q", name)
	}
	op, err := c.p(project).CreateInstanceSnapshot(instance, api.InstanceSnapshotsPost{
		Name: name, Stateful: false, ExpiresAt: &expires,
	})
	if err != nil {
		return err
	}
	return op.Wait()
}

// DeleteSnapshot deletes a snapshot created by this tool (refuses all others).
func (c *Client) DeleteSnapshot(project, instance, name string) error {
	if !IsOwnSnapshot(name) {
		return fmt.Errorf("refusing to delete snapshot %q: not created by incus-pbs", name)
	}
	op, err := c.p(project).DeleteInstanceSnapshot(instance, name)
	if err != nil {
		return err
	}
	return op.Wait()
}

// ServerVersion returns the Incus server version.
// DaemonVolumes maps "pool/volume" to the server config key that uses it.
// Incus refuses snapshots of these volumes; they hold caches and spools.
func (c *Client) DaemonVolumes() (map[string]string, error) {
	s, _, err := c.srv.GetServer()
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, k := range []string{"storage.backups_volume", "storage.images_volume", "storage.logs_volume"} {
		if v := s.Config[k]; v != "" {
			out[v] = k
		}
	}
	return out, nil
}

func (c *Client) ServerVersion() (string, error) {
	s, _, err := c.srv.GetServer()
	if err != nil {
		return "", err
	}
	return s.Environment.ServerVersion, nil
}

// Metadata collects the environment description stored in incus-metadata.pxar.
func (c *Client) Metadata(project string) (map[string]any, error) {
	ps := c.p(project)
	md := map[string]any{}
	srv, _, err := c.srv.GetServer()
	if err != nil {
		return nil, err
	}
	md["server_environment"] = srv.Environment
	md["server_config"] = Redact(srv.Config)
	var errs []string
	collect := func(key string, f func() (any, error)) {
		v, err := f()
		if err != nil {
			errs = append(errs, key+": "+err.Error())
			return
		}
		md[key] = v
	}
	collect("projects", func() (any, error) { return c.srv.GetProjects() })
	collect("profiles", func() (any, error) { return ps.GetProfiles() })
	collect("networks", func() (any, error) { return ps.GetNetworks() })
	collect("storage_pools", func() (any, error) { return c.srv.GetStoragePools() })
	collect("instances", func() (any, error) {
		list, err := ps.GetInstances(api.InstanceTypeAny)
		for i := range list {
			list[i].Config = Redact(list[i].Config)
			list[i].ExpandedConfig = Redact(list[i].ExpandedConfig)
		}
		return list, err
	})
	collect("storage_volumes", func() (any, error) {
		pools, err := c.srv.GetStoragePools()
		if err != nil {
			return nil, err
		}
		vols := map[string]any{}
		for _, p := range pools {
			v, err := ps.GetStoragePoolVolumes(p.Name)
			if err != nil {
				return nil, err
			}
			vols[p.Name] = v
		}
		return vols, nil
	})
	if len(errs) > 0 {
		return md, fmt.Errorf("metadata incomplete: %s", strings.Join(errs, "; "))
	}
	return md, nil
}

var secretKeyRE = regexp.MustCompile(`(?i)(secret|password|token|trust|key\b|\.key$|cert)`)

// Redact drops config values whose keys look like secrets.
func Redact(cfg map[string]string) map[string]string {
	out := make(map[string]string, len(cfg))
	for k, v := range cfg {
		if secretKeyRE.MatchString(k) {
			out[k] = "<redacted>"
			continue
		}
		out[k] = v
	}
	return out
}

// Datasets returns the ZFS datasets Incus uses for an instance:
// main filesystem dataset and, for VMs, the root block zvol.
// Non-default projects prefix the name with "<project>_".
func Datasets(poolDataset string, inst Instance) (main, block string) {
	name := inst.Name
	if inst.Project != "" && inst.Project != "default" {
		name = inst.Project + "_" + inst.Name
	}
	if inst.IsVM() {
		main = poolDataset + "/virtual-machines/" + name
		return main, main + ".block"
	}
	return poolDataset + "/containers/" + name, ""
}

// SnapshotSuffix is how Incus names the ZFS snapshot for an instance snapshot.
func SnapshotSuffix(snap string) string { return "@snapshot-" + snap }

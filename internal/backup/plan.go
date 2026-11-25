// Package backup is the backup engine: discovery, temporary snapshots,
// read-only exposure, one atomic PBS snapshot per run and cleanup.
package backup

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/incus-pbs/incus-pbs/internal/config"
	"github.com/incus-pbs/incus-pbs/internal/incus"
	"github.com/incus-pbs/incus-pbs/internal/schedule"
)

// Kind of a backup target.
type Kind string

const (
	KindContainer Kind = "container"
	KindVM        Kind = "virtual-machine"
	KindVolume    Kind = "custom-volume"
)

// Target is one thing that gets backed up (instance or custom volume).
type Target struct {
	Kind        Kind       `json:"kind"`
	Project     string     `json:"project"`
	Name        string     `json:"name"`
	Status      string     `json:"status,omitempty"`
	Pool        incus.Pool `json:"pool"`
	ContentType string     `json:"content_type,omitempty"`
	ArchiveBase string     `json:"archive_base"`
	UsedBy      []string   `json:"used_by,omitempty"`
	Problem     string     `json:"problem,omitempty"` // non-empty: cannot be backed up
	// Schedule is the cron expression `backup --scheduled` uses for this target.
	Schedule string `json:"schedule,omitempty"`
	// ScheduleSource: instance (user.incus-pbs.schedule), config
	// (backup.schedule), invalid (unparsable key, backup.schedule applies) or
	// owner (a volume attached to exactly one instance follows that instance).
	ScheduleSource string `json:"schedule_source,omitempty"`
	// Owner is "project/name" of the only instance using a volume; such a
	// volume is stored in that instance's PBS group.
	Owner    string          `json:"owner,omitempty"`
	Instance *incus.Instance `json:"-"`
	Volume   *incus.Volume   `json:"volume,omitempty"`
}

// ID is a human readable identifier.
func (t Target) ID() string {
	if t.Kind == KindVolume {
		return fmt.Sprintf("volume %s/%s/%s", t.Pool.Name, t.Project, t.Name)
	}
	return fmt.Sprintf("%s %s/%s", t.Kind, t.Project, t.Name)
}

// Archives returns the PBS archive names this target produces.
func (t Target) Archives() []string {
	switch {
	case t.Kind == KindContainer:
		return []string{t.ArchiveBase + ".pxar"}
	case t.Kind == KindVM:
		return []string{t.ArchiveBase + ".img", t.ArchiveBase + "-config.pxar"}
	case t.ContentType == "block":
		return []string{t.ArchiveBase + ".img"}
	default:
		return []string{t.ArchiveBase + ".pxar"}
	}
}

// Excluded is something deliberately not backed up.
type Excluded struct {
	What   string `json:"what"`
	Reason string `json:"reason"`
}

// Plan is the result of discovery.
type Plan struct {
	Targets  []Target   `json:"targets"`
	Excluded []Excluded `json:"excluded,omitempty"`
	Warnings []string   `json:"warnings,omitempty"`
}

// Problems returns targets that cannot be backed up.
func (p *Plan) Problems() []Target {
	var out []Target
	for _, t := range p.Targets {
		if t.Problem != "" {
			out = append(out, t)
		}
	}
	return out
}

// Selection narrows a run.
type Selection struct {
	Projects  []string `json:"projects,omitempty"`  // empty: config projects
	Instances []string `json:"instances,omitempty"` // empty: all instances
	// Scheduled backs up only targets whose schedule is due (`backup --scheduled`).
	Scheduled bool `json:"scheduled,omitempty"`
}

var archiveSafeRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// Discover builds the plan for the selection.
func Discover(p incus.Provider, cfg config.Config, sel Selection) (*Plan, error) {
	projects := sel.Projects
	if len(projects) == 0 {
		projects = cfg.Incus.Projects
	}
	poolList, err := p.ListPools()
	if err != nil {
		return nil, fmt.Errorf("listing storage pools: %w", err)
	}
	pools := map[string]incus.Pool{}
	for _, pl := range poolList {
		pools[pl.Name] = pl
	}
	want := map[string]bool{}
	for _, n := range sel.Instances {
		want[n] = true
	}
	found := map[string]bool{}
	plan := &Plan{}
	daemonVols, err := p.DaemonVolumes()
	if err != nil {
		return nil, fmt.Errorf("reading server config: %w", err)
	}
	volumeTargets := map[string]bool{} // volume key -> already planned

	for _, proj := range projects {
		insts, err := p.ListInstances(proj)
		if err != nil {
			return nil, fmt.Errorf("project %s: listing instances: %w", proj, err)
		}
		var vols []incus.Volume
		if cfg.Backup.IncludeCustomVolumes {
			if vols, err = p.ListCustomVolumes(proj); err != nil {
				return nil, fmt.Errorf("project %s: listing custom volumes: %w", proj, err)
			}
		}
		volByRef := map[string]incus.Volume{}
		for _, v := range vols {
			volByRef[v.Pool+"/"+v.Name] = v
		}

		selectedVolumes := map[string]incus.Volume{}
		for i := range insts {
			in := insts[i]
			if len(want) > 0 && !want[in.Name] {
				continue
			}
			found[in.Name] = true
			if cfg.Excluded(proj, in.Name) {
				plan.Excluded = append(plan.Excluded, Excluded{What: fmt.Sprintf("instance %s/%s", proj, in.Name), Reason: "excluded by backup.exclude"})
				continue
			}
			if in.Config[incus.RestoreTestKey] != "" {
				plan.Excluded = append(plan.Excluded, Excluded{What: fmt.Sprintf("instance %s/%s", proj, in.Name), Reason: "restore-test instance (" + incus.RestoreTestKey + ")"})
				continue
			}
			t := Target{Project: proj, Name: in.Name, Status: in.Status, Instance: &insts[i], ArchiveBase: instanceArchiveBase(in)}
			t.Kind = KindContainer
			if in.IsVM() {
				t.Kind = KindVM
			}
			t.Pool, t.Problem = resolvePool(pools, in.RootPool)
			t.Schedule, t.ScheduleSource = cfg.Backup.Schedule, "config"
			if v := strings.TrimSpace(in.Config[incus.ScheduleKey]); v != "" {
				if _, err := schedule.Parse(v); err != nil {
					t.ScheduleSource = "invalid"
					plan.Warnings = append(plan.Warnings, fmt.Sprintf("instance %s/%s: %s: %v; using backup.schedule %q", proj, in.Name, incus.ScheduleKey, err, cfg.Backup.Schedule))
				} else {
					t.Schedule, t.ScheduleSource = v, "instance"
				}
			}
			plan.Targets = append(plan.Targets, t)

			for _, hp := range incus.HostPathDisks(in.Devices) {
				plan.Warnings = append(plan.Warnings, fmt.Sprintf("instance %s/%s: host path disk %s is not backed up", proj, in.Name, hp))
			}
			for _, av := range incus.AttachedVolumes(in.Devices) {
				what := fmt.Sprintf("volume %s/%s/%s (attached to %s/%s as %s)", av.Pool, proj, av.Source, proj, in.Name, av.Device)
				if !cfg.Backup.IncludeCustomVolumes {
					plan.Excluded = append(plan.Excluded, Excluded{What: what, Reason: "backup.include_custom_volumes is false"})
					continue
				}
				v, ok := volByRef[av.Pool+"/"+av.Source]
				if !ok {
					plan.Targets = append(plan.Targets, Target{Kind: KindVolume, Project: proj, Name: av.Source,
						Pool: pools[av.Pool], ArchiveBase: "vol_" + proj + "_" + av.Pool + "_" + av.Source,
						Problem: "attached volume not found in project " + proj + " (cross-project volumes are not supported)"})
					continue
				}
				selectedVolumes[v.Key()] = v
			}
		}
		// Without an instance filter, standalone volumes are included as well.
		if len(want) == 0 {
			for _, v := range vols {
				selectedVolumes[v.Key()] = v
			}
		}
		keys := make([]string, 0, len(selectedVolumes))
		for k := range selectedVolumes {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if volumeTargets[k] {
				continue
			}
			volumeTargets[k] = true
			v := selectedVolumes[k]
			if v.ContentType == "iso" {
				plan.Excluded = append(plan.Excluded, Excluded{What: "volume " + k, Reason: "ISO volumes are read-only installer media"})
				continue
			}
			if key, ok := daemonVols[v.Pool+"/"+v.Name]; ok && (v.Project == "" || v.Project == "default") {
				plan.Excluded = append(plan.Excluded, Excluded{What: "volume " + k, Reason: "used by Incus itself (" + key + "); cache/spool, cannot be snapshotted"})
				continue
			}
			vc := v
			t := Target{Kind: KindVolume, Project: v.Project, Name: v.Name, Volume: &vc, ContentType: v.ContentType,
				UsedBy: v.UsedBy, ArchiveBase: "vol_" + v.Project + "_" + v.Pool + "_" + v.Name}
			t.Pool, t.Problem = resolvePool(pools, v.Pool)
			if t.Problem == "" && v.ContentType != "filesystem" && v.ContentType != "block" {
				t.Problem = "unsupported content type " + v.ContentType
			}
			plan.Targets = append(plan.Targets, t)
		}
	}
	for n := range want {
		if !found[n] {
			return nil, fmt.Errorf("instance %q not found in projects %v", n, projects)
		}
	}
	if err := checkArchiveNames(plan); err != nil {
		return nil, err
	}
	assignVolumeSchedules(plan, cfg.Backup.Schedule)
	return plan, nil
}

// assignVolumeSchedules lets a volume used by exactly one planned instance
// follow that instance (it is stored in the instance's PBS group); all
// other volumes follow backup.schedule together with the host index.
func assignVolumeSchedules(plan *Plan, def string) {
	inst := map[string]Target{}
	for _, t := range plan.Targets {
		if t.Kind != KindVolume {
			inst[t.Project+"/"+t.Name] = t
		}
	}
	for i := range plan.Targets {
		t := &plan.Targets[i]
		if t.Kind != KindVolume {
			continue
		}
		t.Schedule, t.ScheduleSource = def, "config"
		if owner, ok := soleOwner(*t); ok {
			if o, planned := inst[owner]; planned {
				t.Owner, t.Schedule, t.ScheduleSource = owner, o.Schedule, "owner"
			}
		}
	}
}

func instanceArchiveBase(in incus.Instance) string {
	kind := "ct"
	if in.IsVM() {
		kind = "vm"
	}
	return kind + "_" + in.Project + "_" + in.Name
}

func resolvePool(pools map[string]incus.Pool, name string) (incus.Pool, string) {
	pl, ok := pools[name]
	switch {
	case name == "":
		return pl, "no root disk storage pool"
	case !ok:
		return incus.Pool{Name: name}, "storage pool " + name + " not found"
	case pl.Driver != "zfs":
		return pl, "storage driver " + pl.Driver + " is not supported yet (only zfs)"
	case pl.Dataset == "":
		return pl, "pool " + name + " has no zfs.pool_name"
	}
	return pl, ""
}

func checkArchiveNames(plan *Plan) error {
	seen := map[string]string{}
	for i, t := range plan.Targets {
		if !archiveSafeRE.MatchString(t.ArchiveBase) {
			if plan.Targets[i].Problem == "" {
				plan.Targets[i].Problem = "name cannot be used as a PBS archive name"
			}
			continue
		}
		for _, a := range t.Archives() {
			if other, dup := seen[a]; dup {
				return fmt.Errorf("archive name collision %q between %s and %s", a, other, t.ID())
			}
			seen[a] = t.ID()
		}
	}
	for _, reserved := range []string{"incus-metadata.pxar", "host-config.pxar"} {
		if other, dup := seen[reserved]; dup {
			return fmt.Errorf("%s collides with reserved archive %s", other, reserved)
		}
	}
	return nil
}

// Summary returns a one-line description of the plan.
func (p *Plan) Summary() string {
	var ct, vm, vol int
	for _, t := range p.Targets {
		switch t.Kind {
		case KindContainer:
			ct++
		case KindVM:
			vm++
		case KindVolume:
			vol++
		}
	}
	return strings.TrimSpace(fmt.Sprintf("%d containers, %d VMs, %d custom volumes, %d excluded, %d with problems",
		ct, vm, vol, len(p.Excluded), len(p.Problems())))
}

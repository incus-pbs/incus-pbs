package restore

import (
	"context"
	"slices"
	"sort"
	"strings"

	"github.com/incus-pbs/incus-pbs/internal/backup"
	"github.com/incus-pbs/incus-pbs/internal/pbc"
)

// GuestGroup is one instance's PBS group (ct/<id> or vm/<id>) and its
// snapshots, newest first. Each snapshot restores on its own (incus-guest.conf).
type GuestGroup struct {
	Group     string         `json:"group"`
	Snapshots []pbc.Snapshot `json:"snapshots"`
}

// Listing is every backup in the namespace: this host's run indexes
// (host/<backup_id>: metadata, host config, shared and standalone volumes)
// and the guest groups. With per-instance schedules guest snapshots are
// independent of the run indexes.
type Listing struct {
	HostIndexes []pbc.Snapshot `json:"host_indexes"` // newest first
	Guests      []GuestGroup   `json:"guests"`       // sorted by group
	// InProgress are snapshots still being written by a running backup;
	// they cannot be restored yet.
	InProgress []pbc.Snapshot `json:"in_progress,omitempty"`
}

// List reads the namespace's snapshots.
func (r *Restorer) List(ctx context.Context) (*Listing, error) {
	all, err := r.PBC.ListSnapshots(ctx, r.Cfg.PBS.Namespace)
	if err != nil {
		return nil, err
	}
	l := &Listing{}
	groups := map[string]*GuestGroup{}
	for _, s := range all {
		switch {
		case !s.Finished() && (s.BackupType == "ct" || s.BackupType == "vm" || s.BackupID == r.Cfg.PBS.BackupID):
			l.InProgress = append(l.InProgress, s)
		case s.BackupType == "host" && s.BackupID == r.Cfg.PBS.BackupID:
			l.HostIndexes = append(l.HostIndexes, s)
		case s.BackupType == "ct" || s.BackupType == "vm":
			key := s.BackupType + "/" + s.BackupID
			if groups[key] == nil {
				groups[key] = &GuestGroup{Group: key}
			}
			groups[key].Snapshots = append(groups[key].Snapshots, s)
		}
	}
	newestFirst(l.HostIndexes)
	newestFirst(l.InProgress)
	for _, g := range groups {
		newestFirst(g.Snapshots)
		l.Guests = append(l.Guests, *g)
	}
	sort.Slice(l.Guests, func(i, j int) bool { return l.Guests[i].Group < l.Guests[j].Group })
	return l, nil
}

func newestFirst(s []pbc.Snapshot) {
	sort.Slice(s, func(i, j int) bool { return s[i].BackupTime > s[j].BackupTime })
}

// InstanceSnapshots lists the backups of one instance, newest first.
func (l *Listing) InstanceSnapshots(project, name string) []pbc.Snapshot {
	ids := []string{backup.GroupID(project, name), backup.LegacyGroupID(project, name)}
	var out []pbc.Snapshot
	for _, g := range l.Guests {
		_, id, _ := strings.Cut(g.Group, "/")
		if IsGuestPath(g.Group) && slices.Contains(ids, id) {
			out = append(out, g.Snapshots...)
		}
	}
	newestFirst(out)
	return out
}

// IsGuestPath reports whether p names a guest snapshot or group (ct/…, vm/…).
func IsGuestPath(p string) bool {
	return strings.HasPrefix(p, "ct/") || strings.HasPrefix(p, "vm/")
}

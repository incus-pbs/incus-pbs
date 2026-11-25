package backup

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/incus-pbs/incus-pbs/internal/execx"
	"github.com/incus-pbs/incus-pbs/internal/incus"
	"github.com/incus-pbs/incus-pbs/internal/zfs"
)

// CleanupReport lists what cleanup found and (unless dry-run) removed.
type CleanupReport struct {
	Unmounted         []string `json:"unmounted"`
	DestroyedClones   []string `json:"destroyed_clones"`
	DeletedSnapshots  []string `json:"deleted_snapshots"`
	ReleasedBookmarks []string `json:"released_bookmarks,omitempty"`
	Errors            []string `json:"errors,omitempty"`
}

// Cleanup removes leftovers of interrupted runs, in a safe order:
// mounts under the staging mount root, clones under the staging datasets,
// then Incus snapshots named pbs-backup-YYYYMMDD-HHMMSS. Nothing else is touched.
func (e *Engine) Cleanup(ctx context.Context, sel Selection, dryRun bool) (*CleanupReport, error) {
	rep := &CleanupReport{}
	fail := func(what string, err error) { rep.Errors = append(rep.Errors, what+": "+err.Error()) }
	projects := sel.Projects
	if len(projects) == 0 {
		projects = e.Cfg.Incus.Projects
	}

	// 1. Mounts, deepest first.
	if mi, err := e.mountInfo(); err != nil {
		fail("reading mountinfo", err)
	} else {
		z := e.ZFS(incus.Pool{})
		mounts := z.MountsUnder(mi)
		sort.Sort(sort.Reverse(sort.StringSlice(mounts)))
		for _, m := range mounts {
			if !dryRun {
				if err := z.Unmount(ctx, m); err != nil {
					fail("unmount "+m, err)
					continue
				}
			}
			rep.Unmounted = append(rep.Unmounted, m)
		}
	}

	// 2. Clones (a clone pins its origin snapshot, so it must go first).
	pools, err := e.Incus.ListPools()
	if err != nil {
		return rep, fmt.Errorf("listing pools: %w", err)
	}
	for _, p := range pools {
		if p.Driver != "zfs" || p.Dataset == "" {
			continue
		}
		z := e.ZFS(p)
		clones, err := z.ListStaging(ctx)
		if err != nil {
			continue // staging dataset does not exist on this pool
		}
		for _, c := range clones {
			if !dryRun {
				if err := z.Destroy(ctx, c); err != nil {
					fail("destroy "+c, err)
					continue
				}
			}
			rep.DestroyedClones = append(rep.DestroyedClones, c)
		}
	}

	// 3. Own snapshots on instances and custom volumes.
	for _, proj := range projects {
		insts, err := e.Incus.ListInstances(proj)
		if err != nil {
			fail("list instances "+proj, err)
			continue
		}
		for _, in := range insts {
			snaps, err := e.Incus.ListSnapshots(proj, in.Name)
			if err != nil {
				fail("list snapshots "+in.Name, err)
				continue
			}
			for _, s := range snaps {
				if !incus.IsOwnSnapshot(s) {
					continue
				}
				if !dryRun {
					if err := e.Incus.DeleteSnapshot(proj, in.Name, s); err != nil {
						fail("delete snapshot "+proj+"/"+in.Name+"/"+s, err)
						continue
					}
				}
				rep.DeletedSnapshots = append(rep.DeletedSnapshots, "instance "+proj+"/"+in.Name+"/"+s)
			}
		}
		vols, err := e.Incus.ListCustomVolumes(proj)
		if err != nil {
			fail("list volumes "+proj, err)
			continue
		}
		for _, v := range vols {
			snaps, err := e.Incus.ListVolumeSnapshots(v)
			if err != nil {
				fail("list snapshots "+v.Key(), err)
				continue
			}
			for _, s := range snaps {
				if !incus.IsOwnSnapshot(s) {
					continue
				}
				if !dryRun {
					if err := e.Incus.DeleteVolumeSnapshot(v, s); err != nil {
						fail("delete snapshot "+v.Key()+"/"+s, err)
						continue
					}
				}
				rep.DeletedSnapshots = append(rep.DeletedSnapshots, "volume "+v.Key()+"/"+s)
			}
		}
	}
	rep.DeletedSnapshots = dedupe(rep.DeletedSnapshots)
	if len(rep.Errors) > 0 {
		return rep, fmt.Errorf("cleanup incomplete: %s", strings.Join(rep.Errors, "; "))
	}
	return rep, nil
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// ReleaseBookmarks destroys all incus-pbs change-tracking bookmarks on all ZFS
// pools and resets the block chain, so `incus snapshot restore` of older
// snapshots works again. The next backup reads block devices in full.
func (e *Engine) ReleaseBookmarks(ctx context.Context, dryRun bool) ([]string, error) {
	pools, err := e.Incus.ListPools()
	if err != nil {
		return nil, err
	}
	var released, errs []string
	for _, p := range pools {
		if p.Driver != "zfs" || p.Dataset == "" {
			continue
		}
		z := e.ZFS(p)
		res, err := e.X.Run(ctx, execx.Cmd{Name: "zfs", Args: []string{"list", "-H", "-t", "bookmark", "-o", "name", "-r", p.Dataset}})
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		for _, bm := range strings.Split(strings.TrimSpace(res.Stdout), "\n") {
			if !zfs.IsOwnBookmark(bm) {
				continue
			}
			if !dryRun {
				if err := z.DestroyBookmark(ctx, bm); err != nil {
					errs = append(errs, err.Error())
					continue
				}
			}
			released = append(released, bm)
		}
	}
	if !dryRun {
		_ = os.Remove(filepath.Join(e.Cfg.Storage.StateDirectory, blockStateFile))
	}
	if len(errs) > 0 {
		return released, fmt.Errorf("releasing bookmarks: %s", strings.Join(errs, "; "))
	}
	return released, nil
}

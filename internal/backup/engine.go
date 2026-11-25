package backup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/incus-pbs/incus-pbs/internal/config"
	"github.com/incus-pbs/incus-pbs/internal/execx"
	"github.com/incus-pbs/incus-pbs/internal/incus"
	"github.com/incus-pbs/incus-pbs/internal/pbc"
	"github.com/incus-pbs/incus-pbs/internal/pbsqemu"
	"github.com/incus-pbs/incus-pbs/internal/zfs"
)

// Exit codes of a backup run.
const (
	ExitOK      = 0
	ExitFailed  = 1 // no PBS snapshot was created
	ExitPartial = 2 // a PBS snapshot was created but some targets are missing (see manifest)
)

// Engine runs backups.
type Engine struct {
	Cfg     config.Config
	Incus   incus.Provider
	PBC     *pbc.Client
	X       execx.Runner
	Log     *slog.Logger
	Version string

	// Block is the changed-block backend (nil = libproxmox-backup-qemu).
	Block BlockBackend
	// Diff produces ZFS change streams (nil = zfs send | zstream dump).
	Diff DiffFunc

	// Test hooks.
	Now       func() time.Time
	MountInfo func() (string, error)
	DevRoot   string
	Hostname  func() (string, error)
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Engine) mountInfo() (string, error) {
	if e.MountInfo != nil {
		return e.MountInfo()
	}
	b, err := os.ReadFile("/proc/self/mountinfo")
	return string(b), err
}

func (e *Engine) hostname() string {
	f := e.Hostname
	if f == nil {
		f = os.Hostname
	}
	h, _ := f()
	return h
}

func (e *Engine) mountRoot() string { return filepath.Join(e.Cfg.Storage.StagingDirectory, "mnt") }

// ZFS returns the ZFS helper for a pool.
func (e *Engine) ZFS(pool incus.Pool) *zfs.ZFS {
	ds := ""
	if pool.Dataset != "" {
		ds = pool.Dataset + "/" + e.Cfg.Storage.StagingDatasetName
	}
	return &zfs.ZFS{X: e.X, StagingDataset: ds, MountRoot: e.mountRoot(), DevRoot: e.DevRoot}
}

// ItemResult is the outcome for one target.
type ItemResult struct {
	Kind     Kind     `json:"kind"`
	Project  string   `json:"project"`
	Name     string   `json:"name"`
	Pool     string   `json:"pool"`
	Status   string   `json:"status"` // included (in the PBS snapshot if it exists) | ok (upload confirmed) | failed
	Archives []string `json:"archives,omitempty"`
	Datasets []string `json:"datasets,omitempty"`
	Error    string   `json:"error,omitempty"`
	Seconds  float64  `json:"prepare_seconds"`
	// Block describes .img archives written through changed-block tracking.
	Block []BlockStats `json:"block,omitempty"`
	// Snapshot is the PBS snapshot holding this target (per-guest layout).
	Snapshot string `json:"snapshot,omitempty"`
	// Schedule the target had in this run.
	Schedule string `json:"schedule,omitempty"`
}

// Report summarizes a run.
type Report struct {
	RunID           string             `json:"run_id"`
	Start           time.Time          `json:"start"`
	DurationSeconds float64            `json:"duration_seconds"`
	PBSSnapshot     string             `json:"pbs_snapshot,omitempty"`
	Complete        bool               `json:"complete"`
	ExitCode        int                `json:"exit_code"`
	Error           string             `json:"error,omitempty"`
	InstancesOK     int                `json:"instances_ok"`
	InstancesFailed int                `json:"instances_failed"`
	VolumesOK       int                `json:"volumes_ok"`
	VolumesFailed   int                `json:"volumes_failed"`
	LogicalBytes    int64              `json:"logical_bytes"`
	UploadedBytes   int64              `json:"uploaded_bytes"`
	UploadSeconds   float64            `json:"upload_seconds"`
	Items           []ItemResult       `json:"items"`
	Excluded        []Excluded         `json:"excluded,omitempty"`
	Warnings        []string           `json:"warnings,omitempty"`
	CleanupErrors   []string           `json:"cleanup_errors,omitempty"`
	ArchiveStats    []pbc.ArchiveStats `json:"archive_stats,omitempty"`
	Hooks           []HookResult       `json:"hooks,omitempty"`
	BlockStats      []BlockStats       `json:"block_stats,omitempty"`
	// ArchiveSnapshots maps every archive to the PBS snapshot holding it (per-guest layout).
	ArchiveSnapshots map[string]string `json:"archive_snapshots,omitempty"`
	CrashRecovery    *CleanupReport    `json:"crash_recovery,omitempty"`
	Versions         map[string]string `json:"versions"`
	// Scheduled: started by `backup --scheduled`. NothingDue: such a run
	// found nothing due and did nothing (it is not recorded as a run).
	Scheduled  bool `json:"scheduled,omitempty"`
	NothingDue bool `json:"nothing_due,omitempty"`
	// Deferred counts targets a scheduled run left for a later slot.
	Deferred int `json:"deferred,omitempty"`
	// HostContent: the run covered the host index content (host config,
	// shared and standalone volumes) and not just some instances.
	HostContent bool `json:"host_content,omitempty"`
	// Known lists every target this host currently backs up (state keys),
	// set when the run saw the complete plan; the state forgets the rest.
	Known []string `json:"-"`
}

type cleanupStack struct {
	mu    sync.Mutex
	steps []func(context.Context) error
	names []string
}

func (c *cleanupStack) push(name string, f func(context.Context) error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.steps = append(c.steps, f)
	c.names = append(c.names, name)
}

func (c *cleanupStack) run(ctx context.Context, log *slog.Logger) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var errs []string
	for i := len(c.steps) - 1; i >= 0; i-- {
		if err := c.steps[i](ctx); err != nil {
			log.Error("cleanup_step_failed", "event", "cleanup_step_failed", "step", c.names[i], "error", err)
			errs = append(errs, c.names[i]+": "+err.Error())
		} else {
			log.Debug("cleanup_step_done", "event", "cleanup_step_done", "step", c.names[i])
		}
	}
	c.steps, c.names = nil, nil
	return errs
}

// prepared is a target that has been snapshotted and exposed.
type prepared struct {
	t        Target
	archives []pbc.Archive
	datasets []string
	err      error
	seconds  float64
}

// Run performs one complete backup. The report is always non-nil.
func (e *Engine) Run(ctx context.Context, sel Selection) (rep *Report, err error) {
	start := e.now()
	runID := incus.NewSnapshotName(start)
	log := e.Log.With("run_id", runID)
	rep = &Report{RunID: runID, Start: start.UTC(), Versions: map[string]string{"incus-pbs": e.Version}, ExitCode: ExitFailed}
	var cleanup cleanupStack
	instanceStatus := map[string]string{}

	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		rep.CleanupErrors = cleanup.run(cctx, log)
		if rep.NothingDue {
			log.Debug("nothing_due", "event", "nothing_due", "deferred", rep.Deferred)
			return
		}
		if len(rep.CleanupErrors) > 0 {
			log.Error("cleanup_incomplete", "event", "cleanup_incomplete", "errors", rep.CleanupErrors,
				"hint", "run `incus-pbs cleanup` after fixing the cause")
			if rep.ExitCode == ExitOK {
				rep.ExitCode = ExitPartial
			}
		}
		rep.DurationSeconds = time.Since(start).Seconds()
		if err != nil {
			rep.Error = err.Error()
		}
		for _, it := range rep.Items {
			ok := it.Status == "ok"
			switch {
			case it.Kind == KindVolume && ok:
				rep.VolumesOK++
			case it.Kind == KindVolume:
				rep.VolumesFailed++
			case ok:
				rep.InstancesOK++
			default:
				rep.InstancesFailed++
			}
		}
		if len(e.Cfg.Hooks.PostBackup) > 0 {
			status := map[int]string{ExitOK: "success", ExitPartial: "partial"}[rep.ExitCode]
			if status == "" {
				status = "failed"
			}
			env := map[string]string{
				"INCUS_PBS_PHASE": "post_backup", "INCUS_PBS_RUN_ID": runID, "INCUS_PBS_STATUS": status,
				"INCUS_PBS_EXIT_CODE": fmt.Sprint(rep.ExitCode), "INCUS_PBS_SNAPSHOT": rep.PBSSnapshot,
			}
			for k, v := range runEnv(sel, instanceStatus) {
				env[k] = v
			}
			_ = e.runHooks(context.Background(), "post_backup", e.Cfg.Hooks.PostBackup, env, instanceStatus, rep, log)
		}
		level := slog.LevelInfo
		if rep.ExitCode != ExitOK {
			level = slog.LevelError
		}
		log.Log(context.Background(), level, "backup_run_finished", "event", "backup_run_finished",
			"exit_code", rep.ExitCode, "complete", rep.Complete, "pbs_snapshot", rep.PBSSnapshot,
			"duration_seconds", round(rep.DurationSeconds), "upload_seconds", round(rep.UploadSeconds),
			"instances_ok", rep.InstancesOK, "instances_failed", rep.InstancesFailed,
			"volumes_ok", rep.VolumesOK, "volumes_failed", rep.VolumesFailed,
			"logical_bytes", rep.LogicalBytes, "uploaded_bytes", rep.UploadedBytes, "error", rep.Error)
	}()

	// 1. Discovery (and, for scheduled runs, what is due now).
	plan, err := Discover(e.Incus, e.Cfg, sel)
	if err != nil {
		return rep, err
	}
	rep.Scheduled = sel.Scheduled
	rep.HostContent = len(sel.Instances) == 0
	writeIndex := true
	if sel.Scheduled {
		st, err := LoadState(e.Cfg.Storage.StateDirectory)
		if err != nil {
			return rep, fmt.Errorf("reading state: %w", err)
		}
		due, err := SelectDue(plan, st, e.Cfg.Backup.Schedule, start)
		if err != nil {
			return rep, err
		}
		rep.Deferred = due.Deferred
		if due.Empty() {
			rep.NothingDue, rep.ExitCode = true, ExitOK
			return rep, nil
		}
		plan, writeIndex, rep.HostContent, rep.Known = due.Plan, due.Host, due.Host, due.Known
	} else if len(sel.Instances) == 0 && len(sel.Projects) == 0 {
		for _, t := range plan.Targets {
			rep.Known = append(rep.Known, TargetKey(t.Kind, t.Project, t.Name))
		}
	}
	log.Info("backup_run_started", "event", "backup_run_started", "version", e.Version, "scheduled", sel.Scheduled,
		"targets", len(plan.Targets), "deferred", rep.Deferred, "host_index", writeIndex)
	rep.Excluded, rep.Warnings = plan.Excluded, plan.Warnings
	for _, w := range plan.Warnings {
		log.Warn("backup_warning", "event", "backup_warning", "warning", w)
	}
	for _, x := range plan.Excluded {
		log.Info("target_excluded", "event", "target_excluded", "what", x.What, "reason", x.Reason)
	}
	if len(plan.Targets) == 0 && !sel.Scheduled {
		return rep, errors.New("nothing to back up")
	}
	for _, t := range plan.Targets {
		if t.Kind != KindVolume {
			instanceStatus[t.Project+"/"+t.Name] = t.Status
		}
	}
	var targets []Target
	for _, t := range plan.Targets {
		if t.Problem != "" {
			rep.Items = append(rep.Items, ItemResult{Kind: t.Kind, Project: t.Project, Name: t.Name, Pool: t.Pool.Name,
				Status: "failed", Error: t.Problem, Schedule: t.Schedule})
			log.Error("target_unsupported", "event", "target_unsupported", "target", t.ID(), "error", t.Problem)
			continue
		}
		targets = append(targets, t)
	}
	if len(targets) < len(plan.Targets) && e.Cfg.Backup.OnInstanceError == "abort" {
		return rep, fmt.Errorf("%d target(s) cannot be backed up (on_instance_error=abort)", len(plan.Targets)-len(targets))
	}
	if len(targets) == 0 && len(plan.Targets) > 0 {
		return rep, errors.New("no target can be backed up")
	}

	if v, verr := e.Incus.ServerVersion(); verr == nil {
		rep.Versions["incus"] = v
	}
	rep.Versions["proxmox-backup-client"] = e.PBC.Version(ctx)

	// 2. Preflight: staging area must be clean, PBS must be reachable.
	if err := e.preflight(ctx, sel, targets, rep, log); err != nil {
		return rep, err
	}

	// 3. pre_backup hooks, temporary snapshots, post_snapshot hooks.
	// post_snapshot (e.g. thaw) always runs once pre_backup started, even if
	// pre_backup failed or the run was interrupted.
	hookEnv := runEnv(sel, instanceStatus)
	hookEnv["INCUS_PBS_RUN_ID"], hookEnv["INCUS_PBS_SNAPSHOT_NAME"] = runID, runID
	postSnapshot := func() error {
		env := map[string]string{"INCUS_PBS_PHASE": "post_snapshot"}
		for k, v := range hookEnv {
			env[k] = v
		}
		return e.runHooks(context.WithoutCancel(ctx), "post_snapshot", e.Cfg.Hooks.PostSnapshot, env, instanceStatus, rep, log)
	}
	preEnv := map[string]string{"INCUS_PBS_PHASE": "pre_backup"}
	for k, v := range hookEnv {
		preEnv[k] = v
	}
	if err := e.runHooks(ctx, "pre_backup", e.Cfg.Hooks.PreBackup, preEnv, instanceStatus, rep, log); err != nil {
		_ = postSnapshot()
		return rep, err
	}
	snapStart := e.now()
	results := make([]prepared, len(targets))
	e.forEach(ctx, len(targets), func(i int) {
		results[i] = prepared{t: targets[i]}
		results[i].err = e.snapshot(targets[i], runID, start, &cleanup, log)
	})
	log.Info("snapshots_created", "event", "snapshots_created", "count", len(targets), "seconds", round(time.Since(snapStart).Seconds()))
	if err := postSnapshot(); err != nil {
		return rep, err
	}

	// 4. Read-only exposure (bounded parallelism).
	e.forEach(ctx, len(targets), func(i int) {
		if results[i].err != nil {
			return
		}
		t0 := time.Now()
		results[i].archives, results[i].datasets, results[i].err = e.expose(ctx, targets[i], runID, &cleanup)
		results[i].seconds = time.Since(t0).Seconds()
	})
	if ctx.Err() != nil {
		return rep, fmt.Errorf("interrupted: %w", ctx.Err())
	}

	var archives []pbc.Archive
	itemIdx := map[string]int{}
	failed := 0
	for _, r := range results {
		it := ItemResult{Kind: r.t.Kind, Project: r.t.Project, Name: r.t.Name, Pool: r.t.Pool.Name,
			Datasets: r.datasets, Seconds: round(r.seconds), Status: "included", Schedule: r.t.Schedule}
		if r.err != nil {
			it.Status, it.Error = "failed", r.err.Error()
			failed++
			log.Error("target_prepare_failed", "event", "target_prepare_failed", "target", r.t.ID(), "error", r.err)
		} else {
			for _, a := range r.archives {
				it.Archives = append(it.Archives, a.Name)
			}
			archives = append(archives, r.archives...)
		}
		itemIdx[r.t.ID()] = len(rep.Items)
		rep.Items = append(rep.Items, it)
	}
	if failed > 0 && e.Cfg.Backup.OnInstanceError == "abort" {
		return rep, fmt.Errorf("%d target(s) failed to prepare (on_instance_error=abort)", failed)
	}
	if len(archives) == 0 && len(results) > 0 {
		return rep, errors.New("no target could be prepared")
	}
	complete := true
	for _, it := range rep.Items {
		if it.Status == "failed" {
			complete = false
		}
	}

	perr := e.runPerGuest(ctx, runID, start, sel, plan, results, complete, writeIndex, rep, &cleanup, log)
	for _, s := range rep.ArchiveStats {
		rep.LogicalBytes += s.TotalBytes
		rep.UploadedBytes += s.UploadedBytes
	}
	for _, b := range rep.BlockStats {
		rep.LogicalBytes += int64(b.Size)
		rep.UploadedBytes += int64(b.ChunksUploaded) * pbsqemu.ChunkSize
	}
	return rep, perr
}

// forEach runs f for 0..n-1 with backup.parallelism workers. A panic in a
// worker is re-raised on the caller's goroutine after all workers finished,
// so Run's deferred cleanup (snapshots, clones, mounts) still runs.
func (e *Engine) forEach(ctx context.Context, n int, f func(i int)) {
	var g errgroup.Group
	g.SetLimit(max(1, e.Cfg.Backup.Parallelism))
	var once sync.Once
	var panicked any
	var stack []byte
	for i := range n {
		if ctx.Err() != nil {
			break
		}
		g.Go(func() error {
			defer func() {
				if r := recover(); r != nil {
					once.Do(func() { panicked, stack = r, debug.Stack() })
				}
			}()
			f(i)
			return nil
		})
	}
	_ = g.Wait()
	if panicked != nil {
		panic(fmt.Sprintf("%v\n\nworker stack:\n%s", panicked, stack))
	}
}

func (e *Engine) preflight(ctx context.Context, sel Selection, targets []Target, rep *Report, log *slog.Logger) error {
	if err := os.MkdirAll(e.Cfg.Storage.StagingDirectory, 0o700); err != nil {
		return fmt.Errorf("staging directory: %w", err)
	}
	if err := os.Chmod(e.Cfg.Storage.StagingDirectory, 0o700); err != nil {
		return fmt.Errorf("staging directory: %w", err)
	}
	pools := map[string]incus.Pool{}
	for _, t := range targets {
		pools[t.Pool.Name] = t.Pool
	}
	leftovers := func() ([]string, error) {
		var left []string
		mi, err := e.mountInfo()
		if err != nil {
			return nil, fmt.Errorf("reading mountinfo: %w", err)
		}
		left = append(left, e.ZFS(incus.Pool{}).MountsUnder(mi)...)
		for _, p := range pools {
			z := e.ZFS(p)
			if err := z.EnsureStaging(ctx); err != nil {
				return nil, fmt.Errorf("pool %s: staging dataset %s: %w", p.Name, z.StagingDataset, err)
			}
			l, err := z.ListStaging(ctx)
			if err != nil {
				return nil, err
			}
			left = append(left, l...)
		}
		for _, t := range targets {
			var snaps []string
			if t.Kind == KindVolume {
				snaps, _ = e.Incus.ListVolumeSnapshots(*t.Volume)
			} else {
				snaps, _ = e.Incus.ListSnapshots(t.Project, t.Name)
			}
			for _, s := range snaps {
				if incus.IsOwnSnapshot(s) {
					left = append(left, t.ID()+"@"+s)
				}
			}
		}
		return left, nil
	}
	left, err := leftovers()
	if err != nil {
		return err
	}
	if len(left) > 0 {
		if !e.Cfg.Backup.AutoCleanup {
			return fmt.Errorf("leftovers from a previous run: %v; run `incus-pbs cleanup` (or enable backup.auto_cleanup)", left)
		}
		// We hold the run lock, so these belong to a crashed earlier run.
		log.Warn("crash_recovery", "event", "crash_recovery", "leftovers", left)
		cr, cerr := e.Cleanup(ctx, sel, false)
		rep.CrashRecovery = cr
		if cerr != nil {
			return fmt.Errorf("crash recovery failed: %w", cerr)
		}
		if left, err = leftovers(); err != nil {
			return err
		} else if len(left) > 0 {
			return fmt.Errorf("leftovers remain after crash recovery: %v", left)
		}
	}
	if _, err := e.PBC.ListSnapshots(ctx, e.Cfg.PBS.Namespace); err != nil {
		return fmt.Errorf("PBS not reachable or not authorized: %w", err)
	}
	return nil
}

func (e *Engine) snapshot(t Target, snap string, start time.Time, cl *cleanupStack, log *slog.Logger) error {
	expires := start.Add(e.Cfg.Backup.SnapshotExpiry.D())
	if t.Kind == KindVolume {
		v := *t.Volume
		if err := e.Incus.CreateVolumeSnapshot(v, snap, expires); err != nil {
			return fmt.Errorf("creating snapshot: %w", err)
		}
		cl.push("delete snapshot "+t.ID()+"/"+snap, func(context.Context) error { return e.Incus.DeleteVolumeSnapshot(v, snap) })
	} else {
		if err := e.Incus.CreateSnapshot(t.Project, t.Name, snap, expires); err != nil {
			return fmt.Errorf("creating snapshot: %w", err)
		}
		cl.push("delete snapshot "+t.ID()+"/"+snap, func(context.Context) error { return e.Incus.DeleteSnapshot(t.Project, t.Name, snap) })
	}
	log.Debug("snapshot_created", "event", "snapshot_created", "target", t.ID(), "snapshot", snap)
	return nil
}

func (e *Engine) expose(ctx context.Context, t Target, snap string, cl *cleanupStack) ([]pbc.Archive, []string, error) {
	z := e.ZFS(t.Pool)
	suffix := incus.SnapshotSuffix(snap)
	root := e.mountRoot()
	mount := func(ds, base string) (pbc.Archive, error) {
		dir := filepath.Join(root, base)
		if err := z.MountSnapshot(ctx, ds+suffix, dir); err != nil {
			return pbc.Archive{}, fmt.Errorf("mounting %s: %w", ds+suffix, err)
		}
		cl.push("unmount "+dir, func(c context.Context) error { return z.Unmount(c, dir) })
		return pbc.Archive{Name: base + ".pxar", Source: dir}, nil
	}
	clone := func(ds, base string) (pbc.Archive, error) {
		target := z.StagingDataset + "/" + base
		if err := z.Clone(ctx, ds+suffix, target); err != nil {
			return pbc.Archive{}, fmt.Errorf("cloning %s: %w", ds+suffix, err)
		}
		cl.push("destroy clone "+target, func(c context.Context) error { return z.Destroy(c, target) })
		dev, err := z.WaitDevice(ctx, target, 60*time.Second)
		if err != nil {
			return pbc.Archive{}, err
		}
		return pbc.Archive{Name: base + ".img", Source: dev}, nil
	}

	switch t.Kind {
	case KindContainer:
		main, _ := incus.Datasets(t.Pool.Dataset, *t.Instance)
		a, err := mount(main, t.ArchiveBase)
		return []pbc.Archive{a}, []string{main}, err
	case KindVM:
		main, block := incus.Datasets(t.Pool.Dataset, *t.Instance)
		img, err := clone(block, t.ArchiveBase)
		if err != nil {
			return nil, nil, err
		}
		conf, err := mount(main, t.ArchiveBase+"-config")
		return []pbc.Archive{img, conf}, []string{block, main}, err
	default:
		ds := incus.VolumeDatasets(t.Pool.Dataset, *t.Volume)
		var a pbc.Archive
		var err error
		if t.ContentType == "block" {
			a, err = clone(ds, t.ArchiveBase)
		} else {
			a, err = mount(ds, t.ArchiveBase)
		}
		return []pbc.Archive{a}, []string{ds}, err
	}
}

// runEnv describes the run to hooks: whether it is scheduled and which
// instances it covers ("project/name", space separated).
func runEnv(sel Selection, instanceStatus map[string]string) map[string]string {
	names := make([]string, 0, len(instanceStatus))
	for n := range instanceStatus {
		names = append(names, n)
	}
	sort.Strings(names)
	env := map[string]string{"INCUS_PBS_TARGETS": strings.Join(names, " "), "INCUS_PBS_SCHEDULED": "0"}
	if sel.Scheduled {
		env["INCUS_PBS_SCHEDULED"] = "1"
	}
	return env
}

func round(f float64) float64 { return float64(int64(f*100)) / 100 }

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/urfave/cli/v3"

	"github.com/incus-pbs/incus-pbs/internal/backup"
	"github.com/incus-pbs/incus-pbs/internal/pbc"
	"github.com/incus-pbs/incus-pbs/internal/restore"
)

func restoreCommand() *cli.Command {
	snap := func(c *cli.Command) string { return c.Args().Get(0) }
	return &cli.Command{
		Name:  "restore",
		Usage: "list, inspect, mount and restore backups (never overwrites anything)",
		Commands: []*cli.Command{
			{
				Name: "list", Before: maxArgs(1), ArgsUsage: "[instance]",
				Usage: "run indexes and every instance's backups, or the backups of one instance",
				Flags: []cli.Flag{jsonFlag(), &cli.StringFlag{Name: "project", Usage: "project of the instance (with [instance])"}},
				Action: withRuntime(true, func(ctx context.Context, c *cli.Command, rt *runtime) error {
					l, err := rt.rs.List(ctx)
					if err != nil {
						return restoreFailed(rt, "list", err)
					}
					local := func(s pbc.Snapshot) string { return time.Unix(s.BackupTime, 0).Local().Format("2006-01-02 15:04") }
					if name := c.Args().Get(0); name != "" {
						snaps := l.InstanceSnapshots(c.String("project"), name)
						if c.Bool("json") {
							emitJSON(rt.stdout, snaps)
							return nil
						}
						if len(snaps) == 0 {
							return restoreFailed(rt, "list", fmt.Errorf("no backups of instance %q (group ct|vm/%s)", name, backup.GroupID(c.String("project"), name)))
						}
						t := newTable(rt.stdout, "SNAPSHOT", "LOCAL TIME", "SIZE")
						for _, s := range snaps {
							t.AppendRow([]any{s.Path(), local(s), bytesStr(s.Size)})
						}
						t.Render()
						return nil
					}
					if c.Bool("json") {
						emitJSON(rt.stdout, l)
						return nil
					}
					t := newTable(rt.stdout, "SNAPSHOT", "LOCAL TIME", "SIZE")
					row := func(s pbc.Snapshot) table.Row { return table.Row{"  " + s.Path(), local(s), bytesStr(s.Size)} }
					t.AppendRow(table.Row{"run indexes (metadata, host config, shared volumes)", "", ""})
					for _, s := range l.HostIndexes {
						t.AppendRow(row(s))
					}
					if len(l.InProgress) > 0 {
						t.AppendSeparator()
						t.AppendRow(table.Row{"in progress (a backup is writing them; not restorable yet)", "", ""})
						for _, s := range l.InProgress {
							t.AppendRow(table.Row{"  " + s.Path(), local(s), "-"})
						}
					}
					for _, g := range l.Guests {
						t.AppendSeparator()
						t.AppendRow(table.Row{g.Group, "", ""})
						for _, s := range g.Snapshots {
							t.AppendRow(row(s))
						}
					}
					t.Render()
					return nil
				}),
			},
			{
				Name: "inspect", Before: requireArgs("snapshot"), Usage: "show a manifest: a run index (completeness, targets, archives) or a guest snapshot", ArgsUsage: "<snapshot>",
				Flags: []cli.Flag{jsonFlag()},
				Action: withRuntime(true, func(ctx context.Context, c *cli.Command, rt *runtime) error {
					if restore.IsGuestPath(snap(c)) {
						gm, err := rt.rs.GuestManifest(ctx, snap(c))
						if err != nil {
							return restoreFailed(rt, "inspect", err)
						}
						if c.Bool("json") {
							emitJSON(rt.stdout, gm)
							return nil
						}
						printGuestManifest(rt, snap(c), gm)
						return nil
					}
					s, err := rt.rs.Resolve(ctx, snap(c))
					if err != nil {
						return restoreFailed(rt, "inspect", err)
					}
					m, err := rt.rs.Manifest(ctx, s)
					if err != nil {
						return restoreFailed(rt, "inspect", err)
					}
					if c.Bool("json") {
						emitJSON(rt.stdout, m)
						return nil
					}
					printManifest(rt, s, m)
					return nil
				}),
			},
			{
				Name: "instance", Before: requireArgs("snapshot", "instance"), Usage: "restore a container or VM as a new instance", ArgsUsage: "<snapshot> <instance>",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "project", Usage: "project of the instance at backup time"},
					&cli.StringFlag{Name: "target-project", Usage: "restore into this project (default: original)"},
					&cli.StringFlag{Name: "name", Usage: "new instance name (default: original)"},
					&cli.StringFlag{Name: "pool", Usage: "target storage pool (default: original)"},
					&cli.StringFlag{Name: "identity", Value: "auto", Usage: "auto | keep | new (MAC addresses, UUIDs)"},
					&cli.BoolFlag{Name: "drop-missing-devices", Usage: "restore without devices whose volume/network is missing or in use"},
					&cli.BoolFlag{Name: "staging-only", Usage: "restore into the staging directory only"},
					&cli.BoolFlag{Name: "keep-staging", Usage: "keep the staging directory after import"},
				},
				Action: withRuntime(true, locked(func(ctx context.Context, c *cli.Command, rt *runtime) error {
					res, err := rt.rs.RestoreInstance(ctx, restore.InstanceOptions{
						Snapshot: snap(c), Instance: c.Args().Get(1), SourceProject: c.String("project"),
						TargetProject: c.String("target-project"), NewName: c.String("name"), Pool: c.String("pool"),
						Identity: c.String("identity"), DropMissingDevices: c.Bool("drop-missing-devices"),
						StagingOnly: c.Bool("staging-only"), KeepStaging: c.Bool("keep-staging"),
					})
					return restoreResult(rt, "instance", res, err)
				})),
			},
			{
				Name: "test", Before: requireTestArgs, Usage: "restore drill: restore an instance as an isolated throwaway copy, start it, check files, delete it",
				ArgsUsage: "<snapshot> <instance>",
				Flags: []cli.Flag{
					&cli.StringSliceFlag{Name: "file", Aliases: []string{"f"}, Required: true, Usage: "absolute path that must exist as a regular file in the instance (repeatable)"},
					&cli.StringFlag{Name: "contains", Usage: "the file must contain this text (only with exactly one --file)"},
					&cli.StringFlag{Name: "project", Usage: "project of the instance at backup time"},
					&cli.StringFlag{Name: "target-project", Usage: "create the test instance in this project (default: original)"},
					&cli.StringFlag{Name: "name", Usage: "test instance name (default: restoretest-<instance>)"},
					&cli.StringFlag{Name: "pool", Usage: "target storage pool (default: original)"},
					&cli.DurationFlag{Name: "timeout", Value: 5 * time.Minute, Usage: "time allowed for start-up and the file checks"},
					&cli.BoolFlag{Name: "keep", Usage: "keep the stopped test instance instead of deleting it"},
				},
				Action: withRuntime(true, locked(func(ctx context.Context, c *cli.Command, rt *runtime) error {
					res, err := rt.rs.TestInstance(ctx, restore.TestOptions{
						Snapshot: snap(c), Instance: c.Args().Get(1), SourceProject: c.String("project"),
						TargetProject: c.String("target-project"), NewName: c.String("name"), Pool: c.String("pool"),
						Files: c.StringSlice("file"), Contains: c.String("contains"),
						Timeout: c.Duration("timeout"), Keep: c.Bool("keep"),
					})
					recordRestoreTest(rt.cfg, rt.log, restoreTestState(c.String("project"), c.Args().Get(1), res, err, time.Now()))
					return restoreResult(rt, "test", res, err)
				})),
			},
			{
				Name: "volume", Before: requireArgs("snapshot", "volume"), Usage: "restore a custom volume as a new volume", ArgsUsage: "<snapshot> <volume>",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "project", Usage: "project of the volume at backup time"},
					&cli.StringFlag{Name: "source-pool", Usage: "pool of the volume at backup time"},
					&cli.StringFlag{Name: "target-project", Usage: "restore into this project (default: original)"},
					&cli.StringFlag{Name: "name", Usage: "new volume name (default: original)"},
					&cli.StringFlag{Name: "pool", Usage: "target storage pool (default: original)"},
					&cli.BoolFlag{Name: "staging-only", Usage: "restore into the staging directory only"},
					&cli.BoolFlag{Name: "keep-staging", Usage: "keep the staging directory after import"},
				},
				Action: withRuntime(true, locked(func(ctx context.Context, c *cli.Command, rt *runtime) error {
					res, err := rt.rs.RestoreVolume(ctx, restore.VolumeOptions{
						Snapshot: snap(c), Volume: c.Args().Get(1), SourceProject: c.String("project"),
						SourcePool: c.String("source-pool"), TargetProject: c.String("target-project"),
						NewName: c.String("name"), Pool: c.String("pool"),
						StagingOnly: c.Bool("staging-only"), KeepStaging: c.Bool("keep-staging"),
					})
					return restoreResult(rt, "volume", res, err)
				})),
			},
			{
				Name: "host", Before: requireArgs("snapshot"), Usage: "write a host recovery kit (metadata, host config, preseed.yaml, RECOVERY.md); changes nothing",
				ArgsUsage: "<snapshot>",
				Flags:     []cli.Flag{&cli.StringFlag{Name: "dir", Required: true, Usage: "empty directory for the recovery kit"}},
				Action: withRuntime(true, locked(func(ctx context.Context, c *cli.Command, rt *runtime) error {
					res, err := rt.rs.RestoreHost(ctx, snap(c), c.String("dir"))
					return restoreResult(rt, "host", res, err)
				})),
			},
			{
				Name:      "mount",
				Before:    requireArgs("snapshot", "name"),
				Usage:     "instant read-only access without a full restore (FUSE mount / mapped disk image)",
				ArgsUsage: "<snapshot> <instance|volume>",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "dir", Required: true, Usage: "empty directory to mount on"},
					&cli.StringFlag{Name: "project", Usage: "project at backup time"},
					&cli.IntFlag{Name: "partition", Usage: "VM disks: partition number (default: largest filesystem)"},
				},
				Action: withRuntime(true, func(ctx context.Context, c *cli.Command, rt *runtime) error {
					res, err := rt.rs.Mount(ctx, restore.MountOptions{Snapshot: snap(c), Name: c.Args().Get(1),
						Project: c.String("project"), Dir: c.String("dir"), Partition: c.Int("partition")})
					return restoreResult(rt, "mount", res, err)
				}),
			},
			{
				Name: "unmount", Before: requireArgs("target"), Usage: "release a mount made by `restore mount`", ArgsUsage: "<dir|/dev/loopN>",
				Action: withRuntime(true, func(ctx context.Context, c *cli.Command, rt *runtime) error {
					if err := rt.rs.Unmount(ctx, c.Args().Get(0)); err != nil {
						return restoreFailed(rt, "unmount", err)
					}
					return nil
				}),
			},
		},
	}
}

// requireTestArgs validates `restore test` arguments before anything is touched.
func requireTestArgs(ctx context.Context, c *cli.Command) (context.Context, error) {
	if completing(ctx) {
		return ctx, nil
	}
	if ctx, err := requireArgs("snapshot", "instance")(ctx, c); err != nil {
		return ctx, err
	}
	files := c.StringSlice("file")
	for _, f := range files {
		if !strings.HasPrefix(f, "/") {
			return ctx, cli.Exit(fmt.Sprintf("%s: --file %q must be an absolute path", c.FullName(), f), exitUsage)
		}
	}
	if c.String("contains") != "" && len(files) != 1 {
		return ctx, cli.Exit(c.FullName()+": --contains needs exactly one --file", exitUsage)
	}
	return ctx, nil
}

func restoreFailed(rt *runtime, cmd string, err error) error {
	rt.log.Error("restore_failed", "event", "restore_failed", "command", cmd, "error", err)
	return cli.Exit("error: "+err.Error(), exitFailed)
}

// restoreResult prints the (possibly partial) result and maps the error.
func restoreResult[T any](rt *runtime, cmd string, res *T, err error) error {
	if res != nil {
		emitJSON(rt.stdout, res)
	}
	if err != nil {
		return restoreFailed(rt, cmd, err)
	}
	return nil
}

func printManifest(rt *runtime, snap string, m *backup.Manifest) {
	out := rt.stdout
	fmt.Fprintf(out, "snapshot:  %s\nhost:      %s\ncreated:   %s\ncomplete:  %v\nversions:  %v\n", snap, m.Hostname,
		m.CreatedAt.Local().Format(time.RFC3339), m.Complete, m.Versions)
	fmt.Fprintln(out)
	status := map[string]backup.ItemResult{}
	for _, it := range m.Items {
		status[string(it.Kind)+"/"+it.Project+"/"+it.Name] = it
	}
	tbl := newTable(out, "KIND", "PROJECT", "NAME", "POOL", "STATUS", "PBS SNAPSHOT", "ARCHIVES")
	for _, t := range m.Targets {
		it := status[string(t.Kind)+"/"+t.Project+"/"+t.Name]
		st := it.Status
		if it.Error != "" {
			st += " (" + it.Error + ")"
		}
		archives := it.Archives
		if len(archives) == 0 {
			archives = t.Archives()
		}
		where := it.Snapshot
		if where == "" {
			where = snap
		}
		tbl.AppendRow(table.Row{t.Kind, t.Project, t.Name, t.Pool.Name, dash(st), where, strings.Join(archives, ",")})
	}
	tbl.Render()
	for _, x := range m.Excluded {
		fmt.Fprintf(out, "excluded: %s (%s)\n", x.What, x.Reason)
	}
	for _, w := range m.Warnings {
		fmt.Fprintf(out, "warning: %s\n", w)
	}
}

func printGuestManifest(rt *runtime, snap string, m *backup.GuestManifest) {
	out := rt.stdout
	fmt.Fprintf(out, "snapshot:  %s\nhost:      %s (host/%s)\ncreated:   %s\nrun:       %s  scheduled=%v\nversion:   %s\n\n", snap, m.Hostname,
		m.HostBackupID, m.CreatedAt.Local().Format(time.RFC3339), m.RunID, m.Scheduled, m.ToolVersion)
	tbl := newTable(out, "KIND", "PROJECT", "NAME", "POOL", "SCHEDULE", "ARCHIVES")
	for _, t := range m.Targets {
		tbl.AppendRow(table.Row{t.Kind, t.Project, t.Name, t.Pool.Name, dash(t.Schedule), strings.Join(t.Archives(), ",")})
	}
	tbl.Render()
}

// restoreTestState turns a drill result into its state entry. The source
// instance is taken from the restore result when the backup was found, else
// from the command line (the kind is then unknown).
func restoreTestState(project, instance string, res *restore.TestResult, err error, end time.Time) backup.RestoreTestState {
	r := backup.RestoreTestState{Project: project, Name: instance, LastRun: end, LastOK: err == nil && res != nil && res.OK}
	if res != nil {
		r.DurationSeconds, r.BootSeconds = res.Seconds, res.BootSeconds
		if rr := res.Restore; rr != nil {
			r.Kind, r.Snapshot = rr.Kind, rr.Snapshot
			if p, n, ok := strings.Cut(rr.Source, "/"); ok {
				r.Project, r.Name = p, n
			}
		}
	}
	if err != nil {
		r.LastError = err.Error()
	}
	return r
}

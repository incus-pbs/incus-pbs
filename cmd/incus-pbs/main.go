// Command incus-pbs backs up Incus instances and custom volumes to a
// Proxmox Backup Server using the official proxmox-backup-client.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/urfave/cli/v3"

	"github.com/incus-pbs/incus-pbs/internal/backup"
	"github.com/incus-pbs/incus-pbs/internal/config"
	"github.com/incus-pbs/incus-pbs/internal/execx"
	"github.com/incus-pbs/incus-pbs/internal/incus"
	"github.com/incus-pbs/incus-pbs/internal/lock"
	"github.com/incus-pbs/incus-pbs/internal/pbc"
	"github.com/incus-pbs/incus-pbs/internal/restore"
)

// Set via -ldflags "-X main.version=...".
var (
	version = "dev"
	commit  = "unknown"
)

// Exit codes (documented in the root help).
const (
	exitOK      = 0
	exitFailed  = 1
	exitPartial = 2
	exitUsage   = 64 // EX_USAGE: bad command line, never a backup result
	exitConfig  = 3
	exitLocked  = 75
)

const rootDescription = `Backs up Incus containers, VMs and custom volumes to a Proxmox Backup Server.

<snapshot> arguments accept "latest", a time (2026-11-24T02:30:00Z: the newest
backup at or before it), a guest snapshot (ct|vm/<id>/<time>) or, for volumes and
the host kit, a run index (host/<id>/<time>).
Restores never overwrite existing instances or volumes.

Exit codes: 0 ok, 1 failed, 2 partial backup or cleanup problems,
3 configuration error, 64 usage error, 75 another run is in progress.`

func main() { os.Exit(run(context.Background(), os.Args, os.Stdout, os.Stderr)) }

// run executes the CLI and returns the process exit code.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if len(args) > 0 && args[len(args)-1] == completionFlag {
		ctx = context.WithValue(ctx, completingKey{}, true)
	}
	err := newApp(stdout, stderr).Run(ctx, args)
	if err == nil {
		return exitOK
	}
	var ec cli.ExitCoder
	if errors.As(err, &ec) {
		if msg := ec.Error(); msg != "" {
			fmt.Fprintln(stderr, msg)
		}
		return ec.ExitCode()
	}
	fmt.Fprintln(stderr, "error:", err) // flag/usage errors from the parser
	return exitUsage
}

func newApp(stdout, stderr io.Writer) *cli.Command {
	return &cli.Command{
		Name:        "incus-pbs",
		Usage:       "back up Incus to Proxmox Backup Server",
		Description: rootDescription,
		Version:     fmt.Sprintf("%s (commit %s)", version, commit),
		Writer:      stdout,
		ErrWriter:   stderr,
		Suggest:     true,
		// `incus-pbs completion bash|zsh|fish|pwsh`, listed in --help.
		EnableShellCompletion:           true,
		ConfigureShellCompletionCommand: func(c *cli.Command) { c.Hidden = false },
		// Exit codes are handled by run(), never by os.Exit inside the library.
		ExitErrHandler: func(context.Context, *cli.Command, error) {},
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "config", Aliases: []string{"c"}, Value: config.DefaultPath,
				Usage: "configuration file", Sources: cli.EnvVars("INCUS_PBS_CONFIG")},
			&cli.StringFlag{Name: "log-level", Usage: "override logging.level (debug, info, warn, error)"},
		},
		Commands: []*cli.Command{
			{
				Name: "version", Usage: "print the version",
				Action: func(_ context.Context, c *cli.Command) error {
					fmt.Fprintf(c.Root().Writer, "incus-pbs %s (commit %s)\n", version, commit)
					return nil
				},
			},
			{
				Name: "check", Before: requireArgs(), Usage: "validate configuration, credentials, PBS, Incus and ZFS",
				Flags:  []cli.Flag{jsonFlag()},
				Action: withRuntime(true, cmdCheck),
			},
			{
				Name: "list", Before: requireArgs(), Usage: "show what would be backed up",
				Flags:  []cli.Flag{jsonFlag(), projectsFlag()},
				Action: withRuntime(true, cmdList),
			},
			{
				Name: "backup", Usage: "create one PBS backup snapshot of this host",
				Flags: []cli.Flag{
					&cli.StringSliceFlag{Name: "instance", Aliases: []string{"i"}, Usage: "only this instance (repeatable)"},
					projectsFlag(),
					&cli.BoolFlag{Name: "scheduled", Usage: "only targets whose schedule (user.incus-pbs.schedule or backup.schedule) is due; for the systemd timer"},
				},
				Before: func(ctx context.Context, c *cli.Command) (context.Context, error) {
					if completing(ctx) {
						return ctx, nil
					}
					if ctx, err := requireArgs()(ctx, c); err != nil {
						return ctx, err
					}
					if c.Bool("scheduled") && len(c.StringSlice("instance")) > 0 {
						return ctx, cli.Exit("backup: --scheduled and --instance cannot be combined", exitUsage)
					}
					return ctx, nil
				},
				Action: withRuntime(true, scheduledLock(locked(cmdBackup))),
			},
			{
				Name:   "cleanup",
				Before: requireArgs(),
				Usage:  "remove leftovers of interrupted runs (own snapshots, clones and mounts only)",
				Flags: []cli.Flag{
					&cli.BoolFlag{Name: "dry-run", Usage: "only report what would be removed"},
					&cli.BoolFlag{Name: "release-bookmarks", Usage: "also drop change-tracking bookmarks (unblocks `incus snapshot restore`; next VM backup reads in full)"},
				},
				Action: withRuntime(true, locked(cmdCleanup)),
			},
			{
				Name:   "status",
				Before: requireArgs(),
				Usage:  "last run and per-target history; exits non-zero if the last run failed, is too old or a target is overdue",
				Flags: []cli.Flag{
					jsonFlag(),
					&cli.DurationFlag{Name: "max-age", Value: defaultMaxAge, Usage: "maximum age of the last successful run"},
					&cli.DurationFlag{Name: "grace", Value: defaultGrace, Usage: "a target is overdue this long after its first schedule slot without a successful backup"},
				},
				Action: withRuntime(false, cmdStatus),
			},
			restoreCommand(),
		},
		Action: func(_ context.Context, c *cli.Command) error {
			if c.NArg() > 0 {
				return cli.Exit(fmt.Sprintf("unknown command %q (see --help)", c.Args().First()), exitUsage)
			}
			_ = cli.ShowRootCommandHelp(c)
			return cli.Exit("", exitUsage)
		},
	}
}

func jsonFlag() cli.Flag { return &cli.BoolFlag{Name: "json", Usage: "JSON output"} }

func projectsFlag() cli.Flag {
	return &cli.StringSliceFlag{Name: "project", Aliases: []string{"p"}, Usage: "project (repeatable; default: incus.projects from the config)"}
}

// runtime holds everything a command needs, built lazily so that `version`
// and `--help` work without a configuration.
type runtime struct {
	cfg    config.Config
	log    *slog.Logger
	eng    *backup.Engine
	rs     *restore.Restorer
	stdout io.Writer
	stderr io.Writer
}

type action func(ctx context.Context, c *cli.Command, rt *runtime) error

// withRuntime loads the configuration (and connects to Incus if needed).
func withRuntime(needIncus bool, f action) cli.ActionFunc {
	return func(ctx context.Context, c *cli.Command) error {
		cfg, err := config.Load(c.String("config"))
		if err != nil {
			return cli.Exit(err.Error(), exitConfig)
		}
		if lvl := c.String("log-level"); lvl != "" {
			cfg.Logging.Level = lvl
			if err := cfg.Validate(); err != nil {
				return cli.Exit(err.Error(), exitConfig)
			}
		}
		root := c.Root()
		rt := &runtime{cfg: cfg, log: newLogger(cfg.Logging, root.ErrWriter), stdout: root.Writer, stderr: root.ErrWriter}
		if needIncus {
			x := execx.OS{}
			ic, err := incus.Connect(cfg.Incus.Socket)
			if err != nil {
				rt.log.Error("incus_connect_failed", "event", "incus_connect_failed", "error", err)
				return cli.Exit("", exitFailed)
			}
			client := &pbc.Client{Bin: cfg.PBS.ClientBinary, Repository: cfg.PBS.Repository,
				PasswordFile: cfg.PBS.PasswordFile, Fingerprint: cfg.PBS.Fingerprint, X: x}
			rt.eng = &backup.Engine{Cfg: cfg, Incus: ic, X: x, Log: rt.log, Version: version, PBC: client}
			rt.rs = &restore.Restorer{Cfg: cfg, Incus: ic, PBC: client, Log: rt.log}
		}
		return f(ctx, c, rt)
	}
}

// locked runs f under the run lock (backup, cleanup, mutating restores).
func locked(f action) action {
	return func(ctx context.Context, c *cli.Command, rt *runtime) error {
		l, err := lock.Acquire(rt.cfg.Backup.LockFile)
		if err != nil {
			rt.log.Error("lock_failed", "event", "lock_failed", "error", err)
			if errors.Is(err, lock.ErrLocked) {
				return cli.Exit("", exitLocked)
			}
			return cli.Exit("", exitFailed)
		}
		defer l.Release()
		return f(ctx, c, rt)
	}
}

// scheduledLock turns a held lock into a quiet success for `backup
// --scheduled`: the run in progress continues and the next timer tick
// catches up on whatever became due meanwhile.
func scheduledLock(f action) action {
	return func(ctx context.Context, c *cli.Command, rt *runtime) error {
		err := f(ctx, c, rt)
		var ec cli.ExitCoder
		if c.Bool("scheduled") && errors.As(err, &ec) && ec.ExitCode() == exitLocked {
			rt.log.Info("scheduled_run_skipped", "event", "scheduled_run_skipped", "reason", "another run holds the lock")
			return nil
		}
		return err
	}
}

// completionFlag is appended by the shell completion scripts; while it is
// present, urfave/cli still runs the Before hooks on partial input.
const completionFlag = "--generate-shell-completion"

type completingKey struct{}

// completing reports whether this run only answers a completion request, in
// which case argument validation is skipped so it cannot print errors.
func completing(ctx context.Context) bool {
	v, _ := ctx.Value(completingKey{}).(bool)
	return v
}

// requireArgs validates the number of positional arguments before anything
// (config, Incus, PBS) is touched.
func requireArgs(names ...string) cli.BeforeFunc {
	return func(ctx context.Context, c *cli.Command) (context.Context, error) {
		if completing(ctx) {
			return ctx, nil
		}
		if c.NArg() != len(names) {
			return ctx, cli.Exit(fmt.Sprintf("%s: expected arguments %v, got %d (see --help)", c.FullName(), names, c.NArg()), exitUsage)
		}
		return ctx, nil
	}
}

func maxArgs(n int) cli.BeforeFunc {
	return func(ctx context.Context, c *cli.Command) (context.Context, error) {
		if completing(ctx) {
			return ctx, nil
		}
		if c.NArg() > n {
			return ctx, cli.Exit(fmt.Sprintf("%s: expected at most %d argument(s), got %d (see --help)", c.FullName(), n, c.NArg()), exitUsage)
		}
		return ctx, nil
	}
}

func newLogger(c config.Logging, w io.Writer) *slog.Logger {
	var lvl slog.Level
	_ = lvl.UnmarshalText([]byte(c.Level))
	opts := &slog.HandlerOptions{Level: lvl}
	if c.Format == "text" {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}

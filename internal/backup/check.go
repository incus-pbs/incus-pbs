package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/dustin/go-humanize"

	"github.com/incus-pbs/incus-pbs/internal/config"
	"github.com/incus-pbs/incus-pbs/internal/execx"
	"github.com/incus-pbs/incus-pbs/internal/incus"
	"github.com/incus-pbs/incus-pbs/internal/pbc"
)

// CheckStatus of a single check.
type CheckStatus string

const (
	CheckOK   CheckStatus = "ok"
	CheckWarn CheckStatus = "warn"
	CheckFail CheckStatus = "fail"
)

// CheckResult is one line of `incus-pbs check`.
type CheckResult struct {
	Name   string      `json:"name"`
	Status CheckStatus `json:"status"`
	Detail string      `json:"detail"`
}

// Check validates the environment without changing anything (except creating
// the staging directory and staging datasets if missing).
func (e *Engine) Check(ctx context.Context) []CheckResult {
	var out []CheckResult
	add := func(name string, st CheckStatus, f string, a ...any) {
		out = append(out, CheckResult{Name: name, Status: st, Detail: fmt.Sprintf(f, a...)})
	}

	// Credentials.
	if err := pbc.CheckSecretFile(e.Cfg.PBS.PasswordFile); err != nil {
		add("pbs password file", CheckFail, "%v", err)
	} else {
		add("pbs password file", CheckOK, "%s (mode 0600-compatible)", e.Cfg.PBS.PasswordFile)
	}
	if kf := e.Cfg.PBS.EncryptionKeyFile; kf == "" {
		add("encryption key", CheckWarn, "no encryption_key_file configured: backups are NOT client-side encrypted")
	} else if err := pbc.CheckSecretFile(kf); err != nil {
		add("encryption key", CheckFail, "%v", err)
	} else if b, err := os.ReadFile(kf); err != nil {
		add("encryption key", CheckFail, "not readable: %v", err)
	} else if !json.Valid(b) {
		add("encryption key", CheckFail, "%s is not a PBS key file (invalid JSON)", kf)
	} else {
		add("encryption key", CheckOK, "%s readable; keep a copy OUTSIDE this host (paperkey), restores are impossible without it", kf)
	}

	// Incus.
	v, err := e.Incus.ServerVersion()
	if err != nil {
		add("incus api", CheckFail, "%v", err)
	} else {
		add("incus api", CheckOK, "server version %s", v)
	}

	// PBS client and server.
	if path, err := exec.LookPath(e.PBC.Binary()); err != nil {
		add("pbs client binary", CheckFail, "%s not found in PATH", e.PBC.Binary())
	} else {
		add("pbs client binary", CheckOK, "%s (%s)", path, firstLine(e.PBC.Version(ctx)))
		ns := e.Cfg.PBS.Namespace
		snaps, err := e.PBC.ListSnapshots(ctx, ns)
		if err != nil {
			add("pbs connection", CheckFail, "%s", pbsHint(err))
		} else {
			add("pbs connection", CheckOK, "repository reachable, authenticated, namespace %q accessible (%d snapshots)", ns, len(snaps))
			others := map[string]bool{}
			for _, sn := range snaps {
				if sn.BackupType == "host" && sn.BackupID != e.Cfg.PBS.BackupID {
					others[sn.BackupID] = true
				}
			}
			if len(others) > 0 {
				ids := make([]string, 0, len(others))
				for id := range others {
					ids = append(ids, id)
				}
				sort.Strings(ids)
				add("pbs namespace", CheckWarn, "namespace %q is also used by host(s) %s; give every host its own namespace, "+
					"or instances with the same name share ct/vm groups (broken incremental chains, mixed retention)", ns, strings.Join(ids, ", "))
			}
		}
	}

	// Staging directory.
	sd := e.Cfg.Storage.StagingDirectory
	if err := os.MkdirAll(sd, 0o700); err != nil {
		add("staging directory", CheckFail, "%v", err)
	} else if st, err := os.Stat(sd); err != nil {
		add("staging directory", CheckFail, "%v", err)
	} else if st.Mode().Perm()&0o077 != 0 {
		add("staging directory", CheckWarn, "%s has mode %o; it will be tightened to 0700 on backup", sd, st.Mode().Perm())
	} else if f, err := os.CreateTemp(sd, ".check-*"); err != nil {
		add("staging directory", CheckFail, "not writable: %v", err)
	} else {
		f.Close()
		os.Remove(f.Name())
		free := freeBytes(sd)
		st := CheckOK
		if free < uint64(e.Cfg.Storage.MinFreeBytes) {
			st = CheckFail
		}
		add("staging directory", st, "%s writable, %s free (minimum %s)", sd, humanize.IBytes(free), humanize.IBytes(uint64(e.Cfg.Storage.MinFreeBytes)))
	}
	if mi, err := e.mountInfo(); err == nil {
		if m := e.ZFS(incus.Pool{}).MountsUnder(mi); len(m) > 0 {
			add("stale mounts", CheckFail, "%v; run `incus-pbs cleanup`", m)
		}
	}
	// State, metrics, hooks.
	if err := os.MkdirAll(e.Cfg.Storage.StateDirectory, 0o700); err != nil {
		add("state directory", CheckFail, "%v", err)
	} else {
		add("state directory", CheckOK, "%s", e.Cfg.Storage.StateDirectory)
	}
	if p := e.Cfg.Monitoring.TextfilePath; p != "" {
		if st, err := os.Stat(filepath.Dir(p)); err != nil || !st.IsDir() {
			add("metrics textfile", CheckFail, "directory %s does not exist", filepath.Dir(p))
		} else {
			add("metrics textfile", CheckOK, "%s", p)
		}
	}
	for phase, hooks := range map[string][]config.Hook{"pre_backup": e.Cfg.Hooks.PreBackup, "post_snapshot": e.Cfg.Hooks.PostSnapshot, "post_backup": e.Cfg.Hooks.PostBackup} {
		for _, h := range hooks {
			name := "hook " + phase + "/" + h.Name
			if h.Instance != "" {
				add(name, CheckOK, "runs %q inside instance %s", h.Command, h.Instance)
				continue
			}
			if st, err := os.Stat(h.Command[0]); err != nil {
				add(name, CheckFail, "%v", err)
			} else if st.Mode()&0o111 == 0 || st.IsDir() {
				add(name, CheckFail, "%s is not an executable file", h.Command[0])
			} else if st.Mode()&0o022 != 0 {
				add(name, CheckWarn, "%s is group/world writable (runs as root)", h.Command[0])
			} else {
				add(name, CheckOK, "%q", h.Command)
			}
		}
	}
	// Changed-block tracking.
	switch mode := e.Cfg.Backup.BlockIncremental; {
	case mode == "off":
		add("block incremental", CheckOK, "disabled: VM disks and block volumes are read in full")
	case e.blockBackend().Load() != nil:
		add("block incremental", CheckWarn, "%s; VM disks and block volumes are read in full every run (install libproxmox-backup-qemu0)", e.blockBackend().Load())
	default:
		if _, err := exec.LookPath("zstream"); err != nil {
			add("block incremental", CheckFail, "zstream not found (zfsutils)")
		} else {
			st := loadBlockState(e.Cfg.Storage.StateDirectory)
			add("block incremental", CheckOK, "mode %s, libproxmox-backup-qemu %s, %d tracked device(s)%s", mode, e.blockBackend().Version(), len(st.Devices),
				map[bool]string{true: "; NOTE: bookmarks block `incus snapshot restore` of older snapshots until `incus-pbs cleanup --release-bookmarks`", false: ""}[mode == "always"])
		}
	}
	lockDir := filepath.Dir(e.Cfg.Backup.LockFile)
	if st, err := os.Stat(lockDir); err != nil || !st.IsDir() {
		add("lock file", CheckFail, "directory %s does not exist", lockDir)
	} else {
		add("lock file", CheckOK, "%s", e.Cfg.Backup.LockFile)
	}

	if v == "" { // incus unreachable: discovery checks are pointless
		return out
	}

	// Discovery and storage.
	plan, err := Discover(e.Incus, e.Cfg, Selection{})
	if err != nil {
		add("discovery", CheckFail, "%v", err)
		return out
	}
	add("discovery", CheckOK, "%s", plan.Summary())
	for _, t := range plan.Problems() {
		add("target "+t.ID(), CheckFail, "%s", t.Problem)
	}
	own, off := 0, 0
	for _, t := range plan.Targets {
		switch t.ScheduleSource {
		case "invalid":
			add("schedule "+t.ID(), CheckFail, "invalid %s; backup.schedule %q applies until it is fixed", incus.ScheduleKey, e.Cfg.Backup.Schedule)
		case "instance":
			own++
			if strings.EqualFold(t.Schedule, "off") {
				off++
			}
		}
	}
	add("schedules", CheckOK, "backup.schedule %q; %d instance(s) with their own %s (%d off)", e.Cfg.Backup.Schedule, own, incus.ScheduleKey, off)
	for _, w := range plan.Warnings {
		if strings.Contains(w, incus.ScheduleKey) {
			continue // reported as a schedule failure above
		}
		add("warning", CheckWarn, "%s", w)
	}
	pools := map[string]incus.Pool{}
	for _, t := range plan.Targets {
		if t.Problem == "" {
			pools[t.Pool.Name] = t.Pool
		}
	}
	for _, p := range pools {
		zpool := strings.SplitN(p.Dataset, "/", 2)[0]
		res, err := e.X.Run(ctx, execx.Cmd{Name: "zpool", Args: []string{"list", "-H", "-o", "name,health,free", zpool}})
		switch {
		case err != nil:
			add("zfs pool "+zpool, CheckFail, "%v", err)
		case !strings.Contains(res.Stdout, "ONLINE"):
			add("zfs pool "+zpool, CheckFail, "unhealthy: %s", strings.TrimSpace(res.Stdout))
		default:
			f := strings.Fields(res.Stdout)
			add("zfs pool "+zpool, CheckOK, "ONLINE, %s free", f[len(f)-1])
		}
		z := e.ZFS(p)
		if err := z.EnsureStaging(ctx); err != nil {
			add("staging dataset "+z.StagingDataset, CheckFail, "%v", err)
		} else if left, err := z.ListStaging(ctx); err != nil {
			add("staging dataset "+z.StagingDataset, CheckFail, "%v", err)
		} else if len(left) > 0 {
			add("staging dataset "+z.StagingDataset, CheckFail, "leftover clones %v; run `incus-pbs cleanup`", left)
		} else {
			add("staging dataset "+z.StagingDataset, CheckOK, "exists and is empty")
		}
	}
	// Orphaned temporary snapshots.
	var orphans []string
	for _, t := range plan.Targets {
		var snaps []string
		if t.Kind == KindVolume && t.Volume != nil {
			snaps, _ = e.Incus.ListVolumeSnapshots(*t.Volume)
		} else if t.Kind != KindVolume {
			snaps, _ = e.Incus.ListSnapshots(t.Project, t.Name)
		}
		for _, s := range snaps {
			if incus.IsOwnSnapshot(s) {
				orphans = append(orphans, t.ID()+"/"+s)
			}
		}
	}
	if len(orphans) > 0 {
		add("temporary snapshots", CheckWarn, "orphaned %v (expire automatically; `incus-pbs cleanup` removes them now)", orphans)
	} else {
		add("temporary snapshots", CheckOK, "no orphaned pbs-backup-* snapshots")
	}
	return out
}

// CheckFailed reports whether any check failed.
func CheckFailed(rs []CheckResult) bool {
	for _, r := range rs {
		if r.Status == CheckFail {
			return true
		}
	}
	return false
}

func pbsHint(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "permission check failed") || strings.Contains(msg, "401"):
		return "authentication/permission failed (token secret, ACL on the namespace?): " + firstLine(msg)
	case strings.Contains(msg, "fingerprint"):
		return "TLS fingerprint mismatch or missing (set pbs.fingerprint): " + firstLine(msg)
	case strings.Contains(msg, "namespace"):
		return "namespace problem (does it exist?): " + firstLine(msg)
	}
	return tail(msg, 400)
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func freeBytes(path string) uint64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0
	}
	return uint64(st.Bavail) * uint64(st.Bsize)
}

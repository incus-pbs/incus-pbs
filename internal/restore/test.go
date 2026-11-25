package restore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	pathpkg "path"
	"strings"
	"time"
)

// TestOptions controls a restore test.
type TestOptions struct {
	Snapshot      string
	Instance      string
	SourceProject string
	TargetProject string
	Pool          string
	NewName       string // default: restoretest-<instance>
	Files         []string
	Contains      string // optional; only with exactly one file
	Timeout       time.Duration
	Keep          bool // keep the (stopped) test instance
	// PollInterval between file checks while the instance boots (default 2s).
	PollInterval time.Duration
}

// FileCheck is the outcome of one file check.
type FileCheck struct {
	Path     string `json:"path"`
	Resolved string `json:"resolved,omitempty"` // final path after following symlinks
	OK       bool   `json:"ok"`
	Size     int64  `json:"size"`
	Error    string `json:"error,omitempty"`
}

// TestResult describes a restore test.
type TestResult struct {
	OK          bool        `json:"ok"`
	Restore     *Result     `json:"restore,omitempty"`
	Started     bool        `json:"started"`
	BootSeconds float64     `json:"boot_seconds,omitempty"`
	Files       []FileCheck `json:"files"`
	Deleted     bool        `json:"deleted"`
	Seconds     float64     `json:"seconds"`
}

const (
	maxNameLen      = 63
	maxContainsRead = 16 << 20
	cleanupTimeout  = 2 * time.Minute
	maxSymlinkHops  = 8
)

// TestName is the default name of a restore test instance.
func TestName(instance string) string {
	n := "restoretest-" + instance
	if len(n) > maxNameLen {
		n = strings.TrimRight(n[:maxNameLen], "-")
	}
	return n
}

// TestInstance restores an instance as an isolated throwaway copy, starts it,
// checks that the given files exist (and optionally contain a string), then
// stops and deletes it again.
func (r *Restorer) TestInstance(ctx context.Context, o TestOptions) (res *TestResult, err error) {
	start := r.now()
	if len(o.Files) == 0 {
		return nil, errors.New("at least one --file is required")
	}
	for _, f := range o.Files {
		if !strings.HasPrefix(f, "/") {
			return nil, fmt.Errorf("--file %q must be an absolute path", f)
		}
	}
	if o.Contains != "" && len(o.Files) != 1 {
		return nil, errors.New("--contains needs exactly one --file")
	}
	if o.Timeout <= 0 {
		o.Timeout = 5 * time.Minute
	}
	if o.PollInterval <= 0 {
		o.PollInterval = 2 * time.Second
	}
	name := or(o.NewName, TestName(o.Instance))
	res = &TestResult{Files: []FileCheck{}}

	rr, err := r.RestoreInstance(ctx, InstanceOptions{
		Snapshot: o.Snapshot, Instance: o.Instance, SourceProject: o.SourceProject,
		TargetProject: o.TargetProject, NewName: name, Pool: o.Pool, Isolate: true,
	})
	res.Restore = rr
	if rr == nil || !rr.Imported {
		if err == nil {
			err = errors.New("instance was not imported")
		}
		return r.testDone(res, start, err)
	}
	log := r.Log.With("source", rr.Source, "target", rr.Project+"/"+rr.Name, "snapshot", rr.Snapshot)

	// Clean up even when ctx was cancelled (SIGTERM from systemd).
	startAttempted := false
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		var cerr error
		if startAttempted {
			// Also after a failed or timed-out start: the instance may be
			// running anyway, and a running instance cannot be deleted.
			// Stopping a stopped instance is a no-op.
			cerr = r.Incus.StopInstance(cctx, rr.Project, rr.Name, true)
		}
		if cerr == nil && !o.Keep {
			if cerr = r.Incus.DeleteInstance(cctx, rr.Project, rr.Name); cerr == nil {
				res.Deleted = true
			}
		}
		if cerr != nil {
			log.Error("restore_test_cleanup_failed", "event", "restore_test_cleanup_failed", "error", cerr)
			err = errors.Join(err, fmt.Errorf("cleanup of %s/%s: %w (remove it manually)", rr.Project, rr.Name, cerr))
			res.OK = false
		}
	}()

	tctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	bootStart := r.now()
	startAttempted = true
	if err := r.Incus.StartInstance(tctx, rr.Project, rr.Name); err != nil {
		return r.testDone(res, start, fmt.Errorf("starting test instance: %w", err))
	}
	res.Started = true

	var failed []string
	for i, path := range o.Files {
		fc := r.checkFile(tctx, rr.Project, rr.Name, path, o.Contains, o.PollInterval)
		if i == 0 {
			res.BootSeconds = r.now().Sub(bootStart).Seconds()
		}
		res.Files = append(res.Files, fc)
		if !fc.OK {
			failed = append(failed, fc.Path+": "+fc.Error)
		}
	}
	if len(failed) > 0 {
		err = fmt.Errorf("file checks failed:\n  - %s", strings.Join(failed, "\n  - "))
	}
	return r.testDone(res, start, err)
}

func (r *Restorer) testDone(res *TestResult, start time.Time, err error) (*TestResult, error) {
	res.OK = err == nil
	res.Seconds = r.now().Sub(start).Seconds()
	if err != nil {
		r.Log.Error("restore_test_failed", "event", "restore_test_failed", "error", err)
		return res, err
	}
	r.Log.Info("restore_test_ok", "event", "restore_test_ok", "files", len(res.Files), "boot_seconds", res.BootSeconds, "seconds", res.Seconds)
	return res, nil
}

// checkFile polls until the file API answers (a VM's agent needs time to come
// up) or ctx expires. A missing or wrong file fails immediately.
func (r *Restorer) checkFile(ctx context.Context, project, name, path, contains string, every time.Duration) FileCheck {
	fc := FileCheck{Path: path}
	cur, hops := path, 0
	for {
		rc, fi, err := r.Incus.InstanceFile(project, name, cur)
		if err == nil && fi.Type == "symlink" {
			// Follow links inside the instance (e.g. Debian's /etc/os-release
			// -> ../usr/lib/os-release); relative targets resolve against the
			// link's directory.
			if hops++; hops > maxSymlinkHops || fi.Target == "" {
				fc.Error = fmt.Sprintf("too many or empty symlinks (at %s)", cur)
				return fc
			}
			if strings.HasPrefix(fi.Target, "/") {
				cur = pathpkg.Clean(fi.Target)
			} else {
				cur = pathpkg.Join(pathpkg.Dir(cur), fi.Target)
			}
			fc.Resolved = cur
			continue
		}
		if err == nil {
			if fi.Type != "file" {
				fc.Error = "not a regular file (" + fi.Type + ")"
				return fc
			}
			b, rerr := io.ReadAll(io.LimitReader(rc, maxContainsRead))
			n, _ := io.Copy(io.Discard, rc)
			rc.Close()
			fc.Size = int64(len(b)) + n
			switch {
			case rerr != nil:
				fc.Error = "reading: " + rerr.Error()
			case contains != "" && !bytes.Contains(b, []byte(contains)):
				fc.Error = fmt.Sprintf("does not contain %q", contains)
			default:
				fc.OK, fc.Error = true, "" // drop errors from earlier polls (agent not up yet)
			}
			return fc
		}
		fc.Error = err.Error()
		if errors.Is(err, fs.ErrNotExist) {
			fc.Error = "does not exist"
			return fc
		}
		select {
		case <-ctx.Done():
			fc.Error = fmt.Sprintf("%s (gave up: %v)", fc.Error, ctx.Err())
			return fc
		case <-time.After(every):
		}
	}
}

// removeStaleTestCopy deletes a restore-test instance left behind by an
// earlier drill that was killed before its cleanup ran.
func (r *Restorer) removeStaleTestCopy(ctx context.Context, project, name, marker string) error {
	r.Log.Warn("restore_test_stale_copy", "event", "restore_test_stale_copy", "instance", project+"/"+name, "created", marker)
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	if err := r.Incus.StopInstance(cctx, project, name, true); err != nil {
		return fmt.Errorf("stopping stale test copy %s/%s: %w", project, name, err)
	}
	if err := r.Incus.DeleteInstance(cctx, project, name); err != nil {
		return fmt.Errorf("deleting stale test copy %s/%s: %w", project, name, err)
	}
	return nil
}

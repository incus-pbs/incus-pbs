package backup

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/incus-pbs/incus-pbs/internal/config"
	"github.com/incus-pbs/incus-pbs/internal/execx"
)

// HookResult records one hook execution.
type HookResult struct {
	Phase    string  `json:"phase"`
	Name     string  `json:"name"`
	Instance string  `json:"instance,omitempty"`
	Status   string  `json:"status"` // ok | failed | skipped
	ExitCode int     `json:"exit_code"`
	Seconds  float64 `json:"seconds"`
	Error    string  `json:"error,omitempty"`
}

// runHooks executes hooks of one phase in order. It returns an error for the
// first failing hook with on_failure=abort; later hooks of the phase are not run then.
// instanceStatus maps "project/name" of every instance in this run to its
// Incus status; hooks for stopped instances and for instances outside this
// run (e.g. not due in a scheduled run) are skipped.
func (e *Engine) runHooks(ctx context.Context, phase string, hooks []config.Hook, env map[string]string,
	instanceStatus map[string]string, rep *Report, log *slog.Logger) error {
	for _, h := range hooks {
		res := HookResult{Phase: phase, Name: h.Name, Instance: h.Instance, Status: "ok"}
		hctx, cancel := context.WithTimeout(ctx, h.Timeout.D())
		t0 := e.now()
		var err error
		var out string
		if h.Instance != "" {
			project, name := e.splitInstance(h.Instance)
			if st, known := instanceStatus[project+"/"+name]; !known || st != "Running" {
				res.Status = "skipped"
				res.Error = "instance is " + st
				if !known {
					res.Error = "instance is not part of this run"
				}
				log.Info("hook_skipped", "event", "hook_skipped", "phase", phase, "hook", h.Name, "instance", h.Instance, "reason", res.Error)
				cancel()
				rep.Hooks = append(rep.Hooks, res)
				continue
			}
			xr, xerr := e.Incus.Exec(hctx, project, name, h.Command, env)
			res.ExitCode, out, err = xr.ExitCode, xr.Stdout+xr.Stderr, xerr
			if err == nil && xr.ExitCode != 0 {
				err = fmt.Errorf("exited with code %d", xr.ExitCode)
			}
		} else {
			xr, xerr := e.X.Run(hctx, execx.Cmd{Name: h.Command[0], Args: h.Command[1:], Env: envList(env)})
			res.ExitCode, out, err = xr.ExitCode, xr.Stdout+xr.Stderr, xerr
		}
		cancel()
		res.Seconds = round(e.now().Sub(t0).Seconds())
		if err != nil {
			res.Status, res.Error = "failed", err.Error()
			log.Error("hook_failed", "event", "hook_failed", "phase", phase, "hook", h.Name, "instance", h.Instance,
				"exit_code", res.ExitCode, "error", err, "on_failure", h.OnFailure, "output_tail", tail(out, 1000))
			rep.Hooks = append(rep.Hooks, res)
			if h.OnFailure == "abort" {
				return fmt.Errorf("%s hook %q failed: %w", phase, h.Name, err)
			}
			continue
		}
		log.Info("hook_completed", "event", "hook_completed", "phase", phase, "hook", h.Name, "instance", h.Instance,
			"seconds", res.Seconds, "output_tail", tail(out, 500))
		rep.Hooks = append(rep.Hooks, res)
	}
	return nil
}

func (e *Engine) splitInstance(s string) (project, name string) {
	if p, n, ok := strings.Cut(s, "/"); ok {
		return p, n
	}
	return e.Cfg.Incus.Projects[0], s
}

func envList(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out
}

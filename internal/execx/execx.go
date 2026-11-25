// Package execx runs external programs safely: arguments are always passed
// as a slice (never through a shell), output is captured, and cancellation
// sends SIGINT first so tools like proxmox-backup-client can abort cleanly.
package execx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// Result is the outcome of a finished command.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
	Duration time.Duration
}

// Cmd describes a command to run.
type Cmd struct {
	Name string
	Args []string
	// Env is appended to the current process environment.
	Env []string
}

// Runner abstracts command execution so it can be faked in tests.
type Runner interface {
	Run(ctx context.Context, c Cmd) (Result, error)
}

// ExitError is returned when the command ran but exited non-zero.
type ExitError struct {
	Name     string
	ExitCode int
	Stderr   string
}

func (e *ExitError) Error() string {
	msg := e.Stderr
	if len(msg) > 2000 {
		msg = "..." + msg[len(msg)-2000:]
	}
	return fmt.Sprintf("%s exited with code %d: %s", e.Name, e.ExitCode, msg)
}

// OS runs commands on the local system.
type OS struct {
	// GracePeriod is how long to wait after SIGINT before SIGKILL.
	GracePeriod time.Duration
}

// Run executes c. A non-zero exit yields *ExitError; context cancellation
// yields an error wrapping ctx.Err().
func (o OS) Run(ctx context.Context, c Cmd) (Result, error) {
	grace := o.GracePeriod
	if grace == 0 {
		grace = 30 * time.Second
	}
	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	cmd.Env = append(os.Environ(), c.Env...)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGINT) }
	cmd.WaitDelay = grace
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	start := time.Now()
	err := cmd.Run()
	res := Result{Stdout: stdout.String(), Stderr: stderr.String(), Duration: time.Since(start)}
	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
	}
	if ctx.Err() != nil {
		return res, fmt.Errorf("%s aborted: %w", c.Name, ctx.Err())
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return res, &ExitError{Name: c.Name, ExitCode: res.ExitCode, Stderr: res.Stderr}
	}
	if err != nil {
		return res, fmt.Errorf("running %s: %w", c.Name, err)
	}
	return res, nil
}

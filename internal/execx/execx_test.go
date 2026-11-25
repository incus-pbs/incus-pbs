package execx

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRunCapturesOutput(t *testing.T) {
	res, err := OS{}.Run(context.Background(), Cmd{Name: "sh", Args: []string{"-c", `echo out; echo err >&2; echo "$FOO"`}, Env: []string{"FOO=bar"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != "out\nbar\n" || res.Stderr != "err\n" || res.ExitCode != 0 {
		t.Fatalf("%+v", res)
	}
}

func TestRunArgumentsAreNotShellInterpreted(t *testing.T) {
	res, err := OS{}.Run(context.Background(), Cmd{Name: "echo", Args: []string{"$(touch /tmp/pwned); `id`"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(res.Stdout) != "$(touch /tmp/pwned); `id`" {
		t.Fatalf("argument was interpreted: %q", res.Stdout)
	}
}

func TestRunExitCode(t *testing.T) {
	res, err := OS{}.Run(context.Background(), Cmd{Name: "sh", Args: []string{"-c", "echo boom >&2; exit 7"}})
	var ee *ExitError
	if !errors.As(err, &ee) || ee.ExitCode != 7 || res.ExitCode != 7 || !strings.Contains(ee.Error(), "boom") {
		t.Fatalf("err=%v res=%+v", err, res)
	}
}

func TestRunTimeoutSendsSIGINT(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	// The script traps SIGINT and exits cleanly, proving SIGINT (not SIGKILL) arrived first.
	start := time.Now()
	res, err := OS{GracePeriod: 5 * time.Second}.Run(ctx, Cmd{Name: "sh", Args: []string{"-c", `trap 'echo got-int; kill $!; exit 130' INT; sleep 10 & wait`}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline error, got %v", err)
	}
	if !strings.Contains(res.Stdout, "got-int") {
		t.Fatalf("SIGINT not delivered: %+v", res)
	}
	if time.Since(start) > 4*time.Second {
		t.Fatal("did not stop promptly")
	}
}

func TestRunKillsAfterGracePeriod(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := OS{GracePeriod: 300 * time.Millisecond}.Run(ctx, Cmd{Name: "sh", Args: []string{"-c", `trap '' INT; sleep 10`}})
	if err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("err=%v elapsed=%s", err, time.Since(start))
	}
}

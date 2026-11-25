package backup

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// zstream exiting early must not leave zfs send blocked on a full pipe.
func TestZFSDiffDoesNotHangWhenDumpExitsEarly(t *testing.T) {
	bin := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("zfs", "exec yes incus-pbs-test-stream") // endless stream, far larger than a pipe buffer
	write("zstream", "exit 3")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	out, wait, err := ZFSDiff(context.Background(), "p/v@a", "p/v@b")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, _ = io.Copy(io.Discard, out)
		done <- wait()
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a failed zstream must be reported")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ZFSDiff wait hangs after zstream exited")
	}
}

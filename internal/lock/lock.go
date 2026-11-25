// Package lock provides an exclusive, non-blocking run lock (flock).
// The kernel releases the lock when the process dies, so stale lock files
// never block future runs.
package lock

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// ErrLocked is returned when another process holds the lock.
var ErrLocked = errors.New("another incus-pbs run is in progress")

// Lock is a held lock.
type Lock struct{ f *os.File }

// Acquire takes the lock at path without waiting.
func Acquire(path string) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		b := make([]byte, 32)
		n, _ := f.ReadAt(b, 0)
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			if pid := strings.TrimSpace(string(b[:n])); pid != "" {
				return nil, fmt.Errorf("%w (pid %s, lock %s)", ErrLocked, pid, path)
			}
			return nil, fmt.Errorf("%w (lock %s)", ErrLocked, path)
		}
		return nil, fmt.Errorf("locking %s: %w", path, err)
	}
	_ = f.Truncate(0)
	_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	return &Lock{f: f}, nil
}

// Release drops the lock.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	_ = l.f.Truncate(0)
	err := l.f.Close()
	l.f = nil
	return err
}

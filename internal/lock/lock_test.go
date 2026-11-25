package lock

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestExclusive(t *testing.T) {
	p := filepath.Join(t.TempDir(), "run.lock")
	l1, err := Acquire(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(p); !errors.Is(err, ErrLocked) {
		t.Fatalf("second acquire must fail with ErrLocked, got %v", err)
	}
	if err := l1.Release(); err != nil {
		t.Fatal(err)
	}
	l2, err := Acquire(p)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	l2.Release()
}

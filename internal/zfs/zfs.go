// Package zfs wraps the zfs/mount CLI for read-only exposure of snapshots.
//
// Safety: every destructive operation (destroy, unmount) is restricted to
// the configured staging dataset / mount root. Datasets managed by Incus are
// only ever read (snapshot mount, clone source), never modified.
package zfs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/incus-pbs/incus-pbs/internal/execx"
)

// ZFS exposes snapshots under a staging dataset and mount root.
type ZFS struct {
	X execx.Runner
	// StagingDataset is the parent for temporary clones, e.g. "tank/incus-pbs-staging".
	StagingDataset string
	// MountRoot is the parent directory for read-only snapshot mounts.
	MountRoot string
	// DevRoot is where zvol device nodes appear (default /dev/zvol).
	DevRoot string
}

// ErrOutsideStaging is returned when an operation targets something outside staging.
type ErrOutsideStaging struct{ Target string }

func (e ErrOutsideStaging) Error() string {
	return fmt.Sprintf("refusing to touch %q: outside staging area", e.Target)
}

// InStagingDataset reports whether ds is a strict child of the staging dataset.
func (z *ZFS) InStagingDataset(ds string) bool {
	if z.StagingDataset == "" || strings.ContainsAny(ds, "@# \t\n") || strings.Contains(ds, "..") {
		return false
	}
	return strings.HasPrefix(ds, z.StagingDataset+"/") && len(ds) > len(z.StagingDataset)+1
}

// InMountRoot reports whether dir is a strict child of the mount root.
func (z *ZFS) InMountRoot(dir string) bool {
	if z.MountRoot == "" || !filepath.IsAbs(dir) {
		return false
	}
	root := filepath.Clean(z.MountRoot)
	clean := filepath.Clean(dir)
	return strings.HasPrefix(clean, root+string(filepath.Separator))
}

func (z *ZFS) run(ctx context.Context, name string, args ...string) (execx.Result, error) {
	return z.X.Run(ctx, execx.Cmd{Name: name, Args: args})
}

// EnsureStaging creates the staging parent dataset (not mounted) if missing.
func (z *ZFS) EnsureStaging(ctx context.Context) error {
	if _, err := z.run(ctx, "zfs", "list", "-H", "-o", "name", z.StagingDataset); err == nil {
		return nil
	}
	_, err := z.run(ctx, "zfs", "create", "-o", "canmount=off", "-o", "mountpoint=none", "-o", "volmode=dev", z.StagingDataset)
	return err
}

// ListStaging returns child datasets of the staging dataset.
func (z *ZFS) ListStaging(ctx context.Context) ([]string, error) {
	res, err := z.run(ctx, "zfs", "list", "-H", "-o", "name", "-r", z.StagingDataset)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(res.Stdout), "\n") {
		if l != "" && l != z.StagingDataset {
			out = append(out, l)
		}
	}
	return out, nil
}

// Clone creates a read-only clone of snapshot snap at target (must be in staging).
// For zvols it waits until the /dev/zvol device node exists.
func (z *ZFS) Clone(ctx context.Context, snap, target string) error {
	if !z.InStagingDataset(target) {
		return ErrOutsideStaging{target}
	}
	if !strings.Contains(snap, "@") {
		return fmt.Errorf("clone source %q is not a snapshot", snap)
	}
	// volmode=dev exposes a zvol clone as one raw device without partition
	// nodes, so udev, blkid and LVM on the host never scan or activate the
	// guest's partitions and volume groups (which would keep it busy).
	_, err := z.run(ctx, "zfs", "clone", "-o", "readonly=on", "-o", "volmode=dev", snap, target)
	return err
}

// WaitDevice waits for <DevRoot>/<ds> to appear.
func (z *ZFS) WaitDevice(ctx context.Context, ds string, timeout time.Duration) (string, error) {
	root := z.DevRoot
	if root == "" {
		root = "/dev/zvol"
	}
	dev := root + "/" + ds
	_, _ = z.run(ctx, "udevadm", "settle", "--timeout=30")
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(dev); err == nil {
			return dev, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("zvol device %s did not appear within %s", dev, timeout)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// Destroy destroys a staging dataset (never recursive, never outside staging).
func (z *ZFS) Destroy(ctx context.Context, ds string) error {
	if !z.InStagingDataset(ds) {
		return ErrOutsideStaging{ds}
	}
	_, err := z.run(ctx, "zfs", "destroy", ds)
	return err
}

// MountSnapshot mounts a ZFS snapshot read-only at dir (must be under MountRoot).
func (z *ZFS) MountSnapshot(ctx context.Context, snap, dir string) error {
	if !z.InMountRoot(dir) {
		return ErrOutsideStaging{dir}
	}
	if !strings.Contains(snap, "@") {
		return fmt.Errorf("%q is not a snapshot", snap)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if _, err := z.run(ctx, "mount", "-t", "zfs", "-o", "ro", snap, dir); err != nil {
		_ = os.Remove(dir) // only removes the directory if it is empty
		return err
	}
	return nil
}

// Unmount unmounts dir (must be under MountRoot) and removes the empty directory.
func (z *ZFS) Unmount(ctx context.Context, dir string) error {
	if !z.InMountRoot(dir) {
		return ErrOutsideStaging{dir}
	}
	if _, err := z.run(ctx, "umount", dir); err != nil {
		return err
	}
	if err := os.Remove(dir); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// MountsUnder parses /proc/self/mountinfo and returns mount points under MountRoot.
func (z *ZFS) MountsUnder(mountinfo string) []string {
	var out []string
	for _, l := range strings.Split(mountinfo, "\n") {
		f := strings.Fields(l)
		if len(f) > 4 && z.InMountRoot(f[4]) {
			out = append(out, f[4])
		}
	}
	return out
}

// BookmarkPrefix marks bookmarks created by incus-pbs; no other bookmark is ever touched.
const BookmarkPrefix = "incus-pbs-"

// IsOwnBookmark reports whether name ("pool/ds#incus-pbs-...") was created by incus-pbs.
func IsOwnBookmark(name string) bool {
	_, bm, ok := strings.Cut(name, "#")
	return ok && strings.HasPrefix(bm, BookmarkPrefix) && !strings.ContainsAny(name, "@ \t\n")
}

// Bookmark creates bookmark (ds#incus-pbs-...) from snapshot (ds@...).
func (z *ZFS) Bookmark(ctx context.Context, snapshot, bookmark string) error {
	if !IsOwnBookmark(bookmark) || !strings.Contains(snapshot, "@") {
		return fmt.Errorf("refusing to create bookmark %q from %q", bookmark, snapshot)
	}
	_, err := z.run(ctx, "zfs", "bookmark", snapshot, bookmark)
	return err
}

// DestroyBookmark destroys one of our bookmarks (refuses all others).
func (z *ZFS) DestroyBookmark(ctx context.Context, bookmark string) error {
	if !IsOwnBookmark(bookmark) {
		return ErrOutsideStaging{bookmark}
	}
	_, err := z.run(ctx, "zfs", "destroy", bookmark)
	return err
}

// Bookmarks lists our bookmarks on dataset ds.
func (z *ZFS) Bookmarks(ctx context.Context, ds string) ([]string, error) {
	res, err := z.run(ctx, "zfs", "list", "-H", "-t", "bookmark", "-o", "name", "-d", "1", ds)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(res.Stdout), "\n") {
		if IsOwnBookmark(l) && strings.HasPrefix(l, ds+"#") {
			out = append(out, l)
		}
	}
	return out, nil
}

// Snapshots lists snapshot names (part after @) of dataset ds.
func (z *ZFS) Snapshots(ctx context.Context, ds string) ([]string, error) {
	res, err := z.run(ctx, "zfs", "list", "-H", "-t", "snapshot", "-o", "name", "-d", "1", ds)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(res.Stdout), "\n") {
		if _, snap, ok := strings.Cut(l, "@"); ok {
			out = append(out, snap)
		}
	}
	return out, nil
}

// Prop returns a single property value (-Hp).
func (z *ZFS) Prop(ctx context.Context, name, prop string) (string, error) {
	res, err := z.run(ctx, "zfs", "get", "-Hp", "-o", "value", prop, name)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(res.Stdout), nil
}

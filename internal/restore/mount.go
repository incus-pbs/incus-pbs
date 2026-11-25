package restore

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/incus-pbs/incus-pbs/internal/backup"
	"github.com/incus-pbs/incus-pbs/internal/execx"
)

// MountOptions selects what to mount.
type MountOptions struct {
	Snapshot  string
	Name      string
	Project   string // project at backup time ("" = any)
	Dir       string // empty directory
	Partition int    // VM/block images: partition number (0 = largest with a filesystem)
}

// MountResult describes an instant-access mount.
type MountResult struct {
	Snapshot   string   `json:"snapshot"`
	Archive    string   `json:"archive"`
	Dir        string   `json:"dir,omitempty"`
	Device     string   `json:"device,omitempty"`     // /dev/loopN for images
	Mounted    string   `json:"mounted,omitempty"`    // partition mounted at Dir
	Partitions []string `json:"partitions,omitempty"` // name size fstype
	Hint       string   `json:"hint"`
}

var loopRE = regexp.MustCompile(`mapped on (/dev/loop\d+)`)

// Mount gives read-only access to a backed-up instance or volume without a
// full restore: pxar archives are FUSE-mounted, disk images are mapped to a
// loop device (data is fetched on demand) and a partition is mounted read-only.
func (r *Restorer) Mount(ctx context.Context, o MountOptions) (*MountResult, error) {
	if !filepath.IsAbs(o.Dir) {
		return nil, fmt.Errorf("--dir must be an absolute path")
	}
	if err := os.MkdirAll(o.Dir, 0o700); err != nil {
		return nil, err
	}
	if entries, err := os.ReadDir(o.Dir); err != nil || len(entries) > 0 {
		return nil, fmt.Errorf("%s must be an empty directory", o.Dir)
	}
	loc, err := r.locate(ctx, o.Snapshot, anyKinds, o.Project, "", o.Name)
	if err != nil {
		return nil, err
	}
	t := loc.Target
	archive := t.ArchiveBase + ".pxar"
	if t.Kind == backup.KindVM || t.ContentType == "block" {
		archive = t.ArchiveBase + ".img"
	}
	src := loc.source(archive)
	res := &MountResult{Snapshot: src, Archive: archive}
	x := r.PBC.X
	common := []string{"--ns", r.Cfg.PBS.Namespace}
	if r.Cfg.PBS.EncryptionKeyFile != "" {
		common = append(common, "--keyfile", r.Cfg.PBS.EncryptionKeyFile)
	}
	if r.Cfg.PBS.Namespace == "" {
		common = common[2:]
	}
	if err := r.PBC.Validate(); err != nil {
		return nil, err
	}

	if strings.HasSuffix(archive, ".pxar") {
		args := append([]string{"mount", src, archive, o.Dir}, common...)
		if _, err := x.Run(ctx, execx.Cmd{Name: r.PBC.Binary(), Args: args, Env: r.PBC.Env()}); err != nil {
			return nil, fmt.Errorf("mounting %s: %w", archive, err)
		}
		res.Dir = o.Dir
		res.Hint = "read-only FUSE mount; release with: incus-pbs restore unmount " + o.Dir
		return res, nil
	}

	out, err := x.Run(ctx, execx.Cmd{Name: r.PBC.Binary(), Args: append([]string{"map", src, archive}, common...), Env: r.PBC.Env()})
	if err != nil {
		return nil, fmt.Errorf("mapping %s: %w", archive, err)
	}
	mm := loopRE.FindStringSubmatch(out.Stdout + out.Stderr)
	if mm == nil {
		return nil, fmt.Errorf("could not find the loop device in client output")
	}
	res.Device = mm[1]
	unmap := func(cause error) (*MountResult, error) {
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		if _, uerr := x.Run(uctx, execx.Cmd{Name: r.PBC.Binary(), Args: []string{"unmap", res.Device}, Env: r.PBC.Env()}); uerr != nil {
			return res, fmt.Errorf("%w (and releasing %s failed: %v; run: incus-pbs restore unmount %s)", cause, res.Device, uerr, res.Device)
		}
		res.Device = ""
		return res, cause
	}
	_, _ = x.Run(ctx, execx.Cmd{Name: "udevadm", Args: []string{"settle"}})
	parts, err := listPartitions(ctx, x, res.Device)
	if err != nil {
		return unmap(fmt.Errorf("listing partitions of %s: %w", res.Device, err))
	}
	for _, p := range parts {
		res.Partitions = append(res.Partitions, fmt.Sprintf("%s %d %s", p.dev, p.size, p.fstype))
	}
	part := pickPartition(parts, res.Device, o.Partition)
	if part == nil && o.Partition > 0 {
		return unmap(fmt.Errorf("partition %d not found on %s (partitions: %s)", o.Partition, archive, strings.Join(res.Partitions, "; ")))
	}
	if part == nil {
		res.Hint = "image mapped; no mountable filesystem chosen. Inspect the partitions, then: mount -o ro <partition> <dir>; release with: incus-pbs restore unmount " + res.Device
		return res, nil
	}
	opts := "ro"
	switch part.fstype {
	case "ext4", "ext3":
		opts = "ro,noload" // crash-consistent image: never replay the journal
	case "xfs":
		opts = "ro,norecovery,nouuid" // nouuid: the original may be mounted with the same UUID
	case "btrfs":
		opts = "ro,rescue=nologreplay"
	}
	if _, err := x.Run(ctx, execx.Cmd{Name: "mount", Args: []string{"-o", opts, part.dev, o.Dir}}); err != nil {
		res.Hint = "image mapped but mounting failed; release with: incus-pbs restore unmount " + res.Device
		return res, fmt.Errorf("mounting %s: %w", part.dev, err)
	}
	res.Dir, res.Mounted = o.Dir, part.dev
	res.Hint = "read-only; data is fetched from PBS on demand. Release with: incus-pbs restore unmount " + o.Dir
	return res, nil
}

type partition struct {
	dev    string
	size   int64
	fstype string
	num    int
}

var partNumRE = regexp.MustCompile(`^(/dev/loop\d+)p(\d+)$`)

func listPartitions(ctx context.Context, x execx.Runner, dev string) ([]partition, error) {
	out, err := x.Run(ctx, execx.Cmd{Name: "lsblk", Args: []string{"-nrbp", "-o", "NAME,SIZE,FSTYPE", dev}})
	if err != nil {
		return nil, err
	}
	var parts []partition
	for _, l := range strings.Split(strings.TrimSpace(out.Stdout), "\n") {
		f := strings.Fields(l)
		if len(f) < 2 {
			continue
		}
		size, _ := strconv.ParseInt(f[1], 10, 64)
		p := partition{dev: f[0], size: size}
		if len(f) > 2 {
			p.fstype = f[2]
		}
		if m := partNumRE.FindStringSubmatch(p.dev); m != nil && m[1] == dev {
			p.num, _ = strconv.Atoi(m[2])
		}
		parts = append(parts, p)
	}
	return parts, nil
}

func pickPartition(parts []partition, dev string, want int) *partition {
	var cands []partition
	for _, p := range parts {
		if want > 0 && p.num == want {
			return &p
		}
		if p.fstype != "" && p.fstype != "swap" && p.fstype != "vfat" && !strings.Contains(p.fstype, "LVM") {
			cands = append(cands, p)
		}
	}
	if want > 0 || len(cands) == 0 {
		return nil
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].size > cands[j].size })
	return &cands[0]
}

// Unmount releases a mount made by Mount: a FUSE mount dir, a mounted
// partition dir (also unmaps its loop device) or a mapped /dev/loopN.
func (r *Restorer) Unmount(ctx context.Context, target string) error {
	x := r.PBC.X
	if strings.HasPrefix(target, "/dev/loop") {
		_, err := x.Run(ctx, execx.Cmd{Name: r.PBC.Binary(), Args: []string{"unmap", target}, Env: r.PBC.Env()})
		return err
	}
	mi, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return err
	}
	dir := filepath.Clean(target)
	var source, fstype string
	for _, l := range strings.Split(string(mi), "\n") {
		f := strings.Fields(l)
		if len(f) < 10 || f[4] != dir {
			continue
		}
		for i := 6; i < len(f)-2; i++ {
			if f[i] == "-" {
				fstype, source = f[i+1], f[i+2]
			}
		}
	}
	switch {
	case fstype == "":
		return fmt.Errorf("%s is not mounted", dir)
	case strings.HasPrefix(fstype, "fuse"):
		_, err = x.Run(ctx, execx.Cmd{Name: "umount", Args: []string{dir}})
		return err
	case strings.HasPrefix(source, "/dev/loop"):
		if _, err := x.Run(ctx, execx.Cmd{Name: "umount", Args: []string{dir}}); err != nil {
			return err
		}
		loop := source
		if m := partNumRE.FindStringSubmatch(source); m != nil {
			loop = m[1]
		}
		_, err = x.Run(ctx, execx.Cmd{Name: r.PBC.Binary(), Args: []string{"unmap", loop}, Env: r.PBC.Env()})
		return err
	}
	return fmt.Errorf("%s (%s on %s) was not mounted by incus-pbs", dir, fstype, source)
}

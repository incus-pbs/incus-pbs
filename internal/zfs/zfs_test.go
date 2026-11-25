package zfs

import (
	"context"
	"errors"
	"testing"

	"github.com/incus-pbs/incus-pbs/internal/execx"
)

type recorder struct{ calls []execx.Cmd }

func (r *recorder) Run(_ context.Context, c execx.Cmd) (execx.Result, error) {
	r.calls = append(r.calls, c)
	return execx.Result{}, nil
}

func TestGuardRefusesOutsideStaging(t *testing.T) {
	r := &recorder{}
	z := &ZFS{X: r, StagingDataset: "tank/incus-pbs-staging", MountRoot: "/var/cache/incus-pbs/mnt"}
	ctx := context.Background()
	var guard ErrOutsideStaging
	for _, ds := range []string{
		"tank", "tank/incus-pbs-staging", "tank/containers/c1", "tank/incus-pbs-staging-evil/x",
		"tank/incus-pbs-staging/x@snap", "tank/incus-pbs-staging/../containers/c1", "",
	} {
		if err := z.Destroy(ctx, ds); !errors.As(err, &guard) {
			t.Errorf("Destroy(%q) not refused: %v", ds, err)
		}
		if err := z.Clone(ctx, "tank/virtual-machines/v1.block@snapshot-x", ds); !errors.As(err, &guard) {
			t.Errorf("Clone(->%q) not refused: %v", ds, err)
		}
	}
	for _, dir := range []string{"/", "/var/cache/incus-pbs/mnt", "/var/cache/incus-pbs/mnt/../../etc", "relative/mnt/x", "/var/cache/incus-pbs/mntx/a"} {
		if err := z.Unmount(ctx, dir); !errors.As(err, &guard) {
			t.Errorf("Unmount(%q) not refused: %v", dir, err)
		}
		if err := z.MountSnapshot(ctx, "tank/containers/c1@snapshot-x", dir); !errors.As(err, &guard) {
			t.Errorf("MountSnapshot(%q) not refused: %v", dir, err)
		}
	}
	if len(r.calls) != 0 {
		t.Fatalf("guarded operations executed commands: %+v", r.calls)
	}
}

func TestGuardAllowsStagingChildren(t *testing.T) {
	r := &recorder{}
	z := &ZFS{X: r, StagingDataset: "tank/incus-pbs-staging", MountRoot: t.TempDir()}
	if err := z.Destroy(context.Background(), "tank/incus-pbs-staging/vm_default_v1"); err != nil {
		t.Fatal(err)
	}
	c := r.calls[0]
	if c.Name != "zfs" || len(c.Args) != 2 || c.Args[0] != "destroy" {
		t.Fatalf("destroy must be non-recursive, got %q", c.Args)
	}
	if err := z.Clone(context.Background(), "tank/virtual-machines/v1.block@snapshot-x", "tank/incus-pbs-staging/vm"); err != nil {
		t.Fatal(err)
	}
	if got := r.calls[1].Args; got[0] != "clone" || got[1] != "-o" || got[2] != "readonly=on" || got[4] != "volmode=dev" {
		t.Fatalf("clone must be read-only, got %q", got)
	}
	if err := z.Clone(context.Background(), "tank/virtual-machines/v1.block", "tank/incus-pbs-staging/vm"); err == nil {
		t.Fatal("clone from non-snapshot must be rejected")
	}
}

func TestMountsUnder(t *testing.T) {
	z := &ZFS{MountRoot: "/var/cache/incus-pbs/mnt"}
	mi := `36 25 0:32 / / rw,relatime - ext4 /dev/sda1 rw
90 36 0:50 / /var/cache/incus-pbs/mnt/ct_default_c1 ro - zfs tank/containers/c1@snapshot-pbs-backup-1 ro
91 36 0:51 / /var/cache/incus-pbs/mntother ro - zfs x ro`
	got := z.MountsUnder(mi)
	if len(got) != 1 || got[0] != "/var/cache/incus-pbs/mnt/ct_default_c1" {
		t.Fatalf("got %q", got)
	}
}

package incus

import (
	"testing"
	"time"
)

func TestSnapshotNaming(t *testing.T) {
	n := NewSnapshotName(time.Date(2026, 11, 24, 2, 30, 0, 0, time.UTC))
	if n != "pbs-backup-20261124-023000" || !IsOwnSnapshot(n) {
		t.Fatalf("got %q", n)
	}
	for _, foreign := range []string{"snap0", "pbs-backup", "pbs-backup-manual", "pbs-backup-20261124-023000-keep", "my-pbs-backup-20261124-023000", "pbs-backup-2026112-023000"} {
		if IsOwnSnapshot(foreign) {
			t.Errorf("%q must not be treated as own snapshot", foreign)
		}
	}
}

func TestDatasets(t *testing.T) {
	cases := []struct {
		in          Instance
		main, block string
	}{
		{Instance{Name: "c1", Project: "default", Type: "container"}, "tank/containers/c1", ""},
		{Instance{Name: "c1", Project: "prod", Type: "container"}, "tank/containers/prod_c1", ""},
		{Instance{Name: "v1", Project: "default", Type: "virtual-machine"}, "tank/virtual-machines/v1", "tank/virtual-machines/v1.block"},
	}
	for _, c := range cases {
		m, b := Datasets("tank", c.in)
		if m != c.main || b != c.block {
			t.Errorf("%+v: got %q %q", c.in, m, b)
		}
	}
	if SnapshotSuffix("pbs-backup-x") != "@snapshot-pbs-backup-x" {
		t.Fatal("suffix")
	}
}

func TestRootPool(t *testing.T) {
	d := map[string]map[string]string{
		"data": {"type": "disk", "path": "/data", "pool": "other"},
		"root": {"type": "disk", "path": "/", "pool": "default"},
		"eth0": {"type": "nic"},
	}
	if RootPool(d) != "default" {
		t.Fatal(RootPool(d))
	}
}

func TestRedact(t *testing.T) {
	in := map[string]string{
		"core.https_address": ":8443", "user.db_password": "x", "loki.auth.password": "y",
		"cloud-init.vendor-data": "ok", "core.trust_password": "z", "volatile.uuid": "u", "user.api_token": "t",
	}
	out := Redact(in)
	for _, k := range []string{"user.db_password", "loki.auth.password", "core.trust_password", "user.api_token"} {
		if out[k] != "<redacted>" {
			t.Errorf("%s not redacted", k)
		}
	}
	for _, k := range []string{"core.https_address", "volatile.uuid"} {
		if out[k] != in[k] {
			t.Errorf("%s must be kept", k)
		}
	}
}

func TestVolumeDatasets(t *testing.T) {
	for _, c := range []struct {
		v    Volume
		want string
	}{
		{Volume{Name: "shared", Project: "default", ContentType: "filesystem"}, "tank/custom/default_shared"},
		{Volume{Name: "rawdisk", Project: "default", ContentType: "block"}, "tank/custom/default_rawdisk"},
		{Volume{Name: "data", Project: "prod", ContentType: "filesystem"}, "tank/custom/prod_data"},
		{Volume{Name: "data", ContentType: "filesystem"}, "tank/custom/default_data"},
	} {
		if got := VolumeDatasets("tank", c.v); got != c.want {
			t.Errorf("%+v: got %s want %s", c.v, got, c.want)
		}
	}
}

package restore

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestUntarRejectsSymlinkEscape(t *testing.T) {
	outside := t.TempDir()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: outside, Mode: 0o777})
	body := []byte("pwned")
	_ = tw.WriteHeader(&tar.Header{Name: "link/evil", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))})
	_, _ = tw.Write(body)
	_ = tw.Close()

	if err := untarInto(&buf, t.TempDir()); err == nil {
		t.Fatal("writing through a symlink that leaves the target must fail")
	}
	if _, err := os.Stat(filepath.Join(outside, "evil")); err == nil {
		t.Fatal("file was written outside the target directory")
	}
}

func TestUntarKeepsInternalSymlinks(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: "config/", Typeflag: tar.TypeDir, Mode: 0o500})
	body := []byte("x")
	_ = tw.WriteHeader(&tar.Header{Name: "config/a", Typeflag: tar.TypeReg, Mode: 0o600, Size: 1})
	_, _ = tw.Write(body)
	_ = tw.WriteHeader(&tar.Header{Name: "config/b", Typeflag: tar.TypeSymlink, Linkname: "a"})
	_ = tw.Close()
	dir := t.TempDir()
	if err := untarInto(&buf, dir); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "config", "b")); err != nil || string(b) != "x" {
		t.Fatalf("internal symlink: %q %v", b, err)
	}
	_ = removeAll(dir)
}

func TestStripIdentity(t *testing.T) {
	cfg := map[string]any{"container": map[string]any{
		"config": map[string]any{
			"volatile.eth0.hwaddr":            "00:16:3e:00:00:01",
			"volatile.eth1.last_state.hwaddr": "aa:bb:cc:dd:ee:ff",
			"volatile.uuid":                   "u",
			"volatile.last_state.power":       "RUNNING",
			"limits.cpu":                      "2",
		},
		"devices": map[string]any{
			"eth0": map[string]any{"type": "nic", "network": "br0", "hwaddr": "00:16:3e:12:34:56"},
			"root": map[string]any{"type": "disk", "path": "/", "pool": "default"},
		},
	}}
	got := stripIdentity(cfg)
	want := []string{"devices.eth0.hwaddr", "volatile.eth0.hwaddr", "volatile.uuid"}
	if !slices.Equal(got, want) {
		t.Fatalf("stripped %v, want %v", got, want)
	}
	inst := cfg["container"].(map[string]any)
	c := inst["config"].(map[string]any)
	if c["volatile.eth1.last_state.hwaddr"] == nil {
		t.Fatal("the host NIC's saved MAC must be kept")
	}
	disableAutostart(cfg)
	if c["boot.autostart"] != "false" || c["volatile.last_state.power"] != nil {
		t.Fatalf("autostart: %v", c)
	}
}

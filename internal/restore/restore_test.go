package restore

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/incus-pbs/incus-pbs/internal/backup"
	"github.com/incus-pbs/incus-pbs/internal/config"
	"github.com/incus-pbs/incus-pbs/internal/execx"
	"github.com/incus-pbs/incus-pbs/internal/incus"
	"github.com/incus-pbs/incus-pbs/internal/pbc"
)

const (
	snap    = "host/h1/2026-11-24T02:30:00Z"
	webSnap = "ct/web/2026-11-24T02:29:00Z"
	dbSnap  = "vm/db/2026-11-24T02:29:00Z"
)

var backupYAML = `container:
  name: web
  type: container
  profiles: [default]
  config:
    limits.memory: 2GiB
    volatile.eth0.hwaddr: 10:66:6a:00:00:01
    volatile.uuid: 1111
    volatile.idmap.current: '[]'
    volatile.last_state.idmap: '[]'
  devices:
    data: {type: disk, pool: default, source: webdata, path: /data}
    lan: {type: nic, network: lanbr}
  expanded_config:
    volatile.eth0.hwaddr: 10:66:6a:00:00:01
pool: {name: default, driver: zfs}
snapshots:
  - {name: user-keepme}
`

// fake implements execx.Runner (PBS client), incus.Importer and TarFunc.
type fake struct {
	t             *testing.T
	instances     map[string]bool // project/name
	volumes       map[string]bool // project/pool/name
	profiles      map[string]bool
	networks      map[string]bool
	restored      []string
	imported      []string
	tarFiles      map[string]string // path -> content captured at import time
	manifest      backup.Manifest
	failImg       bool
	metaInstances string                           // instance JSON placed in incus-project-default.json
	snaps         []pbc.Snapshot                   // overrides the default snapshot list
	guests        map[string]*backup.GuestManifest // guest snapshot path -> incus-guest.conf
	inProgress    map[string]bool                  // snapshot paths listed without files (being written)

	// restore test lifecycle
	lifecycle  []string          // start/stop/delete project/name
	files      map[string]string // path -> content inside started instances
	notReady   int               // InstanceFile fails this many times (VM agent booting)
	failStart  bool
	failDelete bool
	instConfig map[string]map[string]string // project/name -> config
	links      map[string]string            // path -> symlink target
}

// guestOf builds the guest manifest of a snapshot holding targets of the
// default manifest (by name).
func (f *fake) guestOf(path string, names ...string) *backup.GuestManifest {
	gm := &backup.GuestManifest{GuestManifestVersion: backup.GuestManifestVersion, HostBackupID: "h1", Hostname: "h1",
		Group: path[:strings.LastIndex(path, "/")]}
	for _, n := range names {
		for _, t := range f.manifest.Targets {
			if t.Name == n {
				gm.Targets = append(gm.Targets, t)
			}
		}
	}
	return gm
}

func newFake(t *testing.T) *fake {
	f := newFakeNoGuests(t)
	f.guests = map[string]*backup.GuestManifest{
		webSnap:                       f.guestOf(webSnap, "web"),
		dbSnap:                        f.guestOf(dbSnap, "db"),
		"ct/web/2026-11-23T02:29:00Z": f.guestOf("ct/web/2026-11-23T02:29:00Z", "web"),
	}
	return f
}

func newFakeNoGuests(t *testing.T) *fake {
	return &fake{
		t: t, instances: map[string]bool{"default/web": true}, volumes: map[string]bool{"default/default/webdata": true},
		profiles: map[string]bool{"default/default": true}, networks: map[string]bool{"default/lanbr": true},
		manifest: backup.Manifest{ManifestVersion: backup.ManifestVersion, Complete: true, Hostname: "h1", Projects: []string{"default"},
			Targets: []backup.Target{
				{Kind: backup.KindContainer, Project: "default", Name: "web", Pool: incus.Pool{Name: "default", Driver: "zfs"}, ArchiveBase: "ct_default_web"},
				{Kind: backup.KindVM, Project: "default", Name: "db", Pool: incus.Pool{Name: "default", Driver: "zfs"}, ArchiveBase: "vm_default_db"},
				{Kind: backup.KindVolume, Project: "default", Name: "webdata", Pool: incus.Pool{Name: "default", Driver: "zfs"}, ContentType: "filesystem",
					ArchiveBase: "vol_default_default_webdata", Volume: &incus.Volume{Name: "webdata", Config: map[string]string{"size": "5GiB", "volatile.x": "y"}}},
				{Kind: backup.KindVolume, Project: "default", Name: "raw", Pool: incus.Pool{Name: "default", Driver: "zfs"}, ContentType: "block", ArchiveBase: "vol_default_default_raw"},
				{Kind: backup.KindContainer, Project: "default", Name: "broken", Pool: incus.Pool{Name: "default", Driver: "zfs"}, ArchiveBase: "ct_default_broken"},
			},
			Items: []backup.ItemResult{
				{Kind: backup.KindContainer, Project: "default", Name: "web", Status: "included"},
				{Kind: backup.KindVM, Project: "default", Name: "db", Status: "included"},
				{Kind: backup.KindVolume, Project: "default", Name: "webdata", Status: "included"},
				{Kind: backup.KindVolume, Project: "default", Name: "raw", Status: "included"},
				{Kind: backup.KindContainer, Project: "default", Name: "broken", Status: "failed", Error: "clone failed"},
			}},
	}
}

func (f *fake) Run(_ context.Context, c execx.Cmd) (execx.Result, error) {
	a := c.Args
	switch a[0] {
	case "snapshot":
		if a[1] == "files" {
			b, _ := json.Marshal([]pbc.File{
				{Filename: "ct_default_web.ppxar.didx", Size: 100}, {Filename: "ct_default_web.mpxar.didx", Size: 1},
				{Filename: "vm_default_db.img.fidx", Size: 1000}, {Filename: "vm_default_db-config.ppxar.didx", Size: 10},
				{Filename: "vm_default_db-config.tar.blob", Size: 10},
				{Filename: "vol_default_default_webdata.ppxar.didx", Size: 50}, {Filename: "vol_default_default_raw.img.fidx", Size: 500},
				{Filename: "incus-metadata.ppxar.didx", Size: 5}, {Filename: "host-config.ppxar.didx", Size: 5},
			})
			return execx.Result{Stdout: string(b)}, nil
		}
		list := f.snaps
		if list == nil {
			list = []pbc.Snapshot{
				{BackupType: "ct", BackupID: "web", BackupTime: time.Date(2026, 11, 24, 2, 29, 0, 0, time.UTC).Unix()},
				{BackupType: "ct", BackupID: "web", BackupTime: time.Date(2026, 11, 23, 2, 29, 0, 0, time.UTC).Unix()},
				{BackupType: "vm", BackupID: "db", BackupTime: time.Date(2026, 11, 24, 2, 29, 0, 0, time.UTC).Unix()},
				{BackupType: "host", BackupID: "h1", BackupTime: time.Date(2026, 11, 24, 2, 30, 0, 0, time.UTC).Unix()},
				{BackupType: "host", BackupID: "h1", BackupTime: time.Date(2026, 11, 23, 2, 30, 0, 0, time.UTC).Unix()},
				{BackupType: "host", BackupID: "other", BackupTime: time.Date(2026, 11, 25, 2, 30, 0, 0, time.UTC).Unix()},
			}
		}
		out := make([]pbc.Snapshot, len(list))
		for i, s := range list {
			if s.Files == nil && !f.inProgress[s.Path()] {
				s.Files = []any{map[string]any{"filename": "index.json.blob"}}
				if s.BackupType == "ct" || s.BackupType == "vm" {
					s.Files = append(s.Files, map[string]any{"filename": backup.GuestManifestArchive + ".blob"})
				}
			}
			out[i] = s
		}
		b, _ := json.Marshal(out)
		return execx.Result{Stdout: string(b)}, nil
	case "restore":
		archive, target := a[2], a[3]
		f.restored = append(f.restored, archive)
		switch {
		case archive == backup.GuestManifestArchive:
			gm, ok := f.guests[a[1]]
			if !ok {
				return execx.Result{ExitCode: 255}, &execx.ExitError{Name: "proxmox-backup-client", ExitCode: 255, Stderr: "no such archive"}
			}
			b, _ := json.Marshal(gm)
			os.WriteFile(target, b, 0o600)
		case archive == "incus-metadata.pxar":
			os.MkdirAll(target, 0o700)
			b, _ := json.Marshal(f.manifest)
			os.WriteFile(filepath.Join(target, "manifest.json"), b, 0o600)
			os.WriteFile(filepath.Join(target, "incus-project-default.json"), []byte(`{
			  "storage_pools":[{"name":"default","driver":"zfs","config":{"source":"tank","volatile.initial_source":"/dev/sdb","zfs.pool_name":"tank"}}],
			  "networks":[{"name":"incusbr0","type":"bridge","managed":true,"config":{"ipv4.address":"10.0.0.1/24"}},{"name":"eth0","type":"physical","managed":false}],
			  "profiles":[{"name":"default","config":{},"devices":{"root":{"type":"disk","path":"/","pool":"default"}}}],
			  "projects":[{"name":"default","config":{}},{"name":"prod","config":{"features.images":"false"}}],
			  "instances":[`+f.metaInstances+`]}`), 0o600)
			os.WriteFile(filepath.Join(target, "incus-project-prod.json"), []byte(`{
			  "networks":[{"name":"incusbr0","type":"bridge","managed":true,"config":{"ipv4.address":"10.0.0.1/24"}}],
			  "profiles":[{"name":"default","config":{},"devices":{}}]}`), 0o600)
		case archive == "host-config.pxar":
			os.MkdirAll(filepath.Join(target, "files/etc"), 0o700)
			os.WriteFile(filepath.Join(target, "files/etc/subuid"), []byte("root:1000000:1000000000\n"), 0o600)
		case strings.HasSuffix(archive, ".tar.blob"):
			var buf bytes.Buffer
			tw := tar.NewWriter(&buf)
			by := []byte(strings.ReplaceAll(backupYAML, "name: web", "name: db"))
			tw.WriteHeader(&tar.Header{Name: "config/", Typeflag: tar.TypeDir, Mode: 0o500})
			tw.WriteHeader(&tar.Header{Name: "config/agent.conf", Typeflag: tar.TypeReg, Mode: 0o644, Size: 2})
			tw.Write([]byte("ok"))
			tw.WriteHeader(&tar.Header{Name: "backup.yaml", Typeflag: tar.TypeReg, Mode: 0o400, Size: int64(len(by))})
			tw.Write(by)
			tw.Close()
			os.WriteFile(target, buf.Bytes(), 0o600)
		case strings.HasSuffix(archive, ".img"):
			if f.failImg {
				return execx.Result{ExitCode: 255}, &execx.ExitError{Name: "proxmox-backup-client", ExitCode: 255, Stderr: "download failed"}
			}
			os.WriteFile(target, []byte("DISK"), 0o600)
		case strings.HasPrefix(archive, "ct_") || strings.HasSuffix(archive, "-config.pxar"):
			os.MkdirAll(filepath.Join(target, "rootfs"), 0o700)
			os.WriteFile(filepath.Join(target, "backup.yaml"), []byte(strings.ReplaceAll(backupYAML, "name: web", "name: x")), 0o400)
		default:
			os.MkdirAll(target, 0o700)
			os.WriteFile(filepath.Join(target, "file.txt"), []byte("data"), 0o600)
		}
		return execx.Result{}, nil
	}
	f.t.Fatalf("unexpected client call %q", a)
	return execx.Result{}, nil
}

func (f *fake) InstanceExists(p, n string) (bool, error) { return f.instances[p+"/"+n], nil }
func (f *fake) InstanceConfig(p, n string) (map[string]string, bool, error) {
	if !f.instances[p+"/"+n] {
		return nil, false, nil
	}
	return f.instConfig[p+"/"+n], true, nil
}
func (f *fake) VolumeExists(p, pool, n string) (bool, error) {
	return f.volumes[p+"/"+pool+"/"+n], nil
}
func (f *fake) VolumeInfo(p, pool, n string) (incus.VolumeState, error) {
	if !f.volumes[p+"/"+pool+"/"+n] {
		return incus.VolumeState{}, nil
	}
	if n == "rawdisk" {
		return incus.VolumeState{Exists: true, ContentType: "block", UsedBy: []string{"/1.0/instances/db"}}, nil
	}
	return incus.VolumeState{Exists: true, ContentType: "filesystem"}, nil
}
func (f *fake) ProfileExists(p, n string) (bool, error) { return f.profiles[p+"/"+n], nil }
func (f *fake) NetworkExists(p, n string) (bool, error) { return f.networks[p+"/"+n], nil }
func (f *fake) GetPool(n string) (incus.Pool, error) {
	if n == "default" || n == "fast" {
		return incus.Pool{Name: n, Driver: "zfs"}, nil
	}
	return incus.Pool{}, errors.New("not found")
}
func (f *fake) ImportInstance(_ context.Context, p, pool, n string, r io.Reader) error {
	b, err := f.read(r)
	if err != nil {
		return err
	}
	f.imported = append(f.imported, "instance "+p+"/"+pool+"/"+n+" "+b)
	return nil
}
func (f *fake) ImportVolume(_ context.Context, p, pool, n string, r io.Reader) error {
	b, err := f.read(r)
	if err != nil {
		return err
	}
	f.imported = append(f.imported, "volume "+p+"/"+pool+"/"+n+" "+b)
	return nil
}

func (f *fake) StartInstance(_ context.Context, p, n string) error {
	f.lifecycle = append(f.lifecycle, "start "+p+"/"+n)
	if f.failStart {
		return errors.New("boot failed")
	}
	return nil
}
func (f *fake) StopInstance(_ context.Context, p, n string, force bool) error {
	f.lifecycle = append(f.lifecycle, "stop "+p+"/"+n)
	return nil
}
func (f *fake) DeleteInstance(_ context.Context, p, n string) error {
	f.lifecycle = append(f.lifecycle, "delete "+p+"/"+n)
	if f.failDelete {
		return errors.New("busy")
	}
	return nil
}
func (f *fake) InstanceFile(p, n, path string) (io.ReadCloser, incus.FileInfo, error) {
	if f.notReady > 0 {
		f.notReady--
		return nil, incus.FileInfo{}, errors.New("VM agent isn't currently running")
	}
	if path == "/etc" {
		return nil, incus.FileInfo{Type: "directory"}, nil
	}
	if target, ok := f.links[path]; ok {
		return nil, incus.FileInfo{Type: "symlink", Target: target}, nil
	}
	c, ok := f.files[path]
	if !ok {
		return nil, incus.FileInfo{}, fmt.Errorf("%s: %w", path, fs.ErrNotExist)
	}
	return io.NopCloser(strings.NewReader(c)), incus.FileInfo{Type: "file"}, nil
}

// read consumes the import stream: a real tar (image streaming) is parsed
// into tarFiles; the fake GNU tar output is a plain path list.
func (f *fake) read(r io.Reader) (string, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	tr := tar.NewReader(bytes.NewReader(b))
	var names []string
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		if names == nil {
			f.tarFiles = map[string]string{}
		}
		names = append(names, strings.TrimSuffix(h.Name, "/"))
		if h.Typeflag == tar.TypeReg {
			c, _ := io.ReadAll(tr)
			f.tarFiles[h.Name] = string(c)
		}
	}
	if names != nil {
		return strings.Join(names, ","), nil
	}
	return string(b), nil
}

// stream fakes `proxmox-backup-client restore <snap> <archive> -`.
func (f *fake) stream(_ context.Context, snap, archive string) (io.ReadCloser, func() error, error) {
	f.restored = append(f.restored, archive+"@"+snap)
	if f.failImg {
		return io.NopCloser(strings.NewReader("DI")), func() error { return errors.New("download failed") }, nil
	}
	n := map[string]int{"vm_default_db.img": 1000, "vol_default_default_raw.img": 500}[archive]
	return io.NopCloser(strings.NewReader(strings.Repeat("D", n))), func() error { return nil }, nil
}

// tar captures the staged layout instead of producing a real tar stream.
func (f *fake) tar(_ context.Context, dir string) (io.ReadCloser, func() error, error) {
	f.tarFiles = map[string]string{}
	var paths []string
	filepath.Walk(filepath.Join(dir, "backup"), func(p string, info os.FileInfo, err error) error {
		if err == nil {
			rel, _ := filepath.Rel(dir, p)
			paths = append(paths, rel)
			if !info.IsDir() {
				b, _ := os.ReadFile(p)
				f.tarFiles[rel] = string(b)
			}
		}
		return nil
	})
	sort.Strings(paths)
	return io.NopCloser(strings.NewReader(strings.Join(paths, ","))), func() error { return nil }, nil
}

func newRestorer(t *testing.T, f *fake) *Restorer {
	dir := t.TempDir()
	pw := filepath.Join(dir, "pw")
	os.WriteFile(pw, []byte("x"), 0o600)
	cfg := config.Default()
	cfg.PBS.Repository, cfg.PBS.PasswordFile, cfg.PBS.BackupID, cfg.PBS.Namespace = "r", pw, "h1", "incus"
	cfg.Storage.StagingDirectory = filepath.Join(dir, "staging")
	return &Restorer{Cfg: cfg, Incus: f, PBC: &pbc.Client{Repository: "r", PasswordFile: pw, X: f},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Tar: f.tar, StreamArchive: f.stream,
		FreeBytes: func(string) uint64 { return 1 << 40 }}
}

func index(t *testing.T, f *fake) map[string]any {
	t.Helper()
	var m map[string]any
	if err := yaml.Unmarshal([]byte(f.tarFiles["backup/index.yaml"]), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestResolve(t *testing.T) {
	f := newFake(t)
	r := newRestorer(t, f)
	for ref, want := range map[string]string{"latest": snap, "2026-11-23T02:30:00Z": "host/h1/2026-11-23T02:30:00Z", snap: snap} {
		got, err := r.Resolve(context.Background(), ref)
		if err != nil || got != want {
			t.Errorf("%s: %s %v", ref, got, err)
		}
	}
	for _, bad := range []string{"", "yesterday", "host/h1/../x", "host/h1/2026-11-23T02:30:00Z; rm -rf /", webSnap} {
		if _, err := r.Resolve(context.Background(), bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestRestoreInstanceNeverOverwrites(t *testing.T) {
	f := newFake(t)
	r := newRestorer(t, f)
	_, err := r.RestoreInstance(context.Background(), InstanceOptions{Snapshot: "latest", Instance: "web"})
	if err == nil || !strings.Contains(err.Error(), "already exists") || len(f.imported) != 0 {
		t.Fatalf("must refuse to overwrite: %v", err)
	}
}

func TestRestoreContainerAsCloneGetsNewIdentity(t *testing.T) {
	f := newFake(t)
	r := newRestorer(t, f)
	res, err := r.RestoreInstance(context.Background(), InstanceOptions{Snapshot: "latest", Instance: "web", NewName: "web-restored", Pool: "fast"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Imported || len(f.imported) != 1 || !strings.HasPrefix(f.imported[0], "instance default/fast/web-restored ") {
		t.Fatalf("import: %v", f.imported)
	}
	if !strings.Contains(f.imported[0], "backup/container/backup.yaml") || !strings.Contains(f.imported[0], "backup/index.yaml") {
		t.Fatalf("tar layout: %s", f.imported[0])
	}
	idx := index(t, f)
	if idx["type"] != "container" || idx["optimized"] != false || idx["name"] != "web" || idx["backend"] != "zfs" {
		t.Fatalf("index: %v", idx)
	}
	cfg := idx["config"].(map[string]any)["container"].(map[string]any)["config"].(map[string]any)
	if _, ok := cfg["volatile.eth0.hwaddr"]; ok {
		t.Error("MAC must be stripped when the original still exists")
	}
	if cfg["volatile.idmap.current"] != "[]" || cfg["volatile.last_state.idmap"] != "[]" || cfg["limits.memory"] != "2GiB" {
		t.Errorf("idmap/user config must be kept: %v", cfg)
	}
	if strings.Contains(f.tarFiles["backup/container/backup.yaml"], "user-keepme") || strings.Contains(f.tarFiles["backup/container/backup.yaml"], "10:66:6a") {
		t.Error("backup.yaml inside the tarball must be sanitized too")
	}
	if _, ok := idx["config"].(map[string]any)["snapshots"]; ok {
		t.Error("instance snapshots must be removed from the import config")
	}
	if _, err := os.Stat(res.StagingDir); !os.IsNotExist(err) {
		t.Error("staging dir must be removed after import")
	}
}

func TestRestoreDisasterRecoveryKeepsIdentity(t *testing.T) {
	f := newFake(t)
	delete(f.instances, "default/web")
	r := newRestorer(t, f)
	if _, err := r.RestoreInstance(context.Background(), InstanceOptions{Snapshot: "latest", Instance: "web"}); err != nil {
		t.Fatal(err)
	}
	cfg := index(t, f)["config"].(map[string]any)["container"].(map[string]any)["config"].(map[string]any)
	if cfg["volatile.eth0.hwaddr"] != "10:66:6a:00:00:01" {
		t.Errorf("identity must be kept for same-name DR restore: %v", cfg)
	}
	f2 := newFake(t)
	if _, err := newRestorer(t, f2).RestoreInstance(context.Background(), InstanceOptions{Snapshot: "latest", Instance: "web", NewName: "w2", Identity: "keep"}); err == nil {
		t.Error("--identity keep must be refused while the original exists")
	}
}

func TestRestoreDependencies(t *testing.T) {
	f := newFake(t)
	delete(f.volumes, "default/default/webdata")
	delete(f.networks, "default/lanbr")
	r := newRestorer(t, f)
	_, err := r.RestoreInstance(context.Background(), InstanceOptions{Snapshot: "latest", Instance: "web", NewName: "w2"})
	if err == nil || !strings.Contains(err.Error(), "webdata") || !strings.Contains(err.Error(), "lanbr") || len(f.imported) != 0 {
		t.Fatalf("missing dependencies must block: %v", err)
	}
	res, err := r.RestoreInstance(context.Background(), InstanceOptions{Snapshot: "latest", Instance: "web", NewName: "w2", DropMissingDevices: true})
	if err != nil || strings.Join(res.DroppedDevices, ",") != "data,lan" {
		t.Fatalf("drop: %+v %v", res, err)
	}
	devs := index(t, f)["config"].(map[string]any)["container"].(map[string]any)["devices"].(map[string]any)
	if len(devs) != 0 {
		t.Fatalf("devices not dropped: %v", devs)
	}
	f3 := newFake(t)
	f3.profiles = map[string]bool{}
	if _, err := newRestorer(t, f3).RestoreInstance(context.Background(), InstanceOptions{Snapshot: "latest", Instance: "web", NewName: "w3", DropMissingDevices: true}); err == nil || !strings.Contains(err.Error(), "profile") {
		t.Fatalf("missing profile must always block: %v", err)
	}
}

func TestRestoreVMLayout(t *testing.T) {
	f := newFake(t)
	r := newRestorer(t, f)
	if _, err := r.RestoreInstance(context.Background(), InstanceOptions{Snapshot: dbSnap, Instance: "db", TargetProject: "default"}); err != nil {
		t.Fatal(err)
	}
	if f.tarFiles["backup/virtual-machine.img"] != strings.Repeat("D", 1000) || f.tarFiles["backup/virtual-machine/backup.yaml"] == "" {
		t.Fatalf("vm layout: %v", f.tarFiles)
	}
	if index(t, f)["type"] != "virtual-machine" {
		t.Fatal("type")
	}
}

func TestRestoreFailedTargetAndErrorsCleanUp(t *testing.T) {
	f := newFake(t)
	r := newRestorer(t, f)
	if _, err := r.RestoreInstance(context.Background(), InstanceOptions{Snapshot: "latest", Instance: "broken", NewName: "b"}); err == nil || !strings.Contains(err.Error(), "not part of any backup") {
		t.Fatalf("an instance without a guest snapshot must be refused: %v", err)
	}
	if _, err := r.RestoreInstance(context.Background(), InstanceOptions{Snapshot: snap, Instance: "web", NewName: "b"}); err == nil || !strings.Contains(err.Error(), "run index") {
		t.Fatalf("instances are not restored from run indexes: %v", err)
	}
	f.failImg = true
	res, err := r.RestoreInstance(context.Background(), InstanceOptions{Snapshot: dbSnap, Instance: "db"})
	if err == nil || len(f.imported) != 0 {
		t.Fatalf("download failure must abort: %v", err)
	}
	if _, serr := os.Stat(res.StagingDir); !os.IsNotExist(serr) {
		t.Error("staging must be removed after failure")
	}
	if _, err := r.RestoreInstance(context.Background(), InstanceOptions{Snapshot: "latest", Instance: "nope"}); err == nil {
		t.Error("unknown instance")
	}
	if _, err := r.RestoreInstance(context.Background(), InstanceOptions{Snapshot: "latest", Instance: "web", NewName: "x", Pool: "missing"}); err == nil {
		t.Error("missing pool must be refused")
	}
}

func TestRestoreStagingOnlyAndSpace(t *testing.T) {
	f := newFake(t)
	r := newRestorer(t, f)
	res, err := r.RestoreInstance(context.Background(), InstanceOptions{Snapshot: "latest", Instance: "web", NewName: "x", StagingOnly: true})
	if err != nil || res.Imported || len(f.imported) != 0 {
		t.Fatalf("staging only: %+v %v", res, err)
	}
	if _, err := os.Stat(filepath.Join(res.StagingDir, "backup/index.yaml")); err != nil {
		t.Fatal("staged index.yaml missing")
	}
	r.FreeBytes = func(string) uint64 { return 10 }
	if _, err := r.RestoreInstance(context.Background(), InstanceOptions{Snapshot: "latest", Instance: "web", NewName: "y"}); err == nil || !strings.Contains(err.Error(), "not enough space") {
		t.Fatalf("space check: %v", err)
	}
	// Staging has room, but Incus' upload buffer does not.
	r.BackupsDir = "/incus-backups"
	r.FreeBytes = func(p string) uint64 {
		if p == "/incus-backups" {
			return 10
		}
		return 1 << 40
	}
	if _, err := r.RestoreInstance(context.Background(), InstanceOptions{Snapshot: "latest", Instance: "web", NewName: "y"}); err == nil || !strings.Contains(err.Error(), "storage.backups_volume") {
		t.Fatalf("incus backups space check: %v", err)
	}
}

func TestRestoreVolumes(t *testing.T) {
	f := newFake(t)
	r := newRestorer(t, f)
	if _, err := r.RestoreVolume(context.Background(), VolumeOptions{Snapshot: snap, Volume: "webdata"}); err == nil {
		t.Fatal("existing volume must not be overwritten")
	}
	if _, err := r.RestoreVolume(context.Background(), VolumeOptions{Snapshot: snap, Volume: "webdata", NewName: "webdata2"}); err != nil {
		t.Fatal(err)
	}
	idx := index(t, f)
	vol := idx["config"].(map[string]any)["volume"].(map[string]any)
	if idx["type"] != "custom" || vol["content_type"] != "filesystem" || vol["config"].(map[string]any)["size"] != "5GiB" {
		t.Fatalf("index: %v", idx)
	}
	if _, ok := vol["config"].(map[string]any)["volatile.x"]; ok {
		t.Error("volatile keys must be dropped")
	}
	if f.tarFiles["backup/volume/file.txt"] != "data" {
		t.Fatalf("layout: %v", f.tarFiles)
	}
	if _, err := r.RestoreVolume(context.Background(), VolumeOptions{Snapshot: snap, Volume: "raw"}); err != nil {
		t.Fatal(err)
	}
	if f.tarFiles["backup/volume.img"] != strings.Repeat("D", 500) {
		t.Fatalf("block layout: %v", f.tarFiles)
	}
}

func TestRestoreHostKit(t *testing.T) {
	f := newFake(t)
	f.manifest.Projects = []string{"default", "prod"}
	r := newRestorer(t, f)
	dir := filepath.Join(t.TempDir(), "kit")
	res, err := r.RestoreHost(context.Background(), "latest", dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 4 {
		t.Fatalf("files: %v", res.Files)
	}
	pre, _ := os.ReadFile(filepath.Join(dir, "preseed.yaml"))
	var doc map[string]any
	if err := yaml.Unmarshal(pre, &doc); err != nil {
		t.Fatal(err)
	}
	nets := doc["networks"].([]any)
	if len(nets) != 1 || nets[0].(map[string]any)["project"] != "default" || len(doc["projects"].([]any)) != 1 ||
		len(doc["profiles"].([]any)) != 2 || strings.Contains(string(pre), "volatile.") {
		t.Fatalf("preseed:\n%s", pre)
	}
	plan, _ := os.ReadFile(filepath.Join(dir, "RECOVERY.md"))
	iv, ii := strings.Index(string(plan), "restore volume "+snap+" webdata"), strings.Index(string(plan), "restore instance "+webSnap+" web")
	if iv < 0 || ii < 0 || iv > ii || !strings.Contains(string(plan), "source: tank") || !strings.Contains(string(plan), "restore instance "+dbSnap+" db") {
		t.Fatalf("plan:\n%s", plan)
	}
	if strings.Contains(string(plan), "broken") || strings.Contains(string(plan), "2026-11-23T02:29") {
		t.Fatalf("only the newest guest snapshot of each instance: \n%s", plan)
	}
	if _, err := r.RestoreHost(context.Background(), "latest", dir); err == nil {
		t.Fatal("non-empty dir must be refused")
	}
}

func TestNewerManifestRefused(t *testing.T) {
	f := newFake(t)
	f.manifest.ManifestVersion = 99
	if _, err := newRestorer(t, f).Manifest(context.Background(), snap); err == nil {
		t.Fatal("newer manifest must be refused")
	}
}

func TestOlderManifestRefused(t *testing.T) {
	f := newFake(t)
	f.manifest.ManifestVersion = 2
	if _, err := newRestorer(t, f).Manifest(context.Background(), snap); err == nil || !strings.Contains(err.Error(), "older incus-pbs") {
		t.Fatalf("version 2 manifest must be refused: %v", err)
	}
	f.guests[webSnap].GuestManifestVersion = 99
	if _, err := newRestorer(t, f).RestoreInstance(context.Background(), InstanceOptions{Snapshot: webSnap, Instance: "web", NewName: "x"}); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("unknown guest manifest version must be refused: %v", err)
	}
}

func TestInstanceLatestComesFromItsOwnGroup(t *testing.T) {
	f := newFake(t)
	delete(f.instances, "default/web")
	// A newer web snapshot than the newest run index (own schedule).
	newer := "ct/web/2026-11-24T09:00:00Z"
	f.guests[newer] = f.guestOf(newer, "web")
	f.snaps = []pbc.Snapshot{
		{BackupType: "host", BackupID: "h1", BackupTime: at(2, 30)},
		{BackupType: "ct", BackupID: "web", BackupTime: at(2, 29)},
		{BackupType: "ct", BackupID: "web", BackupTime: at(9, 0)},
		{BackupType: "ct", BackupID: "prod-web", BackupTime: at(9, 30)}, // other project, same name
		{BackupType: "ct", BackupID: "web", BackupTime: at(10, 0)},      // another host's guest in the namespace
	}
	other := "ct/web/2026-11-24T10:00:00Z"
	f.guests[other] = f.guestOf(other, "web")
	f.guests[other].HostBackupID = "elsewhere"
	prodWeb := "ct/prod-web/2026-11-24T09:30:00Z"
	f.guests[prodWeb] = f.guestOf(prodWeb, "web")
	f.guests[prodWeb].Targets[0].Project = "prod"
	r := newRestorer(t, f)

	// The other host's newer snapshot in the same group is skipped; our 09:00 one is used.
	if res, err := r.RestoreInstance(context.Background(), InstanceOptions{Snapshot: "latest", Instance: "web", SourceProject: "default"}); err != nil || res.Snapshot != newer {
		t.Fatalf("another host's snapshot must not shadow ours: %+v %v", res, err)
	}
	delete(f.instances, "default/web")
	delete(f.guests, other)
	f.snaps = f.snaps[:4]
	if _, err := r.RestoreInstance(context.Background(), InstanceOptions{Snapshot: "latest", Instance: "web"}); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("web exists in two projects: %v", err)
	}
	res, err := r.RestoreInstance(context.Background(), InstanceOptions{Snapshot: "latest", Instance: "web", SourceProject: "default"})
	if err != nil || res.Snapshot != newer {
		t.Fatalf("latest: %+v %v", res, err)
	}
	res, err = r.RestoreInstance(context.Background(), InstanceOptions{Snapshot: "2026-11-24T08:00:00Z", Instance: "web", SourceProject: "default"})
	if err != nil || res.Snapshot != webSnap {
		t.Fatalf("at or before a time: %+v %v", res, err)
	}
	res, err = r.RestoreInstance(context.Background(), InstanceOptions{Snapshot: "latest", Instance: "web", SourceProject: "prod", TargetProject: "default", NewName: "pw"})
	if err != nil || res.Snapshot != prodWeb || res.Source != "prod/web" {
		t.Fatalf("other project: %+v %v", res, err)
	}
	if !strings.Contains(strings.Join(f.restored, " "), "ct_default_web.pxar") {
		t.Fatalf("restored: %v", f.restored)
	}
}

func TestStagingOnlyDownloadsImage(t *testing.T) {
	f := newFake(t)
	r := newRestorer(t, f)
	res, err := r.RestoreInstance(context.Background(), InstanceOptions{Snapshot: dbSnap, Instance: "db", StagingOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(filepath.Join(res.StagingDir, "backup/virtual-machine/config"), 0o700) }) // 0500 from the tar blob
	if b, _ := os.ReadFile(filepath.Join(res.StagingDir, "backup/virtual-machine.img")); string(b) != "DISK" {
		t.Fatalf("staging-only must download the image to the staging dir, got %q", b)
	}
}

func TestPickPartition(t *testing.T) {
	parts := []partition{
		{dev: "/dev/loop10", size: 8 << 30},
		{dev: "/dev/loop10p1", size: 100 << 20, fstype: "vfat", num: 1},
		{dev: "/dev/loop10p2", size: 7 << 30, fstype: "ext4", num: 2},
		{dev: "/dev/loop10p3", size: 1 << 30, fstype: "swap", num: 3},
	}
	if p := pickPartition(parts, "/dev/loop10", 0); p == nil || p.dev != "/dev/loop10p2" {
		t.Fatalf("auto pick: %+v", p)
	}
	if p := pickPartition(parts, "/dev/loop10", 1); p == nil || p.dev != "/dev/loop10p1" {
		t.Fatalf("explicit: %+v", p)
	}
	if p := pickPartition(parts, "/dev/loop10", 9); p != nil {
		t.Fatal("missing partition must not be picked")
	}
	if m := partNumRE.FindStringSubmatch("/dev/loop10"); m != nil {
		t.Fatal("whole loop device must not look like a partition")
	}
	if m := partNumRE.FindStringSubmatch("/dev/loop10p2"); m == nil || m[1] != "/dev/loop10" || m[2] != "2" {
		t.Fatalf("partition parse: %v", m)
	}
}

func TestDependenciesCheckedBeforeDownload(t *testing.T) {
	f := newFake(t)
	f.volumes["default/default/rawdisk"] = true
	f.guests[dbSnap].Instance = map[string]any{"name": "db", "profiles": []any{"default"},
		"devices": map[string]any{"blk": map[string]any{"type": "disk", "pool": "default", "source": "rawdisk"}}}
	r := newRestorer(t, f)
	_, err := r.RestoreInstance(context.Background(), InstanceOptions{Snapshot: dbSnap, Instance: "db", NewName: "db2"})
	if err == nil || !strings.Contains(err.Error(), "already attached") {
		t.Fatalf("in-use block volume must be refused: %v", err)
	}
	for _, a := range f.restored {
		if a != backup.GuestManifestArchive {
			t.Fatalf("nothing but the guest manifest may be downloaded before the check fails: %v", f.restored)
		}
	}
}

func TestRestoreVMFromGroupSnapshot(t *testing.T) {
	f := newFake(t)
	r := newRestorer(t, f)
	if _, err := r.RestoreInstance(context.Background(), InstanceOptions{Snapshot: "latest", Instance: "db"}); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(f.restored, " ")
	if !strings.Contains(got, "vm_default_db.img@"+dbSnap) || !strings.Contains(got, "vm_default_db-config.tar.blob") {
		t.Fatalf("archives must come from the VM's own group: %v", f.restored)
	}
	if f.tarFiles["backup/virtual-machine/config/agent.conf"] != "ok" || f.tarFiles["backup/virtual-machine/backup.yaml"] == "" {
		t.Fatalf("config blob must be unpacked into backup/virtual-machine: %v", f.tarFiles)
	}
	if f.tarFiles["backup/virtual-machine.img"] != strings.Repeat("D", 1000) {
		t.Fatal("image must be streamed")
	}
}

func TestRestoreVolumeFromGuestOrIndex(t *testing.T) {
	f := newFake(t)
	// raw is attached only to db: stored in db's group, newer than the run index.
	newer := "vm/db/2026-11-24T09:00:00Z"
	f.guests[newer] = f.guestOf(newer, "db", "raw")
	f.snaps = []pbc.Snapshot{
		{BackupType: "host", BackupID: "h1", BackupTime: at(2, 30)},
		{BackupType: "vm", BackupID: "db", BackupTime: at(2, 29)},
		{BackupType: "vm", BackupID: "db", BackupTime: at(9, 0)},
		{BackupType: "ct", BackupID: "web", BackupTime: at(2, 29)},
	}
	r := newRestorer(t, f)
	res, err := r.RestoreVolume(context.Background(), VolumeOptions{Snapshot: "latest", Volume: "raw"})
	if err != nil || res.Snapshot != newer {
		t.Fatalf("newest backup of raw is in vm/db: %+v %v", res, err)
	}
	if !strings.Contains(strings.Join(f.restored, " "), "vol_default_default_raw.img@"+newer) {
		t.Fatalf("restored: %v", f.restored)
	}
	res, err = r.RestoreVolume(context.Background(), VolumeOptions{Snapshot: "latest", Volume: "webdata", NewName: "w2"})
	if err != nil || res.Snapshot != snap {
		t.Fatalf("webdata lives in the run index: %+v %v", res, err)
	}
	if _, err := r.RestoreVolume(context.Background(), VolumeOptions{Snapshot: "2026-11-24T05:00:00Z", Volume: "raw"}); err != nil {
		t.Fatalf("raw at 05:00 comes from the run index: %v", err)
	}
}

func TestUntarRejectsEscapes(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "../evil", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1})
	tw.Write([]byte("x"))
	tw.Close()
	if err := untarInto(&buf, t.TempDir()); err == nil || !strings.Contains(err.Error(), "unsafe") {
		t.Fatalf("path traversal must be rejected: %v", err)
	}
}

func TestSnapshotsBeingWrittenAreSkipped(t *testing.T) {
	f := newFake(t)
	running := "vm/db/2026-11-24T09:00:00Z" // a backup is uploading it right now
	f.inProgress = map[string]bool{running: true, "host/h1/2026-11-24T09:01:00Z": true, "vm/frigate/2026-11-24T09:00:00Z": true}
	f.snaps = []pbc.Snapshot{
		{BackupType: "host", BackupID: "h1", BackupTime: at(2, 30)},
		{BackupType: "host", BackupID: "h1", BackupTime: at(9, 1)},
		{BackupType: "vm", BackupID: "db", BackupTime: at(2, 29)},
		{BackupType: "vm", BackupID: "db", BackupTime: at(9, 0)},
		{BackupType: "vm", BackupID: "frigate", BackupTime: at(9, 0)}, // first backup, still running
	}
	r := newRestorer(t, f)
	res, err := r.RestoreInstance(context.Background(), InstanceOptions{Snapshot: "latest", Instance: "db", StagingOnly: true})
	if err != nil || res.Snapshot != dbSnap {
		t.Fatalf("latest must skip the snapshot being written: %+v %v", res, err)
	}
	t.Cleanup(func() { os.Chmod(filepath.Join(res.StagingDir, "backup/virtual-machine/config"), 0o700) })
	if _, err := r.RestoreInstance(context.Background(), InstanceOptions{Snapshot: "latest", Instance: "frigate"}); err == nil || !strings.Contains(err.Error(), "not part of any backup") {
		t.Fatalf("a group with only an unfinished snapshot has no backup yet: %v", err)
	}
	if got, err := r.Resolve(context.Background(), "latest"); err != nil || got != snap {
		t.Fatalf("latest run index must be finished: %s %v", got, err)
	}
	l, err := r.List(context.Background())
	if err != nil || len(l.InProgress) != 3 || len(l.HostIndexes) != 1 || len(l.Guests) != 1 || len(l.Guests[0].Snapshots) != 1 {
		t.Fatalf("listing: %+v %v", l, err)
	}
}

func TestForeignGuestSnapshotsAreSkipped(t *testing.T) {
	f := newFake(t)
	delete(f.instances, "default/web")
	newer := "ct/web/2026-11-24T09:00:00Z"
	f.guests[newer] = f.guestOf(newer, "web")
	f.snaps = []pbc.Snapshot{
		{BackupType: "host", BackupID: "h1", BackupTime: at(2, 30)},
		{BackupType: "ct", BackupID: "web", BackupTime: at(9, 0)},
		// A Proxmox VE backup in the same namespace: no incus-pbs manifest.
		{BackupType: "ct", BackupID: "100", BackupTime: at(11, 0), Files: []any{map[string]any{"filename": "root.pxar.didx"}}},
		// A newer incus-pbs snapshot with a manifest format this version does not read.
		{BackupType: "ct", BackupID: "web", BackupTime: at(10, 0)},
	}
	future := "ct/web/2026-11-24T10:00:00Z"
	f.guests[future] = f.guestOf(future, "web")
	f.guests[future].GuestManifestVersion = 99
	res, err := newRestorer(t, f).RestoreInstance(context.Background(), InstanceOptions{Snapshot: "latest", Instance: "web"})
	if err != nil || res.Snapshot != newer {
		t.Fatalf("foreign and unreadable snapshots must be skipped: %+v %v", res, err)
	}
}

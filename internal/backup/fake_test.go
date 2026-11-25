package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/incus-pbs/incus-pbs/internal/config"
	"github.com/incus-pbs/incus-pbs/internal/execx"
	"github.com/incus-pbs/incus-pbs/internal/incus"
	"github.com/incus-pbs/incus-pbs/internal/pbc"
)

// pbcCall records one proxmox-backup-client backup invocation.
type pbcCall struct {
	group    string
	archives []string
}

// fakeHost simulates Incus, ZFS, mount and proxmox-backup-client. Every side
// effect is recorded in order so tests can assert ordering and cleanup.
type fakeHost struct {
	mu        sync.Mutex
	t         *testing.T
	events    []string
	instances map[string][]incus.Instance // project -> instances
	volumes   map[string][]incus.Volume   // project -> volumes
	pools     []incus.Pool
	snapshots map[string][]string // target key -> snapshot names
	clones    map[string]bool
	mounts    map[string]bool
	failOn    map[string]bool // event prefixes that fail
	devRoot   string
	pbcHang   bool
	execs     []string
	execCode  int
	calls     []pbcCall
	bookmarks map[string]string   // name -> guid
	zsnaps    map[string][]string // dataset -> snapshot names (without @)
	pbcArgs   []string
	pbcEnv    []string
	archives  map[string]string // archive name -> source (at backup time)
	metadata  map[string]string // file name -> content of the metadata dir at backup time
	guest     map[string]string // group -> incus-guest.conf content at backup time
}

func newFakeHost(t *testing.T) *fakeHost {
	return &fakeHost{
		t: t, snapshots: map[string][]string{}, clones: map[string]bool{}, mounts: map[string]bool{},
		bookmarks: map[string]string{}, zsnaps: map[string][]string{},
		failOn: map[string]bool{}, devRoot: filepath.Join(t.TempDir(), "dev"),
		pools: []incus.Pool{{Name: "default", Driver: "zfs", Dataset: "tank"}, {Name: "lvm", Driver: "lvm"}},
		instances: map[string][]incus.Instance{"default": {
			{Name: "c1", Project: "default", Type: "container", Status: "Running", RootPool: "default",
				Devices: map[string]map[string]string{
					"root":   {"type": "disk", "path": "/", "pool": "default"},
					"data":   {"type": "disk", "path": "/data", "pool": "default", "source": "shared"},
					"hostfs": {"type": "disk", "path": "/mnt/host", "source": "/srv/host"},
				}},
			{Name: "c2", Project: "default", Type: "container", Status: "Stopped", RootPool: "default",
				Devices: map[string]map[string]string{"root": {"type": "disk", "path": "/", "pool": "default"}}},
			{Name: "v1", Project: "default", Type: "virtual-machine", Status: "Running", RootPool: "default",
				Devices: map[string]map[string]string{
					"root": {"type": "disk", "path": "/", "pool": "default"},
					"data": {"type": "disk", "path": "/data", "pool": "default", "source": "shared"},
					"blk":  {"type": "disk", "pool": "default", "source": "rawdisk"},
				}},
		}},
		volumes: map[string][]incus.Volume{"default": {
			{Pool: "default", Project: "default", Name: "shared", ContentType: "filesystem", UsedBy: []string{"/1.0/instances/c1", "/1.0/instances/v1"}},
			{Pool: "default", Project: "default", Name: "rawdisk", ContentType: "block"},
			{Pool: "default", Project: "default", Name: "standalone", ContentType: "filesystem"},
			{Pool: "default", Project: "default", Name: "installer", ContentType: "iso"},
		}},
	}
}

func (f *fakeHost) record(e string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, e)
	for prefix := range f.failOn {
		if strings.HasPrefix(e, prefix) {
			return errors.New("injected failure: " + e)
		}
	}
	return nil
}

// --- incus.Provider ---

func (f *fakeHost) ListInstances(p string) ([]incus.Instance, error) { return f.instances[p], nil }
func (f *fakeHost) GetPool(n string) (incus.Pool, error) {
	for _, p := range f.pools {
		if p.Name == n {
			return p, nil
		}
	}
	return incus.Pool{}, errors.New("no pool")
}
func (f *fakeHost) ListPools() ([]incus.Pool, error)                   { return f.pools, nil }
func (f *fakeHost) ListCustomVolumes(p string) ([]incus.Volume, error) { return f.volumes[p], nil }
func (f *fakeHost) ServerVersion() (string, error)                     { return "7.6", nil }
func (f *fakeHost) Metadata(p string) (map[string]any, error) {
	return map[string]any{"project": p, "server_config": incus.Redact(map[string]string{"core.trust_password": "SECRET"})}, nil
}

func (f *fakeHost) snaps(key string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{"user-snap"}, f.snapshots[key]...)
}
func (f *fakeHost) addSnap(key, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snapshots[key] = append(f.snapshots[key], name)
}
func (f *fakeHost) delSnap(key, name string) error {
	if !incus.IsOwnSnapshot(name) {
		f.t.Errorf("attempt to delete foreign snapshot %s/%s", key, name)
		return errors.New("foreign")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var keep []string
	for _, s := range f.snapshots[key] {
		if s != name {
			keep = append(keep, s)
		}
	}
	f.snapshots[key] = keep
	return nil
}

func (f *fakeHost) ListSnapshots(p, i string) ([]string, error) {
	return f.snaps("i:" + p + "/" + i), nil
}
func (f *fakeHost) CreateSnapshot(p, i, n string, _ time.Time) error {
	if err := f.record("snapshot " + p + "/" + i); err != nil {
		return err
	}
	f.addSnap("i:"+p+"/"+i, n)
	return nil
}
func (f *fakeHost) DeleteSnapshot(p, i, n string) error {
	if err := f.record("delete-snapshot " + p + "/" + i); err != nil {
		return err
	}
	return f.delSnap("i:"+p+"/"+i, n)
}
func (f *fakeHost) ListVolumeSnapshots(v incus.Volume) ([]string, error) {
	return f.snaps("v:" + v.Key()), nil
}
func (f *fakeHost) CreateVolumeSnapshot(v incus.Volume, n string, _ time.Time) error {
	if err := f.record("snapshot volume " + v.Name); err != nil {
		return err
	}
	f.addSnap("v:"+v.Key(), n)
	return nil
}
func (f *fakeHost) DeleteVolumeSnapshot(v incus.Volume, n string) error {
	if err := f.record("delete-snapshot volume " + v.Name); err != nil {
		return err
	}
	return f.delSnap("v:"+v.Key(), n)
}

func (f *fakeHost) Exec(_ context.Context, p, i string, cmd []string, env map[string]string) (incus.ExecResult, error) {
	if err := f.record("exec " + p + "/" + i + " " + strings.Join(cmd, " ") + " phase=" + env["INCUS_PBS_PHASE"]); err != nil {
		return incus.ExecResult{}, err
	}
	return incus.ExecResult{ExitCode: f.execCode, Stdout: "out"}, nil
}

func (f *fakeHost) mountinfo() (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var b strings.Builder
	for m := range f.mounts {
		b.WriteString("90 36 0:50 / " + m + " ro - zfs x ro\n")
	}
	return b.String(), nil
}

// --- execx.Runner ---

func (f *fakeHost) Run(ctx context.Context, c execx.Cmd) (execx.Result, error) {
	a := c.Args
	switch c.Name {
	case "incus", "udevadm", "uname", "zpool":
		return execx.Result{Stdout: "tank\tONLINE\t10G\n"}, nil
	case "zfs":
		switch a[0] {
		case "list" /* bookmarks / snapshots */ :
			if len(a) > 3 && a[2] == "-t" && (a[3] == "bookmark" || a[3] == "snapshot") {
				f.mu.Lock()
				defer f.mu.Unlock()
				ds := a[len(a)-1]
				var out []string
				if a[3] == "bookmark" {
					for bm := range f.bookmarks {
						if strings.HasPrefix(bm, ds+"#") || (a[len(a)-2] == "-r" && strings.HasPrefix(bm, ds)) {
							out = append(out, bm)
						}
					}
				} else {
					for _, sn := range f.zsnaps[ds] {
						out = append(out, ds+"@"+sn)
					}
				}
				sort.Strings(out)
				return execx.Result{Stdout: strings.Join(out, "\n") + "\n"}, nil
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			parent := a[len(a)-1]
			out := []string{parent}
			for cl := range f.clones {
				if strings.HasPrefix(cl, parent+"/") {
					out = append(out, cl)
				}
			}
			sort.Strings(out)
			return execx.Result{Stdout: strings.Join(out, "\n") + "\n"}, nil
		case "get":
			prop, name := a[len(a)-2], a[len(a)-1]
			f.mu.Lock()
			defer f.mu.Unlock()
			switch prop {
			case "volsize":
				return execx.Result{Stdout: "16777216\n"}, nil
			case "guid":
				if g, ok := f.bookmarks[name]; ok {
					return execx.Result{Stdout: g + "\n"}, nil
				}
				return execx.Result{ExitCode: 1}, &execx.ExitError{Name: "zfs", ExitCode: 1, Stderr: "dataset does not exist"}
			}
			return execx.Result{Stdout: "0\n"}, nil
		case "bookmark":
			if err := f.record("bookmark " + a[2]); err != nil {
				return execx.Result{}, err
			}
			f.mu.Lock()
			f.bookmarks[a[2]] = fmt.Sprintf("guid-%d", len(f.events))
			f.mu.Unlock()
			return execx.Result{}, nil
		case "version", "create":
			return execx.Result{Stdout: "0\n"}, nil
		case "clone":
			target := a[len(a)-1]
			if err := f.record("clone " + filepath.Base(target)); err != nil {
				return execx.Result{}, err
			}
			f.mu.Lock()
			f.clones[target] = true
			f.mu.Unlock()
			dev := filepath.Join(f.devRoot, target)
			os.MkdirAll(filepath.Dir(dev), 0o700)
			// 16 MiB device = 4 chunks; chunk 1 and 2 contain data, 0 and 3 are zero.
			img := make([]byte, 16<<20)
			copy(img[5<<20:], "DATA-ONE")
			copy(img[9<<20:], "DATA-TWO")
			os.WriteFile(dev, img, 0o600)
			return execx.Result{}, nil
		case "destroy":
			if strings.Contains(a[1], "#") {
				if err := f.record("destroy-bookmark " + a[1]); err != nil {
					return execx.Result{}, err
				}
				f.mu.Lock()
				delete(f.bookmarks, a[1])
				f.mu.Unlock()
				return execx.Result{}, nil
			}
			if err := f.record("destroy " + filepath.Base(a[1])); err != nil {
				return execx.Result{}, err
			}
			f.mu.Lock()
			delete(f.clones, a[1])
			f.mu.Unlock()
			return execx.Result{}, nil
		}
	case "mount":
		dir := a[len(a)-1]
		if err := f.record("mount " + filepath.Base(dir)); err != nil {
			return execx.Result{}, err
		}
		f.mu.Lock()
		f.mounts[dir] = true
		f.mu.Unlock()
		return execx.Result{}, nil
	case "umount":
		if err := f.record("umount " + filepath.Base(a[0])); err != nil {
			return execx.Result{}, err
		}
		f.mu.Lock()
		delete(f.mounts, a[0])
		f.mu.Unlock()
		return execx.Result{}, nil
	case "/hooks/ok", "/hooks/fail":
		env := strings.Join(c.Env, " ")
		phase := ""
		for _, kv := range c.Env {
			if strings.HasPrefix(kv, "INCUS_PBS_PHASE=") {
				phase = strings.TrimPrefix(kv, "INCUS_PBS_PHASE=")
			}
		}
		if err := f.record("hook " + strings.Join(a, " ") + " phase=" + phase); err != nil || c.Name == "/hooks/fail" {
			return execx.Result{ExitCode: 3, Stderr: "boom " + env}, &execx.ExitError{Name: c.Name, ExitCode: 3}
		}
		return execx.Result{Stdout: env}, nil
	case "proxmox-backup-client":
		switch a[0] {
		case "version":
			return execx.Result{Stdout: "client version: 4.2.8"}, nil
		case "snapshot":
			if f.failOn["pbs-unreachable"] {
				return execx.Result{ExitCode: 255}, &execx.ExitError{Name: c.Name, ExitCode: 255, Stderr: "Error: permission check failed"}
			}
			return execx.Result{Stdout: "[]"}, nil
		case "backup":
			f.mu.Lock()
			f.pbcArgs, f.pbcEnv = a, c.Env
			if f.archives == nil { // accumulates over all groups (one call per group)
				f.archives = map[string]string{}
			}
			f.metadata = map[string]string{}
			var callArchives []string
			guestSrc := ""
			for _, spec := range a[1:] {
				if strings.HasPrefix(spec, "--") {
					break
				}
				name, src, _ := strings.Cut(spec, ":")
				f.archives[name] = src
				callArchives = append(callArchives, name)
				if name == GuestManifestArchive {
					b, _ := os.ReadFile(src)
					guestSrc = string(b)
				}
				if name == "incus-metadata.pxar" || name == "host-config.pxar" {
					filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
						if err == nil && !info.IsDir() {
							b, _ := os.ReadFile(p)
							rel, _ := filepath.Rel(src, p)
							f.metadata[name+"/"+rel] = string(b)
						}
						return nil
					})
				}
			}
			typ, id := "", ""
			for i := 0; i+1 < len(a); i++ {
				switch a[i] {
				case "--backup-type":
					typ = a[i+1]
				case "--backup-id":
					id = a[i+1]
				}
			}
			call := pbcCall{group: typ + "/" + id, archives: callArchives}
			if guestSrc != "" {
				if f.guest == nil {
					f.guest = map[string]string{}
				}
				f.guest[call.group] = guestSrc
			}
			sort.Strings(call.archives)
			f.calls = append(f.calls, call)
			f.mu.Unlock()
			if err := f.record("pbc backup " + typ + "/" + id); err != nil {
				return execx.Result{ExitCode: 255, Stderr: "Error: broken pipe"}, &execx.ExitError{Name: c.Name, ExitCode: 255}
			}
			if f.pbcHang {
				<-ctx.Done()
				return execx.Result{ExitCode: 130}, ctx.Err()
			}
			return execx.Result{Stderr: "Starting backup: [incus]:" + typ + "/" + id + "/2026-11-24T02:30:00Z\n" +
				"ct_default_c1.ppxar: had to backup 1 MiB of 1 GiB (compressed 1 MiB) in 1.00 s (average 1 MiB/s)\n"}, nil
		}
	}
	f.t.Errorf("unexpected command %s %q", c.Name, a)
	return execx.Result{}, errors.New("unexpected command")
}

func (f *fakeHost) indexOf(prefix string) int {
	for i, e := range f.events {
		if strings.HasPrefix(e, prefix) {
			return i
		}
	}
	return -1
}

func (f *fakeHost) count(prefix string) int {
	n := 0
	for _, e := range f.events {
		if strings.HasPrefix(e, prefix) {
			n++
		}
	}
	return n
}

func (f *fakeHost) assertClean(t *testing.T) {
	t.Helper()
	for k, s := range f.snapshots {
		if len(s) != 0 {
			t.Errorf("leftover snapshots %s: %v", k, s)
		}
	}
	if len(f.clones) != 0 {
		t.Errorf("leftover clones: %v", f.clones)
	}
	if len(f.mounts) != 0 {
		t.Errorf("leftover mounts: %v", f.mounts)
	}
}

func newEngine(t *testing.T, f *fakeHost, mutate func(*config.Config)) *Engine {
	t.Helper()
	dir := t.TempDir()
	pw := filepath.Join(dir, "token")
	key := filepath.Join(dir, "key.json")
	os.WriteFile(pw, []byte("TOKENSECRET\n"), 0o600)
	os.WriteFile(key, []byte(`{"kdf":null,"data":"KEYMATERIAL"}`), 0o600)
	hostFile := filepath.Join(dir, "hostname")
	os.WriteFile(hostFile, []byte("incus-host\n"), 0o644)

	cfg := config.Default()
	cfg.PBS.Repository = "backup@pbs!incus@pbs:ds"
	cfg.PBS.PasswordFile = pw
	cfg.PBS.EncryptionKeyFile = key
	cfg.PBS.Namespace = "incus"
	cfg.PBS.BackupID = "h1"
	cfg.Storage.StagingDirectory = filepath.Join(dir, "staging")
	cfg.Storage.StateDirectory = filepath.Join(dir, "state")
	cfg.Backup.LockFile = filepath.Join(dir, "lock")
	cfg.Backup.HostConfigFiles = []string{hostFile, filepath.Join(dir, "does-not-exist")}
	if mutate != nil {
		mutate(&cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return &Engine{
		Cfg: cfg, Incus: f, X: f, Version: "test",
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		PBC:       &pbc.Client{Repository: cfg.PBS.Repository, PasswordFile: pw, X: f},
		MountInfo: f.mountinfo, DevRoot: f.devRoot,
		Hostname: func() (string, error) { return "incus-host", nil },
	}
}

func (f *fakeHost) DaemonVolumes() (map[string]string, error) {
	return map[string]string{"default/backups": "storage.backups_volume"}, nil
}

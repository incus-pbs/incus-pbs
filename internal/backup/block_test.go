package backup

import (
	"context"
	"errors"
	"io"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/incus-pbs/incus-pbs/internal/config"
	"github.com/incus-pbs/incus-pbs/internal/pbsqemu"
)

// fakeLib simulates libproxmox-backup-qemu.
type fakeLib struct {
	loadErr     error
	canIncr     bool
	finishErr   error
	state       []byte
	imported    []byte
	registered  map[string]bool // device -> incremental
	writes      map[string][]string
	devs        []string
	opened      int
	aborted     bool
	backupTimes []int64
	backupIDs   []string
	curID       string
	blobs       map[string][]byte // backupID/name -> data
	hang        chan struct{}     // non-nil: Write blocks until Abort (a stalled connection)
}

func newFakeLib() *fakeLib {
	return &fakeLib{canIncr: true, registered: map[string]bool{}, writes: map[string][]string{}}
}

func (l *fakeLib) Load() error                  { return l.loadErr }
func (l *fakeLib) Version() string              { return "2.0.4-test" }
func (l *fakeLib) ImportState(b []byte) error   { l.imported = b; return nil }
func (l *fakeLib) ExportState() ([]byte, error) { return []byte("STATE"), nil }
func (l *fakeLib) Open(o pbsqemu.Options) (BlockSession, error) {
	l.opened++
	l.backupTimes = append(l.backupTimes, o.BackupTime)
	l.backupIDs = append(l.backupIDs, o.BackupID)
	l.curID = o.BackupID
	if o.Password != "TOKENSECRET" {
		return nil, errors.New("bad password")
	}
	return &fakeSess{l: l}, nil
}

type fakeSess struct{ l *fakeLib }

func (s *fakeSess) CanIncremental(string, uint64) bool { return s.l.canIncr }
func (s *fakeSess) RegisterImage(dev string, size uint64, incr bool) (uint8, error) {
	s.l.registered[dev] = incr
	s.l.devs = append(s.l.devs, dev)
	return uint8(len(s.l.devs) - 1), nil
}
func (s *fakeSess) Write(dev uint8, off uint64, data []byte, size uint64) (bool, error) {
	if s.l.hang != nil {
		<-s.l.hang
		return false, errors.New("backup aborted")
	}
	kind := "data"
	if data == nil {
		kind = "zero"
	}
	name := s.l.devs[dev]
	s.l.writes[name] = append(s.l.writes[name], kind+"@"+itoa(off/pbsqemu.ChunkSize))
	return data != nil, nil
}
func (s *fakeSess) CloseImage(uint8) error { return nil }
func (s *fakeSess) Finish() error          { return s.l.finishErr }
func (s *fakeSess) AddConfig(name string, data []byte) error {
	if s.l.blobs == nil {
		s.l.blobs = map[string][]byte{}
	}
	s.l.blobs[s.l.curID+"/"+name] = data
	return nil
}
func (s *fakeSess) Abort(string) {
	s.l.aborted = true
	if s.l.hang != nil {
		close(s.l.hang)
	}
}
func (s *fakeSess) Close() {}

func itoa(u uint64) string { return strings.TrimSpace(strings.Repeat(" ", 0) + string(rune('0'+u))) }

// diffWrites returns a DiffFunc reporting a write in chunk 2 (offset 8 MiB).
func diffWrites(dump string) DiffFunc {
	return func(context.Context, string, string) (io.ReadCloser, func() error, error) {
		return io.NopCloser(strings.NewReader(dump)), func() error { return nil }, nil
	}
}

const chunk2Dump = "BEGIN record\nWRITE object = 1 offset = 9437184 logical_size = 16384\nFREE object = 1 offset = 12582912 length = 4194304\nEND checksum = x\n"

func blockEngine(t *testing.T, f *fakeHost, lib *fakeLib, mutate func(*config.Config)) *Engine {
	e := newEngine(t, f, func(c *config.Config) {
		c.Storage.StateDirectory = t.TempDir()
		if mutate != nil {
			mutate(c)
		}
	})
	e.Block = lib
	e.Diff = diffWrites(chunk2Dump)
	return e
}

func TestBlockFirstRunFullThenIncremental(t *testing.T) {
	f := newFakeHost(t)
	lib := newFakeLib()
	f.volumes["default"][1].UsedBy = []string{"/1.0/instances/v1"} // rawdisk belongs to v1's group
	e := blockEngine(t, f, lib, nil)

	rep, err := e.Run(context.Background(), Selection{})
	if err != nil || rep.ExitCode != ExitOK {
		t.Fatalf("run1: %v", err)
	}
	blockSnap := rep.ArchiveSnapshots["vm_default_v1.img"]
	// Images go through the library, not the client.
	if _, ok := f.archives["vm_default_v1.img"]; ok {
		t.Fatal("vm image must not be passed to proxmox-backup-client")
	}
	if !strings.HasPrefix(blockSnap, "vm/v1/") || rep.ArchiveSnapshots["vol_default_default_rawdisk.img"] != blockSnap {
		t.Fatalf("block archives: %v", rep.ArchiveSnapshots)
	}
	if lib.registered["vm_default_v1"] || len(lib.writes["vm_default_v1"]) != 4 {
		t.Fatalf("first run must be a full read: %v %v", lib.registered, lib.writes)
	}
	// zero chunks 0 and 3 are written as NULL (zero chunk), 1 and 2 with data
	if got := lib.writes["vm_default_v1"]; !slices.Equal(got, []string{"zero@0", "data@1", "data@2", "zero@3"}) {
		t.Fatalf("writes: %v", got)
	}
	if gm := string(lib.blobs["v1/"+GuestManifestArchive]); !strings.Contains(gm, `"group": "vm/v1"`) || !strings.Contains(gm, `"rawdisk"`) {
		t.Fatalf("block path must store the guest manifest blob: %q (blobs %v)", gm, maps.Keys(lib.blobs))
	}
	if f.metadata["incus-metadata.pxar/manifest.json"] == "" || !strings.Contains(f.metadata["incus-metadata.pxar/manifest.json"], blockSnap) {
		t.Fatal("manifest must reference the block snapshot")
	}
	bm1 := "tank/virtual-machines/v1.block#incus-pbs-" + rep.RunID
	if _, ok := f.bookmarks[bm1]; !ok {
		t.Fatalf("bookmark missing: %v", f.bookmarks)
	}
	f.assertClean(t)

	// Second run: only the chunks ZFS reports.
	lib2 := newFakeLib()
	e.Block = lib2
	e.Now = func() time.Time { return time.Now().Add(time.Minute) }
	rep2, err := e.Run(context.Background(), Selection{})
	if err != nil {
		t.Fatal(err)
	}
	if string(lib2.imported) != "STATE" {
		t.Fatal("library state must be imported from the previous run")
	}
	if !lib2.registered["vm_default_v1"] {
		t.Fatal("second run must be incremental")
	}
	if got := lib2.writes["vm_default_v1"]; !slices.Equal(got, []string{"data@2", "zero@3"}) {
		t.Fatalf("incremental writes: %v", got)
	}
	var st BlockStats
	for _, b := range rep2.BlockStats {
		if b.Archive == "vm_default_v1.img" {
			st = b
		}
	}
	if st.Mode != "incremental" || st.ChunksRead != 1 || st.ChunksZero != 1 {
		t.Fatalf("stats: %+v", st)
	}
	if _, ok := f.bookmarks[bm1]; ok {
		t.Fatal("previous bookmark must be replaced")
	}
	if _, ok := f.bookmarks["tank/virtual-machines/v1.block#incus-pbs-"+rep2.RunID]; !ok {
		t.Fatal("new bookmark missing")
	}
}

func TestBlockFallbacks(t *testing.T) {
	cases := map[string]struct {
		prep   func(f *fakeHost, lib *fakeLib, e *Engine)
		reason string
	}{
		"library chain mismatch": {func(f *fakeHost, lib *fakeLib, e *Engine) { lib.canIncr = false }, "previous PBS index"},
		"bookmark gone": {func(f *fakeHost, lib *fakeLib, e *Engine) {
			for k := range f.bookmarks {
				delete(f.bookmarks, k)
			}
		}, "bookmark missing"},
		"unreadable stream": {func(f *fakeHost, lib *fakeLib, e *Engine) { e.Diff = diffWrites("garbage") }, "incomplete send stream"},
		"periodic full": {func(f *fakeHost, lib *fakeLib, e *Engine) {
			e.Now = func() time.Time { return time.Now().Add(8 * 24 * time.Hour) }
		}, "periodic full"},
		"user snapshot (auto)": {func(f *fakeHost, lib *fakeLib, e *Engine) {
			f.zsnaps["tank/virtual-machines/v1.block"] = []string{"snapshot-before-upgrade"}
		}, "user snapshots present"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeHost(t)
			e := blockEngine(t, f, newFakeLib(), nil)
			if _, err := e.Run(context.Background(), Selection{Instances: []string{"v1"}}); err != nil {
				t.Fatal(err)
			}
			lib := newFakeLib()
			e.Block = lib
			if e.Now == nil {
				e.Now = func() time.Time { return time.Now().Add(time.Minute) }
			}
			c.prep(f, lib, e)
			rep, err := e.Run(context.Background(), Selection{Instances: []string{"v1"}})
			if err != nil {
				t.Fatal(err)
			}
			if lib.registered["vm_default_v1"] || len(lib.writes["vm_default_v1"]) != 4 {
				t.Fatalf("must fall back to a full read: %v", lib.writes)
			}
			if !strings.Contains(rep.BlockStats[0].Reason, c.reason) {
				t.Fatalf("reason %q, want %q", rep.BlockStats[0].Reason, c.reason)
			}
			if name == "user snapshot (auto)" {
				for bm := range f.bookmarks {
					if strings.HasPrefix(bm, "tank/virtual-machines/v1.block#") {
						t.Fatalf("auto mode must not keep bookmarks next to user snapshots: %v", f.bookmarks)
					}
				}
			}
		})
	}
}

func TestBlockAlwaysModeKeepsBookmarkWithUserSnapshots(t *testing.T) {
	f := newFakeHost(t)
	e := blockEngine(t, f, newFakeLib(), func(c *config.Config) { c.Backup.BlockIncremental = "always" })
	f.zsnaps["tank/virtual-machines/v1.block"] = []string{"snapshot-mine"}
	e.Run(context.Background(), Selection{Instances: []string{"v1"}})
	lib := newFakeLib()
	e.Block, e.Now = lib, func() time.Time { return time.Now().Add(time.Minute) }
	if _, err := e.Run(context.Background(), Selection{Instances: []string{"v1"}}); err != nil {
		t.Fatal(err)
	}
	if !lib.registered["vm_default_v1"] {
		t.Fatal("always mode must stay incremental despite user snapshots")
	}
}

func TestBlockLibraryFailureFallsBackToClient(t *testing.T) {
	f := newFakeHost(t)
	lib := newFakeLib()
	lib.finishErr = errors.New("connection reset")
	e := blockEngine(t, f, lib, nil)
	rep, err := e.Run(context.Background(), Selection{})
	if err != nil || rep.ExitCode != ExitOK {
		t.Fatalf("fallback run must succeed: %v", err)
	}
	if !lib.aborted || callFor(f, "vm/v1") == nil {
		t.Fatalf("library session must be aborted and not referenced: %+v", rep)
	}
	if _, ok := f.archives["vm_default_v1.img"]; !ok {
		t.Fatal("image must be uploaded by proxmox-backup-client instead")
	}
	if len(f.bookmarks) != 0 {
		t.Fatal("no bookmark may be created without a finished block snapshot")
	}
}

func TestBlockStalledLibraryIsAbortedByTimeout(t *testing.T) {
	f := newFakeHost(t)
	lib := newFakeLib()
	lib.hang = make(chan struct{})
	e := blockEngine(t, f, lib, func(c *config.Config) { c.PBS.Timeout = config.Duration(50 * time.Millisecond) })
	done := make(chan struct{})
	var rep *Report
	var err error
	go func() { defer close(done); rep, err = e.Run(context.Background(), Selection{}) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a blocking library call must be aborted by pbs.timeout")
	}
	if err != nil || rep.ExitCode != ExitOK || !lib.aborted {
		t.Fatalf("stalled session must be aborted and fall back to the client: %v %+v", err, rep)
	}
	if _, ok := f.archives["vm_default_v1.img"]; !ok {
		t.Fatal("image must be uploaded by proxmox-backup-client instead")
	}
	f.assertClean(t)
}

func TestBlockDisabledOrUnavailable(t *testing.T) {
	for name, mut := range map[string]func(*config.Config, *fakeLib){
		"off":         func(c *config.Config, l *fakeLib) { c.Backup.BlockIncremental = "off" },
		"unavailable": func(c *config.Config, l *fakeLib) { l.loadErr = errors.New("not installed") },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeHost(t)
			lib := newFakeLib()
			e := blockEngine(t, f, lib, func(c *config.Config) { mut(c, lib) })
			if _, err := e.Run(context.Background(), Selection{}); err != nil {
				t.Fatal(err)
			}
			if lib.opened != 0 || f.archives["vm_default_v1.img"] == "" {
				t.Fatal("must use proxmox-backup-client")
			}
		})
	}
}

func TestReleaseBookmarks(t *testing.T) {
	f := newFakeHost(t)
	f.volumes["default"][1].UsedBy = []string{"/1.0/instances/v1"} // rawdisk in v1's group, so it is tracked too
	e := blockEngine(t, f, newFakeLib(), nil)
	e.Run(context.Background(), Selection{})
	f.bookmarks["tank/other#not-ours"] = "x"
	rel, err := e.ReleaseBookmarks(context.Background(), false)
	if err != nil || len(rel) != 2 {
		t.Fatalf("released %v %v", rel, err)
	}
	if _, ok := f.bookmarks["tank/other#not-ours"]; !ok || len(f.bookmarks) != 1 {
		t.Fatalf("foreign bookmark touched or ours left: %v", f.bookmarks)
	}
}

package backup

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/incus-pbs/incus-pbs/internal/cbt"
	"github.com/incus-pbs/incus-pbs/internal/incus"
	"github.com/incus-pbs/incus-pbs/internal/pbsqemu"
	"github.com/incus-pbs/incus-pbs/internal/zfs"
)

// Changed-block tracking for VM disks and block volumes.
//
// After every successful upload a ZFS bookmark of the backed-up snapshot is
// kept on the source dataset (costs no space). The next run asks ZFS for the
// changes since that bookmark (`zfs send -i`), and writes only those 4 MiB
// chunks through libproxmox-backup-qemu, which seeds the new image index from
// the previous PBS snapshot. Every uncertainty falls back to a full read:
// missing/replaced bookmark, size change, library chain check failure,
// unparsable stream, periodic full interval.
//
// The images of a run go into one PBS snapshot of type "vm"
// (vm/<backup_id>/<time>), referenced from the host snapshot's manifest.

// BlockSession is the subset of a pbsqemu session the engine uses.
type BlockSession interface {
	CanIncremental(device string, size uint64) bool
	RegisterImage(device string, size uint64, incremental bool) (uint8, error)
	Write(dev uint8, offset uint64, data []byte, size uint64) (bool, error)
	CloseImage(dev uint8) error
	Finish() error
	AddConfig(name string, data []byte) error
	Abort(reason string)
	Close()
}

// BlockBackend creates sessions (libproxmox-backup-qemu in production).
type BlockBackend interface {
	Load() error
	Version() string
	ImportState([]byte) error
	ExportState() ([]byte, error)
	Open(pbsqemu.Options) (BlockSession, error)
}

type libBackend struct{}

func (libBackend) Load() error                                  { return pbsqemu.Load() }
func (libBackend) Version() string                              { return pbsqemu.Version() }
func (libBackend) ImportState(b []byte) error                   { return pbsqemu.ImportState(b) }
func (libBackend) ExportState() ([]byte, error)                 { return pbsqemu.ExportState() }
func (libBackend) Open(o pbsqemu.Options) (BlockSession, error) { return pbsqemu.New(o) }

// DiffFunc streams `zfs send -i from to | zstream dump -v`.
type DiffFunc func(ctx context.Context, from, to string) (io.ReadCloser, func() error, error)

// ZFSDiff is the production DiffFunc.
func ZFSDiff(ctx context.Context, from, to string) (io.ReadCloser, func() error, error) {
	send := exec.CommandContext(ctx, "zfs", "send", "-i", from, to)
	dump := exec.CommandContext(ctx, "zstream", "dump", "-v")
	var sendErr, dumpErr strings.Builder
	send.Stderr, dump.Stderr = &sendErr, &dumpErr
	pipe, err := send.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	dump.Stdin = pipe
	out, err := dump.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	if err := send.Start(); err != nil {
		return nil, nil, err
	}
	if err := dump.Start(); err != nil {
		_ = send.Process.Kill()
		_ = send.Wait()
		return nil, nil, err
	}
	// dump owns the read end now. Keeping the parent's copy open would leave
	// zfs send blocked on a full pipe (instead of SIGPIPE) if dump exits early.
	_ = pipe.Close()
	return out, func() error {
		e1, e2 := send.Wait(), dump.Wait()
		if e1 != nil || e2 != nil {
			return fmt.Errorf("zfs send: %v %s / zstream: %v %s", e1, sendErr.String(), e2, dumpErr.String())
		}
		return nil
	}, nil
}

// BlockStats describes how one block image was backed up.
type BlockStats struct {
	Archive        string  `json:"archive"`
	Mode           string  `json:"mode"` // incremental | full
	Reason         string  `json:"reason,omitempty"`
	Size           uint64  `json:"size"`
	Chunks         uint64  `json:"chunks"`
	ChunksRead     int     `json:"chunks_read"`
	ChunksZero     int     `json:"chunks_zero"`
	ChunksUploaded int     `json:"chunks_uploaded"`
	Seconds        float64 `json:"seconds"`
}

// BlockState persists the change-tracking chain (state_directory/block-state.json).
type BlockState struct {
	LibState string                 `json:"lib_state"` // proxmox_export_state, base64
	Snapshot string                 `json:"snapshot"`  // last vm/<id>/<time> written
	Devices  map[string]DeviceState `json:"devices"`
}

// DeviceState is the chain state of one block image.
type DeviceState struct {
	Dataset      string    `json:"dataset"`
	Bookmark     string    `json:"bookmark"`
	BookmarkGUID string    `json:"bookmark_guid"`
	VolSize      uint64    `json:"vol_size"`
	LastFull     time.Time `json:"last_full"`
}

const blockStateFile = "block-state.json"

func loadBlockState(dir string) *BlockState {
	st := &BlockState{Devices: map[string]DeviceState{}}
	if b, err := os.ReadFile(filepath.Join(dir, blockStateFile)); err == nil {
		_ = json.Unmarshal(b, st)
	}
	if st.Devices == nil {
		st.Devices = map[string]DeviceState{}
	}
	return st
}

func (st *BlockState) save(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, blockStateFile), b, 0o600)
}

// blockImage is a block device prepared for upload.
type blockImage struct {
	device   string // archive base, e.g. vm_default_v1 (archive vm_default_v1.img)
	dataset  string // source zvol (Incus-managed)
	snapshot string // dataset@snapshot-<run>
	dev      string // read-only clone device
	pool     incus.Pool
	itemIdx  int
	size     uint64
	plan     *cbt.Result // nil = full
	reason   string
}

func (e *Engine) blockBackend() BlockBackend {
	if e.Block != nil {
		return e.Block
	}
	return libBackend{}
}

func (e *Engine) diff() DiffFunc {
	if e.Diff != nil {
		return e.Diff
	}
	return ZFSDiff
}

// blockAvailable reports whether changed-block tracking can be used in this
// run (enabled and library loadable) and loads the persisted chain state.
func (e *Engine) blockAvailable(rep *Report, log *slog.Logger) (BlockBackend, *BlockState, bool) {
	if e.Cfg.Backup.BlockIncremental == "off" {
		return nil, nil, false
	}
	backend := e.blockBackend()
	if lerr := backend.Load(); lerr != nil {
		log.Info("block_incremental_unavailable", "event", "block_incremental_unavailable", "reason", lerr.Error())
		return nil, nil, false
	}
	rep.Versions["libproxmox-backup-qemu"] = backend.Version()
	st := loadBlockState(e.Cfg.Storage.StateDirectory)
	if st.LibState != "" {
		if b, derr := base64.StdEncoding.DecodeString(st.LibState); derr == nil {
			_ = backend.ImportState(b)
		}
	}
	return backend, st, true
}

// collectBlockImages returns the .img archives of prepared targets.
func collectBlockImages(runID string, results []prepared, rep *Report) []*blockImage {
	var imgs []*blockImage
	for _, r := range results {
		if r.err != nil {
			continue
		}
		for _, a := range r.archives {
			if !strings.HasSuffix(a.Name, ".img") || len(r.datasets) == 0 {
				continue
			}
			ds := r.datasets[0] // VM: .block zvol first; block volume: its zvol
			idx := -1
			for i, it := range rep.Items {
				if it.Kind == r.t.Kind && it.Project == r.t.Project && it.Name == r.t.Name {
					idx = i
				}
			}
			imgs = append(imgs, &blockImage{device: strings.TrimSuffix(a.Name, ".img"), dataset: ds,
				snapshot: ds + incus.SnapshotSuffix(runID), dev: a.Source, pool: r.t.Pool, itemIdx: idx})
		}
	}
	return imgs
}

// planBlockImages determines size and change set (or full read) per image.
func (e *Engine) planBlockImages(ctx context.Context, imgs []*blockImage, st *BlockState, now time.Time, log *slog.Logger) error {
	for _, im := range imgs {
		z := e.ZFS(im.pool)
		// The size of the snapshot that is read, not of the live zvol: a
		// resize after the snapshot must neither truncate nor overrun the image.
		v, perr := z.Prop(ctx, im.snapshot, "volsize")
		if perr != nil {
			return fmt.Errorf("volsize of %s: %w", im.snapshot, perr)
		}
		im.size, _ = strconv.ParseUint(v, 10, 64)
		if im.size == 0 {
			return fmt.Errorf("volsize of %s is 0", im.dataset)
		}
		im.plan, im.reason = e.planBlock(ctx, z, im, st.Devices[im.device], now, log)
	}
	return nil
}

// blockSession writes imgs (and optional small config blobs) into one PBS
// snapshot vm/<backupID>/<time> through the library. ok=false means the
// library failed before the snapshot was final (caller falls back to the
// client); err is only returned when the run was interrupted.
func (e *Engine) blockSession(ctx context.Context, backend BlockBackend, st *BlockState, backupID string,
	imgs []*blockImage, blobs map[string][]byte, runID string, now time.Time, rep *Report, log *slog.Logger) (snapshot string, ok bool, err error) {
	pw, err := os.ReadFile(e.Cfg.PBS.PasswordFile)
	if err != nil {
		log.Warn("block_session_failed", "event", "block_session_failed", "backup_id", backupID, "error", err, "fallback", "proxmox-backup-client full read")
		return "", false, nil
	}
	backupTime := e.now().Unix()
	sess, err := backend.Open(pbsqemu.Options{
		Repository: e.Cfg.PBS.Repository, Namespace: e.Cfg.PBS.Namespace, BackupID: backupID,
		BackupTime: backupTime, Password: strings.TrimSpace(string(pw)), KeyFile: e.Cfg.PBS.EncryptionKeyFile,
		Fingerprint: e.Cfg.PBS.Fingerprint,
	})
	if err != nil {
		log.Warn("block_session_failed", "event", "block_session_failed", "backup_id", backupID, "error", err, "fallback", "proxmox-backup-client full read")
		return "", false, nil
	}
	// Library calls block in C and do not see ctx. A watchdog aborts the
	// session on SIGTERM or pbs.timeout, which makes the pending call return
	// an error, so the run can unwind and clean up.
	sessCtx, cancelSess := context.WithCancel(ctx)
	if d := e.Cfg.PBS.Timeout.D(); d > 0 {
		sessCtx, cancelSess = context.WithTimeout(ctx, d)
	}
	var abortOnce sync.Once
	abort := func(reason string) { abortOnce.Do(func() { sess.Abort(reason) }) }
	watchDone := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(watchDone)
		select {
		case <-sessCtx.Done():
			abort("incus-pbs: " + sessCtx.Err().Error())
		case <-finished:
		}
	}()
	var stopOnce sync.Once
	stopWatch := func() {
		stopOnce.Do(func() {
			close(finished)
			<-watchDone
		})
	}
	defer func() {
		stopWatch() // never abort a closed handle
		cancelSess()
		sess.Close()
	}()
	runCtx := ctx
	ctx = sessCtx
	var stats []BlockStats
	fail := func(err error) (string, bool, error) {
		abort(err.Error())
		if runCtx.Err() != nil {
			return "", false, runCtx.Err() // interrupted: no fallback
		}
		log.Warn("block_upload_failed", "event", "block_upload_failed", "backup_id", backupID, "error", err, "fallback", "proxmox-backup-client full read")
		return "", false, nil
	}
	buf := make([]byte, pbsqemu.ChunkSize)
	for _, im := range imgs {
		t0 := e.now()
		bs := &BlockStats{Archive: im.device + ".img", Mode: "full", Reason: im.reason, Size: im.size,
			Chunks: (im.size + pbsqemu.ChunkSize - 1) / pbsqemu.ChunkSize}
		incremental := im.plan != nil
		if incremental && !sess.CanIncremental(im.device, im.size) {
			incremental, bs.Reason = false, "previous PBS index does not match the local chain"
		}
		if incremental {
			bs.Mode, bs.Reason = "incremental", ""
		}
		devID, err := sess.RegisterImage(im.device, im.size, incremental)
		if err != nil {
			return fail(err)
		}
		f, err := os.Open(im.dev)
		if err != nil {
			return fail(err)
		}
		write := func(c uint64, zero bool) error {
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("library session: %w", err)
			}
			off := c * pbsqemu.ChunkSize
			size := min(uint64(pbsqemu.ChunkSize), im.size-off)
			var data []byte
			if !zero || size != pbsqemu.ChunkSize {
				n, rerr := f.ReadAt(buf[:size], int64(off))
				if uint64(n) != size {
					return fmt.Errorf("reading %s at %d: %w", im.dev, off, rerr)
				}
				bs.ChunksRead++
				data = buf[:size]
				if size == pbsqemu.ChunkSize && allZero(data) {
					data = nil
				}
			}
			if data == nil {
				bs.ChunksZero++
			}
			up, werr := sess.Write(devID, off, data, size)
			if up {
				bs.ChunksUploaded++
			}
			return werr
		}
		var werr error
		if incremental {
			for _, c := range im.plan.Dirty {
				if werr = write(c, false); werr != nil {
					break
				}
			}
			for _, c := range im.plan.Zero {
				if werr != nil {
					break
				}
				werr = write(c, true)
			}
		} else {
			for c := uint64(0); c < bs.Chunks && werr == nil; c++ {
				werr = write(c, false)
			}
		}
		f.Close()
		if werr != nil {
			return fail(werr)
		}
		if err := sess.CloseImage(devID); err != nil {
			return fail(err)
		}
		bs.Seconds = round(e.now().Sub(t0).Seconds())
		stats = append(stats, *bs)
		log.Info("block_image_completed", "event", "block_image_completed", "backup_id", backupID, "archive", bs.Archive, "mode", bs.Mode,
			"reason", bs.Reason, "chunks", bs.Chunks, "read", bs.ChunksRead, "zero", bs.ChunksZero,
			"uploaded", bs.ChunksUploaded, "seconds", bs.Seconds)
	}
	names := make([]string, 0, len(blobs))
	for n := range blobs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if err := sess.AddConfig(n, blobs[n]); err != nil {
			return fail(err)
		}
	}
	if err := sess.Finish(); err != nil {
		return fail(err)
	}
	stopWatch() // the snapshot is final; nothing left to abort
	ctx = runCtx
	snapshot = fmt.Sprintf("vm/%s/%s", backupID, time.Unix(backupTime, 0).UTC().Format("2006-01-02T15:04:05Z"))
	for i, im := range imgs {
		if im.itemIdx >= 0 {
			rep.Items[im.itemIdx].Block = append(rep.Items[im.itemIdx].Block, stats[i])
		}
	}
	rep.BlockStats = append(rep.BlockStats, stats...)

	// Advance the chain only after the PBS snapshot is final.
	if libState, xerr := backend.ExportState(); xerr == nil {
		st.LibState = base64.StdEncoding.EncodeToString(libState)
	} else {
		st.LibState = "" // forces full reads next time; never a wrong incremental
	}
	st.Snapshot = snapshot
	for i, im := range imgs {
		prev := st.Devices[im.device]
		ds := DeviceState{Dataset: im.dataset, VolSize: im.size, LastFull: prev.LastFull}
		if im.plan == nil || stats[i].Mode == "full" {
			ds.LastFull = now
		}
		if e.Cfg.Backup.BlockIncremental == "auto" && strings.HasPrefix(im.reason, "user snapshots") {
			delete(st.Devices, im.device) // no bookmark while user snapshots exist
			continue
		}
		z := e.ZFS(im.pool)
		bm := im.dataset + "#" + zfs.BookmarkPrefix + runID
		if berr := z.Bookmark(ctx, im.snapshot, bm); berr != nil {
			// Keep the previous (older) bookmark: its change set is a superset, still correct.
			log.Warn("bookmark_failed", "event", "bookmark_failed", "dataset", im.dataset, "error", berr)
			continue
		}
		guid, gerr := z.Prop(ctx, bm, "guid")
		if gerr != nil || guid == "" {
			log.Warn("bookmark_guid_failed", "event", "bookmark_guid_failed", "bookmark", bm, "error", gerr)
		}
		ds.Bookmark, ds.BookmarkGUID = bm, guid
		st.Devices[im.device] = ds
		if old, lerr := z.Bookmarks(ctx, im.dataset); lerr == nil {
			for _, o := range old {
				if o != bm {
					_ = z.DestroyBookmark(ctx, o)
				}
			}
		}
	}
	if serr := st.save(e.Cfg.Storage.StateDirectory); serr != nil {
		log.Error("block_state_save_failed", "event", "block_state_save_failed", "error", serr)
	}
	return snapshot, true, nil
}

// planBlock decides incremental vs full for one image.
func (e *Engine) planBlock(ctx context.Context, z *zfs.ZFS, im *blockImage, prev DeviceState, now time.Time, log *slog.Logger) (*cbt.Result, string) {
	if e.Cfg.Backup.BlockIncremental == "auto" {
		snaps, err := z.Snapshots(ctx, im.dataset)
		if err != nil {
			return nil, "cannot list snapshots: " + err.Error()
		}
		for _, s := range snaps {
			if !incus.IsOwnSnapshot(strings.TrimPrefix(s, "snapshot-")) {
				// Our bookmarks would block `incus snapshot restore` of older snapshots.
				if bms, _ := z.Bookmarks(ctx, im.dataset); len(bms) > 0 {
					for _, b := range bms {
						_ = z.DestroyBookmark(ctx, b)
					}
					log.Info("bookmarks_released", "event", "bookmarks_released", "dataset", im.dataset, "reason", "user snapshot "+s)
				}
				return nil, "user snapshots present (incremental disabled so `incus snapshot restore` keeps working)"
			}
		}
	}
	switch {
	case prev.Bookmark == "":
		return nil, "no previous chain"
	case prev.Dataset != im.dataset:
		return nil, "dataset changed"
	case prev.VolSize != im.size:
		return nil, "volume size changed"
	case now.Sub(prev.LastFull) >= e.Cfg.Backup.BlockFullInterval.D():
		return nil, "periodic full read"
	}
	if guid, err := z.Prop(ctx, prev.Bookmark, "guid"); err != nil || guid != prev.BookmarkGUID {
		return nil, "bookmark missing or replaced"
	}
	rc, wait, err := e.diff()(ctx, prev.Bookmark, im.snapshot)
	if err != nil {
		return nil, "zfs send failed: " + err.Error()
	}
	res, perr := cbt.Parse(rc, pbsqemu.ChunkSize, im.size)
	_, _ = io.Copy(io.Discard, rc)
	rc.Close()
	werr := wait()
	switch {
	case perr != nil:
		return nil, "change set unreadable: " + perr.Error()
	case werr != nil:
		return nil, "change set failed: " + werr.Error()
	case res.Full:
		return nil, res.Reason
	}
	return res, ""
}

func (r *Report) blockMode(device string) string {
	for _, b := range r.BlockStats {
		if b.Archive == device+".img" {
			return b.Mode
		}
	}
	return ""
}

func allZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}

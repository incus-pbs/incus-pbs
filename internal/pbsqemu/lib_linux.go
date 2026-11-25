//go:build linux

// Linux implementation of the pbsqemu bindings (dlopen via purego).
package pbsqemu

import (
	"errors"
	"fmt"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

var (
	loadOnce sync.Once
	loadErr  error

	fnVersion          func() *byte
	fnFreeError        func(*byte)
	fnNewNS            func(repo, ns, backupID string, backupTime, chunkSize uint64, password, keyfile, keyPassword, masterKeyfile *byte, compress, encrypt bool, fingerprint *byte, err **byte) uintptr
	fnConnect          func(h uintptr, err **byte) int32
	fnCheckIncremental func(h uintptr, device string, size uint64) int32
	fnRegisterImage    func(h uintptr, device string, size uint64, incremental bool, err **byte) int32
	fnWriteData        func(h uintptr, devID uint8, data unsafe.Pointer, offset, size uint64, err **byte) int32
	fnCloseImage       func(h uintptr, devID uint8, err **byte) int32
	fnAddConfig        func(h uintptr, name string, data unsafe.Pointer, size uint64, err **byte) int32
	fnFinish           func(h uintptr, err **byte) int32
	fnAbort            func(h uintptr, reason string)
	fnDisconnect       func(h uintptr)
	fnExportState      func(size *uintptr) *byte
	fnImportState      func(buf unsafe.Pointer, size uintptr)
	fnFreeStateBuf     func(buf *byte)
)

// Load opens the library once. It returns an error when it is not installed.
func Load() error {
	loadOnce.Do(func() {
		lib, err := purego.Dlopen(LibName, purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			loadErr = fmt.Errorf("%s not available (install libproxmox-backup-qemu0): %w", LibName, err)
			return
		}
		reg := func(fptr any, name string) {
			if loadErr != nil {
				return
			}
			defer func() {
				if r := recover(); r != nil {
					loadErr = fmt.Errorf("symbol %s: %v", name, r)
				}
			}()
			purego.RegisterLibFunc(fptr, lib, name)
		}
		reg(&fnVersion, "proxmox_backup_qemu_version")
		reg(&fnFreeError, "proxmox_backup_free_error")
		reg(&fnNewNS, "proxmox_backup_new_ns")
		reg(&fnConnect, "proxmox_backup_connect")
		reg(&fnCheckIncremental, "proxmox_backup_check_incremental")
		reg(&fnRegisterImage, "proxmox_backup_register_image")
		reg(&fnWriteData, "proxmox_backup_write_data")
		reg(&fnCloseImage, "proxmox_backup_close_image")
		reg(&fnAddConfig, "proxmox_backup_add_config")
		reg(&fnFinish, "proxmox_backup_finish")
		reg(&fnAbort, "proxmox_backup_abort")
		reg(&fnDisconnect, "proxmox_backup_disconnect")
		reg(&fnExportState, "proxmox_export_state")
		reg(&fnImportState, "proxmox_import_state")
		reg(&fnFreeStateBuf, "proxmox_free_state_buf")
	})
	return loadErr
}

// Version returns the library version.
func Version() string {
	if Load() != nil {
		return ""
	}
	return goString(fnVersion())
}

// goString copies a NUL-terminated C string.
func goString(p *byte) string {
	if p == nil {
		return ""
	}
	n := 0
	for *(*byte)(unsafe.Add(unsafe.Pointer(p), n)) != 0 {
		n++
	}
	return string(unsafe.Slice(p, n))
}

func takeErr(e *byte) error {
	if e == nil {
		return errors.New("unknown error")
	}
	msg := goString(e)
	fnFreeError(e)
	return errors.New(msg)
}

func cstr(s string) *byte {
	if s == "" {
		return nil
	}
	b := append([]byte(s), 0)
	return &b[0]
}

// Session is one PBS backup snapshot of type "vm" written through the library.
type Session struct {
	h           uintptr
	HasPrevious bool
}

// ImportState restores the per-device checksum cache from an earlier process.
// Must be called before Connect.
func ImportState(b []byte) error {
	if err := Load(); err != nil {
		return err
	}
	if len(b) == 0 {
		return nil
	}
	fnImportState(unsafe.Pointer(&b[0]), uintptr(len(b)))
	return nil
}

// ExportState serializes the per-device checksum cache (after Finish).
func ExportState() ([]byte, error) {
	if err := Load(); err != nil {
		return nil, err
	}
	var n uintptr
	p := fnExportState(&n)
	if p == nil {
		return nil, errors.New("export_state returned NULL")
	}
	out := make([]byte, n)
	copy(out, unsafe.Slice(p, n))
	fnFreeStateBuf(p)
	return out, nil
}

// New creates and connects a session. The PBS snapshot is vm/<BackupID>/<BackupTime>.
func New(o Options) (*Session, error) {
	if err := Load(); err != nil {
		return nil, err
	}
	var e *byte
	h := fnNewNS(o.Repository, o.Namespace, o.BackupID, uint64(o.BackupTime), ChunkSize,
		cstr(o.Password), cstr(o.KeyFile), nil, nil, true, o.KeyFile != "", cstr(o.Fingerprint), &e)
	if h == 0 {
		return nil, fmt.Errorf("proxmox_backup_new_ns: %w", takeErr(e))
	}
	s := &Session{h: h}
	r := fnConnect(h, &e)
	if r < 0 {
		fnDisconnect(h)
		return nil, fmt.Errorf("connect: %w", takeErr(e))
	}
	s.HasPrevious = r == 1
	return s, nil
}

// CanIncremental reports whether the previous snapshot's index for device
// matches the imported checksum cache (same chain, size, crypt mode and key).
func (s *Session) CanIncremental(device string, size uint64) bool {
	return s.HasPrevious && fnCheckIncremental(s.h, device, size) > 0
}

// RegisterImage creates <device>.img.fidx and returns its dev id.
func (s *Session) RegisterImage(device string, size uint64, incremental bool) (uint8, error) {
	var e *byte
	r := fnRegisterImage(s.h, device, size, incremental, &e)
	if r < 0 {
		return 0, fmt.Errorf("register_image %s: %w", device, takeErr(e))
	}
	return uint8(r), nil
}

// Write uploads one chunk at a chunk-aligned offset. data == nil writes the
// zero chunk (only for full-size chunks). Returns true if the chunk was new.
func (s *Session) Write(dev uint8, offset uint64, data []byte, size uint64) (bool, error) {
	if offset%ChunkSize != 0 {
		return false, fmt.Errorf("write at unaligned offset %d", offset)
	}
	var e *byte
	var p unsafe.Pointer
	if data != nil {
		if uint64(len(data)) != size {
			return false, fmt.Errorf("data length %d != size %d", len(data), size)
		}
		p = unsafe.Pointer(&data[0])
	} else if size != ChunkSize {
		return false, errors.New("zero chunk must be full size")
	}
	r := fnWriteData(s.h, dev, p, offset, size, &e)
	if r < 0 {
		return false, fmt.Errorf("write_data at %d: %w", offset, takeErr(e))
	}
	return r > 0, nil
}

// CloseImage closes an image (updates the library's checksum cache).
func (s *Session) CloseImage(dev uint8) error {
	var e *byte
	if fnCloseImage(s.h, dev, &e) < 0 {
		return fmt.Errorf("close_image: %w", takeErr(e))
	}
	return nil
}

// AddConfig uploads a small blob "<name>.blob" into the snapshot.
func (s *Session) AddConfig(name string, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	var e *byte
	if fnAddConfig(s.h, name, unsafe.Pointer(&data[0]), uint64(len(data)), &e) < 0 {
		return fmt.Errorf("add_config %s: %w", name, takeErr(e))
	}
	return nil
}

// Finish uploads the manifest; only then the snapshot exists on PBS.
func (s *Session) Finish() error {
	var e *byte
	if fnFinish(s.h, &e) < 0 {
		return fmt.Errorf("finish: %w", takeErr(e))
	}
	return nil
}

// Abort aborts the server-side backup (the snapshot is discarded).
func (s *Session) Abort(reason string) { fnAbort(s.h, reason) }

// Close frees the handle.
func (s *Session) Close() {
	if s.h != 0 {
		fnDisconnect(s.h)
		s.h = 0
	}
}

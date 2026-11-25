// Package pbsqemu binds libproxmox-backup-qemu (the official library QEMU and
// Proxmox VE use to write VM disk images to PBS) via dlopen, without cgo.
// It is optional: without the library, incus-pbs falls back to
// proxmox-backup-client and reads block devices in full.
//
// The library supports incremental image backups: the new fixed index is
// seeded from the previous snapshot and only written chunks replace digests.
// Callers MUST write every chunk that changed since the previous backup.
package pbsqemu

// ChunkSize is PROXMOX_BACKUP_DEFAULT_CHUNK_SIZE.
const ChunkSize = 4 << 20

// LibName is the soname of the library.
const LibName = "libproxmox_backup_qemu.so.0"

// Options for a backup session (a PBS snapshot of type "vm").
type Options struct {
	Repository  string
	Namespace   string
	BackupID    string
	BackupTime  int64
	Password    string // API token secret, kept in memory only
	KeyFile     string // "" = unencrypted
	Fingerprint string
}

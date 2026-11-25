# Incremental VM disks and block volumes

Without change tracking, every run reads and hashes the whole VM disk, because `proxmox-backup-client` cannot know which blocks changed. That costs real time: on an i5-6500T (the class of the reference host), the client's SHA-256 runs at ~300 MB/s and ZSTD compression at ~260 MB/s, so an unchanged 8 GiB disk took ~65 s. 1 TiB of VM disks would take hours every night.

incus-pbs therefore tracks changes with ZFS and writes only changed chunks through **libproxmox-backup-qemu**. That is the official Proxmox library QEMU uses for incremental ("dirty bitmap") backups.

## How it works

```
run N    Incus snapshot → zvol clone → all 4 MiB chunks → PBS vm/<id>/<time>
         → ZFS bookmark  <zvol>#incus-pbs-<run>   (costs no space)
         → state: library chain checksum + bookmark GUID
run N+1  Incus snapshot
         → zfs send -i <bookmark> <snapshot> | zstream dump -v
             WRITE ranges → chunks to read
             FREE ranges covering whole chunks → zero chunks, nothing read
         → the library seeds the new index from run N and replaces only those chunks
```

- Each VM's block images (root disk and block volumes attached only to it) go into that VM's group as one snapshot (`vm/<id>/<time>`), together with its config as a `-config.tar.blob`. The library only writes `vm`-type snapshots, so standalone or shared block volumes are uploaded by the client into the run index without change tracking.
- The guest manifest `incus-guest.conf` is written into the same snapshot as a blob, so the VM is restorable from its own group. The run index's `manifest.json` still maps every archive of its run to its snapshot (`archive_snapshots`).
- The chain (bookmark + library state) only advances after the VM's snapshot is final.

## Measured on the reference host

| | Before (client) | With change tracking |
|---|---|---|
| Unchanged run, whole host (2 containers, 1 VM 8 GiB, 4 volumes) | 82 s | **7.8 s** |
| Of that, the 8 GiB VM disk | ~65 s | **0.05 s** |
| 200 MiB written in the VM | ~74 s | **13 s** (VM disk 3.5 s, 66 chunks read) |
| First (full) run | 82 s | 60 s (zero chunks are not uploaded) |

Correctness was verified by restoring images from the incremental chain: full, unchanged, after churn, after fallbacks. The images match the source snapshot's SHA-256, and a restored VM boots with identical file checksums.

## Requirements

- **Library:** `libproxmox-backup-qemu0`. It is not in the PBS client repository, only in the Proxmox VE one. Install just this package, for example:

  ```bash
  cat > /etc/apt/sources.list.d/pve-lib.sources <<EOF
  Types: deb
  URIs: http://download.proxmox.com/debian/pve
  Suites: trixie
  Components: pve-no-subscription
  Signed-By: /usr/share/keyrings/proxmox-archive-keyring.gpg
  EOF
  apt-get update && apt-get download libproxmox-backup-qemu0
  rm /etc/apt/sources.list.d/pve-lib.sources && apt-get update
  apt-get install ./libproxmox-backup-qemu0_*.deb     # depends only on libc, libssl, libzstd
  ```

  The library is loaded at runtime with dlopen; incus-pbs stays a static binary. Without the library, everything works as before (full reads through the client), and `incus-pbs check` says so.
- **ZFS tools:** `zstream` (part of zfsutils).
- **PBS permissions:** the same token as before. `DatastoreBackup` on the namespace covers the `vm/` group.

## Interaction with Incus snapshots

ZFS refuses to roll back past a bookmark. While our bookmark exists, `incus snapshot restore` of a snapshot **older than the last backup** fails with "more recent snapshots or bookmarks exist". This was verified with Incus 7.6. Hence the modes:

| `backup.block_incremental` | Behaviour |
|---|---|
| `auto` (default) | Change tracking only for zvols with **no other ZFS snapshots** (Incus manual or scheduled snapshots, sanoid, …). As soon as one appears, our bookmarks on that zvol are removed and that disk is read in full, so `incus snapshot restore` always works. It becomes incremental again once the snapshots are gone. |
| `always` | Change tracking regardless of snapshots. Restoring an older Incus snapshot needs `incus-pbs cleanup --release-bookmarks` first; the next backup then reads in full. |
| `off` | Never use change tracking. |

Containers and filesystem volumes are unaffected; they never get bookmarks.

## Safety: when it falls back to a full read

Every uncertainty means a full read, never a guessed incremental:

| Situation | Result |
|---|---|
| First run, new VM, or the dataset changed (rename, restore, move) | full read |
| Volume size changed | full read |
| Bookmark missing or replaced (GUID check) | full read |
| The library's chain check fails: the previous PBS snapshot's index checksum, crypt mode or key doesn't match the saved state (for example, the snapshot was pruned or a run crashed before saving) | full read |
| Change stream unreadable or incomplete (no `END` record), or it frees the zvol's data object | full read |
| `block_full_interval` (default 7 days) since the last full read | full read |
| Library error during upload | that session is aborted, and the images go through `proxmox-backup-client` in the same run |

If creating the new bookmark fails, the previous, older bookmark is kept. Its change set is a superset of the real changes, so the next run is still correct.

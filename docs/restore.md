# Restore

Every restore runs in two steps:
1. The required archives are restored from PBS into a private directory below `storage.staging_directory`. They are laid out exactly like an Incus backup tarball (`backup/index.yaml`, `backup/container/`, `backup/virtual-machine{/,.img}`, `backup/volume{/,.img}`).
2. That layout is streamed into Incus' official import API (the same one `incus import` uses).

As a result, configuration, devices and profiles are restored by Incus itself, onto **any** storage driver, and a backup taken from ZFS can be restored to a non-ZFS pool.

VM disks and block volumes are **streamed** from PBS straight into the import: no staging copy, and the download overlaps the import. Images written through [change tracking](block-incremental.md) live in a separate `vm/` snapshot; restore finds them through the manifest automatically.

All procedures below were verified on Incus 7.6 / PBS 4.2: containers, VMs, filesystem and block volumes, restores across projects, and disaster recovery under the original name.

## Safety rules

- **Existing instances or volumes are never overwritten.** Restore under a new `--name`, or delete the old one yourself after checking the restored copy.
- **Missing dependencies stop the restore before anything is imported.** This covers profiles, custom volumes and networks in the target project.
  - `--drop-missing-devices` restores without the missing *devices*, and the dropped devices are reported.
  - Missing profiles are always an error: recreate them first (see `restore host`).
- **Identity** means the MAC addresses, instance UUIDs and cloud-init instance-id. `--identity auto` (the default) handles it like this:
  - **Original instance still exists, or a new name is given:** a new identity is generated, so the copy can run next to the original. A MAC address set directly on a NIC device (`hwaddr:`) is removed too, so Incus generates a new one. When the original still exists, the copy also gets `boot.autostart=false`, so it doesn't start next to the original after a host reboot.
  - **Original instance is gone and the name is unchanged (disaster recovery):** the identity is kept, so DHCP leases and MACs stay the same.
  - `--identity keep` / `--identity new` force either behaviour. `keep` is refused while the original exists.
- **Instance snapshots are not restored.** Backups contain the current state only; the snapshot list is removed from the restored config.
- **Dependencies are checked before anything large is downloaded,** using the instance config from the metadata archive: profiles, volumes, networks, and block volumes that are already attached elsewhere. The `backup.yaml` from the archive is checked again afterwards.
- **Free space is checked before downloading.** The staging directory must hold the pxar archives; images are streamed. Incus itself spools the upload in `/var/lib/incus/backups` while importing, so plan for the instance size there.
- **Restores take the run lock**, so they never overlap with a backup or cleanup.
- With `--staging-only`, restore stops after step 1 and prints the directory for inspection. With `--keep-staging`, the directory is kept after import.

## Commands

```bash
incus-pbs restore list                          # run indexes, then every instance group with its snapshots
incus-pbs restore list web                      # backups of one instance, newest first
incus-pbs restore inspect latest                # newest run index: complete?, targets, archives, warnings
incus-pbs restore inspect ct/web/2026-11-24T02:21:13Z   # one guest snapshot's own manifest

# Container/VM, as a copy next to the original
incus-pbs restore instance latest web --name web-restored
# ... into another project / pool
incus-pbs restore instance 2026-11-24T02:30:00Z db --target-project staging --pool fast --name db-test
# ... from a specific instance snapshot (as shown by `restore list web`)
incus-pbs restore instance ct/web/2026-11-24T02:21:13Z web --name web-restored
# Disaster recovery: the original is gone, so restore under the same name with the same MACs
incus-pbs restore instance latest web

# Custom volumes (restore them BEFORE instances that use them)
incus-pbs restore volume latest webdata                      # original name, pool and project
incus-pbs restore volume latest webdata --name webdata-old   # next to the existing one
incus-pbs restore volume latest pvol --project prod --target-project test
```

`<snapshot>` can be one of these:

- `latest`.
- A time (`2026-11-24T02:30:00Z`): the newest backup at or before it.
- A guest snapshot (`ct/<id>/<time>`, `vm/<id>/<time>`).
- For volumes, a run index (`host/<backup_id>/<time>`).

Every guest snapshot carries its own manifest (`incus-guest.conf.blob`: the instance, the volumes stored with it, its Incus config and profiles). An instance is therefore always restored from its own group, independent of the run index. This matters with [per-instance schedules](../README.md#schedules): the newest run index usually does not contain every instance.

- **Instances:** `latest` and a time look at the newest snapshot of the instance's group (`ct|vm/<name>`, or `<project>_<name>` outside the default project; groups written by versions before 0.2 as `<project>-<name>` are still found). If the name exists in several projects, pass `--project`.
- **Volumes:** A volume used by exactly one instance lives in that instance's group. Shared and standalone volumes live in the run index. `latest` picks whichever is newer.
- **Other hosts:** Guest snapshots written by another host into the same namespace (a different `pbs.backup_id`) are ignored unless named explicitly.

Notes:
- VM backups are crash-consistent. Expect a filesystem journal replay on first boot, and check databases inside the guest, or use [hooks](hooks.md).
- A copy that runs next to the original on the same network needs a different name: Incus refuses two instances with the same DNS name on one bridge.
- The container archive contains `credentials/` (the instance's systemd credentials). Restored copies carry them as well.

## Restore tests

`restore test` runs a complete restore drill unattended: it restores a container or VM from PBS as a throwaway copy, starts it, checks that files exist inside it, and deletes it again.

```bash
incus-pbs restore test latest web --file /etc/hostname --file /srv/app/config.yml
incus-pbs restore test latest db  --file /etc/os-release --contains 'ID=debian' --timeout 10m
```

- The copy is named `restoretest-<instance>` (`--name` to override) and created next to the original (`--target-project`, `--pool`).
- An existing instance of that name is never overwritten, with one exception: a copy left behind by an earlier drill that was killed hard (SIGKILL, power loss). Such a copy carries the `user.incus-pbs.restore-test` marker, so the next drill stops and deletes it first and reports `replaced_stale_test_copy`. An instance without the marker is always refused and left untouched.
- **The copy is isolated.** Every device except the root disk (and cloud-init/agent config drives) is masked with a local `type: none` device, including devices inherited from profiles. The copy has no network, so it never clashes with the running original, and it never attaches the original's custom volumes. It always gets a new identity and `boot.autostart=false`.
- Profiles must still exist in the target project, as for `restore instance`.
- `--file` (repeatable, absolute paths) must exist as a regular file. Symlinks are followed inside the instance (up to 8 hops; relative targets resolve against the link's directory), so Debian's `/etc/os-release` → `/usr/lib/os-release` works. The result shows the final path as `resolved`. `--contains` additionally checks the content and needs exactly one `--file`. Files are read through the Incus file API; for VMs this waits for the incus-agent. A missing file fails at once; an agent that never answers fails after `--timeout` (default 5m, covering start-up and all checks).
- The copy is force-stopped and deleted afterwards, also when a check fails or the run is interrupted. `--keep` leaves it stopped for inspection.
- Copies carry the `user.incus-pbs.restore-test` config key and are never included in backups.
- Exit code 0 means restored, started and all files found; 1 means any step failed (including cleanup, which then names the instance to remove manually). A JSON result is printed either way.

A weekly drill as a systemd unit pair:

```ini
# /etc/systemd/system/incus-pbs-restore-test.service
[Unit]
Description=incus-pbs restore drill
After=incus.service

[Service]
Type=oneshot
ExecStart=/usr/bin/incus-pbs restore test latest web --file /etc/hostname

# /etc/systemd/system/incus-pbs-restore-test.timer
[Timer]
OnCalendar=Sun 06:00
Persistent=true

[Install]
WantedBy=timers.target
```

## Instant access, without a full restore

Usually you need a few files, not a whole instance:

```bash
incus-pbs restore mount latest web --dir /mnt/web          # container / fs volume: read-only FUSE mount
incus-pbs restore mount latest db  --dir /mnt/db           # VM disk: mapped + largest filesystem mounted read-only
incus-pbs restore mount latest db  --dir /mnt/db --partition 1
incus-pbs restore unmount /mnt/web
```

- **Containers and filesystem volumes:** a read-only FUSE mount of the archive.
- **VM disks and block volumes:** the image is mapped to a loop device (`proxmox-backup-client map`), and data is fetched from PBS on demand. The largest filesystem partition (or `--partition N`) is mounted with `ro,noload` (ext4) or `ro,norecovery` (xfs), so a crash-consistent image is never modified by a journal replay.
- On the reference host, both took under a second, independent of disk size. The partitions found are listed in the output. `restore unmount` takes the directory or a `/dev/loopN`.

## Single files with the client

You can also use the client directly:

```bash
export PBS_REPOSITORY='backup@pbs!incus@pbs:datastore' PBS_PASSWORD_FILE=/etc/incus-pbs/pbs-token
proxmox-backup-client catalog shell ct/web/<time> ct_default_web.pxar --ns incus --keyfile /etc/incus-pbs/encryption-key.json
proxmox-backup-client mount host/<id>/<time> vol_default_default_webdata.pxar /mnt/x --ns incus --keyfile …
```

Inside container archives, files live under `rootfs/`.

## Complete host recovery

Never copy an old Incus database onto a new installation. `restore host` builds a **recovery kit** and changes nothing:

```bash
incus-pbs restore host latest --dir /root/recovery
```

| File | Purpose |
|---|---|
| `incus-metadata/manifest.json` | What the snapshot contains, versions, warnings |
| `incus-metadata/incus-project-<p>.json` | Full API dump per project: instances, profiles, networks, pools, volumes (secrets redacted) |
| `host-config/` | Allowlisted host files (`/etc/subuid`, `/etc/hosts`, …), `zpool list -v`, `zfs list` |
| `preseed.yaml` | Suggested `incus admin init --preseed`: projects, storage pools, managed networks, profiles. **Review it**: disk paths and addresses must match the new host. |
| `RECOVERY.md` | Ordered plan with the exact `restore volume` / `restore instance` commands. Each instance comes from its newest guest snapshot (for `latest`), or from the newest one at or before the run index's time (for an explicit run index). Instances on their own schedule are covered either way. |

Procedure on a new host:

1. Install the OS, ZFS, Incus, proxmox-backup-client and incus-pbs. Put the config, token and **encryption key** in place (from your off-host copy).
2. Run `incus-pbs restore host latest --dir /root/recovery` and read `RECOVERY.md`.
3. Edit `preseed.yaml` (pool `source` → `/dev/disk/by-id/...` of the new disks), then `incus admin init --preseed < preseed.yaml`.
4. Run the `restore volume …` commands, then the `restore instance …` commands from `RECOVERY.md`. Instances keep their identity, because the originals don't exist on the new host.
5. Start and verify the instances, then `incus-pbs check` and `systemctl enable --now incus-pbs.timer`.

Consider a fresh `backup_id` on the new host if you want to keep the old host's backup group separate.

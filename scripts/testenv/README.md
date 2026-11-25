# Integration test environment

These scripts build a disposable environment for testing incus-pbs against real Incus, ZFS and PBS. The unit tests (`make test`) fake all three; use this environment for anything that touches the real tools: new Incus or PBS versions, restore paths, change tracking, crash recovery.

| Script | Runs on | Creates |
|---|---|---|
| `pve-create-vms.sh <ssh-pubkeys-file>` | a Proxmox VE host, as root | VM 9101 `incus-test` (4 vCPU, 6 GiB, nested KVM, 40 GiB root + 20 GiB disk for ZFS) and VM 9102 `pbs-test` (2 vCPU, 2 GiB, 32 GiB), both Debian 13 via cloud-init, tagged `incus-pbs-test` |
| `setup-pbs.sh` | `pbs-test`, as root | PBS 4, datastore `test` at `/srv/pbs-test`, namespace `incus`, user `backup@pbs` with token `incus`, both limited to `DatastoreBackup` on that namespace. The token secret is written once to `/root/incus-pbs-token.secret`. |
| `setup-incus.sh <disk>` | `incus-test`, as root | ZFS, Incus (Zabbly stable), `proxmox-backup-client`, ZFS storage pool `default` on pool `tank`, a running container `c1` and VM `v1`, and an encryption key in `/etc/incus-pbs/encryption-key.json` |

All scripts are idempotent. `pve-create-vms.sh` expects the Debian 13 generic cloud image at `/var/lib/vz/template/cache/debian-13-generic-amd64.qcow2`; `STORAGE`, `BRIDGE` and `IMAGE` can be overridden through the environment.

## Building it

```bash
# On the PVE host
scripts/testenv/pve-create-vms.sh ~/.ssh/authorized_keys

# On pbs-test: note the printed fingerprint and repository
scripts/testenv/setup-pbs.sh

# On incus-test: pass the ZFS disk by a stable path, never /dev/sdX.
# On a fresh cloud image the first run may ask for a reboot (new kernel for zfs-dkms); rerun afterwards.
scripts/testenv/setup-incus.sh /dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_drive-scsi1
```

For [incremental VM disks](../../docs/block-incremental.md), also install `libproxmox-backup-qemu0` on `incus-test` as described there.

## Pointing incus-pbs at it

On `incus-test`:

```bash
ssh root@<pbs-test> cat /root/incus-pbs-token.secret | install -m 600 /dev/stdin /etc/incus-pbs/pbs-token
install -m 600 examples/config.yaml /etc/incus-pbs/config.yaml
```

Then set in `/etc/incus-pbs/config.yaml`:

```yaml
pbs:
  repository: "backup@pbs!incus@<pbs-test ip>:test"
  namespace: incus
  fingerprint: "<fingerprint printed by setup-pbs.sh>"
```

Build and copy the binary from a workstation (`GOOS=linux GOARCH=amd64 make build`, then `scp bin/incus-pbs root@<incus-test>:/usr/local/bin/`).

## Smoke test

```bash
incus-pbs check
incus-pbs list
incus-pbs backup                                  # full
incus-pbs backup                                  # unchanged: should take seconds
incus-pbs status
incus-pbs restore inspect latest
incus-pbs restore instance latest c1 --name c1-restored
incus-pbs restore instance latest v1 --name v1-restored
incus-pbs restore mount latest c1 --dir /mnt/c1 && ls /mnt/c1 && incus-pbs restore unmount /mnt/c1

# Crash recovery: kill a run mid-upload, the next run must clean up and succeed
systemd-run --unit incus-pbs-test incus-pbs backup; sleep 20; systemctl kill -s KILL incus-pbs-test
incus-pbs cleanup --dry-run
incus-pbs backup
```

Nothing in this environment is meant to be kept: delete the two VMs on PVE when done.

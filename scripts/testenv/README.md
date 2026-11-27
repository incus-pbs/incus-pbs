# Integration test environment

These scripts build a disposable environment for testing incus-pbs against real Incus, ZFS and PBS. The unit tests (`make test`) fake all three; use this environment for anything that touches the real tools: new Incus or PBS versions, restore paths, change tracking, crash recovery.

| Script | Runs on | Creates |
|---|---|---|
| `create-vms.sh [ssh-pubkeys-file]` | an Incus host, as root or `incus-admin` | VM `incus-test` (4 vCPU, 6 GiB, nested KVM, Secure Boot off, 40 GiB root + 20 GiB block volume `incus-test-zfs` attached as device `zfs`) and VM `pbs-test` (2 vCPU, 2 GiB, 32 GiB), both `images:debian/13/cloud`, marked `user.incus-pbs-test=true` |
| `setup-pbs.sh` | `pbs-test`, as root | PBS 4, datastore `test` at `/srv/pbs-test`, namespace `incus`, user `backup@pbs` with token `incus`, both limited to `DatastoreBackup` on that namespace. The token secret is written once to `/root/incus-pbs-token.secret`. |
| `setup-incus.sh <disk>` | `incus-test`, as root | ZFS, Incus (Zabbly stable), `proxmox-backup-client`, ZFS storage pool `default` on pool `tank`, a running container `c1` and VM `v1`, and an encryption key in `/etc/incus-pbs/encryption-key.json` |

All scripts are idempotent. `create-vms.sh` takes `IMAGE`, `STORAGE` (default `default`) and `NETWORK` (default: the default profile's NIC) from the environment. The host needs nested KVM enabled, since `incus-test` runs the VM `v1`. Without a keys file the VMs are reached with `incus exec`; with one, the keys are installed for root through cloud-init and plain ssh works too.

## Building it

```bash
# On the Incus host
scripts/testenv/create-vms.sh                       # optionally: ~/.ssh/authorized_keys

# pbs-test: note the printed fingerprint
incus file push scripts/testenv/setup-pbs.sh pbs-test/root/
incus exec pbs-test -- bash /root/setup-pbs.sh

# incus-test: the ZFS disk has a stable path from its Incus device name, never use /dev/sdX.
# On a fresh cloud image the first run may ask for a reboot (new kernel for zfs-dkms):
# run `incus restart incus-test` and rerun.
incus file push scripts/testenv/setup-incus.sh incus-test/root/
incus exec incus-test -- bash /root/setup-incus.sh /dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_incus_zfs
```

For [incremental VM disks](../../docs/block-incremental.md), also install `libproxmox-backup-qemu0` on `incus-test` as described there.

## Pointing incus-pbs at it

From the Incus host:

```bash
incus exec pbs-test -- cat /root/incus-pbs-token.secret | incus exec incus-test -- install -D -m 600 /dev/stdin /etc/incus-pbs/pbs-token
incus file push --mode 0600 examples/config.yaml incus-test/etc/incus-pbs/config.yaml
incus list user.incus-pbs-test=true                 # IPv4 of pbs-test for the repository below
```

Then set in `/etc/incus-pbs/config.yaml`:

```yaml
pbs:
  repository: "backup@pbs!incus@<pbs-test ip>:test"
  namespace: incus
  fingerprint: "<fingerprint printed by setup-pbs.sh>"
```

Build and copy the binary from a workstation (`GOOS=linux GOARCH=amd64 make build`, then `incus file push --mode 0755 bin/incus-pbs incus-test/usr/local/bin/` on the Incus host, or `scp` it when ssh keys were given).

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

Nothing in this environment is meant to be kept. Delete it on the Incus host when done:

```bash
incus delete -f incus-test pbs-test
incus storage volume delete default incus-test-zfs     # or your $STORAGE
```

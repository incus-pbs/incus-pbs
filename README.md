<p align="center"><img src="docs/assets/logo-wordmark.svg" alt="incus-pbs" width="360"></p>

# incus-pbs

Back up Incus instances (containers and VMs) and custom storage volumes to a Proxmox Backup Server, without downtime, using the official `proxmox-backup-client`.

- **One PBS snapshot per run** holds every instance, volume and the environment metadata, so a host has one consistent recovery point.
- **No export files.** Incus ZFS snapshots are exposed read-only (snapshot mounts and zvol clones) and streamed straight into the client. Containers use pxar metadata change detection: an unchanged container takes seconds.
- **Incremental VM disks:** ZFS bookmarks plus the official libproxmox-backup-qemu mean only changed 4 MiB chunks are read and written. An unchanged host run went from 82 s to 8 s ([details](docs/block-incremental.md)).
- **Safe by default:**
  - It only ever deletes its own `pbs-backup-*` snapshots.
  - Failed runs never produce a PBS snapshot.
  - The run lock and SIGTERM handling work with systemd.
  - Secrets are never on the command line.
- **Restore through Incus' own import API:** instances and volumes come back with their config, devices and profiles, into any project and pool, never overwriting anything. A recovery kit (preseed + ordered plan) covers complete host rebuilds.
- **Restore drills:** `restore test` restores an isolated throwaway copy, boots it, checks files inside it and deletes it again, so backups can be proven restorable on a timer.
- **Per-instance schedules:** a cron expression in `user.incus-pbs.schedule` gives an instance its own backup times; everything else follows `backup.schedule`.
- **Operations:** pre/post snapshot hooks for application consistency, automatic crash recovery, a `status` command, a Prometheus textfile and JSON logs to journald.

## How it works

Each run takes a short-lived Incus snapshot of every selected instance and volume, exposes the underlying ZFS snapshots read-only (mounts for filesystems, read-only clones for zvols), and uploads each instance into its own PBS group (`ct/<name>`, `vm/<name>`). Every guest snapshot carries its own small manifest, so it can be restored on its own. A run index in `host/<backup_id>` holds the Incus metadata, host config and shared or standalone volumes. PBS only finalizes a snapshot whose upload completed. VM disks with an unchanged ZFS history since the last run are written incrementally through `libproxmox-backup-qemu`. Afterwards, every temporary mount, clone and snapshot is removed. Restores rebuild an Incus backup tarball layout from PBS and feed it to Incus' own import API.

```
Incus snapshot ─► ZFS read-only mount / clone ─► proxmox-backup-client ─► PBS host/<id>/<time>
                         └─ zfs send -i (changed blocks) ─► libproxmox-backup-qemu ─► PBS vm/<id>/<time>
```

See [docs/architecture.md](docs/architecture.md) for the full picture: components, run lifecycle, ZFS layout, PBS data model, restore path, state files and design decisions.

## Requirements

- Linux host with Incus (tested with 7.6) on a **ZFS** storage pool (tested with ZFS 2.3)
- `proxmox-backup-client` ≥ 3.3 (tested with 4.2.8), from the Proxmox `pbs-client` repository
- Optional, recommended for VMs: `libproxmox-backup-qemu0` (tested with 2.0.4) for [incremental VM disks](docs/block-incremental.md)
- A PBS datastore, plus an API token with `DatastoreBackup` on the target namespace

## Installation

Releases on GitHub ship a `.deb` package and a tarball for amd64 and arm64, plus `SHA256SUMS`.

### Debian / Ubuntu package

```bash
VER=x.y.z
curl -fLO https://github.com/incus-pbs/incus-pbs/releases/download/v$VER/incus-pbs_${VER}_amd64.deb
apt install ./incus-pbs_${VER}_amd64.deb
install -m 600 /usr/share/doc/incus-pbs/examples/config.yaml /etc/incus-pbs/config.yaml   # then edit it
```

The package installs `/usr/bin/incus-pbs` and the systemd units, and creates `/etc/incus-pbs` (mode 0700). It does not enable the timer. Continue with the token and key steps below, skipping the `install` lines; the units are already in place, so `systemctl enable --now incus-pbs.timer` is all that remains.

### Tarball

```bash
VER=x.y.z
curl -fLO https://github.com/incus-pbs/incus-pbs/releases/download/v$VER/incus-pbs_${VER}_linux_amd64.tar.gz
curl -fLO https://github.com/incus-pbs/incus-pbs/releases/download/v$VER/SHA256SUMS
curl -fLO https://github.com/incus-pbs/incus-pbs/releases/download/v$VER/SHA256SUMS.sigstore.json
cosign verify-blob --bundle SHA256SUMS.sigstore.json \
  --certificate-identity "https://github.com/incus-pbs/incus-pbs/.github/workflows/release.yml@refs/tags/v$VER" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com SHA256SUMS
sha256sum -c SHA256SUMS --ignore-missing
tar xzf incus-pbs_${VER}_linux_amd64.tar.gz      # paths below are relative to the extracted tarball
install -m 755 incus-pbs /usr/bin/incus-pbs

install -d -m 700 /etc/incus-pbs
install -m 600 examples/config.yaml /etc/incus-pbs/config.yaml     # then edit it
install -m 600 /dev/stdin /etc/incus-pbs/pbs-token <<< 'TOKEN-SECRET'
proxmox-backup-client key create --kdf none /etc/incus-pbs/encryption-key.json
proxmox-backup-client key paperkey /etc/incus-pbs/encryption-key.json   # store OFF-host

incus-pbs check
incus-pbs list
incus-pbs backup                               # first run is a full upload

install -m 644 dist/systemd/incus-pbs.service dist/systemd/incus-pbs.timer /etc/systemd/system/
systemctl daemon-reload && systemctl enable --now incus-pbs.timer
```

### Shell completion

The `.deb` installs bash and zsh completion, which takes effect in new shells; the tarball ships the same scripts in `completions/`. To set it up by hand:

```bash
install -m 644 completions/incus-pbs.bash /usr/share/bash-completion/completions/incus-pbs
install -m 644 completions/_incus-pbs /usr/share/zsh/vendor-completions/_incus-pbs
```

Or load it from your shell's rc file: `source <(incus-pbs completion bash)` in `~/.bashrc`, or `source <(incus-pbs completion zsh)` in `~/.zshrc`. `incus-pbs completion fish` and `completion pwsh` work too. Completion covers subcommands and flags, not instance or snapshot names.

### PBS side (least privilege)

```bash
proxmox-backup-manager user create backup@pbs
proxmox-backup-manager user generate-token backup@pbs incus      # prints the secret once
proxmox-backup-client namespace create incus                     # as an admin
for id in 'backup@pbs' 'backup@pbs!incus'; do
  proxmox-backup-manager acl update /datastore/<ds>/incus DatastoreBackup --auth-id "$id"
done
```

`DatastoreBackup` can create backups and read/restore its own (restores were tested with exactly this token), but cannot delete or prune them. Configure retention and pruning with PBS prune jobs and verify jobs on the server.

## Usage

```
incus-pbs [-c /etc/incus-pbs/config.yaml] [--log-level debug] <command> [--help]

  version
  check   [--json]                       validate everything, change nothing*
  list    [--json] [--project P]         show targets, archives, problems, exclusions
  backup  [--instance N]... [--project P]... | --scheduled
  cleanup [--dry-run] [--release-bookmarks]
                                         remove leftovers of interrupted runs
  status  [--json] [--max-age 26h] [--grace 2h]
                                         last run, per-target history and overdue targets

  restore list [instance] | inspect <snap>
  restore instance <snap> <name> [--project P] [--target-project P] [--name NEW]
                   [--pool P] [--identity auto|keep|new] [--drop-missing-devices]
                   [--staging-only] [--keep-staging]
  restore volume   <snap> <name> [--project P] [--source-pool P] [--target-project P]
                   [--name NEW] [--pool P] [--staging-only] [--keep-staging]
  restore test     <snap> <name> --file PATH... [--contains TEXT] [--project P]
                   [--target-project P] [--pool P] [--name NEW] [--timeout 5m] [--keep]
                                         restore drill: isolated copy, start, check files, delete
  restore host     <snap> --dir DIR      recovery kit, changes nothing
  restore mount    <snap> <name> --dir DIR [--project P] [--partition N]
  restore unmount  <dir|/dev/loopN>      instant read-only access, no full restore
```

Flags may come before or after arguments. The config path can also come from `INCUS_PBS_CONFIG`. Every command has `--help`.

\* `check` creates the staging/state directories and the staging dataset if they are missing. `<snap>` is `latest`, a time (the newest backup at or before it), a guest snapshot `ct|vm/<id>/<time>`, or a run index `host/<id>/<time>` (volumes, `inspect`, `restore host`).

| Exit code | Meaning |
|---|---|
| 0 | success |
| 1 | failed: no run index was written; guest snapshots uploaded before the failure stay restorable on their own (or status: last run failed, a target is overdue, or too old) |
| 2 | partial: the snapshot exists but is marked incomplete (`on_instance_error: skip`), or cleanup had problems |
| 3 | configuration error |
| 64 | usage error (bad command line) |
| 75 | another run holds the lock |

## Configuration

See [examples/config.yaml](examples/config.yaml). Unknown keys are errors, and all validation problems are reported at once.

## Schedules

The systemd timer starts `incus-pbs backup --scheduled` every 5 minutes. Each tick backs up only what is due:

- An instance with `user.incus-pbs.schedule` follows that cron expression. One without the key follows `backup.schedule` (default `30 2 * * *`).
- A volume used by exactly one instance follows that instance (it is stored in the instance's PBS group). Shared and standalone volumes, the Incus metadata and the host config follow `backup.schedule`.
- `off` leaves an instance out of scheduled runs. Manual `incus-pbs backup` still includes it.

```bash
incus config set web user.incus-pbs.schedule "0 */6 * * *"           # every 6 hours
incus config set db  user.incus-pbs.schedule "CRON_TZ=UTC */30 * * * *"
incus config set lab user.incus-pbs.schedule off
incus config unset web user.incus-pbs.schedule                      # back to backup.schedule
incus-pbs list                                                      # SCHEDULE and NEXT per target
```

Accepted values are 5-field cron expressions, `@hourly`, `@daily`, `@weekly`, `@monthly`, `@every 2h`, and an optional `CRON_TZ=<zone>` prefix. Times use the host's local zone. The timer tick sets the resolution (5 minutes).

- **Catch-up:** A slot missed while the host was off, or while another backup was still running, fires once on the next tick.
- **Failures:** A failed instance is retried at its next slot, not on every tick. `incus-pbs status` reports targets that have no success within `--grace` of their slot.
- **Invalid values:** An invalid value is a warning and an `incus-pbs check` failure, and the instance falls back to `backup.schedule`, so it is never left without backups.
- **New instances:** A new instance is backed up on the next tick.

## Documentation

- [Architecture](docs/architecture.md): components, backup and restore flow, data model, state, safety properties, design decisions
- [Restore and host recovery](docs/restore.md)
- [Incremental VM disks](docs/block-incremental.md): change tracking, requirements, Incus snapshot interaction
- [Hooks](docs/hooks.md) for application-consistent backups
- [Monitoring](docs/monitoring.md): journald events, `status`, Prometheus metrics and alerts

## Development

```bash
make test        # go vet + unit tests (Incus, ZFS and the PBS client are faked)
make build
make release     # goreleaser snapshot into build/goreleaser/ (needs goreleaser)
```

Releases are published by GoReleaser when a `v*` tag is pushed.

A disposable integration environment (Incus+ZFS host and PBS, as VMs on Proxmox VE) can be built with the scripts in [scripts/testenv/](scripts/testenv/README.md).

## License

MIT, see [LICENSE](LICENSE).

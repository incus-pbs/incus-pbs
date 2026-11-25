# Hooks

ZFS snapshots are crash-consistent: a running VM or database sees a restored backup like a power loss. Hooks let applications flush or freeze **just before** the snapshots, and thaw **right after**. The freeze window is only as long as taking the snapshots (typically under a second), not the whole upload.

```yaml
hooks:
  pre_backup:        # before snapshots; failure aborts the run (default on_failure: abort)
    - name: dump-mariadb                      # consistent dump inside the guest,
      instance: prod/db                       # captured by the snapshot that follows
      command: [sh, -c, "mariadb-dump --all-databases --single-transaction > /var/backups/all.sql"]
      timeout: 15m
    - name: freeze-vm-data
      instance: default/files
      command: [fsfreeze, --freeze, /srv]
      timeout: 30s
  post_snapshot:     # right after the snapshots; ALWAYS runs once pre_backup started,
    - name: thaw-vm-data      # also when a pre hook failed or the run was interrupted
      instance: default/files
      command: [fsfreeze, --unfreeze, /srv]
  post_backup:       # after upload and cleanup; failures are only logged
    - name: notify
      command: [/usr/local/lib/incus-pbs/hooks/notify.sh]
```

## Rules

- `command` is an argv list and never goes through a shell. Host commands need an absolute path; `incus-pbs check` verifies that they exist, are executable and are not group/world writable.
- `instance: name` uses the first configured project, while `instance: project/name` is explicit.
- Hooks are skipped and logged for instances that are not running, or that are not part of this run (for example not due in a scheduled run, or not selected with `--instance`).
- Host hooks (without `instance`) run in every run that backs something up. A scheduled tick with nothing due runs no hooks. Use `INCUS_PBS_TARGETS` to act only when a certain instance is included.
- `timeout` defaults to 5m. `on_failure` is `abort` or `continue`; `post_backup` hooks can only `continue`.
- Environment variables:

| Variable | Set for |
|---|---|
| `INCUS_PBS_PHASE` | all phases |
| `INCUS_PBS_RUN_ID` | all phases |
| `INCUS_PBS_SNAPSHOT_NAME` | `pre_backup`, `post_snapshot` |
| `INCUS_PBS_TARGETS` (space-separated `project/name` of the run's instances) | all phases |
| `INCUS_PBS_SCHEDULED` (`1` for `backup --scheduled`, else `0`) | all phases |
| `INCUS_PBS_STATUS` (`success`, `partial` or `failed`) | `post_backup` |
| `INCUS_PBS_EXIT_CODE` | `post_backup` |
| `INCUS_PBS_SNAPSHOT` (the run index; empty when a scheduled run wrote none) | `post_backup` |

- Results show up in the run report (`hooks`), as `hook_completed` / `hook_failed` / `hook_skipped` journal events, with the output tail.

## Freezing a VM's root filesystem

`fsfreeze /` inside a guest blocks the agent's own I/O, so don't do it. Prefer freezing data mounts, or application-level flushes: DB dumps, `FLUSH TABLES`, `pg_backup_start`/`pg_backup_stop`, or Redis `BGSAVE`.

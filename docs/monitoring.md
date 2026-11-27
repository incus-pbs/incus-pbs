# Monitoring

## journald

Every log line is JSON and has an `event` field:

```bash
journalctl -u incus-pbs -o cat | jq -c 'select(.event=="backup_run_finished")'
journalctl -u incus-pbs -o cat | jq -c 'select(.level=="ERROR")'
```

| Group | Events |
|---|---|
| Run | `backup_run_started` (scheduled, targets, deferred, host_index), `snapshots_created`, `pbs_upload_started`, `backup_run_finished` (summary: exit code, durations, bytes, counts), `scheduled_run_skipped` (lock held), `nothing_due` (debug) |
| Targets | `instance_backup_completed`, `volume_backup_completed`, `target_prepare_failed`, `target_unsupported`, `target_excluded`, `backup_warning` (also invalid `user.incus-pbs.schedule`), `guest_metadata_incomplete` |
| Upload and cleanup | `pbs_upload_failed` (with the client output tail), `crash_recovery`, `cleanup_step_failed`, `cleanup_incomplete` |
| Block images | `block_image_completed` (mode incremental/full, reason, chunks read/zero/uploaded), `bookmarks_released`, `block_upload_failed` / `block_session_failed` (fallback to the client), `block_incremental_unavailable` |
| Hooks | `hook_completed`, `hook_failed`, `hook_skipped` |
| Restore | `archive_restored`, `instance_restored`, `volume_restored`, `restore_failed`, `restore_test_ok`, `restore_test_failed`, `restore_test_cleanup_failed`, `restore_test_stale_copy` |

## status command

`incus-pbs status` reads `/var/lib/incus-pbs/state.json` and exits non-zero if:
- the last run that did something failed or was partial,
- the last fully successful run is older than `--max-age` (default 26h; raise it if `backup.schedule` runs less than daily), or
- a target is **overdue**: its schedule had a slot after its last success, and `--grace` (default 2h) has passed since then. An hourly instance is overdue about 3 hours after its last success, a weekly one after a week and 2 hours.

That makes it a ready-made Nagios/Icinga/healthcheck probe:

```bash
incus-pbs status --max-age 26h --grace 2h >/dev/null || alert "incus backups failing"
```

Targets that no longer exist, are excluded or set to `off` disappear from the state with the next run that sees the whole plan.

## Prometheus (node_exporter textfile collector)

```yaml
monitoring:
  textfile_path: /var/lib/prometheus/node-exporter/incus_pbs.prom
```

The file is rewritten atomically after every run that did something. Scheduled ticks with nothing due leave it alone, so compare the timestamps with `time()` instead of expecting fresh samples:

| Metric | Meaning |
|---|---|
| `incus_pbs_last_success_timestamp_seconds` | End of the last fully successful run |
| `incus_pbs_last_run_timestamp_seconds`, `_exit_code`, `_complete` | Last run outcome |
| `incus_pbs_last_run_duration_seconds`, `_upload_seconds` | Durations |
| `incus_pbs_last_run_logical_bytes`, `_uploaded_bytes` | Size vs actually transferred |
| `incus_pbs_last_run_targets{kind,status}` | Target counts |
| `incus_pbs_target_last_success_timestamp_seconds{kind,project,name}` | Per instance/volume |
| `incus_pbs_target_next_due_timestamp_seconds{kind,project,name}` | Next schedule slot after the last attempt |
| `incus_pbs_target_expected_by_timestamp_seconds{kind,project,name}` | First schedule slot after the last success; past it (plus grace) the target is overdue. 0 if it never succeeded |
| `incus_pbs_restore_test_last_run_timestamp_seconds{kind,project,name}` | End of the last `restore test` drill of each instance |
| `incus_pbs_restore_test_last_success_timestamp_seconds{kind,project,name}` | End of the last successful drill; 0 if none succeeded |
| `incus_pbs_restore_test_last_ok{kind,project,name}` | 1 if the last drill restored, started and found all files |
| `incus_pbs_restore_test_last_duration_seconds{kind,project,name}`, `_boot_seconds` | Drill duration, and time until the first file check answered |

Every `restore test` records its result in the state file and rewrites the textfile, so drill results appear without a backup run. A drill that fails before the backup is located (PBS unreachable, no backup yet) updates the existing entry of that instance; without one (for example a mistyped name) only its exit code reports the failure. Drills of instances that a complete backup plan no longer contains are forgotten, like targets. `status` lists the drills but does not fail on them; alert on the metrics instead.

Example alert rules:

```yaml
groups:
- name: incus-pbs
  rules:
  - alert: IncusBackupTooOld
    expr: time() - incus_pbs_last_success_timestamp_seconds > 26*3600
    for: 30m
  - alert: IncusBackupTargetOverdue      # respects each target's own schedule
    expr: incus_pbs_target_expected_by_timestamp_seconds > 0 and time() > incus_pbs_target_expected_by_timestamp_seconds + 2*3600
    for: 15m
    annotations:
      summary: "{{ $labels.kind }} {{ $labels.project }}/{{ $labels.name }} missed its backup schedule"
  - alert: IncusBackupFailed
    expr: incus_pbs_last_run_exit_code != 0
  - alert: IncusRestoreDrillFailed
    expr: incus_pbs_restore_test_last_ok == 0
  - alert: IncusRestoreDrillOverdue      # adjust to the drill timers' period
    expr: time() - incus_pbs_restore_test_last_success_timestamp_seconds > 35*86400
    for: 1h
```

## systemd

`OnFailure=` in `incus-pbs.service` (commented out in the shipped unit) can trigger a notification unit. Exit code 2 (partial) counts as a failure.

Retention, pruning, garbage collection and verify jobs belong on the PBS server; monitor them there.

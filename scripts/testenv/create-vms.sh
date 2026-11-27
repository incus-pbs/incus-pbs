#!/usr/bin/env bash
# Creates the two throwaway test-environment VMs on an Incus host.
# Run on the Incus host as root or as a member of incus-admin. Idempotent: skips VMs that already exist.
#
#   incus-test : Debian 13, nested KVM, root disk + extra block volume (device "zfs") for the ZFS pool
#   pbs-test   : Debian 13, hosts the dedicated test PBS datastore
#
# Usage: create-vms.sh [ssh-pubkeys-file]
#   Without a keys file, reach the VMs with `incus exec` (the cloud image ships the incus-agent).
#   With one, the keys are also installed for root through cloud-init.
#
# Environment: IMAGE (default images:debian/13/cloud), STORAGE (default "default"),
#   NETWORK (optional: attach eth0 to this network instead of the default profile's NIC).
# The VMs carry user.incus-pbs.schedule=off, so an incus-pbs already running on the host skips them.
set -euo pipefail

KEYS_FILE=${1:-}
IMAGE=${IMAGE:-images:debian/13/cloud}
STORAGE=${STORAGE:-default}
NETWORK=${NETWORK:-}
TAG=user.incus-pbs-test

# incus-test runs VMs itself, so the host must allow nested KVM.
nested=$(cat /sys/module/kvm_intel/parameters/nested /sys/module/kvm_amd/parameters/nested 2>/dev/null || true)
case "$nested" in
  *Y*|*1*) ;;
  *) echo "warning: nested KVM looks disabled on this host; the VM v1 inside incus-test will not start" >&2 ;;
esac

user_data=""
if [ -n "$KEYS_FILE" ]; then
  user_data="#cloud-config
disable_root: false
users:
  - name: root
    ssh_authorized_keys:
$(grep -E '^(ssh-|ecdsa-|sk-)' "$KEYS_FILE" | sed 's/^/      - /')
"
fi

create_vm() {
  local name=$1 cpus=$2 mem_gb=$3 root_gb=$4 extra_gb=$5
  shift 5
  if incus info "$name" >/dev/null 2>&1; then
    echo "== VM $name exists, skipping"
    return
  fi
  echo "== creating VM $name"
  local args=(--vm -s "$STORAGE" -c "limits.cpu=$cpus" -c "limits.memory=${mem_gb}GiB"
    -c "$TAG=true" -c user.incus-pbs.schedule=off -d "root,size=${root_gb}GiB")
  [ -n "$user_data" ] && args+=(-c "cloud-init.user-data=$user_data")
  [ -n "$NETWORK" ] && args+=(-n "$NETWORK")
  incus init "$IMAGE" "$name" "${args[@]}" "$@"
  if [ "$extra_gb" -gt 0 ]; then
    incus storage volume create "$STORAGE" "$name-zfs" --type=block "size=${extra_gb}GiB"
    incus config device add "$name" zfs disk pool="$STORAGE" source="$name-zfs"
  fi
  incus start "$name"
}

# zfs-dkms builds an unsigned module, which Secure Boot would refuse to load.
create_vm incus-test 4 6 40 20 -c security.secureboot=false
create_vm pbs-test   2 2 32 0

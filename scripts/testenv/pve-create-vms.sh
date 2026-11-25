#!/usr/bin/env bash
# Creates the two throwaway test-environment VMs on a Proxmox VE host.
# Run ON the PVE host as root. Idempotent: skips VMs that already exist.
#
#   incus-test : Debian 13, CPU type host (nested KVM), root disk + extra disk for ZFS pool
#   pbs-test   : Debian 13, hosts the dedicated test PBS datastore
#
# Usage: pve-create-vms.sh <ssh-pubkeys-file>
set -euo pipefail

KEYS_FILE=${1:?usage: $0 <ssh-pubkeys-file>}
STORAGE=${STORAGE:-local-zfs-data}
BRIDGE=${BRIDGE:-vmbr0}
IMAGE=${IMAGE:-/var/lib/vz/template/cache/debian-13-generic-amd64.qcow2}
TAG=incus-pbs-test

create_vm() {
  local id=$1 name=$2 cores=$3 mem=$4 root_gb=$5 extra_gb=$6
  if qm status "$id" >/dev/null 2>&1; then
    echo "== VM $id ($name) exists, skipping"
    return
  fi
  echo "== creating VM $id ($name)"
  qm create "$id" --name "$name" --tags "$TAG" --ostype l26 \
    --cpu host --cores "$cores" --memory "$mem" --balloon 0 \
    --net0 "virtio,bridge=$BRIDGE" --scsihw virtio-scsi-single \
    --agent enabled=1 --serial0 socket --vga serial0 --onboot 0
  qm set "$id" --scsi0 "$STORAGE:0,import-from=$IMAGE,discard=on,iothread=1"
  qm resize "$id" scsi0 "${root_gb}G"
  if [ "$extra_gb" -gt 0 ]; then
    qm set "$id" --scsi1 "$STORAGE:${extra_gb},discard=on,iothread=1"
  fi
  qm set "$id" --ide2 "$STORAGE:cloudinit" --boot order=scsi0 \
    --ciuser root --sshkeys "$KEYS_FILE" --ipconfig0 ip=dhcp
  qm start "$id"
}

create_vm 9101 incus-test 4 6144 40 20
create_vm 9102 pbs-test  2 2048 32 0

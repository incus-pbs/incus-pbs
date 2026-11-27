#!/usr/bin/env bash
# Prepares the Incus host of the test environment (Debian 13): ZFS, Incus (Zabbly stable),
# proxmox-backup-client, a ZFS storage pool on a dedicated disk and two
# running test instances: container c1 and VM v1.
#
# Run as root on the Incus test VM. Idempotent.
# Usage: setup-incus.sh <zfs-disk by stable path, e.g. /dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_incus_zfs>
# (never /dev/sdX: kernel names can swap across reboots)
set -euo pipefail
set -x
DISK=${1:?usage: $0 <disk for zfs pool>}
export DEBIAN_FRONTEND=noninteractive

# ZFS (contrib) + headers for dkms.
if ! command -v zfs >/dev/null; then
  # Debian cloud images use deb822 debian.sources; the Incus images: image already lists contrib.
  if [ -f /etc/apt/sources.list.d/debian.sources ]; then
    sed -i 's/^Components: main$/Components: main contrib/' /etc/apt/sources.list.d/debian.sources
  fi
  apt-get update -q
  # Cloud images may ship a kernel whose headers are gone from the archive:
  # move to the current kernel + headers; dkms builds ZFS for it.
  apt-get full-upgrade -y -q
  apt-get install -y -q linux-image-amd64 linux-headers-amd64 zfs-dkms zfsutils-linux
fi
if ! modprobe zfs; then
  echo "ZFS module not loadable for running kernel $(uname -r): reboot and rerun this script" >&2
  exit 3
fi

# Incus from Zabbly stable.
if ! command -v incus >/dev/null; then
  install -d /etc/apt/keyrings
  curl -fsSL https://pkgs.zabbly.com/key.asc -o /etc/apt/keyrings/zabbly.asc
  cat >/etc/apt/sources.list.d/zabbly-incus-stable.sources <<EOF
Enabled: yes
Types: deb
URIs: https://pkgs.zabbly.com/incus/stable
Suites: trixie
Components: main
Architectures: amd64
Signed-By: /etc/apt/keyrings/zabbly.asc
EOF
  apt-get update -q
  apt-get install -y -q incus qemu-system-x86 ovmf
fi

# Official PBS client.
if ! command -v proxmox-backup-client >/dev/null; then
  curl -fsSL https://enterprise.proxmox.com/debian/proxmox-archive-keyring-trixie.gpg \
    -o /usr/share/keyrings/proxmox-archive-keyring.gpg
  cat >/etc/apt/sources.list.d/pbs-client.sources <<EOF
Types: deb
URIs: http://download.proxmox.com/debian/pbs-client
Suites: trixie
Components: main
Signed-By: /usr/share/keyrings/proxmox-archive-keyring.gpg
EOF
  apt-get update -q
  apt-get install -y -q proxmox-backup-client
fi

if ! incus storage show default >/dev/null 2>&1; then
  incus admin init --preseed <<EOF
storage_pools:
- name: default
  driver: zfs
  config:
    source: $DISK
    zfs.pool_name: tank
networks:
- name: incusbr0
  type: bridge
  config:
    ipv4.address: auto
    ipv6.address: none
profiles:
- name: default
  devices:
    root: {path: /, pool: default, type: disk}
    eth0: {name: eth0, network: incusbr0, type: nic}
EOF
fi

incus info c1 >/dev/null 2>&1 || incus launch images:debian/13 c1
incus info v1 >/dev/null 2>&1 || incus launch images:debian/13 v1 --vm \
  -c limits.cpu=2 -c limits.memory=1GiB -d root,size=8GiB

# Wait for the VM agent.
for _ in $(seq 1 60); do incus exec v1 -- true 2>/dev/null && break; sleep 5; done

install -d -m 700 /etc/incus-pbs
[ -f /etc/incus-pbs/encryption-key.json ] ||
  proxmox-backup-client key create --kdf none /etc/incus-pbs/encryption-key.json
chmod 600 /etc/incus-pbs/encryption-key.json

set +x
incus version; zfs version | head -1; proxmox-backup-client version
incus list -c nstS
zfs list -o name,used,type -r tank

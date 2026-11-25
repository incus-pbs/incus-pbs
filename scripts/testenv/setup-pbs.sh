#!/usr/bin/env bash
# Installs a throwaway Proxmox Backup Server (Debian 13 / PBS 4) for the test environment
# and creates: datastore "test", namespace "incus", user backup@pbs with an
# API token that only has DatastoreBackup on that namespace.
#
# Run as root on the test PBS VM. Idempotent.
# Prints the token secret ONCE into /root/incus-pbs-token.secret (mode 0600).
set -euo pipefail
set -x

DS=test
DS_PATH=/srv/pbs-test
NS=incus
USER_ID=backup@pbs
TOKEN=incus

if ! command -v proxmox-backup-manager >/dev/null; then
  curl -fsSL https://enterprise.proxmox.com/debian/proxmox-archive-keyring-trixie.gpg \
    -o /usr/share/keyrings/proxmox-archive-keyring.gpg
  cat >/etc/apt/sources.list.d/pbs.sources <<EOF
Types: deb
URIs: http://download.proxmox.com/debian/pbs
Suites: trixie
Components: pbs-no-subscription
Signed-By: /usr/share/keyrings/proxmox-archive-keyring.gpg
EOF
  apt-get update -q
  DEBIAN_FRONTEND=noninteractive apt-get install -y -q proxmox-backup-server proxmox-backup-client
fi

proxmox-backup-manager datastore list --output-format json | grep -q "\"$DS\"" ||
  proxmox-backup-manager datastore create "$DS" "$DS_PATH"

proxmox-backup-manager user list --output-format json | grep -q "\"$USER_ID\"" ||
  proxmox-backup-manager user create "$USER_ID"

if ! proxmox-backup-manager user list-tokens "$USER_ID" --output-format json | grep -q "$TOKEN"; then
  umask 077
  proxmox-backup-manager user generate-token "$USER_ID" "$TOKEN" \
    | python3 -c 'import json,sys; t=sys.stdin.read(); print(json.loads(t[t.index("{"):])["value"])' >/root/incus-pbs-token.secret
fi

# Namespace: create with a short-lived admin token, then delete it again.
if [ ! -d "$DS_PATH/ns/$NS" ]; then
  umask 077
  proxmox-backup-manager user generate-token root@pam setup \
    | python3 -c 'import json,sys; t=sys.stdin.read(); print(json.loads(t[t.index("{"):])["value"])' >/root/setup.secret
  proxmox-backup-manager acl update "/datastore/$DS" DatastoreAdmin --auth-id 'root@pam!setup'
  FP=$(proxmox-backup-manager cert info | sed -n 's/^Fingerprint (sha256): //p')
  PBS_FINGERPRINT=$FP PBS_PASSWORD_FILE=/root/setup.secret \
    PBS_REPOSITORY="root@pam!setup@localhost:$DS" proxmox-backup-client namespace create "$NS"
  proxmox-backup-manager user delete-token root@pam setup
  rm -f /root/setup.secret
fi

# Least privilege: backup-only on the test namespace, for user and token (privsep).
for id in "$USER_ID" "$USER_ID!$TOKEN"; do
  proxmox-backup-manager acl update "/datastore/$DS/$NS" DatastoreBackup --auth-id "$id"
done

set +x
echo "fingerprint: $(proxmox-backup-manager cert info | sed -n 's/^Fingerprint (sha256): //p')"
echo "repository:  $USER_ID!$TOKEN@$(hostname -I | cut -d' ' -f1):$DS"

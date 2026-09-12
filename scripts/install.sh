#!/bin/bash
# install.sh: install oufdee, ouf, and ouf-panic under /usr/local, plus
# the systemd unit. Does NOT enable or start the service — that's a
# separate manual step so nothing autoconnects on install.
#
# Uninstall with scripts/uninstall.sh.

set -euo pipefail

if [ "$(id -u)" != "0" ]; then
    echo "install.sh must be run as root (e.g. via sudo)" >&2
    exit 1
fi

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"

echo "==> checking runtime dependencies"
missing=()
for tool in jq ip sysctl sha256sum; do
    command -v "$tool" >/dev/null 2>&1 || missing+=("$tool")
done
if [ ${#missing[@]} -gt 0 ]; then
    echo "installing missing runtime deps: ${missing[*]}"
    dnf install -y "${missing[@]}" >/dev/null
fi

echo "==> building binaries"
cd "$REPO_ROOT"
make build

echo "==> installing binaries"
install -Dm755 build/ouf     /usr/local/bin/ouf
install -Dm755 build/oufdee  /usr/local/sbin/oufdee
install -Dm755 scripts/ouf-panic /usr/local/sbin/ouf-panic

echo "==> creating dirs"
install -d -m 0755 /etc/outline-fedora
install -d -m 0700 /var/lib/outline-fedora
install -d -m 0700 /var/lib/outline-fedora/backup

echo "==> installing systemd unit"
install -Dm644 systemd/oufdee.service /etc/systemd/system/oufdee.service
systemctl daemon-reload

cat <<EOF

Installed. Nothing has been started or enabled yet.

Next steps:
  sudo systemctl start oufdee            # start the daemon
  ouf add ss://<your-key>#name           # add a key
  ouf list                               # confirm
  ouf connect                            # connect via the active key

If anything goes sideways:
  ouf undo                               # revert host state
  sudo /usr/local/sbin/ouf-panic         # same thing, works even if 'ouf' can't reach oufdee

To enable at boot (only after you've verified it works):
  sudo systemctl enable oufdee
EOF

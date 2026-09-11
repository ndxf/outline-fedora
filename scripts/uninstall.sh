#!/bin/bash
# uninstall.sh: reverse of install.sh. Removes binaries, unit, and
# state (with a prompt). Leaves /etc/outline-fedora/keys.json in place
# unless --purge is passed.

set -euo pipefail

if [ "$(id -u)" != "0" ]; then
    echo "uninstall.sh must be run as root" >&2
    exit 1
fi

PURGE=0
if [ "${1:-}" = "--purge" ]; then
    PURGE=1
fi

echo "==> stopping and disabling service"
systemctl disable --now oufdee.service 2>/dev/null || true

echo "==> best-effort revert (in case a session was live)"
if [ -x /usr/local/sbin/ouf-panic ]; then
    OUF_STATE_DIR=/var/lib/outline-fedora /usr/local/sbin/ouf-panic || true
fi

echo "==> removing files"
rm -f /etc/systemd/system/oufdee.service
rm -f /usr/local/bin/ouf /usr/local/sbin/oufdee /usr/local/sbin/ouf-panic
systemctl daemon-reload

if [ "$PURGE" = "1" ]; then
    echo "==> purging /etc/outline-fedora and /var/lib/outline-fedora"
    rm -rf /etc/outline-fedora /var/lib/outline-fedora
else
    echo "==> keeping /etc/outline-fedora (contains keys) — pass --purge to remove"
    rm -rf /var/lib/outline-fedora
fi

echo "done."

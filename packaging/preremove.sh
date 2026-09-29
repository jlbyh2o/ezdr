#!/bin/sh
set -e
# On removal (not upgrades), remove EZDR's setup so Proxmox VE, the guests,
# and zrepl work without it (docs/design/uninstall.md). In an unsafe state,
# such as during a failover, this fails and the package stays installed.
# `ezdr uninstall` sets EZDR_UNINSTALLING: it already did this.
if [ "$1" = "remove" ] || [ "$1" = "purge" ]; then
    if [ -z "${EZDR_UNINSTALLING:-}" ] && [ -x /usr/bin/ezdr ]; then
        /usr/bin/ezdr unenroll --package-removal
    fi
    systemctl disable --now ezdr.service 2>/dev/null || true
fi

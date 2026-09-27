#!/bin/sh
set -e
# Stop the client on removal, but not during an upgrade.
if [ "$1" = "remove" ] || [ "$1" = "purge" ]; then
    systemctl disable --now ezdr.service 2>/dev/null || true
fi

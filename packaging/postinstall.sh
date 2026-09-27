#!/bin/sh
set -e
systemctl daemon-reload || true
# On upgrade of an enrolled host, restart the client on the new version.
if [ -f /etc/ezdr/config.json ] && systemctl is-enabled --quiet ezdr.service 2>/dev/null; then
    systemctl restart ezdr.service || true
fi

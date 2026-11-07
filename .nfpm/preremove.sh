#!/bin/sh
set -e

if ! command -V systemctl >/dev/null 2>&1; then
	echo "Not running SystemD, ignoring"
	exit 0
fi

# Arguments: deb passes "remove", "upgrade", "deconfigure" or
# "failed-upgrade"; rpm passes the number of versions left installed (0 on
# removal); apk runs this only on removal, with the version.
removing=
case "$1" in
remove) removing=1 ;;
upgrade | deconfigure | failed-upgrade) ;;
'' | *[!0-9]*) removing=1 ;;
0) removing=1 ;;
esac

[ -n "$removing" ] || exit 0

# Stop and disable the service before its binary and unit file are removed.
if [ -d /run/systemd/system ]; then
	systemctl disable --now soft-serve.service || true
else
	systemctl disable soft-serve.service || true
fi

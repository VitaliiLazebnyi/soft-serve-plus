#!/bin/sh
set -e

if ! command -V systemctl >/dev/null 2>&1; then
	echo "Not running SystemD, ignoring"
	exit 0
fi

# Arguments: deb passes "remove", "purge", "upgrade" and others; rpm passes
# the number of versions left installed (0 on removal); apk runs this only on
# removal, with the version.
removing=
case "$1" in
remove) removing=1 ;;
purge | upgrade | failed-upgrade | abort-* | disappear) ;;
'' | *[!0-9]*) removing=1 ;;
0) removing=1 ;;
esac

if [ -d /run/systemd/system ]; then
	systemctl daemon-reload
	if [ -n "$removing" ]; then
		systemctl reset-failed soft-serve.service || true
	fi
fi

if [ -n "$removing" ]; then
	echo "WARN: the soft-serve user/group and /var/lib/soft-serve directory were not removed"
fi

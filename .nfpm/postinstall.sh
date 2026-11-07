#!/bin/sh
set -e

if ! command -V systemctl >/dev/null 2>&1; then
	echo "Not running SystemD, ignoring"
	exit 0
fi

# Arguments: deb passes "configure <previously configured version>" (empty on
# first install) or "abort-*"; rpm passes the number of installed versions (1
# on first install, 2 or more on upgrade); apk runs this as post-install with
# the new version and as post-upgrade with the new and the old version.
first_install=
upgrade=
case "$1" in
configure)
	if [ -n "$2" ]; then
		upgrade=1
	else
		first_install=1
	fi
	;;
abort-*) ;;
'' | *[!0-9]*)
	if [ -n "$2" ]; then
		upgrade=1
	else
		first_install=1
	fi
	;;
0) ;;
1) first_install=1 ;;
*) upgrade=1 ;;
esac

systemd-sysusers
systemd-tmpfiles --create

# Skip talking to the service manager in a chroot or container build.
if [ -d /run/systemd/system ]; then
	systemctl daemon-reload
fi

# Apply the preset only on first install, so an upgrade does not undo an
# admin's "systemctl disable" or "systemctl mask".
if [ -n "$first_install" ]; then
	systemctl preset soft-serve.service
fi

# On upgrade, restart the service if it is running so it picks up the new
# binary and unit; a stopped, disabled or masked service is left alone.
if [ -n "$upgrade" ] && [ -d /run/systemd/system ]; then
	systemctl try-restart soft-serve.service || true
fi

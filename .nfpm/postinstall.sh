#!/bin/sh
set -e

if ! command -V systemctl >/dev/null 2>&1; then
	echo "Not running SystemD, ignoring"
	exit 0
fi

# Arguments: deb passes "configure <previously configured version>" (empty on
# first install) or "abort-*"; rpm passes the number of installed versions (1
# on first install); apk runs this only on first install, with the version.
first_install=
case "$1" in
configure) [ -n "$2" ] || first_install=1 ;;
abort-*) ;;
'' | *[!0-9]*) first_install=1 ;;
1) first_install=1 ;;
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

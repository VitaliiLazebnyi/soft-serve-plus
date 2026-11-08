#!/bin/sh
set -e

if ! command -V systemctl >/dev/null 2>&1; then
	echo "Not running SystemD, ignoring"
	exit 0
fi

# Arguments: deb passes "remove", "upgrade", "deconfigure" or
# "failed-upgrade"; rpm passes the number of versions left installed (0 on
# removal); apk runs this only on removal, with the version.
#
# deb "remove" leaves the package in the config-files state, and a later
# reinstall runs "postinst configure <old version>", which skips the preset.
# So on deb only stop the service and keep its enablement (the dangling
# symlink is harmless and the unit comes back as it was on reinstall);
# postremove disables it on "purge". rpm and apk reinstalls apply the preset
# again, so disable the service there.
action=
case "$1" in
remove) action=stop ;;
upgrade | deconfigure | failed-upgrade) ;;
'' | *[!0-9]*) action=disable ;;
0) action=disable ;;
esac

# Stop (and on rpm/apk disable) the service before its binary and unit file
# are removed.
case "$action" in
stop)
	if [ -d /run/systemd/system ]; then
		systemctl stop soft-serve.service || true
	fi
	;;
disable)
	if [ -d /run/systemd/system ]; then
		systemctl disable --now soft-serve.service || true
	else
		systemctl disable soft-serve.service || true
	fi
	;;
esac

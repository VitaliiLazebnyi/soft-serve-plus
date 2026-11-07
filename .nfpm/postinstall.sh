#!/bin/sh
set -e

# Older packages installed the environment file world-readable, and a
# config|noreplace file keeps its mode on upgrade. Tighten it only while it
# still has that old default mode, so a mode the admin chose is kept.
if [ -f /etc/soft-serve.conf ] && [ "$(stat -c %a /etc/soft-serve.conf 2>/dev/null)" = 644 ]; then
	chmod 600 /etc/soft-serve.conf
fi

if ! command -V systemctl >/dev/null 2>&1; then
	echo "Not running SystemD, ignoring"
	exit 0
fi

systemd-sysusers
systemd-tmpfiles --create

systemctl daemon-reload
systemctl unmask soft-serve.service
systemctl preset soft-serve.service

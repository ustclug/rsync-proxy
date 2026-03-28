#!/bin/sh

# Only run if systemd is running
[ -d /run/systemd ] || exit 0

systemctl disable --now rsync-proxy.service

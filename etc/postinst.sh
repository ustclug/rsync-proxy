#!/bin/sh

# Only run if systemd is running
[ -d /run/systemd ] || exit 0

systemctl daemon-reload
systemctl enable rsync-proxy.service

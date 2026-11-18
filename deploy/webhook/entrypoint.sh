#!/bin/sh
# SPDX-License-Identifier: MIT
set -eu

DISKSPACE_DEPLOY_SECRET=$(cat /run/secrets/deploy_secret)
if [ "${#DISKSPACE_DEPLOY_SECRET}" -lt 32 ]; then
    echo 'Deployment secret must contain at least 32 characters' >&2
    exit 1
fi
export DISKSPACE_DEPLOY_SECRET
export DISKSPACE_DEPLOY_DIR="${DISKSPACE_DEPLOY_DIR:-/opt/diskspace}"
export DISKSPACE_DEPLOY_COMPOSE_FILE="${DISKSPACE_DEPLOY_COMPOSE_FILE:-compose.yaml}"
cd "$DISKSPACE_DEPLOY_DIR"
# limitproxy listens on :9000, caps request bodies (4 KiB) and connections, and proxies
# to webhook on loopback. The secret reaches webhook via env only.
exec limitproxy -- webhook -hooks /etc/diskspace-deploy/hooks.json.tmpl -template -ip 127.0.0.1 -port 9001 -http-methods POST

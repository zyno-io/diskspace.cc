#!/usr/bin/env bash
# SPDX-License-Identifier: MIT
#
# Usage: scripts/notarize-macos.sh [--staple] <path-to-.app|.dmg|.zip|.pkg>
# Submits to the Apple notary service with rcodesign and waits for acceptance
# (a rejection fails the script). With --staple the ticket is stapled to the
# submitted artifact (app bundles and disk images; not zips or bare binaries).
# Required env: APP_STORE_CONNECT_API_KEY_PATH, the JSON written by
# `rcodesign encode-app-store-connect-api-key`.
set -euo pipefail

staple=()
if [[ "${1:-}" == "--staple" ]]; then
  staple=(--staple)
  shift
fi
target="${1:?usage: notarize-macos.sh [--staple] <path>}"
: "${APP_STORE_CONNECT_API_KEY_PATH:?APP_STORE_CONNECT_API_KEY_PATH is required}"

echo "Notarizing: $target"
rcodesign notary-submit --api-key-file "$APP_STORE_CONNECT_API_KEY_PATH" --wait "${staple[@]}" "$target"

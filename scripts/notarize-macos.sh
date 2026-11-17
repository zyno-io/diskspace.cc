#!/usr/bin/env bash
# SPDX-License-Identifier: MIT
# Adapted from github.com/zyno-io/sp2p (MIT)
#
# Usage: scripts/notarize-macos.sh <file.dmg|file.zip> [<path-to-staple>]
# Submits the file to Apple notary service and waits. Credentials, either:
#   APP_STORE_CONNECT_API_KEY_PATH + APP_STORE_CONNECT_KEY_ID + APP_STORE_CONNECT_ISSUER_ID
#   or APPLE_ID + APPLE_TEAM_ID + APPLE_APP_PASSWORD
# After success, staples the ticket to <path-to-staple> (default: the file
# itself; pass the .app when notarizing a zip of it). No-op without credentials.
set -euo pipefail

file="${1:?usage: notarize-macos.sh <file> [staple-target]}"
staple="${2:-$file}"

if [[ -n "${APP_STORE_CONNECT_API_KEY_PATH:-}" ]]; then
  auth=(--key "$APP_STORE_CONNECT_API_KEY_PATH" --key-id "${APP_STORE_CONNECT_KEY_ID:?}" --issuer "${APP_STORE_CONNECT_ISSUER_ID:?}")
elif [[ -n "${APPLE_ID:-}" ]]; then
  auth=(--apple-id "$APPLE_ID" --team-id "${APPLE_TEAM_ID:?}" --password "${APPLE_APP_PASSWORD:?}")
else
  echo "Skipping notarization: no notary credentials set"
  exit 0
fi

echo "Notarizing: $file"
xcrun notarytool submit "$file" "${auth[@]}" --wait
xcrun stapler staple "$staple"

#!/usr/bin/env bash
# SPDX-License-Identifier: MIT
# Adapted from github.com/zyno-io/sp2p (MIT)
#
# Usage: scripts/sign-macos.sh <path-to-.app-or-binary>
# Signs with the hardened runtime using the Developer ID identity in
# MACOS_SIGN_IDENTITY. No-op when the variable is unset (local builds).
set -euo pipefail

target="${1:?usage: sign-macos.sh <path>}"

if [[ -z "${MACOS_SIGN_IDENTITY:-}" ]]; then
  echo "Skipping macOS signing: MACOS_SIGN_IDENTITY not set"
  exit 0
fi

args=(--force --timestamp --options runtime --sign "$MACOS_SIGN_IDENTITY")
if [[ -n "${MACOS_ENTITLEMENTS:-}" ]]; then
  args+=(--entitlements "$MACOS_ENTITLEMENTS")
fi
if [[ -d "$target" ]]; then
  args+=(--deep)
fi

echo "Signing: $target"
codesign "${args[@]}" "$target"
codesign --verify --strict --verbose=2 "$target"

#!/usr/bin/env bash
# SPDX-License-Identifier: MIT
#
# Usage: scripts/sign-macos.sh <path-to-.app|binary|.dmg>
# Signs in place with rcodesign using the Developer ID certificate. Binaries
# and app bundles get the hardened runtime; disk images do not take runtime
# flags. Required env:
#   APPLE_CERTIFICATE_P12_PATH        path to the decoded .p12
#   APPLE_CERTIFICATE_PASSWORD_PATH   file containing the .p12 password
# Optional: MACOS_ENTITLEMENTS (path to an entitlements plist). The release
# workflow sets these up; there is deliberately no skip-when-unset fallback,
# so an unsigned artifact can never pass for a signed one.
set -euo pipefail

target="${1:?usage: sign-macos.sh <path>}"
: "${APPLE_CERTIFICATE_P12_PATH:?APPLE_CERTIFICATE_P12_PATH is required}"
: "${APPLE_CERTIFICATE_PASSWORD_PATH:?APPLE_CERTIFICATE_PASSWORD_PATH is required}"

args=(--p12-file "$APPLE_CERTIFICATE_P12_PATH" --p12-password-file "$APPLE_CERTIFICATE_PASSWORD_PATH")
if [[ "$target" != *.dmg ]]; then
  args+=(--code-signature-flags runtime)
  if [[ -n "${MACOS_ENTITLEMENTS:-}" ]]; then
    args+=(--entitlements-xml-file "$MACOS_ENTITLEMENTS")
  fi
fi

echo "Signing: $target"
rcodesign sign "${args[@]}" "$target"

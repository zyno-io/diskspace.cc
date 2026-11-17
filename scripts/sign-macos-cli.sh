#!/usr/bin/env bash
# SPDX-License-Identifier: MIT
#
# Usage: scripts/sign-macos-cli.sh <dist-dir>
# Signs (hardened runtime) and notarizes the diskspace binary inside every
# <dist-dir>/diskspace_darwin_*.tar.gz, then rewrites each archive with the
# same members in the same order. A bare Mach-O cannot be stapled, so the
# binaries are submitted together in one zip; Gatekeeper finds the ticket
# online by code-directory hash. Env: see sign-macos.sh and notarize-macos.sh.
set -euo pipefail

dist="${1:?usage: sign-macos-cli.sh <dist-dir>}"
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

shopt -s nullglob
archives=("$dist"/diskspace_darwin_*.tar.gz)
if [[ ${#archives[@]} -eq 0 ]]; then
  echo "error: no darwin archives in $dist" >&2
  exit 1
fi

mkdir "$work/submit"
for archive in "${archives[@]}"; do
  name="$(basename "$archive" .tar.gz)"
  mkdir "$work/$name"
  tar -xzf "$archive" -C "$work/$name"
  "$here/sign-macos.sh" "$work/$name/diskspace"
  cp "$work/$name/diskspace" "$work/submit/$name"
done

(cd "$work/submit" && zip -q ../notarize.zip ./*)
"$here/notarize-macos.sh" "$work/notarize.zip"

for archive in "${archives[@]}"; do
  name="$(basename "$archive" .tar.gz)"
  tar -tzf "$archive" > "$work/$name.members"
  tar -czf "$archive" -C "$work/$name" --numeric-owner --owner=0 --group=0 -T "$work/$name.members"
  echo "Re-archived: $archive"
done

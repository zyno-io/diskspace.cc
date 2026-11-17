#!/usr/bin/env bash
# SPDX-License-Identifier: MIT
#
# Usage: scripts/checksums.sh <dist-dir>
# Writes <dist-dir>/checksums.txt covering every release asset in the
# directory, in the goreleaser format ("<sha256>  <name>"). Run it only after
# all signing and re-archiving is done; the install scripts fail closed on a
# mismatch.
set -euo pipefail

dist="${1:?usage: checksums.sh <dist-dir>}"
cd "$dist"
rm -f checksums.txt
shopt -s nullglob
files=(*.tar.gz *.zip *.dmg *.deb *.rpm *.apk)
if [[ ${#files[@]} -eq 0 ]]; then
  echo "error: no release assets in $dist" >&2
  exit 1
fi
printf '%s\n' "${files[@]}" | LC_ALL=C sort | while IFS= read -r f; do sha256sum "$f"; done > checksums.txt
cat checksums.txt

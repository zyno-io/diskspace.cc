#!/usr/bin/env bash
# SPDX-License-Identifier: MIT
#
# Cross-builds the Windows GUI (no CGO) and zips it per arch into
# dist/Diskspace_windows_<arch>.zip. Requires Go, zip, and web/dist built.
# Optional env: VERSION.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

if [[ ! -d gui ]] || ! ls gui/*.go >/dev/null 2>&1; then
  echo "error: ./gui package not found; cannot build the Windows app" >&2
  exit 1
fi

version="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
version="${version#v}"
mkdir -p "$root/dist"

if [[ -f gui/winres/winres.json ]]; then
  echo "Generating Windows resources"
  (cd gui && go run github.com/tc-hib/go-winres@latest make --arch amd64,arm64)
fi

for arch in amd64 arm64; do
  echo "Building windows/$arch"
  out="$(mktemp -d)"
  CGO_ENABLED=0 GOOS=windows GOARCH="$arch" go build -trimpath -tags desktop,production \
    -ldflags "-s -w -H windowsgui -X main.version=$version -X main.defaultRelay=https://diskspace.cc" \
    -o "$out/Diskspace.exe" ./gui
  zipfile="$root/dist/Diskspace_windows_${arch}.zip"
  rm -f "$zipfile"
  (cd "$out" && zip -q "$zipfile" Diskspace.exe)
  rm -rf "$out"
  echo "Built: $zipfile"
done

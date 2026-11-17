#!/usr/bin/env bash
# SPDX-License-Identifier: MIT
#
# Cross-builds the Windows GUI (no CGO) per arch into
# dist/gui-win-build/<arch>/Diskspace.exe. Signing and zipping into
# diskspace-desktop_windows_<arch>.zip happen later on a Windows runner (see
# .github/workflows/release.yml). Requires Go and web/dist built.
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
out="$root/dist/gui-win-build"
rm -rf "$out"
mkdir -p "$out"

if [[ -f gui/winres/winres.json ]]; then
  echo "Generating Windows resources"
  (cd gui && go run github.com/tc-hib/go-winres@v0.3.3 make --arch amd64,arm64)
fi

for arch in amd64 arm64; do
  echo "Building windows/$arch"
  mkdir -p "$out/$arch"
  CGO_ENABLED=0 GOOS=windows GOARCH="$arch" go build -trimpath -tags desktop,production \
    -ldflags "-s -w -H windowsgui -X main.version=$version -X main.defaultRelay=https://diskspace.cc" \
    -o "$out/$arch/Diskspace.exe" ./gui
done
echo "Built: $out"

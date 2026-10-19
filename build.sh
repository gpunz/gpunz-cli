#!/usr/bin/env sh
# Cross-compiles gpunz for every supported platform into dist/ with SHA256SUMS.
# Usage: ./build.sh                 (version from VERSION)
#        GPUNZ_VERSION=0.3.0 ./build.sh
set -eu

ROOT=$(cd "$(dirname "$0")" && pwd)
VERSION=${GPUNZ_VERSION:-$(tr -d ' \r\n' < "$ROOT/VERSION")}
OUT="$ROOT/dist"

rm -rf "$OUT" && mkdir -p "$OUT"
cd "$ROOT"
go test ./...

for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64; do
  os=${target%/*}
  arch=${target#*/}
  ext=""
  [ "$os" = "windows" ] && ext=".exe"
  echo "building gpunz $VERSION for $os/$arch"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" \
    -o "$OUT/gpunz-$os-$arch$ext" ./cmd/gpunz
done

cd "$OUT"
if command -v sha256sum >/dev/null 2>&1; then sha256sum gpunz-* > SHA256SUMS; else shasum -a 256 gpunz-* > SHA256SUMS; fi
ls -l "$OUT"

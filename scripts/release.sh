#!/usr/bin/env bash
set -euo pipefail

# Usage:
#   VERSION=v0.1.0 ./scripts/release.sh
: "${VERSION:?set VERSION like v0.1.0}"

APP=define
OUTDIR=dist
rm -rf "$OUTDIR"
mkdir -p "$OUTDIR"

targets=(
  "linux/amd64"
  "linux/arm64"
)

for t in "${targets[@]}"; do
  GOOS="${t%/*}"
  GOARCH="${t#*/}"
  name="${APP}-${VERSION}-${GOOS}-${GOARCH}"

  echo "Building $name..."
  env CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" \
    go build -trimpath -ldflags="-s -w" -o "$OUTDIR/$APP" ./...

  ( cd "$OUTDIR" && tar -czf "${name}.tar.gz" "$APP" )
  rm -f "$OUTDIR/$APP"
done

echo
echo "Artifacts in: $OUTDIR/"
ls -lh "$OUTDIR"

#!/usr/bin/env sh
set -eu

ROOT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
DIST_DIR="$ROOT_DIR/dist"
APP_NAME="filesync"
MAIN_PKG="."

mkdir -p "$DIST_DIR"

build_target() {
  goos="$1"
  goarch="$2"
  suffix="$3"
  output="$DIST_DIR/${APP_NAME}-${goos}-${goarch}${suffix}"

  echo "building $output"
  (
    cd "$ROOT_DIR"
    CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build \
      -trimpath \
      -ldflags="-s -w" \
      -o "$output" \
      "$MAIN_PKG"
  )
}

build_target linux amd64 ""
build_target windows amd64 ".exe"
build_target darwin "$(go env GOARCH)" ""

echo "done"
echo "outputs:"
echo "  $DIST_DIR/${APP_NAME}-linux-amd64"
echo "  $DIST_DIR/${APP_NAME}-windows-amd64.exe"
echo "  $DIST_DIR/${APP_NAME}-darwin-$(go env GOARCH)"

#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
npm run build
mkdir -p release
for target in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64; do
  target_os=${target%/*}
  target_arch=${target#*/}
  suffix=''
  if [ "$target_os" = windows ]; then suffix='.exe'; fi
  echo "Building $target_os/$target_arch"
  CGO_ENABLED=0 GOOS="$target_os" GOARCH="$target_arch" go build -trimpath -ldflags='-s -w' -o "release/tandem-$target_os-$target_arch$suffix" .
done

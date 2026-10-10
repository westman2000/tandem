#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
npm run build
go build -o tandem .
exec ./tandem "$@"

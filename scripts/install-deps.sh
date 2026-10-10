#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
command -v go >/dev/null || { echo 'Install Go first: brew install go'; exit 1; }
command -v npm >/dev/null || { echo 'Install Node.js first: brew install node'; exit 1; }
echo 'Downloading Go dependencies…'
go mod tidy
echo 'Installing browser UI dependencies…'
npm install
echo 'Dependencies ready.'

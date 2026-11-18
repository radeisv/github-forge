#!/bin/sh
# Build the static forge binary (Linux amd64, no cgo, stripped).
set -e
cd "$(dirname "$0")"
export PATH="$HOME/go/go/bin:$PATH"
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o forge ./go
ls -l forge

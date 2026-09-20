#!/bin/bash
set -e

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$DIR"

echo "Building sensenova-proxy..."
go build -ldflags="-s -w" -o sensenova-proxy .
echo "Build complete: $DIR/sensenova-proxy"

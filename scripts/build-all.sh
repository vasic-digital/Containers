#!/bin/bash
# Build all the consuming project's service binaries for container images

set -euo pipefail

echo "=== Building the consuming project's service binaries ==="

mkdir -p bin

# Build core backend
echo "Building core..."
GOOS=linux GOARCH=amd64 go build -ldflags='-s -w' -o bin/core ./cmd/core/

# Build discovery beacon
echo "Building discovery..."
GOOS=linux GOARCH=amd64 go build -ldflags='-s -w' -o bin/discovery ./cmd/host-agent/discovery/

# Build host agent
echo "Building host-agent..."
GOOS=linux GOARCH=amd64 go build -ldflags='-s -w' -o bin/host-agent ./cmd/host-agent/

echo "=== Build complete ==="
ls -la bin/

#!/usr/bin/env bash
# ==============================================================================
# Build Script for PDF Splitter (Bash)
# Compiles static, trimmed production binaries into dist/
# ==============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DIST_DIR="${SCRIPT_DIR}/dist"
mkdir -p "${DIST_DIR}/data/sessions"

echo "=== 1. Building Linux binary -> dist/pdf-splitter-linux-amd64 ==="
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o "${DIST_DIR}/pdf-splitter-linux-amd64" "${SCRIPT_DIR}"

echo "=== 2. Building Windows binary -> dist/pdf-splitter.exe ==="
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o "${DIST_DIR}/pdf-splitter.exe" "${SCRIPT_DIR}"

echo ""
echo "Builds complete! Files in dist/:"
ls -lh "${DIST_DIR}"

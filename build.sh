#!/usr/bin/env bash
# ==============================================================================
# Build Script for Pdf Splitter (Bash)
# Compiles production-ready binaries into dist/
# ==============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DIST_DIR="${SCRIPT_DIR}/dist"
mkdir -p "${DIST_DIR}/data/sessions"

echo "=== 1. Building Linux binary -> dist/pdf-plitter-linux-amd64 ==="
GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o "${DIST_DIR}/pdf-plitter-linux-amd64" "${SCRIPT_DIR}"

echo "=== 2. Building Windows binary -> dist/pdf-plitter.exe ==="
GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" -o "${DIST_DIR}/pdf-plitter.exe" "${SCRIPT_DIR}"

echo ""
echo "Builds complete! Files in dist/:"
ls -lh "${DIST_DIR}"

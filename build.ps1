# ==============================================================================
# Build Script for Pdf Splitter (Windows PowerShell)
# Compiles production-ready binaries into dist/
# ==============================================================================
$ErrorActionPreference = "Stop"

$distDir = Join-Path $PSScriptRoot "dist"
if (-not (Test-Path $distDir)) {
    New-Item -ItemType Directory -Force -Path $distDir | Out-Null
}
$sessionsDir = Join-Path $distDir "data\sessions"
if (-not (Test-Path $sessionsDir)) {
    New-Item -ItemType Directory -Force -Path $sessionsDir | Out-Null
}

Write-Host "=== 1. Building Windows binary -> dist/pdf-plitter.exe ===" -ForegroundColor Cyan
go build -ldflags="-s -w" -o (Join-Path $distDir "pdf-plitter.exe") .
if ($LASTEXITCODE -ne 0) { throw "Windows build failed" }

Write-Host "=== 2. Building Linux binary -> dist/pdf-plitter-linux-amd64 ===" -ForegroundColor Cyan
$env:GOOS = "linux"
$env:GOARCH = "amd64"
go build -ldflags="-s -w" -o (Join-Path $distDir "pdf-plitter-linux-amd64") .
if ($LASTEXITCODE -ne 0) { throw "Linux build failed" }
$env:GOOS = ""
$env:GOARCH = ""

Write-Host "`nBuilds complete! Files in dist/:" -ForegroundColor Green
Get-ChildItem $distDir | Select-Object Name, Length, LastWriteTime | Format-Table -AutoSize

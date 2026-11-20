# ==============================================================================
# Build Script for PDF Splitter (Windows PowerShell)
# Compiles static, trimmed production binaries into dist/
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

$env:CGO_ENABLED = "0"

Write-Host "=== 1. Building Windows binary -> dist/pdf-splitter.exe ===" -ForegroundColor Cyan
$env:GOOS = "windows"
$env:GOARCH = "amd64"
go build -trimpath -ldflags="-s -w" -o (Join-Path $distDir "pdf-splitter.exe") .
if ($LASTEXITCODE -ne 0) { throw "Windows build failed" }

Write-Host "=== 2. Building Linux binary -> dist/pdf-splitter-linux-amd64 ===" -ForegroundColor Cyan
$env:GOOS = "linux"
$env:GOARCH = "amd64"
go build -trimpath -ldflags="-s -w" -o (Join-Path $distDir "pdf-splitter-linux-amd64") .
if ($LASTEXITCODE -ne 0) { throw "Linux build failed" }

Remove-Item Env:\GOOS -ErrorAction SilentlyContinue
Remove-Item Env:\GOARCH -ErrorAction SilentlyContinue
Remove-Item Env:\CGO_ENABLED -ErrorAction SilentlyContinue

Write-Host "`nBuilds complete! Files in dist/:" -ForegroundColor Green
Get-ChildItem $distDir | Select-Object Name, Length, LastWriteTime | Format-Table -AutoSize

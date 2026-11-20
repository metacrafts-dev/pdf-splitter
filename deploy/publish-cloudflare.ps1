# ==============================================================================
# PDF Splitter — Cloudflare Subdomain Publishing & Tunnel Script (PowerShell)
# NOTE: Cloudflare Free/Pro plans enforce a 100 MB HTTP body limit.
# Configure S3/R2 in .env if users need to upload PDFs larger than 100 MB.
# ==============================================================================
[CmdletBinding()]
param(
    [Parameter(Position=0)]
    [string]$Subdomain,

    [Parameter(Position=1)]
    [int]$Port = 8080,

    [Parameter()]
    [string]$TunnelName = "pdf-splitter"
)

$ErrorActionPreference = "Stop"

Write-Host "`n=== PDF Splitter — Cloudflare Subdomain Publisher ===" -ForegroundColor Cyan

if (-not $Subdomain) {
    $Subdomain = Read-Host "Enter the Cloudflare subdomain (e.g. split.yourdomain.com)"
}

$Subdomain = $Subdomain.Trim().ToLower()
if (-not $Subdomain) {
    Write-Error "Subdomain cannot be empty."
    exit 1
}

Write-Host "Target Subdomain: $Subdomain" -ForegroundColor Green
Write-Host "Local Target:     http://127.0.0.1:$Port" -ForegroundColor Green
Write-Host "[NOTE] Cloudflare Free/Pro enforces a 100 MB HTTP upload limit unless S3/R2 direct upload is enabled." -ForegroundColor Yellow

$cfCmd = Get-Command "cloudflared" -ErrorAction SilentlyContinue
if (-not $cfCmd) {
    Write-Host "[WARN] cloudflared CLI not found in PATH. Install via: winget install --id Cloudflare.cloudflared" -ForegroundColor Yellow
    exit 1
}

$certPath = Join-Path $env:USERPROFILE ".cloudflared\cert.pem"
if (-not (Test-Path $certPath)) {
    & cloudflared tunnel login
}

$tunnels = & cloudflared tunnel list
$tunnelId = ""
if ($tunnels -match "(?<id>[a-f0-9-]{36})\s+$TunnelName") {
    $tunnelId = $Matches['id']
    Write-Host "Using existing tunnel ID: $tunnelId" -ForegroundColor Green
} else {
    $createOut = & cloudflared tunnel create $TunnelName 2>&1
    if ($createOut -match "(?<id>[a-f0-9-]{36})") {
        $tunnelId = $Matches['id']
    }
}

if (-not $tunnelId) {
    Write-Error "Failed to get tunnel UUID."
    exit 1
}

& cloudflared tunnel route dns -f $TunnelName $Subdomain

$configDir = Join-Path $env:USERPROFILE ".cloudflared"
$configFile = Join-Path $configDir "$TunnelName.yml"
$credFile = Join-Path $configDir "$tunnelId.json"

$configContent = @"
tunnel: $tunnelId
credentials-file: $credFile

ingress:
  - hostname: $Subdomain
    service: http://127.0.0.1:$Port
  - service: http_status:404
"@

Set-Content -Path $configFile -Value $configContent
Write-Host "Configuration written to $configFile" -ForegroundColor Green
Write-Host "`nRun: cloudflared --config `"$configFile`" tunnel run $TunnelName" -ForegroundColor Yellow
Write-Host "Public URL: https://$Subdomain" -ForegroundColor Green

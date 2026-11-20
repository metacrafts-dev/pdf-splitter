# ==============================================================================
# Pdf Splitter — Cloudflare Subdomain Publishing & Tunnel Script (PowerShell)
# ==============================================================================
[CmdletBinding()]
param(
    [Parameter(Position=0)]
    [string]$Subdomain,

    [Parameter(Position=1)]
    [int]$Port = 8080,

    [Parameter()]
    [string]$TunnelName = "pdf-plitter"
)

$ErrorActionPreference = "Stop"

Write-Host "`n=== Pdf Splitter — Cloudflare Subdomain Publisher ===" -ForegroundColor Cyan

# 1. Prompt for Subdomain if not provided
if (-not $Subdomain) {
    $Subdomain = Read-Host "Enter the Cloudflare subdomain to publish Pdf Splitter under (e.g. split.yourdomain.com)"
}

$Subdomain = $Subdomain.Trim().ToLower()
if (-not $Subdomain) {
    Write-Error "Subdomain cannot be empty."
    exit 1
}

Write-Host "Target Subdomain: $Subdomain" -ForegroundColor Green
Write-Host "Local Target:     http://127.0.0.1:$Port" -ForegroundColor Green

# 2. Check cloudflared CLI
$cfCmd = Get-Command "cloudflared" -ErrorAction SilentlyContinue
if (-not $cfCmd) {
    Write-Host "[WARN] cloudflared CLI not found in PATH." -ForegroundColor Yellow
    Write-Host "Please download cloudflared for Windows from:" -ForegroundColor Yellow
    Write-Host "https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-windows-amd64.exe" -ForegroundColor Yellow
    Write-Host "Or install via winget / scoop:" -ForegroundColor Yellow
    Write-Host "  winget install --id Cloudflare.cloudflared" -ForegroundColor Yellow
    exit 1
}

# 3. Check login
$certPath = Join-Path $env:USERPROFILE ".cloudflared\cert.pem"
if (-not (Test-Path $certPath)) {
    Write-Host "No Cloudflare certificate found at $certPath. Running login..." -ForegroundColor Yellow
    & cloudflared tunnel login
}

# 4. Create or reuse tunnel
Write-Host "Checking tunnel '$TunnelName'..." -ForegroundColor Cyan
$tunnels = & cloudflared tunnel list
$tunnelId = ""
if ($tunnels -match "(?<id>[a-f0-9-]{36})\s+$TunnelName") {
    $tunnelId = $Matches['id']
    Write-Host "Using existing tunnel ID: $tunnelId" -ForegroundColor Green
} else {
    Write-Host "Creating new tunnel '$TunnelName'..." -ForegroundColor Cyan
    $createOut = & cloudflared tunnel create $TunnelName 2>&1
    if ($createOut -match "(?<id>[a-f0-9-]{36})") {
        $tunnelId = $Matches['id']
    }
}

if (-not $tunnelId) {
    Write-Error "Failed to get tunnel UUID."
    exit 1
}

# 5. Route DNS
Write-Host "Routing DNS: $Subdomain -> $TunnelName..." -ForegroundColor Cyan
& cloudflared tunnel route dns -f $TunnelName $Subdomain

# 6. Write config
$configDir = Join-Path $env:USERPROFILE ".cloudflared"
$configFile = Join-Path $configDir "config.yml"
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

Write-Host "`nTo start the tunnel run:" -ForegroundColor Cyan
Write-Host "  cloudflared tunnel run $TunnelName" -ForegroundColor Yellow
Write-Host "`nPublic URL: https://$Subdomain" -ForegroundColor Green

# Signs the Windows binaries in dist\ with an Authenticode certificate
# (e.g. Certum "Open Source Developer" via SimplySign virtual card) and
# embeds an RFC3161 timestamp so signatures outlive the certificate.
#
# Usage:
#   .\sign.ps1                                  # finds cert by subject "Open Source Developer"
#   .\sign.ps1 -CertSubject "Your Exact CN"     # or by exact subject name
#   .\sign.ps1 -Thumbprint "<cert thumbprint>"  # or by thumbprint (most precise)
param(
    [string]$CertSubject = "Open Source Developer",
    [string]$Thumbprint = "",
    [string]$TimestampServer = "http://timestamp.certum.pl"
)

$ErrorActionPreference = "Stop"

$signtool = Get-ChildItem "C:\Program Files (x86)\Windows Kits\10\bin" -Recurse -Filter signtool.exe -ErrorAction SilentlyContinue |
    Where-Object { $_.FullName -match "\\x64\\" } |
    Sort-Object FullName -Descending | Select-Object -First 1
if (-not $signtool) { $signtool = Get-Command signtool -ErrorAction Stop }

$selector = if ($Thumbprint) { "/sha1 $Thumbprint" } else { "/n `"$CertSubject`"" }

$targets = @(Get-ChildItem "dist\*.exe" -ErrorAction Stop)
if ($targets.Count -eq 0) { throw "No .exe files found in dist\" }

foreach ($f in $targets) {
    Write-Host "==> Signing $($f.Name)"
    & $signtool.FullName sign /fd SHA256 /tr $TimestampServer /td SHA256 $selector $f.FullName
    if ($LASTEXITCODE -ne 0) { throw "signtool failed for $($f.Name) (exit $LASTEXITCODE)" }
    & $signtool.FullName verify /pa /all $f.FullName
    if ($LASTEXITCODE -ne 0) { throw "signature verification failed for $($f.Name)" }
    Write-Host "    OK: signed and verified"
}
Write-Host "`nAll binaries signed. Now create the release:"
Write-Host "  cd dist; sha256sum pdf-splitter.exe pdf-splitter-linux-amd64 > SHA256SUMS.txt"
Write-Host "  gh release create vX.Y.Z dist/* --generate-notes"

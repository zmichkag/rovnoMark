$ErrorActionPreference = "Stop"

# Force UTF-8 for external tools output (Git)
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8
$OutputEncoding = [System.Text.Encoding]::UTF8

$AppName    = "AstraPrint"
$BrandName  = "AstraPrint"
$Vendor     = "AstraForce"
$SupportURL = "support@AstraForce.ru"

$Version   = try { (git describe --tags --always --dirty).Trim() } catch { "v1.0.0" }
$GitCommit = try { (git rev-parse --short HEAD).Trim() } catch { "unknown" }
$BuildDate = (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ")

$LdFlags = "-s -w " +
        "-X 'rovnoMark/internal/version.Version=$Version' " +
        "-X 'rovnoMark/internal/version.GitCommit=$GitCommit' " +
        "-X 'rovnoMark/internal/version.BuildDate=$BuildDate' " +
        "-X 'rovnoMark/internal/brand.Name=$BrandName' " +
        "-X 'rovnoMark/internal/brand.Vendor=$Vendor' " +
        "-X 'rovnoMark/internal/brand.SupportURL=$SupportURL'"

# --- GENERATE FRESH RELEASE CHANGELOG ---
Write-Host "Generating fresh CHANGELOG.md for $Version..." -ForegroundColor Cyan

#

$releaseDate = Get-Date -Format "yyyy-MM-dd"
$gitLog = git -c i18n.logOutputEncoding=utf-8 log $targetRange --pretty=format:"- %s (%h)" 2>$null

#
$changelog = "# $AppName Release Notes`r`n`r`n"
$changelog += "## [$Version] - $releaseDate`r`n`r`n"

if ($gitLog) {
    $features = $gitLog | Where-Object { $_ -match "^-\s*feat" }
    $fixes    = $gitLog | Where-Object { $_ -match "^-\s*fix" }


    if ($features) {
        $changelog += "### Features`r`n" + ($features -join "`r`n") + "`r`n`r`n"
    }
    if ($fixes) {
        $changelog += "### Bug Fixes`r`n" + ($fixes -join "`r`n") + "`r`n`r`n"
    }

} else {
    $changelog += "No code changes recorded since last tag.`r`n`r`n"
}

#
if (-not (Test-Path "$PSScriptRoot\bin")) {
    New-Item -ItemType Directory -Force -Path "$PSScriptRoot\bin" | Out-Null
}

[System.IO.File]::WriteAllText("$PSScriptRoot\CHANGELOG.md", $changelog, [System.Text.Encoding]::UTF8)
[System.IO.File]::WriteAllText("$PSScriptRoot\bin\CHANGELOG.md", $changelog, [System.Text.Encoding]::UTF8)

Write-Host "CHANGELOG.md created successfully!" -ForegroundColor Green

# --- BUILD ARTIFACTS ---
Write-Host "Building $AppName version $Version (commit: $GitCommit)..." -ForegroundColor Cyan

# Windows amd64
$env:GOOS = "windows"
$env:GOARCH = "amd64"
go build -ldflags $LdFlags -o "bin/$AppName-windows-amd64.exe" ./cmd/gateway/

# Linux amd64
$env:GOOS = "linux"
$env:GOARCH = "amd64"
go build -ldflags $LdFlags -o "bin/$AppName-linux-amd64" ./cmd/gateway/

Write-Host "Done! Artifacts generated in bin/" -ForegroundColor Green
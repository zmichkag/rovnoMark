$ErrorActionPreference = "Stop"

$AppName = "AstraPrint"
$BrandName = "AstraPrint"
$Vendor = "AstraForce"
$SupportURL = "support@AstraForce.ru"
$Version = try { git describe --tags --always --dirty } catch { "v1.0.0" }
$GitCommit = try { git rev-parse --short HEAD } catch { "unknown" }
$BuildDate = (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ")

$LdFlags = "-s -w " +
  "-X 'rovnoMark/internal/version.Version=$Version' " +
  "-X 'rovnoMark/internal/version.GitCommit=$GitCommit' " +
  "-X 'rovnoMark/internal/version.BuildDate=$BuildDate' " +
  "-X 'rovnoMark/internal/brand.Name=$BrandName' " +
  "-X 'rovnoMark/internal/brand.Vendor=$Vendor' " +
  "-X 'rovnoMark/internal/brand.SupportURL=$SupportURL'"

Write-Host "Building $AppName ($Version, commit $GitCommit)..." -ForegroundColor Cyan

$env:GOOS = "windows"
$env:GOARCH = "amd64"
go build -ldflags $LdFlags -o "bin/$AppName-windows-amd64.exe" ./cmd/gateway/

$env:GOOS = "linux"
$env:GOARCH = "amd64"
go build -ldflags $LdFlags -o "bin/$AppName-linux-amd64" ./cmd/gateway/

Write-Host "Done! Artifacts generated in bin/" -ForegroundColor Green
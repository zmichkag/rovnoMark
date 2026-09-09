#!/usr/bin/env bash
set -e

APP_NAME="AstraPrint"
BRAND_NAME="AstraPrint"
VENDOR="AstraForce"
SUPPORT_URL="support@AstraForce.ru"
VERSION=$(git describe --tags --always --dirty 2>/dev/null || echo "v1.0.0")
GIT_COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_DATE=$(date -u +'%Y-%m-%dT%H:%M:%SZ')


LDFLAGS="-s -w \
  -X 'rovnoMark/internal/version.Version=${VERSION}' \
  -X 'rovnoMark/internal/version.GitCommit=${GIT_COMMIT}' \
  -X 'rovnoMark/internal/version.BuildDate=${BUILD_DATE}' \
  -X 'rovnoMark/internal/brand.Name=${BRAND_NAME}' \
  -X 'rovnoMark/internal/brand.Vendor=${VENDOR}' \
  -X 'rovnoMark/internal/brand.SupportURL=${SUPPORT_URL}'"


echo "Building ${APP_NAME} (${VERSION}, commit ${GIT_COMMIT})..."

# 1. Сборка Linux amd64
GOOS=linux GOARCH=amd64 go build -ldflags="${LDFLAGS}" -o bin/${APP_NAME}-linux-amd64 .

# 2. Сборка Windows amd64 (.exe)
GOOS=windows GOARCH=amd64 go build -ldflags="${LDFLAGS}" -o bin/${APP_NAME}-windows-amd64.exe .

echo "Build complete: bin/"
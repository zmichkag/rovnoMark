#!/usr/bin/env bash
set -euo pipefail

# Определяем корень проекта (где лежит скрипт)
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

APP_NAME="AstraPrint"
BRAND_NAME="AstraPrint"
VENDOR="AstraForce"
SUPPORT_URL="support@AstraForce.ru"

VERSION=$(git describe --tags --always --dirty 2>/dev/null | tr -d '\r\n' || echo "v1.0.0")
GIT_COMMIT=$(git rev-parse --short HEAD 2>/dev/null | tr -d '\r\n' || echo "unknown")
BUILD_DATE=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
RELEASE_DATE=$(date +"%Y-%m-%d")

LD_FLAGS="-s -w \
  -X 'rovnoMark/internal/version.Version=${VERSION}' \
  -X 'rovnoMark/internal/version.GitCommit=${GIT_COMMIT}' \
  -X 'rovnoMark/internal/version.BuildDate=${BUILD_DATE}' \
  -X 'rovnoMark/internal/brand.Name=${BRAND_NAME}' \
  -X 'rovnoMark/internal/brand.Vendor=${VENDOR}' \
  -X 'rovnoMark/internal/brand.SupportURL=${SUPPORT_URL}'"

# --- GENERATE FRESH RELEASE CHANGELOG ---
echo -e "\033[36mGenerating fresh CHANGELOG.md for ${VERSION}...\033[0m"

# Читаем всю историю коммитов
GIT_LOG=$(git -c i18n.logOutputEncoding=utf-8 log --pretty=format:"- %s (%h)" 2>/dev/null || true)

# Фильтрация строк строго по feat и fix
FEATURES=$(echo "$GIT_LOG" | grep -E '^-\s*feat' || true)
FIXES=$(echo "$GIT_LOG" | grep -E '^-\s*fix' || true)

if [ -n "$FEATURES" ]; then
    CHANGELOG+="### Features\n${FEATURES}\n\n"
fi

if [ -n "$FIXES" ]; then
    CHANGELOG+="### Bug Fixes\n${FIXES}\n\n"
fi

if [ -z "$FEATURES" ] && [ -z "$FIXES" ]; then
    CHANGELOG+="No recorded changes in feat or fix categories.\n\n"
fi

# Запись в корень и в директорию bin/
mkdir -p bin
echo -e "$CHANGELOG" > CHANGELOG.md
echo -e "$CHANGELOG" > bin/CHANGELOG.md

echo -e "\033[32mCHANGELOG.md created successfully!\033[0m"

# --- BUILD ARTIFACTS ---
echo -e "\033[36mBuilding ${APP_NAME} version ${VERSION} (commit: ${GIT_COMMIT})...\033[0m"

# Windows amd64
GOOS=windows GOARCH=amd64 go build -ldflags "${LD_FLAGS}" -o "bin/${APP_NAME}-windows-amd64.exe" ./cmd/gateway/

# Linux amd64
GOOS=linux GOARCH=amd64 go build -ldflags "${LD_FLAGS}" -o "bin/${APP_NAME}-linux-amd64" ./cmd/gateway/

echo -e "\033[32mDone! Artifacts generated in bin/\033[0m"
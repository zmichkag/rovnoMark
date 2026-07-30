# Stage 1: Сборка бинарного файла
FROM golang:1.22-alpine AS builder

WORKDIR /app

# 1. Копируем манифесты зависимостей
COPY go.mod go.sum ./

# 2. Скачиваем модули с монтированием кэша (не перекачивает при неизменных go.mod/go.sum)
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# 3. Копируем исходный код приложения
COPY . .

# 4. Собираем статический бинарник с использованием кэша компилятора Go
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o marking-service ./main.go

# Stage 2: Минималистичный финальный образ
FROM alpine:latest

# Устанавливаем системные сертификаты и таймзоны
RUN apk --no-cache add ca-certificates tzdata

WORKDIR /root/

# Переносим скомпилированный бинарный файл
COPY --from=builder /app/marking-service /bin/marking-service

# Рабочая директория под Volume для базы данных и конфигурации
WORKDIR /app/data

# Запуск сервиса
CMD ["/bin/marking-service"]
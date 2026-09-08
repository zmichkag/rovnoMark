# Stage 1: Сборка бинарного файла
FROM golang:1.25-alpine AS builder

WORKDIR /app

# 1. Копируем манифесты зависимостей
COPY go.mod go.sum ./

# 2. Скачиваем модули с монтированием кэша
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# 3. Предкомпиляция SQLite в кэш для моментальных повторных сборщиков
RUN --mount=type=cache,target=/go/pkg/mod \
    go build modernc.org/sqlite

# 4. Копируем исходный код приложения
COPY . .

# 5. Собираем статический бинарник
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-w -s" -o /bin/marking-service ./main.go

# Stage 2: Минималистичный финальный образ
FROM alpine:latest

RUN apk --no-cache add ca-certificates tzdata
ENV TZ=Europe/Moscow

# Рабочая директория корня приложения (НЕ /app/data!)
WORKDIR /app

# Переносим скомпилированный бинарный файл
COPY --from=builder /bin/marking-service /bin/marking-service

# Создаем папку под базы данных внутри /app
RUN mkdir -p /app/data

EXPOSE 8080
VOLUME ["/app/data"]

# Запуск с явным указанием каталога баз через флаг
CMD ["/bin/marking-service", "--port", "8080", "--data-dir", "/app/data"]
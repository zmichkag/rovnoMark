# Stage 1: Сборка
FROM golang:1.22-alpine AS builder

# Устанавливаем рабочую директорию
WORKDIR /app

# Копируем файлы зависимостей
COPY go.mod go.sum ./
RUN go mod download

# Копируем исходный код
COPY . .

# Собираем статически скомпилированный бинарный файл
# CGO_ENABLED=0 нужен для того, чтобы бинарник не зависел от системных библиотек (libc)
RUN CGO_ENABLED=0 GOOS=linux go build -o marking-service ./main.go

# Stage 2: Финальный образ
FROM alpine:latest

# Добавляем сертификаты для защищенных соединений (нужны для связи с ИС МП / Честный ЗНАК)
RUN apk --no-cache add ca-certificates

WORKDIR /root/

# Копируем только исполняемый файл из первого контейнера
COPY --from=builder /app/marking-service .
# Копируем конфиги, если они не пробрасываются через volumes
COPY --from=builder /app/config ./config

# Запуск приложения
CMD ["./marking-service"]
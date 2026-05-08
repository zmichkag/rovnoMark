# Стейдж сборки
FROM golang:1.22-alpine AS builder
# Устанавливаем зависимости для работы с SQLite (CGO)
RUN apk add --no-cache gcc musl-dev
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Собираем бинарник. CGO_ENABLED=1 важен для SQLite!
RUN CGO_ENABLED=1 GOOS=linux go build -o rovnomark main.go

# Финальный легкий образ
FROM alpine:latest
WORKDIR /root/
# Копируем только готовый файл
COPY --from=builder /app/rovnomark .
# Создаем папку для базы данных
RUN mkdir data
EXPOSE 8080
CMD ["./rovnomark"]
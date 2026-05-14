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
RUN CGO_ENABLED=0 GOOS=linux go build -o marking-service ./main.go

# Stage 2: Финальный образ
FROM alpine:latest

# Добавляем сертификаты и tzdata для настройки времени
RUN apk --no-cache add ca-certificates tzdata

WORKDIR /root/

# Складываем экзешник в системную папку /bin/
COPY --from=builder /app/marking-service /bin/marking-service

# Устанавливаем рабочую директорию туда, куда будет смотреть Volume из compose
WORKDIR /app/data

# Запуск приложения (оно создаст rovnoMark.db прямо в /app/data)
CMD ["/bin/marking-service"]
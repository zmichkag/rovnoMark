.PHONY: build-ui build run

# 1. Команда сборки только фронтенда
build-ui:
	cd web && npm install && npm run build

# 2. Команда сборки всего приложения (сначала UI, потом Go)
build: build-ui
	go build -ldflags="-s -w" -o bin/rovnomark ./cmd/rovnomark

# 3. Запуск скомпилированного бинарника
run:
	./bin/rovnomark
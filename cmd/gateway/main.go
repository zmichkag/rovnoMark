package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"rovnoMark/internal/api"
	"rovnoMark/internal/brand"
	"rovnoMark/internal/core"
	"rovnoMark/internal/drivers/bizerba"
	"rovnoMark/internal/drivers/extserver"
	"rovnoMark/internal/drivers/markem"
	"rovnoMark/internal/drivers/markem_async"
	"rovnoMark/internal/drivers/savema"
	scannerdrivers "rovnoMark/internal/drivers/scanners"
	"rovnoMark/internal/drivers/valentine"
	"rovnoMark/internal/drivers/videojet"
	"rovnoMark/internal/storage"
	"rovnoMark/internal/version"

	"rovnoMark/ui"
	"rovnoMark/ui2"
	ui_okk "rovnoMark/ui_okk"
)

const serviceName = "OpenMarkGateway"

func main() {
	debugMode := flag.Bool("debug", false, "включить расширенный дебаг-режим")
	port := flag.Int("port", 8080, "порт для HTTP сервера")
	validateGS1 := flag.Bool("validate-gs1", false, "включить жесткую валидацию структуры GS1 DataMatrix кодов от 1С")
	dataDir := flag.String("data-dir", "./data", "путь к директории с базами данных SQLite")
	storageModeFlag := flag.String("storage-mode", "", "режим хранилища: 'sharded' или 'monolith'")
	flag.Parse()

	// Приоритет: CLI-флаг -> ENV -> default ("sharded")
	modeStr := string(storage.StorageModeSharded)
	if envMode := os.Getenv("STORAGE_MODE"); envMode != "" {
		modeStr = envMode
	}
	if *storageModeFlag != "" {
		modeStr = *storageModeFlag
	}

	mode := storage.StorageMode(modeStr)
	if mode != storage.StorageModeSharded && mode != storage.StorageModeMonolith {
		slog.Warn("Некорректный режим хранилища, откат на sharded", "provided", modeStr)
		mode = storage.StorageModeSharded
	}

	logLevel := new(slog.LevelVar)
	if *debugMode {
		logLevel.Set(slog.LevelDebug)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel}))
	slog.SetDefault(logger)

	slog.Info("Инициализация сервиса",
		"storage_mode", mode,
		"data_dir", *dataDir,
	)

	// Передаем mode в runner/runApp
	runner := func(ctx context.Context) error {
		return runApp(ctx, *port, *dataDir, mode, *validateGS1, *debugMode)
	}

	// 1. Проверка среды выполнения: запуск под управлением Windows SCM
	if isWindowsService() {
		if err := runWindowsService(serviceName, runner); err != nil {
			slog.Error("Сбой выполнения службы Windows", "err", err)
			os.Exit(1)
		}
		return
	}

	// 2. Консольный запуск (Linux / Docker / Windows CLI)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := runner(ctx); err != nil {
		slog.Error("Остановка шлюза с ошибкой", "err", err)
		os.Exit(1)
	}
}

func runApp(ctx context.Context, port int, dataDir string, mode storage.StorageMode, validateGS1, debugMode bool) error {
	slog.Info(fmt.Sprintf("Запуск шлюза маркировки [%s]", brand.GetName()),
		"version", version.Version,
		"commit", version.GitCommit,
		"build_date", version.BuildDate,
		"port", port,
		"debug", debugMode,
		"validate_gs1", validateGS1,
	)

	store := storage.New(dataDir, mode)
	defer func() {
		slog.Info("Завершение работы шлюза, закрытие соединений...")
		if err := store.Close(); err != nil {
			slog.Error("Ошибка при закрытии хранилища", "err", err)
		} else {
			slog.Info("Хранилище успешно остановлено")
		}
	}()

	manager := core.NewPrinterManager()
	defer func() {
		slog.Info("Освобождение сетевых ресурсов и COM-портов оборудования...")
		if err := manager.CloseAll(); err != nil {
			slog.Error("Ошибка закрытия драйверов оборудования", "err", err)
		}
	}()

	taskProcessor := &core.TaskProcessor{Store: store, Manager: manager}

	// 1. Инициализация принтеров и весов
	savedPrinters, errPrinters := store.GetAllPrinters()
	if errPrinters != nil {
		slog.Error("Ошибка вычитки оборудования из БД", "err", errPrinters)
	}

	for _, cfg := range savedPrinters {
		switch cfg.DriverType {
		case "savema":
			manager.AddPrinter(cfg, savema.New(cfg.IP, cfg.Port))
		case "videojet":
			manager.AddPrinter(cfg, videojet.New(cfg.IP, cfg.Port))
		case "valentine_nice":
			manager.AddPrinter(cfg, valentine.NewNiceLabelDriver(cfg.ID, cfg.IP, cfg.Port))
		case "markem":
			manager.AddPrinter(cfg, markem.New(cfg.IP, cfg.Port, "Actor1"))
		case "markem_async":
			manager.AddPrinter(cfg, markem_async.New(cfg.IP, cfg.Port, "Actor1"))
		case "ext_server", "nicelabel_http":
			manager.AddPrinter(cfg, extserver.New(cfg.IP, cfg.Port))
		case "bizerba":
			manager.AddPrinter(cfg, bizerba.CreateDriver(cfg, store))
		default:
			slog.Warn("Неизвестный тип драйвера оборудования", "type", cfg.DriverType, "id", cfg.ID)
		}
	}

	// 2. Инициализация подсистемы технического зрения (сканеров)
	scannerMgr := core.NewScannerManager(store)
	defer func() {
		if err := scannerMgr.Close(); err != nil {
			slog.Error("Ошибка закрытия драйверов сканеров", "err", err)
		}
	}()
	savedScanners, err := store.GetAllScanners()
	if err != nil {
		slog.Error("Не удалось загрузить конфигурации сканеров", "err", err)
	} else {
		for _, cfg := range savedScanners {
			if !cfg.IsActive {
				continue
			}
			switch cfg.DriverType {
			case "tcp_camera":
				scannerMgr.AddScanner(cfg, scannerdrivers.NewTCPCamera(cfg))
			default:
				slog.Warn("Неизвестный тип драйвера сканера", "type", cfg.DriverType, "id", cfg.ID)
			}
		}
	}
	go scannerMgr.StartPoller(ctx)

	// 3. Запуск фоновых процессов опроса и сбора телеметрии
	go manager.BackgroundPoller(ctx, store)
	manager.StartTelemetryCollector(store, 5*time.Minute)

	// 4. Восстановление активных заданий конвейера (Pumper Recovery)
	time.Sleep(500 * time.Millisecond)

	activeTasks, err := store.GetActiveTasks(0, 0)
	if err == nil && len(activeTasks) > 0 {
		slog.Info("Обнаружены активные задачи в БД. Восстановление фоновых насосов...", "count", len(activeTasks))
		for _, taskMap := range activeTasks {
			taskID, _ := strconv.Atoi(fmt.Sprintf("%v", taskMap["task_id"]))
			lineID, _ := strconv.Atoi(fmt.Sprintf("%v", taskMap["line_id"]))
			if taskID > 0 && lineID > 0 {
				taskProcessor.StartPumping(lineID, taskID)
			}
		}
	}

	// 5. Подготовка встроенных веб-интерфейсов
	contentUI, _ := fs.Sub(ui.FS, ".")
	contentUI2, _ := fs.Sub(ui2.FS, ".")
	contentOKK, _ := fs.Sub(ui_okk.FS, ".")

	// 6. Инициализация HTTP API (передаем и принтеры, и сканеры)
	apiServer := api.NewServer(store, manager, scannerMgr, taskProcessor, validateGS1, contentUI, contentUI2, contentOKK)
	router := apiServer.InitRoutes()

	httpServer := &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: router,
	}

	serverErrors := make(chan error, 1)
	go func() {
		slog.Info("HTTP сервер запущен", "address", "http://localhost"+httpServer.Addr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErrors <- err
		}
	}()

	// 7. Корректная остановка (Graceful Shutdown)
	select {
	case <-ctx.Done():
		slog.Info("Сигнал остановки получен. Завершение работы HTTP сервера...")
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()

		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			slog.Error("Принудительная остановка HTTP сервера", "err", err)
			return err
		}
		slog.Info("HTTP сервер штатно остановлен")
		return nil

	case err := <-serverErrors:
		slog.Error("Критическая ошибка HTTP сервера", "err", err)
		return fmt.Errorf("сбой HTTP сервера: %w", err)
	}
}

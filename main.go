package main

import (
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"log/slog"
	"net/http"
	"os"
	"rovnoMark/internal/core"
	"rovnoMark/internal/models"
	"strconv"
	"strings"
	"time"

	"rovnoMark/internal/drivers/savema"
	"rovnoMark/internal/drivers/videojet"
	"rovnoMark/internal/storage"
)

//go:embed ui/*
var uiFS embed.FS

// sendJSONError отправляет стандартизированный JSON-ответ с ошибкой
func sendJSONError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{
		"error": msg,
	})
}

func main() {
	debugMode := flag.Bool("debug", false, "включить расширенный дебаг-режим")
	port := flag.Int("port", 8080, "порт для HTTP сервера")
	flag.Parse()

	logLevel := new(slog.LevelVar)
	if *debugMode {
		logLevel.Set(slog.LevelDebug)
	}

	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel})
	logger := slog.New(handler)
	slog.SetDefault(logger)

	slog.Info("Запуск сервиса РОВНО", "port", *port, "debug", *debugMode)

	store := storage.New("rovnoMark.db")
	manager := core.NewPrinterManager()
	taskProcessor := &core.TaskProcessor{
		Store:   store,
		Manager: manager,
	}

	savedPrinters, _ := store.GetAllPrinters()
	for _, cfg := range savedPrinters {
		if cfg.DriverType == "savema" {
			manager.AddPrinter(cfg, savema.New(cfg.IP, cfg.Port))
		} else if cfg.DriverType == "videojet" {
			manager.AddPrinter(cfg, videojet.New(cfg.IP, cfg.Port))
		}
	}

	go manager.BackgroundPoller()
	manager.StartTelemetryCollector(store, 5*time.Minute)

	// 2. API для добавления нового принтера
	http.HandleFunc("/api/printers/add", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Only POST", http.StatusMethodNotAllowed)
			return
		}
		var cfg models.PrinterConfig
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		newID, err := store.SavePrinter(cfg)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		cfg.ID = int(newID)
		switch cfg.DriverType {
		case "savema":
			manager.AddPrinter(cfg, savema.New(cfg.IP, cfg.Port))
		case "videojet":
			manager.AddPrinter(cfg, videojet.New(cfg.IP, cfg.Port))
		default:
			http.Error(w, "Неизвестный тип драйвера", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"printer_id": newID})
	})

	// 3. API для дашборда (Мониторинг)
	http.HandleFunc("/api/printers", func(w http.ResponseWriter, r *http.Request) {
		states, logs := manager.GetDashboardData()
		configs, _ := store.GetAllPrinters()
		lines, _ := store.GetAllLines()
		lineMap, _ := store.GetPrinterLineMap()

		type PrinterInfo struct {
			models.PrinterConfig
			models.PrinterState
		}
		type LineGroup struct {
			models.LineConfig
			Printers []PrinterInfo `json:"printers"`
		}

		grouped := make(map[int][]PrinterInfo)
		var allForUI []PrinterInfo
		for _, cfg := range configs {
			info := PrinterInfo{PrinterConfig: cfg, PrinterState: states[cfg.ID]}
			allForUI = append(allForUI, info)
			if lineID, ok := lineMap[cfg.ID]; ok {
				grouped[lineID] = append(grouped[lineID], info)
			}
		}

		var responseLines []LineGroup
		for _, l := range lines {
			responseLines = append(responseLines, LineGroup{
				LineConfig: l,
				Printers:   grouped[l.ID],
			})
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"lines":        responseLines,
			"all_printers": allForUI,
			"logs":         logs,
		})
	})

	// Получение и создание линий
	http.HandleFunc("/api/lines", func(w http.ResponseWriter, r *http.Request) {
		// Если это запрос на получение списка линий (GET)
		if r.Method == http.MethodGet {
			lines, _ := store.GetAllLines()
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(lines)
			return
		}

		// Если это запрос на создание новой линии (POST)
		if r.Method == http.MethodPost {
			var l models.LineConfig
			if err := json.NewDecoder(r.Body).Decode(&l); err != nil {
				http.Error(w, "Ошибка парсинга JSON", http.StatusBadRequest)
				return
			}

			// Сохраняем в базу (метод SaveLine уже есть в вашем storage.go)
			if err := store.SaveLine(l); err != nil {
				http.Error(w, "Ошибка сохранения в БД", http.StatusInternalServerError)
				return
			}

			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"status":"ok"}`))
			return
		}

		http.Error(w, "Метод не поддерживается", http.StatusMethodNotAllowed)
	})

	// Статистика, если что
	http.HandleFunc("/api/stats", func(w http.ResponseWriter, r *http.Request) {
		printerIDStr := r.URL.Query().Get("printer_id")
		limitStr := r.URL.Query().Get("limit")

		idInt, _ := strconv.Atoi(printerIDStr)
		limit, err := strconv.Atoi(limitStr)
		if err != nil || limit <= 0 {
			limit = 100 // По умолчанию 100 записей
		}

		data, err := store.GetTelemetry(idInt, limit)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(data)
	})

	// Универсальный эндпоинт для работы с привязками (Линия <-> Принтер)
	http.HandleFunc("/api/assignments", func(w http.ResponseWriter, r *http.Request) {
		// 1. Получение списка привязок (GET)
		if r.Method == http.MethodGet {
			data, err := store.GetAssignments()
			if err != nil {
				http.Error(w, "Ошибка получения данных", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(data)
			return
		}

		// 2. Создание или обновление привязки (POST)
		if r.Method == http.MethodPost {
			var req struct {
				LineID    int    `json:"line_id"`
				PrinterID int    `json:"printer_id"`
				Role      string `json:"role"`
			}

			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, "Ошибка парсинга JSON", http.StatusBadRequest)
				return
			}

			// Вызываем метод из storage.go (он использует INSERT OR REPLACE)
			err := store.AssignPrinterToLine(req.LineID, req.PrinterID, req.Role)
			if err != nil {
				http.Error(w, "Ошибка БД", http.StatusInternalServerError)
				return
			}

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"status":     "assigned",
				"line_id":    req.LineID,
				"printer_id": req.PrinterID,
			})
			return
		}

		http.Error(w, "Метод не поддерживается", http.StatusMethodNotAllowed)
	})

	// Получение списка шаблонов из памяти принтера (GET /api/templates?printer_id=X)
	http.HandleFunc("/api/templates", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Только GET", http.StatusMethodNotAllowed)
			return
		}

		printerIDStr := r.URL.Query().Get("printer_id")
		idInt, err := strconv.Atoi(printerIDStr)
		if err != nil {
			http.Error(w, "Неверный ID принтера", http.StatusBadRequest)
			return
		}

		p := manager.GetPrinter(idInt)
		if p == nil {
			http.Error(w, "Принтер не найден (возможно, отключен или не в сети)", http.StatusNotFound)
			return
		}

		templates, err := p.GetTemplates()
		if err != nil {
			http.Error(w, "Ошибка чтения шаблонов: "+err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(templates)
	})

	// Получение списка полей конкретного шаблона (GET /api/template/fields?printer_id=X&template=Y)
	http.HandleFunc("/api/template/fields", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Только GET", http.StatusMethodNotAllowed)
			return
		}

		printerIDStr := r.URL.Query().Get("printer_id")
		templateName := r.URL.Query().Get("template")

		idInt, err := strconv.Atoi(printerIDStr)
		if err != nil || templateName == "" {
			http.Error(w, "Неверный ID принтера или пустое имя шаблона", http.StatusBadRequest)
			return
		}

		p := manager.GetPrinter(idInt)
		if p == nil {
			http.Error(w, "Принтер не найден", http.StatusNotFound)
			return
		}

		fields, err := p.GetTemplateFields(templateName)
		if err != nil {
			http.Error(w, "Ошибка чтения полей: "+err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(fields)
	})

	http.HandleFunc("/api/task/create", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			sendJSONError(w, http.StatusMethodNotAllowed, "Only POST allowed")
			return
		}

		var req struct {
			LineID           int               `json:"line_id"`
			TemplateName     string            `json:"template_name"`
			DynamicFieldName string            `json:"dynamic_field_name"`
			StaticFields     map[string]string `json:"static_fields"`
		}

		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			sendJSONError(w, http.StatusBadRequest, "Invalid JSON")
			return
		}

		// 1. Проверяем принтеры
		printersInLine, err := store.GetPrintersByLine(req.LineID)
		if err != nil || len(printersInLine) == 0 {
			sendJSONError(w, http.StatusNotFound, "Линия пуста или не найдена")
			return
		}

		// 2. Выполняем Handshake
		for _, pCfg := range printersInLine {
			p := manager.GetPrinter(pCfg.ID)
			if p == nil {
				continue
			}

			status, _ := p.GetStatus()
			// Принтер должен быть готов (или уже печатать, если мы просто подливаем статику)
			if status == "ОШИБКА" || strings.Contains(status, "ОФФЛАЙН") {
				sendJSONError(w, http.StatusConflict, fmt.Sprintf("Принтер %s не в сети", pCfg.Name))
				return
			}

			if req.DynamicFieldName == "" {
				// Если динамические поля не заданы
				p.ClearQueue()
				if err := p.SelectTemplate(req.TemplateName, req.StaticFields); err != nil {
					sendJSONError(w, http.StatusInternalServerError, "Ошибка SLA: "+err.Error())
					return
				}
			} else {
				// нормальная работа с Чз
				if err := p.SelectTemplate(req.TemplateName, nil); err != nil {
					sendJSONError(w, http.StatusInternalServerError, "Ошибка макета: "+err.Error())
					return
				}
				if err := p.InitSession(req.DynamicFieldName, 2000); err != nil {
					sendJSONError(w, http.StatusInternalServerError, "Ошибка SHO: "+err.Error())
					return
				}
				if err := p.UpdateStaticFields(req.StaticFields); err != nil {
					sendJSONError(w, http.StatusInternalServerError, "Ошибка SCF: "+err.Error())
					return
				}
			}
			manager.UpdatePrinterDeltaState(pCfg.ID, req.TemplateName, fmt.Sprintf("%v", req.StaticFields))
		}

		// 3. Сохраняем и запускаем
		staticBytes, _ := json.Marshal(req.StaticFields)
		taskID, err := store.CreateTask(req.LineID, req.TemplateName, req.DynamicFieldName, string(staticBytes))
		if err != nil {
			sendJSONError(w, http.StatusInternalServerError, "Ошибка БД: "+err.Error())
			return
		}

		// Запускаем фоновый насос только для маркировки
		if req.DynamicFieldName != "" {
			taskProcessor.StartPumping(req.LineID, int(taskID))
		}

		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]interface{}{"status": "active", "task_id": taskID})
	})

	http.HandleFunc("/api/task/append", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			TaskID int      `json:"task_id"`
			Codes  []string `json:"codes"`
		}

		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			slog.Warn("Append: ошибка JSON", "err", err)
			sendJSONError(w, http.StatusBadRequest, "Invalid JSON")
			return
		}

		// Просто складываем коды в базу. Насос (StartPumping),
		// запущенный при create, сам их заберет и распределит.
		err := store.AppendTaskCodes(req.TaskID, req.Codes)
		if err != nil {
			slog.Error("Append: Ошибка записи в БД", "task_id", req.TaskID, "err", err)
			sendJSONError(w, http.StatusInternalServerError, "Ошибка БД при сохранении кодов")
			return
		}

		slog.Info("Коды добавлены в задачу", "task_id", req.TaskID, "count", len(req.Codes))

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status": "received",
			"count":  len(req.Codes),
		})
	})

	// Метод Graceful Stop: корректное завершение печати и сверка остатков
	http.HandleFunc("/api/task/stop", func(w http.ResponseWriter, r *http.Request) {
		// 1. Получаем ID задачи из запроса
		taskIDStr := r.URL.Query().Get("id")
		taskID, err := strconv.Atoi(taskIDStr)
		if err != nil {
			http.Error(w, "Invalid Task ID", http.StatusBadRequest)
			return
		}

		// 2. Нам нужно знать, на какой линии работала задача, чтобы найти принтеры
		// (Для этого можно либо передать line_id в запросе, либо быстро дернуть из БД)
		lineIDStr := r.URL.Query().Get("line_id")
		lineID, _ := strconv.Atoi(lineIDStr)

		log.Printf("[STOP] Инициирована остановка задачи ID:%d на линии %d", taskID, lineID)

		// 3. Опрашиваем принтеры на линии для финальной сверки
		printers, _ := store.GetPrintersByLine(lineID)
		report := make(map[string]interface{})

		for _, pCfg := range printers {
			p := manager.GetPrinter(pCfg.ID)
			if p == nil {
				continue
			}

			// А) Получаем последний реально напечатанный индекс (Команда SGP в Zipher)
			lastIdx, err := p.GetLastPrintedIndex()
			if err != nil {
				log.Printf("Ошибка получения индекса с принтера %d: %v", pCfg.ID, err)
				continue
			}

			// Б) Синхронизируем БД: всё, что принтер подтвердил, помечаем как 'printed'
			// Всё, что было в буфере, но не напечатано, останется в статусе 'in_buffer' или вернется в 'pending'
			affected, _ := store.MarkAsPrinted(taskID, lastIdx)

			// В) Очищаем физическую очередь принтера (Команда CQI), чтобы не допечатывать лишнее
			p.ClearQueue()

			report[pCfg.Name] = map[string]interface{}{
				"last_printed_index": lastIdx,
				"confirmed_codes":    affected,
				"status":             "cleared",
			}
		}

		// 4. Меняем статус задачи в БД на завершенную
		store.SetTaskStatus(taskID, "stopped")

		// 5. Отправляем отчет в 1С/MES
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"task_id": taskID,
			"status":  "stopped",
			"report":  report,
		})
	})

	// 5. Раздача UI (Frontend)
	content, _ := fs.Sub(uiFS, "ui")
	http.Handle("/", http.FileServer(http.FS(content)))

	addr := fmt.Sprintf(":%d", *port)
	slog.Info("HTTP сервер запущен", "address", "http://localhost"+addr)

	// Теперь ошибка сервера будет выводиться структурированно
	if err := http.ListenAndServe(addr, nil); err != nil {
		slog.Error("Критическая ошибка сервера", "err", err)
		os.Exit(1)
	}
}

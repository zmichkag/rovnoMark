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

	// 2. Настраиваем уровень логирования
	logLevel := new(slog.LevelVar) // по умолчанию INFO
	if *debugMode {
		logLevel.Set(slog.LevelDebug)
	}

	// Создаем логгер (текстовый для разработки или JSON для прода)
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

	// 1. Загружаем все принтеры из базы в работу
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

	// 4. API для отправки ПАЧКИ (Честный Знак)
	//http.HandleFunc("/api/batch", func(w http.ResponseWriter, r *http.Request) {
	//	if r.Method != http.MethodPost {
	//		http.Error(w, "Only POST", http.StatusMethodNotAllowed)
	//		return
	//	}
	//	var req struct {
	//		PrinterID string   `json:"printer_id"`
	//		FieldName string   `json:"field_name"`
	//		Codes     []string `json:"codes"`
	//	}
	//	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
	//		http.Error(w, "JSON Error", http.StatusBadRequest)
	//		return
	//	}
	//	idInt, _ := strconv.Atoi(req.PrinterID)
	//	p := manager.GetPrinter(idInt)
	//	if p == nil {
	//		http.Error(w, "Printer not found", http.StatusNotFound)
	//		return
	//	}
	//
	//	// Вызываем метод Batch через унифицированный интерфейс[cite: 4]
	//	loaded, err := p.PrintBatch(req.FieldName, req.Codes)
	//	if err != nil {
	//		http.Error(w, err.Error(), http.StatusInternalServerError)
	//		return
	//	}
	//
	//	w.Header().Set("Content-Type", "application/json")
	//	json.NewEncoder(w).Encode(map[string]interface{}{
	//		"status": "success",
	//		"loaded": loaded,
	//	})
	//})

	//// 4.1. НОВЫЙ API (Версия 1.5): Линейно-центричный и индексированный
	//http.HandleFunc("/api/v2/line/batch", func(w http.ResponseWriter, r *http.Request) {
	//	if r.Method != http.MethodPost {
	//		slog.Warn("Попытка доступа неверным методом", "method", r.Method, "remote", r.RemoteAddr)
	//		http.Error(w, "Only POST", http.StatusMethodNotAllowed)
	//		return
	//	}
	//
	//	var req struct {
	//		LineID       int               `json:"line_id"`
	//		Template     string            `json:"template"`
	//		StaticFields map[string]string `json:"static_fields"`
	//		DynamicField string            `json:"dynamic_field"`
	//		Codes        []string          `json:"codes"`
	//	}
	//
	//	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
	//		slog.Error("Ошибка декодирования JSON", "err", err)
	//		http.Error(w, "JSON Error", http.StatusBadRequest)
	//		return
	//	}
	//
	//	slog.Debug("Входящий запрос v2/batch",
	//		"line_id", req.LineID,
	//		"template", req.Template,
	//		"codes_count", len(req.Codes),
	//	)
	//
	//	// 1. Получаем список принтеров
	//	printersInLine, err := store.GetPrintersByLine(req.LineID)
	//	if err != nil {
	//		slog.Error("Ошибка обращения к БД при поиске принтеров", "line_id", req.LineID, "err", err)
	//		http.Error(w, "DB Error", http.StatusInternalServerError)
	//		return
	//	}
	//
	//	if len(printersInLine) == 0 {
	//		slog.Warn("Запрос на пустую линию", "line_id", req.LineID)
	//		http.Error(w, "No printers on this line", http.StatusNotFound)
	//		return
	//	}
	//
	//	slog.Debug("Принтеры на линии найдены", "count", len(printersInLine), "line_id", req.LineID)
	//
	//	for _, pCfg := range printersInLine {
	//		p := manager.GetPrinter(pCfg.ID)
	//		if p == nil {
	//			slog.Warn("Принтер числится в БД, но отсутствует в памяти менеджера", "id", pCfg.ID)
	//			continue
	//		}
	//
	//		state := manager.GetPrinterState(pCfg.ID)
	//		l := slog.With("printer_id", pCfg.ID, "printer_name", pCfg.Name)
	//
	//		// --- ЛОГИКА DELTA ---
	//
	//		// А) Проверка и смена шаблона
	//		if state.LastTemplate != req.Template {
	//			l.Debug("Delta: Смена шаблона", "old", state.LastTemplate, "new", req.Template)
	//
	//			if err := p.SelectTemplate(req.Template, nil); err != nil {
	//				l.Error("Delta: Ошибка смены шаблона", "err", err)
	//				continue
	//			}
	//			// Обновляем состояние в памяти, чтобы не дергать SelectTemplate в следующий раз
	//			manager.UpdatePrinterDeltaState(pCfg.ID, req.Template, state.LastStaticHash)
	//			l.Info("Delta: Шаблон успешно изменен")
	//		} else {
	//			l.Debug("Delta: Шаблон не изменился, пропускаем SLA")
	//		}
	//
	//		// Б) Проверка статики
	//		currentHash := fmt.Sprintf("%v", req.StaticFields)
	//		if state.LastStaticHash != currentHash && len(req.StaticFields) > 0 {
	//			l.Debug("Delta: Обнаружено изменение статических полей",
	//				"old_hash", state.LastStaticHash,
	//				"new_hash", currentHash,
	//			)
	//
	//			if err := p.UpdateStaticFields(req.StaticFields); err != nil {
	//				l.Error("Delta: Ошибка обновления статики", "err", err)
	//				continue
	//			}
	//
	//			manager.UpdatePrinterDeltaState(pCfg.ID, req.Template, currentHash)
	//			l.Info("Delta: Статические поля обновлены")
	//		} else {
	//			l.Debug("Delta: Статика не изменилась, пропускаем SCF")
	//		}
	//
	//		// --- ПЕЧАТЬ SID ---
	//		startIndex := int(time.Now().Unix())
	//		l.Debug("Запуск загрузки пачки SID", "start_index", startIndex, "field", req.DynamicField)
	//
	//		loaded, err := p.PrintBatchIndexed(req.DynamicField, startIndex, req.Codes)
	//		if err != nil {
	//			l.Error("Критическая ошибка печати пачки", "err", err)
	//			continue
	//		}
	//
	//		l.Info("Пачка успешно загружена",
	//			"loaded", loaded,
	//			"total_sent", len(req.Codes),
	//			"start_index", startIndex,
	//		)
	//	}
	//
	//	w.Header().Set("Content-Type", "application/json")
	//	json.NewEncoder(w).Encode(map[string]string{"status": "accepted"})
	//})

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

	// Регистрация новой партии
	http.HandleFunc("/api/task/create", func(w http.ResponseWriter, r *http.Request) {
		// 1. Проверяем метод запроса
		if r.Method != http.MethodPost {
			http.Error(w, "Only POST", http.StatusMethodNotAllowed)
			return
		}

		// 2. Декодируем запрос с проверкой на ошибки
		var req struct {
			LineID   int    `json:"line_id"`
			Template string `json:"template"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}

		// 3. Создаем задачу в базе данных
		// Метод возвращает ID новой записи
		taskID, err := store.CreateTask(req.LineID, req.Template)
		if err != nil {
			http.Error(w, "DB Error: "+err.Error(), http.StatusInternalServerError)
			return
		}

		// --- ИНИЦИАЛИЗАЦИЯ ПРИНТЕРА ---
		printers, _ := store.GetPrintersByLine(req.LineID)
		for _, pCfg := range printers {
			p := manager.GetPrinter(pCfg.ID)
			if p != nil {
				// Загружаем макет в память (SLA)
				p.SelectTemplate(req.Template, nil)
				// Очищаем буфер и указываем, в какое поле будем лить переменные (SHO, SCB)
				// 2000 - достаточный лимит очереди для Videojet
				p.InitSession("code", 2000)
			}
		}

		// 4. ЗАПУСКАЕМ НАКАЧКУ (Task Pumping)
		taskProcessor.StartPumping(req.LineID, int(taskID))

		// Логируем для контроля в консоли
		log.Printf("[TASK] Создана активная партия ID:%d для линии %d (Шаблон: %s)", taskID, req.LineID, req.Template)

		// 5. Отправляем успешный ответ
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":  "active",
			"task_id": taskID,
			"message": "Партия запущена, процесс накачки кодов активирован",
		})
	})

	http.HandleFunc("/api/task/append", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			sendJSONError(w, http.StatusMethodNotAllowed, "Only POST allowed")
			return
		}

		var req struct {
			TaskID     int      `json:"task_id"`
			PrinterSeq int      `json:"printer_seq"` // Порядковый номер на линии (1, 2...)
			Codes      []string `json:"codes"`
		}

		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			sendJSONError(w, http.StatusBadRequest, "Invalid JSON")
			return
		}

		// 1. Находим Линию по ID задачи напрямую через Store
		lineID, err := store.GetLineIDByTask(req.TaskID)
		if err != nil {
			sendJSONError(w, http.StatusNotFound, "Задача не найдена")
			return
		}

		// 2. Получаем список принтеров этой линии
		printers, err := store.GetPrintersByLine(lineID)
		if err != nil || len(printers) == 0 {
			sendJSONError(w, http.StatusInternalServerError, "Ошибка получения конфигурации линии")
			return
		}

		// 3. Определяем целевой принтер по порядковому номеру (seq)
		// 1С шлет 1, 2, 3... мы переводим в индекс массива 0, 1, 2...
		if req.PrinterSeq < 1 || req.PrinterSeq > len(printers) {
			sendJSONError(w, http.StatusBadRequest, fmt.Sprintf("На линии %d всего %d принтеров. Вы запросили №%d", lineID, len(printers), req.PrinterSeq))
			return
		}
		targetPrinter := printers[req.PrinterSeq-1]

		// 4. Загружаем коды в базу
		err = store.AppendTaskCodes(req.TaskID, targetPrinter.ID, req.Codes)
		if err != nil {
			slog.Error("Append: Ошибка вставки кодов", "err", err)
			sendJSONError(w, http.StatusInternalServerError, "Ошибка БД при сохранении кодов")
			return
		}

		slog.Info("Append: Коды успешно приняты",
			"task", req.TaskID,
			"line", lineID,
			"printer", targetPrinter.Name,
			"count", len(req.Codes))

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":  "received",
			"task_id": req.TaskID,
			"printer": targetPrinter.Name,
			"added":   len(req.Codes),
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

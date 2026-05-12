package main

import (
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
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

//
//go:embed ui/*
var uiFS embed.FS

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
	//taskProcessor := &core.TaskProcessor{
	//	Store:   store,
	//	Manager: manager,
	//}

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

			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"status":"assigned"}`))
			return
		}

		http.Error(w, "Метод не поддерживается", http.StatusMethodNotAllowed)
	})

	// 4. API для отправки ПАЧКИ (Честный Знак)
	http.HandleFunc("/api/batch", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Only POST", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			PrinterID string   `json:"printer_id"`
			FieldName string   `json:"field_name"`
			Codes     []string `json:"codes"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "JSON Error", http.StatusBadRequest)
			return
		}
		idInt, _ := strconv.Atoi(req.PrinterID)
		p := manager.GetPrinter(idInt)
		if p == nil {
			http.Error(w, "Printer not found", http.StatusNotFound)
			return
		}

		// Вызываем метод Batch через унифицированный интерфейс[cite: 4]
		loaded, err := p.PrintBatch(req.FieldName, req.Codes)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status": "success",
			"loaded": loaded,
		})
	})

	// 4.1. НОВЫЙ API (Версия 1.5): Линейно-центричный и индексированный
	http.HandleFunc("/api/v2/line/batch", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			slog.Warn("Попытка доступа неверным методом", "method", r.Method, "remote", r.RemoteAddr)
			http.Error(w, "Only POST", http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			LineID       int               `json:"line_id"`
			Template     string            `json:"template"`
			StaticFields map[string]string `json:"static_fields"`
			DynamicField string            `json:"dynamic_field"`
			Codes        []string          `json:"codes"`
		}

		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			slog.Error("Ошибка декодирования JSON", "err", err)
			http.Error(w, "JSON Error", http.StatusBadRequest)
			return
		}

		slog.Debug("Входящий запрос v2/batch",
			"line_id", req.LineID,
			"template", req.Template,
			"codes_count", len(req.Codes),
		)

		// 1. Получаем список принтеров
		printersInLine, err := store.GetPrintersByLine(req.LineID)
		if err != nil {
			slog.Error("Ошибка обращения к БД при поиске принтеров", "line_id", req.LineID, "err", err)
			http.Error(w, "DB Error", http.StatusInternalServerError)
			return
		}

		if len(printersInLine) == 0 {
			slog.Warn("Запрос на пустую линию", "line_id", req.LineID)
			http.Error(w, "No printers on this line", http.StatusNotFound)
			return
		}

		slog.Debug("Принтеры на линии найдены", "count", len(printersInLine), "line_id", req.LineID)

		for _, pCfg := range printersInLine {
			p := manager.GetPrinter(pCfg.ID)
			if p == nil {
				slog.Warn("Принтер числится в БД, но отсутствует в памяти менеджера", "id", pCfg.ID)
				continue
			}

			state := manager.GetPrinterState(pCfg.ID)
			l := slog.With("printer_id", pCfg.ID, "printer_name", pCfg.Name)

			// --- ЛОГИКА DELTA ---

			// А) Проверка и смена шаблона
			if state.LastTemplate != req.Template {
				l.Debug("Delta: Смена шаблона", "old", state.LastTemplate, "new", req.Template)

				// Добавляем nil, так как здесь нам нужно просто переключить макет
				if err := p.SelectTemplate(req.Template, nil); err != nil {
					l.Error("Delta: Ошибка смены шаблона", "err", err)
					continue
				}

				// Обновляем состояние в памяти
				manager.UpdatePrinterDeltaState(pCfg.ID, req.Template, state.LastStaticHash)
				l.Info("Delta: Шаблон успешно изменен")
			}

			// Б) Проверка статики
			currentHash := fmt.Sprintf("%v", req.StaticFields)
			if state.LastStaticHash != currentHash && len(req.StaticFields) > 0 {
				l.Debug("Delta: Обнаружено изменение статических полей",
					"old_hash", state.LastStaticHash,
					"new_hash", currentHash,
				)

				if err := p.UpdateStaticFields(req.StaticFields); err != nil {
					l.Error("Delta: Ошибка обновления статики", "err", err)
					continue
				}

				manager.UpdatePrinterDeltaState(pCfg.ID, req.Template, currentHash)
				l.Info("Delta: Статические поля обновлены")
			} else {
				l.Debug("Delta: Статика не изменилась, пропускаем SCF")
			}

			// --- ПЕЧАТЬ SID ---
			startIndex := int(time.Now().Unix())
			l.Debug("Запуск загрузки пачки SID", "start_index", startIndex, "field", req.DynamicField)

			loaded, err := p.PrintBatchIndexed(req.DynamicField, startIndex, req.Codes)
			if err != nil {
				l.Error("Критическая ошибка печати пачки", "err", err)
				continue
			}

			l.Info("Пачка успешно загружена",
				"loaded", loaded,
				"total_sent", len(req.Codes),
				"start_index", startIndex,
			)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "accepted"})
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

	// Регистрация новой партии с проверкой оборудования (Handshake)
	http.HandleFunc("/api/task/create", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.Method != http.MethodPost {
			sendJSONError(w, http.StatusMethodNotAllowed, "Only POST allowed")
			return
		}

		var req struct {
			LineID           int               `json:"line_id"`
			Template         string            `json:"template_name"`
			DynamicFieldName string            `json:"dynamic_field_name"` // Может быть пустым для статики
			StaticFields     map[string]string `json:"static_fields"`
		}

		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			sendJSONError(w, http.StatusBadRequest, "Invalid JSON")
			return
		}

		// --- 1. ПРОВЕРКА СУЩЕСТВОВАНИЯ ЛИНИИ ---
		exists, err := store.CheckLineExists(req.LineID)
		if err != nil || !exists {
			sendJSONError(w, http.StatusNotFound, "Линия не найдена")
			return
		}

		// --- 2. МЕХАНИЗМ БЛОКИРОВКИ: Есть ли активная задача? ---
		activeID, activeTmpl, _ := store.GetActiveTaskID(req.LineID)
		if activeID != 0 {
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"error":            "Line is busy",
				"active_task_id":   activeID,
				"current_template": activeTmpl,
			})
			return
		}

		// --- 3. ПОИСК ПРИНТЕРОВ ---
		printersInLine, _ := store.GetPrintersByLine(req.LineID)
		if len(printersInLine) == 0 {
			sendJSONError(w, http.StatusUnprocessableEntity, "На линии нет принтеров")
			return
		}

		// --- 4. HANDSHAKE
		// --- 4. HANDSHAKE (РАЗВИЛКА СТАТИКА / ДИНАМИКА) ---
		for _, pCfg := range printersInLine {
			p := manager.GetPrinter(pCfg.ID)
			if p == nil {
				continue
			}

			// Проверка статуса (ГОТОВ)
			status, _ := p.GetStatus()
			if status != "ГОТОВ" {
				sendJSONError(w, http.StatusConflict, fmt.Sprintf("Принтер %s занят (%s)", pCfg.Name, status))
				return
			}

			if req.DynamicFieldName == "" {
				// --- ПУТЬ А: СТАТИЧЕСКАЯ ПЕЧАТЬ (ВСЁ В ОДНОМ SLA) ---
				slog.Info("Handshake: Статический режим", "printer", pCfg.Name)

				// Очищаем старые очереди кодов (на всякий случай)
				p.ClearQueue()

				// Вызываем обновленный SelectTemplate с полями. Принтер загрузит макет и СРАЗУ обновит текст.
				if err := p.SelectTemplate(req.Template, req.StaticFields); err != nil {
					sendJSONError(w, http.StatusBadRequest, fmt.Sprintf("Ошибка SLA на %s: %v", pCfg.Name, err))
					return
				}
			} else {
				// --- ПУТЬ Б: ДИНАМИЧЕСКАЯ ПЕЧАТЬ (МАРКИРОВКА) ---
				slog.Info("Handshake: Динамический режим (ЧЗ)", "field", req.DynamicFieldName)

				// 1. Сначала просто выбираем макет (SLA без полей)
				if err := p.SelectTemplate(req.Template, nil); err != nil {
					sendJSONError(w, http.StatusBadRequest, "Макет не найден")
					return
				}

				// 2. Инициализируем сессию (SHO). Теперь принтер ждет SCF и данные для SID/SDO.
				if err := p.InitSession(req.DynamicFieldName, 1000); err != nil {
					sendJSONError(w, http.StatusBadRequest, "Ошибка SHO: "+err.Error())
					return
				}

				// 3. Теперь SCF легален, так как поле сериализации задано!
				if err := p.UpdateStaticFields(req.StaticFields); err != nil {
					sendJSONError(w, http.StatusInternalServerError, "Ошибка SCF: "+err.Error())
					return
				}
			}

			// Фиксируем состояние
			currentHash := fmt.Sprintf("%v", req.StaticFields)
			manager.UpdatePrinterDeltaState(pCfg.ID, req.Template, currentHash)
		}

		// --- 5. СОХРАНЕНИЕ ЗАДАЧИ ---
		staticBytes, _ := json.Marshal(req.StaticFields)
		taskID, err := store.CreateTask(req.LineID, req.Template, req.DynamicFieldName, string(staticBytes))
		if err != nil {
			sendJSONError(w, http.StatusInternalServerError, "Ошибка записи задачи в БД")
			return
		}

		slog.Info("Задача создана", "id", taskID, "mode", "dynamic="+strconv.FormatBool(req.DynamicFieldName != ""))
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]interface{}{"status": "active", "task_id": taskID})
	})

	// Дозаливка кодов (Асинхронно)
	http.HandleFunc("/api/task/append", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			TaskID int64    `json:"task_id"`
			Codes  []string `json:"codes"`
		}
		json.NewDecoder(r.Body).Decode(&req)

		err := store.AppendCodes(req.TaskID, req.Codes)
		if err != nil {
			http.Error(w, "DB Error", http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]interface{}{"status": "appended", "count": len(req.Codes)})
	})

	http.HandleFunc("/api/task/stop", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			sendJSONError(w, http.StatusMethodNotAllowed, "Only POST allowed")
			return
		}

		var req struct {
			LineID int `json:"line_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			sendJSONError(w, http.StatusBadRequest, "Invalid JSON")
			return
		}

		// 1. Ищем, какая задача сейчас активна
		// Используем нормальное имя переменной, например activeTemplate
		taskID, activeTemplate, err := store.GetActiveTaskID(req.LineID)
		if err != nil || taskID == 0 {
			sendJSONError(w, http.StatusNotFound, "Нет активных задач на этой линии")
			return
		}

		// 2. СВЕРКА (RECONCILIATION)
		printers, _ := store.GetPrintersByLine(req.LineID)
		report := make(map[string]interface{})

		for _, pCfg := range printers {
			p := manager.GetPrinter(pCfg.ID)
			if p == nil {
				continue
			}

			lastIdx, err := p.GetLastPrintedIndex()
			if err != nil {
				slog.Error("Ошибка индекса", "printer", pCfg.Name, "err", err)
				continue
			}

			confirmed, _ := store.MarkAsPrinted(taskID, pCfg.ID, lastIdx)
			p.ClearQueue()

			report[pCfg.Name] = map[string]int{
				"last_index": lastIdx,
				"confirmed":  int(confirmed),
			}
		}

		// 3. Закрываем задачу в БД
		if err := store.StopTask(taskID); err != nil {
			sendJSONError(w, http.StatusInternalServerError, "Ошибка при закрытии задачи")
			return
		}

		// ИСПОЛЬЗУЕМ ПЕРЕМЕННУЮ ЗДЕСЬ:
		slog.Info("Задача остановлена",
			"task_id", taskID,
			"template", activeTemplate, // Теперь Go доволен
			"line", req.LineID,
		)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":   "stopped",
			"task_id":  taskID,
			"template": activeTemplate,
			"results":  report,
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

func sendJSONError(w http.ResponseWriter, code int, msg string) {
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

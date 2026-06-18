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
	"strings"
	"time"

	"rovnoMark/internal/drivers/savema"
	"rovnoMark/internal/drivers/tsc"
	"rovnoMark/internal/drivers/videojet"
	"rovnoMark/internal/storage"
)

var (
	Version   = "dev"
	BuildTime = "unknown"
	GitCommit = "unknown"
)

//go:embed ui/*
var uiFS embed.FS

// sendJSONError отправляет стандартизированный JSON-ответ с ошибкой
func sendJSONError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	err := json.NewEncoder(w).Encode(map[string]string{
		"error": msg,
	})
	if err != nil {
		return
	}
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

	// Найди эту строку (в районе 41-й) и замени её:
	// slog.Info("Запуск сервиса РОВНО", "port", *port, "debug", *debugMode)

	slog.Info("=== Запуск сервиса РОВНО ===",
		"version", Version,
		"build_time", BuildTime,
		"git_commit", GitCommit,
		"port", *port,
		"debug", *debugMode,
	)

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
		} else if cfg.DriverType == "tsc" { // <-- ДОБАВИТЬ ЭТО
			manager.AddPrinter(cfg, tsc.New(cfg.IP, cfg.Port))
		}
	}

	go manager.BackgroundPoller(store)
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
		case "tsc": // <-- ДОБАВИТЬ ЭТО
			manager.AddPrinter(cfg, tsc.New(cfg.IP, cfg.Port))
		default:
			http.Error(w, "Неизвестный тип драйвера", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		err = json.NewEncoder(w).Encode(map[string]interface{}{"printer_id": newID})
		if err != nil {
			return
		}
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
			"version":      Version,
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

		// Пытаемся вытащить тело макета из таблицы local_templates
		templateBody, errBody := store.GetTemplateBody(templateName)
		if errBody == nil && templateBody != "" {
			// Если драйвер поддерживает динамическое наполнение (TSC)
			if loader, ok := p.(core.LocalTemplateLoader); ok {
				loader.SetTemplateBody(templateBody)
			}
		}

		fields, err := p.GetTemplateFields(templateName)
		if err != nil {
			http.Error(w, "Ошибка чтения полей: "+err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(fields)
	})

	// Получение сырого текста шаблона из БД для визуализатора (GET /api/template/raw?template=Y)
	http.HandleFunc("/api/template/raw", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Только GET", http.StatusMethodNotAllowed)
			return
		}

		templateName := r.URL.Query().Get("template")
		if templateName == "" {
			http.Error(w, "Пустое имя шаблона", http.StatusBadRequest)
			return
		}

		// Достаем макет из нашей таблицы local_templates
		body, err := store.GetTemplateBody(templateName)
		if err != nil {
			http.Error(w, "Шаблон не найден в локальной БД: "+err.Error(), http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"name": templateName,
			"body": body,
		})
	})

	// Сохранение или обновление шаблона из конструктора (POST /api/template/save)
	http.HandleFunc("/api/template/save", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Только POST", http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			Name string `json:"name"`
			Body string `json:"body"`
		}

		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Ошибка парсинга JSON: "+err.Error(), http.StatusBadRequest)
			return
		}

		if req.Name == "" || req.Body == "" {
			http.Error(w, "Имя шаблона и его содержимое не могут быть пустыми", http.StatusBadRequest)
			return
		}

		// Выполняем запись напрямую в SQLite local_templates
		_, err := store.SaveTemplate(req.Name, req.Body) // Убедитесь, что метод в storage.go умеет делать INSERT OR REPLACE
		if err != nil {
			// Если метода SaveTemplate нет или он с другой сигнатурой, пишем SQL напрямую:
			// _, err = store.db.Exec("INSERT OR REPLACE INTO local_templates (name, body) VALUES (?, ?)", req.Name, req.Body)
			http.Error(w, "Ошибка сохранения шаблона в БД: "+err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "saved", "name": req.Name})
	})

	http.HandleFunc("/api/task/create", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			sendJSONError(w, http.StatusMethodNotAllowed, "Only POST allowed")
			return
		}

		// 1. Читаем line_id из Query-параметров URL, как просил Ваге
		lineIDStr := r.URL.Query().Get("line_id")
		lineID, err := strconv.Atoi(lineIDStr)
		if err != nil || lineID <= 0 {
			sendJSONError(w, http.StatusBadRequest, "Missing or invalid line_id parameter in URL")
			return
		}

		var req struct {
			TemplateName     string            `json:"template_name"`
			DynamicFieldName string            `json:"dynamic_field_name"`
			StaticFields     map[string]string `json:"static_fields"`
			RndText          string            `json:"rnd_text"`
		}

		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			sendJSONError(w, http.StatusBadRequest, "Invalid JSON")
			return
		}

		// 0. Проверяем, не занята ли линия другой задачей
		activeID, err := store.GetActiveTaskByLine(lineID)
		if err != nil {
			sendJSONError(w, http.StatusInternalServerError, "Ошибка проверки занятости линии")
			return
		}
		if activeID != 0 {
			sendJSONError(w, http.StatusConflict, fmt.Sprintf("Линия %d уже занята задачей %d. Сначала остановите её.", lineID, activeID))
			return
		}

		// 1. Проверяем принтеры
		printersInLine, err := store.GetPrintersByLine(lineID)
		if err != nil || len(printersInLine) == 0 {
			sendJSONError(w, http.StatusNotFound, "Линия пуста или не найдена")
			return
		}

		// 2. Выполняем Handshake и проверку связи с принтерами
		for _, pCfg := range printersInLine {
			p := manager.GetPrinter(pCfg.ID)
			if p == nil {
				sendJSONError(w, http.StatusConflict, fmt.Sprintf("Принтер %s не зарегистрирован в системе или отключен", pCfg.Name))
				return
			}

			// Свежий опрос статуса
			// Пытаемся получить статус «здесь и сейчас»
			status, err := p.GetStatus()

			// Собираем всё в одну строку для анализа (и ошибку сокета, и текстовый статус)
			var checkString string
			if err != nil {
				checkString = strings.ToUpper(err.Error())
			} else {
				checkString = strings.ToUpper(status)
			}

			// Словарь (массив) «плохих» статусов, при которых работать нельзя
			badStatuses := []string{
				"TIMEOUT",      // Сетевой таймаут сокета
				"INITIALIZING", // Инициализация (в вашем случае — застрявший оффлайн)
				"STARTING",     // Запуск устройства
				"ОФФЛАЙН",      //
				"OFFLINE",
				"ОШИБКА", //
				"ERROR",
				"REFUSED", // Сброс соединения сокетом
			}

			// Бежим по словарю и проверяем, не поймали ли мы проблему
			for _, bad := range badStatuses {
				if strings.Contains(checkString, bad) {
					slog.Warn("Принтер забракован перед стартом задачи", "printer", pCfg.Name, "detected_status", status, "err", err)

					sendJSONError(w, http.StatusServiceUnavailable, fmt.Sprintf(
						"Принтер %s не готов к работе (Текущее состояние: %s). Проверьте питание, сеть или устраните ошибку на устройстве.",
						pCfg.Name,
						status,
					))
					return // Полностью прерываем создание задачи, Ваге получает от ворот поворот
				}
			}

			// Если принтер успешно прошел сетевой и логический чек — только тогда применяем шаблон
			if req.DynamicFieldName == "" {
				// Если динамические поля не заданы
				p.ClearQueue()
				if err := p.SelectTemplate(req.TemplateName, req.StaticFields); err != nil {
					sendJSONError(w, http.StatusInternalServerError, fmt.Sprintf("Ошибка установки шаблона (SLA) на %s: %v", pCfg.Name, err))
					return
				}
			} else {
				// нормальная работа с Чз
				if err := p.SelectTemplate(req.TemplateName, nil); err != nil {
					sendJSONError(w, http.StatusInternalServerError, fmt.Sprintf("Ошибка макета на %s: %v", pCfg.Name, err))
					return
				}
				if err := p.InitSession(req.DynamicFieldName, 10); err != nil {
					sendJSONError(w, http.StatusInternalServerError, fmt.Sprintf("Ошибка инициализации сессии (SHO) на %s: %v", pCfg.Name, err))
					return
				}
				if err := p.UpdateStaticFields(req.StaticFields); err != nil {
					sendJSONError(w, http.StatusInternalServerError, fmt.Sprintf("Ошибка статических полей (SCF) на %s: %v", pCfg.Name, err))
					return
				}
			}
			manager.UpdatePrinterDeltaState(pCfg.ID, req.TemplateName, fmt.Sprintf("%v", req.StaticFields))
		}

		// 3. Сохраняем задачу
		staticBytes, _ := json.Marshal(req.StaticFields)
		taskID, err := store.CreateTask(lineID, req.TemplateName, req.DynamicFieldName, string(staticBytes), req.RndText)
		if err != nil {
			sendJSONError(w, http.StatusInternalServerError, "Ошибка БД: "+err.Error())
			return
		}

		w.WriteHeader(http.StatusOK)
		// Отвечаем 1С, что задача ГОТОВА к приему кодов
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":   "ready",
			"task_id":  taskID,
			"rnd_text": req.RndText,
		})
	})

	http.HandleFunc("/api/task/append", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			sendJSONError(w, http.StatusMethodNotAllowed, "Only POST allowed")
			return
		}

		// 1. Читаем task_id из Query-параметров URL, как просит Ваге
		taskIDStr := r.URL.Query().Get("task_id")
		taskID, err := strconv.Atoi(taskIDStr)
		if err != nil || taskID <= 0 {
			sendJSONError(w, http.StatusBadRequest, "Missing or invalid task_id parameter in URL")
			return
		}

		var req struct {
			Codes []string `json:"codes"`
		}

		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			slog.Warn("Append: ошибка JSON", "err", err)
			sendJSONError(w, http.StatusBadRequest, "Ошибка в теле JSON")
			return
		}

		// Валидация: если 1С прислала пустой массив кодов
		if len(req.Codes) == 0 {
			sendJSONError(w, http.StatusBadRequest, "Пришел пустой массив кодов")
			return
		}

		// Нормализуем коды перед записью в БД:
		for i, code := range req.Codes {
			req.Codes[i] = strings.ReplaceAll(code, "\x1d", "<GS>")
		}

		slog.Info("[APPEND-DIAG] Коды подготовлены к записи в БД", "task_id", taskID, "count", len(req.Codes))

		// 3. Складываем коды в базу. Используем проверенную локальную переменную taskID
		err = store.AppendTaskCodes(taskID, req.Codes)
		if err != nil {
			slog.Error("Append: Ошибка записи в БД", "task_id", taskID, "err", err)
			sendJSONError(w, http.StatusInternalServerError, "Ошибка БД при сохранении кодов")
			return
		}

		// 4. ПЫТАЕМСЯ АКТИВИРОВАТЬ ЗАДАЧУ И ЗАПУСТИТЬ НАСОС
		// TryActivateTask переведет задачу из 'ready' в 'active' при первой пачке
		activated, _ := store.TryActivateTask(taskID)
		if activated {
			// Нам нужен ID линии, чтобы передать его горутине-насосу
			lineID, err := store.GetLineIDByTask(taskID)
			if err == nil {
				taskProcessor.StartPumping(lineID, taskID)
				slog.Info("ПЕРВАЯ ПАЧКА КОДОВ ПОЛУЧЕНА: Насос (Pumper) успешно запущен", "task_id", taskID, "line_id", lineID)
			} else {
				slog.Error("Append: Задача активирована, но line_id не найден в БД", "task_id", taskID, "err", err)
			}
		}

		slog.Debug("Коды успешно добавлены в задачу", "task_id", taskID, "count", len(req.Codes))

		rndText, _ := store.GetRndTextByTask(taskID)

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":         "received",
			"count":          len(req.Codes),
			"pumper_started": activated,
			"rnd_text":       rndText,
		})
	})

	// Метод Graceful Stop: корректное завершение печати и сверка остатков
	http.HandleFunc("/api/task/stop", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost && r.Method != http.MethodGet { // Рекомендуется POST, но оставляем поддержку GET
			sendJSONError(w, http.StatusMethodNotAllowed, "Метод не поддерживается")
			return
		}

		// Получаем и проверяем ID задачи из запроса
		taskIDStr := r.URL.Query().Get("task_id")
		taskID, err := strconv.Atoi(taskIDStr)
		if err != nil || taskID <= 0 {
			sendJSONError(w, http.StatusBadRequest, "Неверный или отсутствующий ID задачи (параметр 'line_id')")
			return
		}

		// Автоматически вытягиваем ID линии из БД по ID задачи, чтобы избежать ошибок ручного ввода
		lineID, err := store.GetLineIDByTask(taskID)
		if err != nil {
			slog.Error("[STOP] Задача не найдена в БД", "task_id", taskID, "err", err)
			sendJSONError(w, http.StatusNotFound, fmt.Sprintf("Задача с ID %d не найдена", taskID))
			return
		}

		//  Узнаем текущий статус задачи из БД
		currentStatus, err := store.GetTaskStatus(taskID)
		if err != nil {
			slog.Error("[STOP] Не удалось получить статус задачи", "task_id", taskID, "err", err)
			sendJSONError(w, http.StatusNotFound, fmt.Sprintf("Задача с ID %d не найдена", taskID))
			return
		}

		// Сверяем полученный статус. Если она уже остановлена — прерываем выполнение
		if currentStatus == "stopped" {
			slog.Warn("[STOP] Попытка остановить уже остановленную задачу", "task_id", taskID)
			sendJSONError(w, http.StatusBadRequest, "Задача уже остановлена")
			return
		}

		// Если задача, например, уже "completed" (завершена успешно), её тоже нет смысла останавливать:
		if currentStatus == "completed" {
			sendJSONError(w, http.StatusBadRequest, "Задача уже успешно завершена, остановка не требуется")
			return
		}

		slog.Info("[STOP] Инициирована остановка задачи", "task_id", taskID, "line_id", lineID)

		// Опрашиваем принтеры на линии для финальной сверки
		printers, err := store.GetPrintersByLine(lineID)
		if err != nil {
			slog.Error("[STOP] Ошибка получения принтеров для линии", "line_id", lineID, "err", err)
			sendJSONError(w, http.StatusInternalServerError, "Ошибка БД при поиске принтеров на линии")
			return
		}

		if len(printers) == 0 {
			slog.Warn("[STOP] На линии нет привязанных принтеров", "line_id", lineID)
		}

		report := make(map[string]interface{})
		totalConfirmed := 0
		printerErrors := make([]string, 0)

		for _, pCfg := range printers {
			p := manager.GetPrinter(pCfg.ID)
			if p == nil {
				slog.Warn("[STOP] Принтер отсутствует в менеджере (оффлайн)", "printer_id", pCfg.ID, "name", pCfg.Name)
				report[pCfg.Name] = map[string]interface{}{
					"status": "offline_skipped",
					"error":  "Принтер не подключен к менеджеру",
				}
				printerErrors = append(printerErrors, fmt.Sprintf("принтер %s оффлайн", pCfg.Name))
				continue
			}

			// А) Получаем последний реально напечатанный индекс
			lastIdx, err := p.GetLastPrintedIndex()
			if err != nil {
				slog.Error("[STOP] Ошибка получения индекса с устройства", "printer_id", pCfg.ID, "err", err)
				report[pCfg.Name] = map[string]interface{}{
					"status": "error",
					"error":  err.Error(),
				}
				printerErrors = append(printerErrors, fmt.Sprintf("ошибка опроса %s: %v", pCfg.Name, err))
				continue
			}

			// Б) Синхронизируем БД
			affected, err := store.MarkAsPrinted(taskID, lastIdx)
			if err != nil {
				slog.Error("[STOP] Ошибка обновления статусов кодов в БД", "task_id", taskID, "printer_id", pCfg.ID, "err", err)
			}
			totalConfirmed += int(affected)

			// В) Очищаем физическую очередь принтера
			p.ClearQueue()

			// TODO передать rnd_text

			report[pCfg.Name] = map[string]interface{}{
				"status":             "cleared",
				"last_printed_index": lastIdx,
				"confirmed_codes":    affected,
			}
		}

		// 4. Меняем статус задачи в БД на завершенную
		err = store.SetTaskStatus(taskID, "stopped")
		if err != nil {
			slog.Error("[STOP] Не удалось обновить статус задачи в БД", "task_id", taskID, "err", err)
			sendJSONError(w, http.StatusInternalServerError, "Ошибка при сохранении финального статуса задачи")
			return
		}

		// 5. Формируем красивый и информативный ответ для 1С/MES

		rndText, _ := store.GetRndTextByTask(taskID)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		response := map[string]interface{}{
			"task_id":         taskID,
			"line_id":         lineID,
			"status":          "stopped",
			"timestamp":       time.Now().Format(time.RFC3339),
			"total_confirmed": totalConfirmed,
			"printers_report": report,
			"rnd_text":        rndText,
		}

		if len(printerErrors) > 0 {
			response["warnings"] = printerErrors
		}

		json.NewEncoder(w).Encode(response)
	})

	// Получение списка активных задач
	http.HandleFunc("/api/task/active", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			sendJSONError(w, http.StatusMethodNotAllowed, "Разрешен только метод GET")
			return
		}

		// Читаем параметры из запроса
		query := r.URL.Query()
		lineIDStr := query.Get("line_id")
		printerIDStr := query.Get("printer_id")

		// 1. ПРОВЕРКА НА ЧУШЬ (ВАЛИДАЦИЯ ПАРАМЕТРОВ)
		// Если параметр передан, но это не число (например, line_id=abc) — возвращаем 400 Bad Request
		var lineID, printerID int
		var err error

		if lineIDStr != "" {
			lineID, err = strconv.Atoi(lineIDStr)
			if err != nil || lineID < 0 {
				sendJSONError(w, http.StatusBadRequest, "Неверный формат параметра line_id. Ожидается целое положительное число.")
				return
			}
		}

		if printerIDStr != "" {
			printerID, err = strconv.Atoi(printerIDStr)
			if err != nil || printerID < 0 {
				sendJSONError(w, http.StatusBadRequest, "Неверный формат параметра printer_id. Ожидается целое положительное число.")
				return
			}
		}

		// Передаем проверенные фильтры в метод БД
		tasks, err := store.GetActiveTasks(lineID, printerID)
		if err != nil {
			sendJSONError(w, http.StatusInternalServerError, "Ошибка при обращении к БД: "+err.Error())
			return
		}

		// 2. ПРОВЕРКА НА ОТСУТСТВИЕ АКТИВНЫХ ЗАДАЧ
		// Если задач нет, возвращаем статус 200 OK (так как сам запрос корректный),
		// но добавляем в JSON понятный статус и пустой массив, чтобы 1С не падала при парсинге.
		if len(tasks) == 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"status":  "no_active_tasks",
				"message": "Активные или готовые к запуску задачи не найдены",
				"tasks":   []interface{}{}, // Возвращаем пустой массив
			})
			return
		}

		// Если задачи найдены — отдаем их как обычно
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(tasks)
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

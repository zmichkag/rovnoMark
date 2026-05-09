package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
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

func main() {
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
		json.NewEncoder(w).Encode(map[string]interface{}{"id": newID})
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
			http.Error(w, "JSON Error", http.StatusBadRequest)
			return
		}

		// 1. Получаем список принтеров, привязанных к линии
		printersInLine, err := store.GetPrintersByLine(req.LineID)
		if err != nil || len(printersInLine) == 0 {
			http.Error(w, "No printers on this line", http.StatusNotFound)
			return
		}

		for _, pCfg := range printersInLine {
			p := manager.GetPrinter(pCfg.ID)
			if p == nil {
				continue // На случай, если принтер удален из памяти
			}

			state := manager.GetPrinterState(pCfg.ID)

			// --- ЛОГИКА DELTA ---

			// А) Проверка шаблона
			if state.LastTemplate != req.Template {
				log.Printf("[LINE %d] Смена шаблона на %s для принтера %d", req.LineID, req.Template, pCfg.ID)
				// TODO: p.SelectTemplate(req.Template)
				state.LastTemplate = req.Template
			}

			// Б) Проверка статики (Используем команду SCF)
			currentHash := fmt.Sprintf("%v", req.StaticFields)
			if state.LastStaticHash != currentHash && len(req.StaticFields) > 0 {
				log.Printf("[LINE %d] Обновление статических полей для принтера %d", req.LineID, pCfg.ID)

				// Вызываем наш новый метод обновления "на лету"
				err := p.UpdateStaticFields(req.StaticFields)
				if err != nil {
					log.Printf("[LINE %d] Ошибка обновления статики на принтере %d: %v", req.LineID, pCfg.ID, err)
					// Пропускаем печать кодов для этого принтера, чтобы не напечатать брак со старой датой!
					continue
				}

				// Фиксируем успешное обновление в менеджере
				manager.UpdatePrinterDeltaState(pCfg.ID, state.LastTemplate, currentHash)
				state.LastStaticHash = currentHash
			}

			// --- ПЕЧАТЬ SID ---
			// В качестве индекса пока используем Timestamp
			startIndex := int(time.Now().Unix())

			// Вызываем метод с поддержкой SID и SLR
			loaded, err := p.PrintBatchIndexed(req.DynamicField, startIndex, req.Codes)
			if err != nil {
				log.Printf("Ошибка печати на принтере %d: %v", pCfg.ID, err)
				continue
			}

			log.Printf("[LINE %d] Загружено %d кодов на принтер %d (Start Index: %d)", req.LineID, loaded, pCfg.ID, startIndex)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "accepted"})
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

		// 4. ЗАПУСКАЕМ НАКАЧКУ (Task Pumping)
		// Эта функция запускает бесконечный цикл в горутине, который будет
		// следить за буфером принтеров на линии и подливать туда коды
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

	fmt.Println("=== РОВНО: Стендалон запущен (http://localhost:8080) ===")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"strconv"
	"time"

	"rovnoMark/internal/core"
	"rovnoMark/internal/drivers/savema"
	"rovnoMark/internal/drivers/videojet"
	"rovnoMark/internal/storage"
)

//go:embed ui/*
var uiFS embed.FS

func main() {
	store := storage.New("rovnoMark.db")
	manager := core.NewPrinterManager()

	// 1. Загружаем все принтеры из базы в работу
	savedPrinters, _ := store.GetAllPrinters()
	for _, cfg := range savedPrinters {
		if cfg.DriverType == "savema" {
			manager.AddPrinter(cfg, savema.New(cfg.IP, cfg.Port))
		} else if cfg.DriverType == "videojet" {
			manager.AddPrinter(cfg, videojet.New(cfg.IP, cfg.Port))
		}
	}

	// 2. API для добавления нового принтера
	http.HandleFunc("/api/printers/add", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Only POST", http.StatusMethodNotAllowed)
			return
		}
		var cfg core.PrinterConfig
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
			core.PrinterConfig
			core.PrinterState
		}
		type LineGroup struct {
			core.LineConfig
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

		// 1. Получаем список принтеров, привязанных к линии из storage.go
		// Нам нужно будет добавить метод GetPrintersByLine в storage.go
		printersInLine, err := store.GetPrintersByLine(req.LineID)
		if err != nil || len(printersInLine) == 0 {
			http.Error(w, "No printers on this line", http.StatusNotFound)
			return
		}

		for _, pCfg := range printersInLine {
			p := manager.GetPrinter(pCfg.ID)
			state := manager.GetPrinterState(pCfg.ID) // Получаем текущее состояние из памяти

			// --- ЛОГИКА DELTA ---

			// А) Проверка шаблона (JDA)
			if state.LastTemplate != req.Template {
				log.Printf("[LINE %d] Смена шаблона на %s для принтера %d", req.LineID, req.Template, pCfg.ID)
				// Здесь вызываем p.SelectTemplate(req.Template)
				state.LastTemplate = req.Template
			}

			// Б) Проверка статики (JDU)
			currentHash := fmt.Sprintf("%v", req.StaticFields) // Упрощенный хеш
			if state.LastStaticHash != currentHash {
				log.Printf("[LINE %d] Обновление статических полей для принтера %d", req.LineID, pCfg.ID)
				// Здесь вызываем p.UpdateStaticFields(req.StaticFields)
				state.LastStaticHash = currentHash
			}

			// --- ПЕЧАТЬ SID ---
			// В качестве индекса пока используем Timestamp или ID из БД
			startIndex := int(time.Now().Unix())

			// Вызываем новый метод с поддержкой SID и SLR
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

	// Получение всех линий для выпадающих списков
	http.HandleFunc("/api/lines", func(w http.ResponseWriter, r *http.Request) {
		lines, _ := store.GetAllLines()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(lines)
	})

	// Получение таблицы связок (Линия - Принтер - Роль)
	http.HandleFunc("/api/assignments", func(w http.ResponseWriter, r *http.Request) {
		data, _ := store.GetAssignments()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(data)
	})

	// 5. Раздача UI (Frontend)
	content, _ := fs.Sub(uiFS, "ui")
	http.Handle("/", http.FileServer(http.FS(content)))

	fmt.Println("=== РОВНО: Стендалон запущен (http://localhost:8080) ===")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

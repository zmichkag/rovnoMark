package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"rovnoMark/internal/drivers/videojet"
	"rovnoMark/internal/storage"
	"strconv"

	"rovnoMark/internal/core"
	"rovnoMark/internal/drivers/savema"
)

//go:embed ui/*
var uiFS embed.FS

func main() {
	store := storage.New("rovnoMark.db")
	manager := core.NewPrinterManager()

	// Загружаем все принтеры из базы в работу
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

		// Сохраняем в базу
		newID, err := store.SavePrinter(cfg)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		cfg.ID = int(newID)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"id": newID})

		// Сразу добавляем в активный пул (чтобы не перезапускать .exe)
		switch cfg.DriverType {
		case "savema":
			manager.AddPrinter(cfg, savema.New(cfg.IP, cfg.Port))
		case "videojet":
			manager.AddPrinter(cfg, videojet.New(cfg.IP, cfg.Port))
		default:
			http.Error(w, "Неизвестный тип драйвера", http.StatusBadRequest)
			return
		}

		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, "Принтер добавлен в систему")
	})

	// 2. Настраиваем API Маршруты

	// API для дашборда (Мониторинг)
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

		// Группируем принтеры
		grouped := make(map[int][]PrinterInfo)
		var unassigned []PrinterInfo

		for _, cfg := range configs {
			info := PrinterInfo{PrinterConfig: cfg, PrinterState: states[cfg.ID]}
			if lineID, ok := lineMap[cfg.ID]; ok {
				grouped[lineID] = append(grouped[lineID], info)
			} else {
				unassigned = append(unassigned, info)
			}
		}

		// Формируем финальный ответ
		var responseLines []LineGroup
		for _, l := range lines {
			responseLines = append(responseLines, LineGroup{
				LineConfig: l,
				Printers:   grouped[l.ID],
			})
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"lines":      responseLines,
			"unassigned": unassigned,
			"logs":       logs,
		})
	})

	// 3. API для отправки задания на печать
	http.HandleFunc("/api/print", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Метод не поддерживается", http.StatusMethodNotAllowed)
			return
		}

		var job struct {
			PrinterID string            `json:"printer_id"`
			Template  string            `json:"template"`
			Fields    map[string]string `json:"fields"`
		}

		if err := json.NewDecoder(r.Body).Decode(&job); err != nil {
			http.Error(w, "Ошибка формата JSON", http.StatusBadRequest)
			return
		}

		// КОНВЕРТАЦИЯ: превращаем строку "1" в число 1
		idInt, err := strconv.Atoi(job.PrinterID)
		if err != nil {
			http.Error(w, "Некорректный ID принтера (должен быть числом)", http.StatusBadRequest)
			return
		}

		// Находим нужный принтер в диспетчере (теперь по int)
		p := manager.GetPrinter(idInt)
		if p == nil {
			http.Error(w, "Принтер не найден", http.StatusNotFound)
			return
		}

		// Отправляем задание через драйвер
		if err := p.PrintTemplate(job.Template, job.Fields); err != nil {
			http.Error(w, fmt.Sprintf("Ошибка печати: %v", err), http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "Задание успешно загружено в принтер")
	})

	// API для Линий
	http.HandleFunc("/api/lines", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			lines, _ := store.GetAllLines()
			json.NewEncoder(w).Encode(lines)
		} else if r.Method == http.MethodPost {
			var l core.LineConfig
			json.NewDecoder(r.Body).Decode(&l)
			store.SaveLine(l)
			w.WriteHeader(http.StatusCreated)
		}
	})

	// API для привязки принтера к линии
	http.HandleFunc("/api/lines/assign", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			return
		}
		var req struct {
			LineID    int    `json:"line_id"`
			PrinterID int    `json:"printer_id"`
			Role      string `json:"role"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		store.AssignPrinterToLine(req.LineID, req.PrinterID, req.Role)
		w.WriteHeader(http.StatusOK)
	})

	// API для получения списка привязок
	http.HandleFunc("/api/assignments", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			return
		}
		data, err := store.GetAssignments()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(data)
	})

	// 3. Раздача UI (Frontend)
	content, _ := fs.Sub(uiFS, "ui")
	http.Handle("/", http.FileServer(http.FS(content)))

	fmt.Println("=== РОВНО: Стендалон запущен (http://localhost:8080) ===")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

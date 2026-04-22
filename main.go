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
		store.SavePrinter(cfg)

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
		if r.Method != http.MethodGet {
			return
		}

		states, logs := manager.GetDashboardData()
		configs, _ := store.GetAllPrinters()

		type PrinterInfo struct {
			core.PrinterConfig
			core.PrinterState // Встраиваем телеметрию (Status, Ribbon, Queue)
		}

		response := struct {
			Printers []PrinterInfo   `json:"printers"`
			Logs     []core.LogEntry `json:"logs"`
		}{}

		for _, cfg := range configs {
			state := states[cfg.ID]
			response.Printers = append(response.Printers, PrinterInfo{
				PrinterConfig: cfg,
				PrinterState:  state,
			})
		}
		response.Logs = logs

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	})

	// 3. API для отправки задания на печать
	http.HandleFunc("/api/print", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Метод не поддерживается", http.StatusMethodNotAllowed)
			return
		}

		// Структура того, что пришлет нам браузер (или 1С)
		var job struct {
			PrinterID string            `json:"printer_id"`
			Template  string            `json:"template"`
			Fields    map[string]string `json:"fields"`
		}

		if err := json.NewDecoder(r.Body).Decode(&job); err != nil {
			http.Error(w, "Ошибка формата JSON", http.StatusBadRequest)
			return
		}

		// Находим нужный принтер в диспетчере
		p := manager.GetPrinter(job.PrinterID)
		if p == nil {
			http.Error(w, "Принтер с таким ID не найден (оффлайн или не добавлен)", http.StatusNotFound)
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

	// 3. Раздача UI (Frontend)
	content, _ := fs.Sub(uiFS, "ui")
	http.Handle("/", http.FileServer(http.FS(content)))

	fmt.Println("=== РОВНО: Стендалон запущен (http://localhost:8080) ===")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

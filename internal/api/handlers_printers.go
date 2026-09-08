package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"rovnoMark/internal/drivers/extserver"
	"rovnoMark/internal/drivers/markem"
	"rovnoMark/internal/drivers/savema"
	"rovnoMark/internal/drivers/valentine"
	"rovnoMark/internal/drivers/videojet"
	"rovnoMark/internal/models"
	"strconv"
	"strings"
	"time"
)

func (s *Server) handlePrinters(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		sendJSONError(w, http.StatusMethodNotAllowed, "Разрешен только GET")
		return
	}

	states, logs := s.manager.GetDashboardData()
	configs, _ := s.store.GetAllPrinters()
	lines, _ := s.store.GetAllLines()
	lineMap, _ := s.store.GetPrinterLineMap()

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

	sendJSON(w, http.StatusOK, map[string]interface{}{
		"lines":        responseLines,
		"all_printers": allForUI,
		"logs":         logs,
	})
}

func (s *Server) handlePrintersAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		sendJSONError(w, http.StatusMethodNotAllowed, "Only POST allowed")
		return
	}

	var cfg models.PrinterConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		sendJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	newID, err := s.store.SavePrinter(cfg)
	if err != nil {
		sendJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	cfg.ID = int(newID)

	switch cfg.DriverType {
	case "savema":
		s.manager.AddPrinter(cfg, savema.New(cfg.IP, cfg.Port))
	case "videojet":
		s.manager.AddPrinter(cfg, videojet.New(cfg.IP, cfg.Port))
	case "valentine_nice":
		s.manager.AddPrinter(cfg, valentine.NewNiceLabelDriver(cfg.ID, cfg.IP, cfg.Port))
	case "markem":
		s.manager.AddPrinter(cfg, markem.New(cfg.IP, cfg.Port, "Actor1"))
	case "ext_server", "nicelabel_http":
		s.manager.AddPrinter(cfg, extserver.New(cfg.IP, cfg.Port))
	default:
		sendJSONError(w, http.StatusBadRequest, "Неизвестный тип драйвера: "+cfg.DriverType)
		return
	}

	sendJSON(w, http.StatusOK, map[string]interface{}{"printer_id": newID})
}

func (s *Server) handleTemplates(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		sendJSONError(w, http.StatusMethodNotAllowed, "Только GET")
		return
	}

	idInt, err := strconv.Atoi(r.URL.Query().Get("printer_id"))
	if err != nil {
		sendJSONError(w, http.StatusBadRequest, "Неверный ID принтера")
		return
	}

	p := s.manager.GetPrinter(idInt)
	if p == nil {
		sendJSONError(w, http.StatusNotFound, "Принтер не найден в пуле активных устройств")
		return
	}

	templates, err := p.GetTemplates()
	if err != nil {
		sendJSONError(w, http.StatusInternalServerError, "Ошибка чтения шаблонов: "+err.Error())
		return
	}

	sendJSON(w, http.StatusOK, templates)
}

func (s *Server) handleTemplateFields(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		sendJSONError(w, http.StatusMethodNotAllowed, "Только GET")
		return
	}

	printerIDStr := r.URL.Query().Get("printer_id")
	templateName := r.URL.Query().Get("template")

	idInt, err := strconv.Atoi(printerIDStr)
	if err != nil || templateName == "" {
		sendJSONError(w, http.StatusBadRequest, "Неверный ID принтера или пустое имя шаблона")
		return
	}

	p := s.manager.GetPrinter(idInt)
	if p == nil {
		sendJSONError(w, http.StatusNotFound, "Принтер не найден")
		return
	}

	fields, err := p.GetTemplateFields(templateName)
	if err != nil {
		sendJSONError(w, http.StatusInternalServerError, "Ошибка чтения полей: "+err.Error())
		return
	}

	sendJSON(w, http.StatusOK, fields)
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	idInt, _ := strconv.Atoi(r.URL.Query().Get("printer_id"))
	limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || limit <= 0 {
		limit = 100
	}

	data, err := s.store.GetTelemetry(idInt, limit)
	if err != nil {
		sendJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	sendJSON(w, http.StatusOK, data)
}

func (s *Server) handleLogsHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		sendJSONError(w, http.StatusMethodNotAllowed, "Разрешен только метод GET")
		return
	}

	query := r.URL.Query()
	var filter models.LogFilter

	if lineIDStr := query.Get("line_id"); lineIDStr != "" {
		if id, err := strconv.Atoi(lineIDStr); err == nil && id > 0 {
			filter.LineID = id
		}
	}
	if printerIDStr := query.Get("printer_id"); printerIDStr != "" {
		if id, err := strconv.Atoi(printerIDStr); err == nil && id > 0 {
			filter.PrinterID = id
		}
	}
	if typeStr := query.Get("type"); typeStr != "" {
		filter.EventType = strings.ToLower(typeStr)
	}
	if dateFromStr := query.Get("date_from"); dateFromStr != "" {
		if t, err := time.Parse(time.RFC3339, dateFromStr); err == nil {
			filter.DateFrom = t
		} else if t, err := time.Parse("2006-01-02 15:04:05", dateFromStr); err == nil {
			filter.DateFrom = t
		} else if t, err := time.Parse("2006-01-02", dateFromStr); err == nil {
			filter.DateFrom = t
		}
	}
	if dateToStr := query.Get("date_to"); dateToStr != "" {
		if t, err := time.Parse(time.RFC3339, dateToStr); err == nil {
			filter.DateTo = t
		} else if t, err := time.Parse("2006-01-02 15:04:05", dateToStr); err == nil {
			filter.DateTo = t
		} else if t, err := time.Parse("2006-01-02", dateToStr); err == nil {
			filter.DateTo = t.Add(23*time.Hour + 59*time.Minute + 59*time.Second)
		}
	}
	if limitStr := query.Get("limit"); limitStr != "" {
		if lim, err := strconv.Atoi(limitStr); err == nil && lim > 0 {
			filter.Limit = lim
		}
	}
	if offsetStr := query.Get("offset"); offsetStr != "" {
		if off, err := strconv.Atoi(offsetStr); err == nil && off >= 0 {
			filter.Offset = off
		}
	}

	logs, err := s.store.GetEventLogsHistory(filter)
	if err != nil {
		slog.Error("API LOGS-HISTORY Error", "err", err)
		sendJSONError(w, http.StatusInternalServerError, "Ошибка получения данных: "+err.Error())
		return
	}

	sendJSON(w, http.StatusOK, map[string]interface{}{
		"status": "ok",
		"count":  len(logs),
		"filter": filter,
		"items":  logs,
	})
}

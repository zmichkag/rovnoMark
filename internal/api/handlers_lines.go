package api

import (
	"encoding/json"
	"net/http"
	"rovnoMark/internal/models"
)

func (s *Server) handleLines(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		lines, err := s.store.GetAllLines()
		if err != nil {
			sendJSONError(w, http.StatusInternalServerError, "Ошибка получения линий: "+err.Error())
			return
		}
		sendJSON(w, http.StatusOK, lines)

	case http.MethodPost:
		var l models.LineConfig
		if err := json.NewDecoder(r.Body).Decode(&l); err != nil {
			sendJSONError(w, http.StatusBadRequest, "Ошибка парсинга JSON: "+err.Error())
			return
		}

		if err := s.store.SaveLine(l); err != nil {
			sendJSONError(w, http.StatusInternalServerError, "Ошибка сохранения линии: "+err.Error())
			return
		}

		sendJSON(w, http.StatusOK, map[string]string{"status": "ok"})

	default:
		sendJSONError(w, http.StatusMethodNotAllowed, "Метод не поддерживается")
	}
}

func (s *Server) handleAssignments(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		data, err := s.store.GetAssignments()
		if err != nil {
			sendJSONError(w, http.StatusInternalServerError, "Ошибка получения данных: "+err.Error())
			return
		}
		sendJSON(w, http.StatusOK, data)

	case http.MethodPost:
		var req struct {
			LineID    int    `json:"line_id"`
			PrinterID int    `json:"printer_id"`
			Role      string `json:"role"`
		}

		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			sendJSONError(w, http.StatusBadRequest, "Ошибка парсинга JSON: "+err.Error())
			return
		}

		if err := s.store.AssignPrinterToLine(req.LineID, req.PrinterID, req.Role); err != nil {
			sendJSONError(w, http.StatusInternalServerError, "Ошибка БД: "+err.Error())
			return
		}

		sendJSON(w, http.StatusOK, map[string]interface{}{
			"status":     "assigned",
			"line_id":    req.LineID,
			"printer_id": req.PrinterID,
		})

	default:
		sendJSONError(w, http.StatusMethodNotAllowed, "Метод не поддерживается")
	}
}

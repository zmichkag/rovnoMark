package api

import (
	"encoding/json"
	"net/http"
	"rovnoMark/internal/models"
	"strconv"
)

func (s *Server) handleScanners(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		scanners, err := s.store.GetAllScanners()
		if err != nil {
			sendJSONError(w, http.StatusInternalServerError, "Ошибка получения сканеров: "+err.Error())
			return
		}
		if scanners == nil {
			scanners = []models.ScannerConfig{}
		}
		sendJSON(w, http.StatusOK, scanners)

	case http.MethodPost:
		var scanner models.ScannerConfig
		if err := json.NewDecoder(r.Body).Decode(&scanner); err != nil {
			sendJSONError(w, http.StatusBadRequest, "Ошибка разбора JSON: "+err.Error())
			return
		}

		id, err := s.store.SaveLineScanner(scanner)
		if err != nil {
			sendJSONError(w, http.StatusBadRequest, "Ошибка сохранения сканера: "+err.Error())
			return
		}
		sendJSON(w, http.StatusOK, map[string]interface{}{
			"status":     "saved",
			"scanner_id": id,
		})

	default:
		sendJSONError(w, http.StatusMethodNotAllowed, "Метод не поддерживается")
	}
}

func (s *Server) handleScannerReads(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		sendJSONError(w, http.StatusMethodNotAllowed, "Метод не поддерживается")
		return
	}
	scannerID, err := strconv.Atoi(r.URL.Query().Get("scanner_id"))
	if err != nil || scannerID <= 0 {
		sendJSONError(w, http.StatusBadRequest, "Некорректный scanner_id")
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	reads, err := s.store.GetRecentScannerReads(scannerID, limit)
	if err != nil {
		sendJSONError(w, http.StatusInternalServerError, "Ошибка получения чтений сканера: "+err.Error())
		return
	}
	sendJSON(w, http.StatusOK, reads)
}

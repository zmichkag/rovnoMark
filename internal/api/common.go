package api

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"rovnoMark/internal/core"
	"rovnoMark/internal/storage"
)

type Server struct {
	store         *storage.Store
	manager       *core.PrinterManager
	taskProcessor *core.TaskProcessor
	validateGS1   bool

	uiFS    fs.FS
	ui2FS   fs.FS
	uiOkkFS fs.FS
}

func NewServer(
	store *storage.Store,
	manager *core.PrinterManager,
	taskProcessor *core.TaskProcessor,
	validateGS1 bool,
	uiFS, ui2FS, uiOkkFS fs.FS,
) *Server {
	return &Server{
		store:         store,
		manager:       manager,
		taskProcessor: taskProcessor,
		validateGS1:   validateGS1,
		uiFS:          uiFS,
		ui2FS:         ui2FS,
		uiOkkFS:       uiOkkFS,
	}
}

func sendJSON(w http.ResponseWriter, code int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(data)
}

func sendJSONError(w http.ResponseWriter, code int, msg string) {
	sendJSON(w, code, map[string]string{"error": msg})
}

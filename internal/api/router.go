package api

import (
	"log/slog"
	"net/http"
)

func (s *Server) InitRoutes() http.Handler {
	mux := http.NewServeMux()

	// 1. Системные и диагностические роуты
	mux.HandleFunc("/api/system/info", s.handleSystemInfo)

	// 2. Линии и привязки оборудования
	mux.HandleFunc("/api/lines", s.handleLines)
	mux.HandleFunc("/api/assignments", s.handleAssignments)

	// 3. Управление парком принтеров
	mux.HandleFunc("/api/printers", s.handlePrinters)
	mux.HandleFunc("/api/printers/add", s.handlePrintersAdd)
	mux.HandleFunc("/api/templates", s.handleTemplates)
	mux.HandleFunc("/api/template/fields", s.handleTemplateFields)
	mux.HandleFunc("/api/stats", s.handleStats)
	mux.HandleFunc("/api/logs/history", s.handleLogsHistory)

	// 4. Очередь заданий и маркировка
	mux.HandleFunc("/api/task/create", s.handleTaskCreate)
	mux.HandleFunc("/api/task/append", s.handleTaskAppend)
	mux.HandleFunc("/api/task/stop", s.handleTaskStop)
	mux.HandleFunc("/api/task/info", s.handleTaskInfo)
	mux.HandleFunc("/api/task/active", s.handleTaskActive)
	mux.HandleFunc("/api/code/info", s.handleCodeInfo)
	mux.HandleFunc("/api/dashboard/live", s.handleDashboardLive)

	// 5. Встраиваемый Frontend
	if s.uiFS != nil {
		mux.Handle("/", http.FileServer(http.FS(s.uiFS)))
	}
	if s.ui2FS != nil {
		mux.Handle("/frontend2/", http.StripPrefix("/frontend2/", http.FileServer(http.FS(s.ui2FS))))
		slog.Info("Развернут Dev Frontend v1.5", "url", "/frontend2/")
	}
	if s.uiOkkFS != nil {
		mux.Handle("/okk/", http.StripPrefix("/okk/", http.FileServer(http.FS(s.uiOkkFS))))
		slog.Info("Развернут Терминал ОКК v1.0", "url", "/okk/")
	}

	return mux
}

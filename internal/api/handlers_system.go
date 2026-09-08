package api

import (
	"net/http"
	"rovnoMark/internal/brand"
	"rovnoMark/internal/storage"
	"rovnoMark/internal/version"
	"runtime"
)

func (s *Server) handleSystemInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		sendJSONError(w, http.StatusMethodNotAllowed, "Разрешен только метод GET")
		return
	}

	sendJSON(w, http.StatusOK, map[string]interface{}{
		"brand": map[string]string{
			"name":        brand.GetName(),
			"vendor":      brand.Vendor,
			"support_url": brand.SupportURL,
		},
		"build": version.Info{
			Version:   version.Version,
			GitCommit: version.GitCommit,
			BuildDate: version.BuildDate,
			GoVersion: runtime.Version(),
		},
		"schema_version": storage.TargetMasterSchemaVersion,
	})
}

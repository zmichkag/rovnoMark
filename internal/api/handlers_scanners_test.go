package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"rovnoMark/internal/core"
	"rovnoMark/internal/models"
	"rovnoMark/internal/storage"
	"testing"
)

func TestScannersAPIStoresAndListsConfiguration(t *testing.T) {
	store := storage.New(filepath.Join(t.TempDir(), "scanner-api"))
	defer store.Close()

	if err := store.SaveLine(models.LineConfig{Name: "Line 1", IsActive: true}); err != nil {
		t.Fatal(err)
	}
	lines, err := store.GetAllLines()
	if err != nil || len(lines) != 1 {
		t.Fatalf("GetAllLines() = %#v, %v", lines, err)
	}
	printerID, err := store.SavePrinter(models.PrinterConfig{
		Name: "Printer 1", IP: "10.0.0.10", Port: 9100, DriverType: "videojet", IsActive: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AssignPrinterToLine(lines[0].ID, int(printerID), "PRIMARY"); err != nil {
		t.Fatal(err)
	}
	targetID := int(printerID)
	payload, err := json.Marshal(models.ScannerConfig{
		LineID:         lines[0].ID,
		Name:           "Camera 1",
		DriverType:     "tcp_camera",
		Address:        "10.0.0.20",
		Port:           23,
		Role:           models.ScannerRoleInlineVerifier,
		TargetDeviceID: &targetID,
		SettingsJSON:   `{}`,
		IsActive:       true,
	})
	if err != nil {
		t.Fatal(err)
	}

	server := NewServer(store, core.NewPrinterManager(), core.NewScannerManager(store), nil, false, nil, nil, nil)
	router := server.InitRoutes()
	post := httptest.NewRequest(http.MethodPost, "/api/scanners", bytes.NewReader(payload))
	post.Header.Set("Content-Type", "application/json")
	postResponse := httptest.NewRecorder()
	router.ServeHTTP(postResponse, post)
	if postResponse.Code != http.StatusOK {
		t.Fatalf("POST /api/scanners status = %d, body = %s", postResponse.Code, postResponse.Body.String())
	}

	getResponse := httptest.NewRecorder()
	router.ServeHTTP(getResponse, httptest.NewRequest(http.MethodGet, "/api/scanners", nil))
	if getResponse.Code != http.StatusOK {
		t.Fatalf("GET /api/scanners status = %d, body = %s", getResponse.Code, getResponse.Body.String())
	}
	var scanners []models.ScannerConfig
	if err := json.NewDecoder(getResponse.Body).Decode(&scanners); err != nil {
		t.Fatal(err)
	}
	if len(scanners) != 1 || scanners[0].Name != "Camera 1" ||
		scanners[0].TargetDeviceID == nil || *scanners[0].TargetDeviceID != targetID {
		t.Fatalf("scanners = %#v", scanners)
	}

	printersResponse := httptest.NewRecorder()
	router.ServeHTTP(printersResponse, httptest.NewRequest(http.MethodGet, "/api/printers", nil))
	if printersResponse.Code != http.StatusOK {
		t.Fatalf("GET /api/printers status = %d, body = %s", printersResponse.Code, printersResponse.Body.String())
	}
	var dashboard struct {
		Lines []struct {
			Scanners []models.ScannerConfig `json:"scanners"`
		} `json:"lines"`
	}
	if err := json.NewDecoder(printersResponse.Body).Decode(&dashboard); err != nil {
		t.Fatal(err)
	}
	if len(dashboard.Lines) != 1 || len(dashboard.Lines[0].Scanners) != 1 ||
		dashboard.Lines[0].Scanners[0].Name != "Camera 1" {
		t.Fatalf("dashboard scanners = %#v", dashboard.Lines)
	}
}

package storage

import (
	"database/sql"
	"path/filepath"
	"rovnoMark/internal/models"
	"testing"
	"time"
)

func TestLineScannerMigrationAndStorage(t *testing.T) {
	store := New(filepath.Join(t.TempDir(), "scanner-config"))
	defer store.Close()

	lineResult, err := store.db.Exec(`INSERT INTO lines (name) VALUES ('Line 1')`)
	if err != nil {
		t.Fatal(err)
	}
	lineID, _ := lineResult.LastInsertId()
	printerResult, err := store.db.Exec(`
		INSERT INTO printers (name, ip, driver_type) VALUES ('Printer 1', '10.0.0.10', 'videojet')`)
	if err != nil {
		t.Fatal(err)
	}
	printerID, _ := printerResult.LastInsertId()
	targetID := int(printerID)

	scannerID, err := store.SaveLineScanner(models.ScannerConfig{
		LineID:         int(lineID),
		Name:           "Camera 1",
		DriverType:     "tcp_camera",
		Address:        "10.0.0.20",
		Port:           23,
		Role:           models.ScannerRoleInlineVerifier,
		TargetDeviceID: &targetID,
		SettingsJSON:   `{"trigger":"sensor"}`,
		IsActive:       true,
	})
	if err != nil {
		t.Fatal(err)
	}

	scanners, err := store.GetScannersByLine(int(lineID))
	if err != nil {
		t.Fatal(err)
	}
	if len(scanners) != 1 {
		t.Fatalf("GetScannersByLine() returned %d scanners, want 1", len(scanners))
	}
	got := scanners[0]
	if got.ID != int(scannerID) || got.Role != models.ScannerRoleInlineVerifier ||
		got.TargetDeviceID == nil || *got.TargetDeviceID != targetID || got.Port != 23 {
		t.Fatalf("scanner = %#v", got)
	}

	got.IsActive = false
	if _, err := store.SaveLineScanner(got); err != nil {
		t.Fatal(err)
	}
	scanners, err = store.GetScannersByLine(int(lineID))
	if err != nil {
		t.Fatal(err)
	}
	if len(scanners) != 0 {
		t.Fatalf("inactive scanner returned by GetScannersByLine(): %#v", scanners)
	}
	allScanners, err := store.GetAllScanners()
	if err != nil {
		t.Fatal(err)
	}
	if len(allScanners) != 1 || allScanners[0].IsActive {
		t.Fatalf("GetAllScanners() = %#v, want one inactive scanner", allScanners)
	}
}

func TestInlineScannerRequiresTargetDevice(t *testing.T) {
	store := New(filepath.Join(t.TempDir(), "scanner-validation"))
	defer store.Close()

	_, err := store.SaveLineScanner(models.ScannerConfig{
		LineID:     1,
		Name:       "Camera 1",
		DriverType: "tcp_camera",
		Address:    "10.0.0.20",
		Role:       models.ScannerRoleInlineVerifier,
		IsActive:   true,
	})
	if err == nil || err == sql.ErrNoRows {
		t.Fatalf("SaveLineScanner() error = %v, want target validation error", err)
	}
}

func TestMigrateMasterFromVersionOneAddsLineScanners(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "master-v1.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.Exec(`
		CREATE TABLE lines (id INTEGER PRIMARY KEY);
		CREATE TABLE printers (id INTEGER PRIMARY KEY);
		PRAGMA user_version = 1;
	`); err != nil {
		t.Fatal(err)
	}
	if err := MigrateMaster(db); err != nil {
		t.Fatal(err)
	}

	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != TargetMasterSchemaVersion {
		t.Fatalf("master schema version = %d, want %d", version, TargetMasterSchemaVersion)
	}

	var tableName string
	if err := db.QueryRow(`
		SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'line_scanners'`).Scan(&tableName); err != nil {
		t.Fatal(err)
	}
}

func TestRecordScannerReadVerifiesCodeAndKeepsHistory(t *testing.T) {
	store := New(filepath.Join(t.TempDir(), "scanner-reads"))
	defer store.Close()

	lineResult, err := store.db.Exec(`INSERT INTO lines (name) VALUES ('Line 1')`)
	if err != nil {
		t.Fatal(err)
	}
	lineID64, _ := lineResult.LastInsertId()
	lineID := int(lineID64)
	printerResult, err := store.db.Exec(`
		INSERT INTO printers (name, ip, driver_type) VALUES ('Printer 1', '10.0.0.10', 'videojet')`)
	if err != nil {
		t.Fatal(err)
	}
	printerID64, _ := printerResult.LastInsertId()
	printerID := int(printerID64)

	scannerID64, err := store.SaveLineScanner(models.ScannerConfig{
		LineID:         lineID,
		Name:           "Camera 1",
		DriverType:     "tcp_camera",
		Address:        "10.0.0.20",
		Port:           3000,
		Role:           models.ScannerRoleInlineVerifier,
		TargetDeviceID: &printerID,
		IsActive:       true,
	})
	if err != nil {
		t.Fatal(err)
	}
	scannerID := int(scannerID64)
	scanner := models.ScannerConfig{
		ID:             scannerID,
		LineID:         lineID,
		TargetDeviceID: &printerID,
	}

	taskID64, err := store.CreateTask(lineID, "template", "code", `{}`, "")
	if err != nil {
		t.Fatal(err)
	}
	taskID := int(taskID64)
	if err := store.AppendTaskCodes(taskID, []models.InboundCodeItem{{Code: "010460123456789021ABC"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.getCodesDB().Exec(`
		UPDATE task_codes SET status = 'printed', printer_id = ? WHERE task_id = ?`, printerID, taskID); err != nil {
		t.Fatal(err)
	}

	readAt := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	read, err := store.RecordScannerRead(scanner, "010460123456789021ABC", []byte("010460123456789021ABC\r\n"), readAt)
	if err != nil {
		t.Fatal(err)
	}
	if read.MatchStatus != "verified" || read.TaskCodeID == nil {
		t.Fatalf("first read = %#v, want verified task code", read)
	}

	var status string
	var verifiedAt time.Time
	var verifiedBy int
	if err := store.getCodesDB().QueryRow(`
		SELECT status, verified_at, verified_by_scanner_id
		FROM task_codes WHERE task_id = ?`, taskID).Scan(&status, &verifiedAt, &verifiedBy); err != nil {
		t.Fatal(err)
	}
	if status != "verified" || verifiedBy != scannerID || !verifiedAt.Equal(readAt) {
		t.Fatalf("code state = (%q, %v, %d), want verified at %v by scanner %d",
			status, verifiedAt, verifiedBy, readAt, scannerID)
	}

	duplicate, err := store.RecordScannerRead(scanner, "010460123456789021ABC", nil, readAt.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if duplicate.MatchStatus != "duplicate" {
		t.Fatalf("second read status = %q, want duplicate", duplicate.MatchStatus)
	}

	reads, err := store.GetRecentScannerReads(scannerID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(reads) != 2 || reads[0].MatchStatus != "duplicate" || reads[1].MatchStatus != "verified" {
		t.Fatalf("read history = %#v", reads)
	}
}

func TestMigrateMasterRemovesRegionPrefixFromExistingReads(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "master-v3.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.Exec(`
		CREATE TABLE scanner_reads (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			code TEXT NOT NULL,
			raw_data BLOB NOT NULL
		);
		INSERT INTO scanner_reads (code, raw_data)
		VALUES ('Region12,010460123456789021ABC', CAST('Region12,010460123456789021ABC' || CHAR(13) || CHAR(10) AS BLOB));
		PRAGMA user_version = 3;
	`); err != nil {
		t.Fatal(err)
	}
	if err := MigrateMaster(db); err != nil {
		t.Fatal(err)
	}

	var code string
	var rawData []byte
	if err := db.QueryRow(`SELECT code, raw_data FROM scanner_reads`).Scan(&code, &rawData); err != nil {
		t.Fatal(err)
	}
	if code != "010460123456789021ABC" || string(rawData) != code {
		t.Fatalf("migrated read = (%q, %q), want region-free payload", code, rawData)
	}
}

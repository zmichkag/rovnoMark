package storage

import (
	"path/filepath"
	"rovnoMark/internal/models"
	"testing"
	"time"
)

func TestRecordScannerNoRead(t *testing.T) {
	store := New(filepath.Join(t.TempDir(), "scanner-no-reads"))
	defer store.Close()

	if err := store.SaveLine(models.LineConfig{Name: "Line 1", IsActive: true}); err != nil {
		t.Fatal(err)
	}
	lines, err := store.GetAllLines()
	if err != nil || len(lines) != 1 {
		t.Fatalf("GetAllLines() = %#v, %v", lines, err)
	}
	scannerID, err := store.SaveLineScanner(models.ScannerConfig{
		LineID:     lines[0].ID,
		Name:       "Camera 1",
		DriverType: "tcp_camera",
		Address:    "127.0.0.1",
		Port:       3000,
		Role:       models.ScannerRoleAuditCheck,
		IsActive:   true,
	})
	if err != nil {
		t.Fatal(err)
	}

	readAt := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	read, err := store.RecordScannerNoRead(models.ScannerConfig{
		ID: int(scannerID), LineID: lines[0].ID,
	}, "Noread", []byte("Noread"), readAt)
	if err != nil {
		t.Fatal(err)
	}
	if read.Code != "Noread" || read.MatchStatus != "no_read" {
		t.Fatalf("RecordScannerNoRead() = %#v", read)
	}
	readAt2 := readAt.Add(time.Second)
	read, err = store.RecordScannerNoRead(models.ScannerConfig{
		ID: int(scannerID), LineID: lines[0].ID,
	}, "NoRead", []byte("NoRead"), readAt2)
	if err != nil {
		t.Fatal(err)
	}
	if read.Code != "NoRead" || read.MatchStatus != "no_read" {
		t.Fatalf("RecordScannerNoRead() = %#v", read)
	}
	if _, err := store.RecordScannerNoRead(models.ScannerConfig{
		ID: int(scannerID), LineID: lines[0].ID,
	}, "NoReads", []byte("NoReads"), readAt2); err == nil {
		t.Fatal("RecordScannerNoRead() accepted unsupported NoReads marker")
	}

	reads, err := store.GetRecentScannerReads(int(scannerID), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(reads) != 2 || reads[0].Code != "NoRead" || reads[0].MatchStatus != "no_read" || !reads[0].ReadAt.Equal(readAt2) || reads[1].Code != "Noread" || reads[1].MatchStatus != "no_read" || !reads[1].ReadAt.Equal(readAt) {
		t.Fatalf("GetRecentScannerReads() = %#v", reads)
	}
}

func TestRecordScannerReadDoesNotUpdateTaskCode(t *testing.T) {
	store := New(filepath.Join(t.TempDir(), "scanner-read-only-log"))
	defer store.Close()

	if err := store.SaveLine(models.LineConfig{Name: "Line 1", IsActive: true}); err != nil {
		t.Fatal(err)
	}
	lines, err := store.GetAllLines()
	if err != nil || len(lines) != 1 {
		t.Fatalf("GetAllLines() = %#v, %v", lines, err)
	}
	scannerID, err := store.SaveLineScanner(models.ScannerConfig{
		LineID:     lines[0].ID,
		Name:       "Camera 1",
		DriverType: "tcp_camera",
		Address:    "127.0.0.1",
		Port:       3000,
		Role:       models.ScannerRoleAuditCheck,
		IsActive:   true,
	})
	if err != nil {
		t.Fatal(err)
	}

	taskResult, err := store.db.Exec(`INSERT INTO tasks (line_id, template_name, status) VALUES (?, 'label', 'active')`, lines[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := taskResult.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	const code = "010460123456789021ABC"
	if _, err := store.getCodesDB().Exec(`
		INSERT INTO task_codes (task_id, code, status) VALUES (?, ?, 'printed')`, taskID, code); err != nil {
		t.Fatal(err)
	}

	read, err := store.RecordScannerRead(models.ScannerConfig{
		ID: int(scannerID), LineID: lines[0].ID,
	}, code, []byte(code), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if read.MatchStatus != "unmatched" || read.TaskCodeID != nil || read.TaskID == nil || *read.TaskID != int(taskID) {
		t.Fatalf("RecordScannerRead() = %#v", read)
	}

	var status string
	if err := store.getCodesDB().QueryRow(`SELECT status FROM task_codes WHERE task_id = ? AND code = ?`, taskID, code).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "printed" {
		t.Fatalf("task code status = %q, want printed", status)
	}
}

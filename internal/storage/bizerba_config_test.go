package storage

import (
	"fmt"
	"path/filepath"
	"rovnoMark/internal/models"
	"testing"
)

func TestPrinterBCSDeviceRoundTrip(t *testing.T) {
	store := New(filepath.Join(t.TempDir(), "config.db"))
	defer store.db.Close()

	_, err := store.SavePrinter(models.PrinterConfig{
		Name:            "Bizerba line 1",
		IP:              "brain2",
		Port:            2020,
		DriverType:      "bizerba",
		BCSDevice:       "GLPMax",
		BizerbaMode:     "unique",
		BizerbaConveyor: true,
		CaptureWeight:   true,
		IsActive:        true,
	})
	if err != nil {
		t.Fatal(err)
	}

	printers, err := store.GetAllPrinters()
	if err != nil {
		t.Fatal(err)
	}
	if len(printers) != 1 || printers[0].BCSDevice != "GLPMax" ||
		printers[0].BizerbaMode != "unique" || !printers[0].BizerbaConveyor || !printers[0].CaptureWeight {
		t.Fatalf("printers = %#v", printers)
	}
}

func TestPrinterBCSSettingsDefaultToStream(t *testing.T) {
	store := New(filepath.Join(t.TempDir(), "config.db"))
	defer store.db.Close()

	if _, err := store.SavePrinter(models.PrinterConfig{
		Name: "Bizerba default", DriverType: "bizerba", BCSDevice: "GLPMax", IsActive: true,
	}); err != nil {
		t.Fatal(err)
	}
	printers, err := store.GetAllPrinters()
	if err != nil {
		t.Fatal(err)
	}
	if len(printers) != 1 || printers[0].BizerbaMode != "stream" || printers[0].BizerbaConveyor || printers[0].CaptureWeight {
		t.Fatalf("printers = %#v", printers)
	}
}

func TestSaveMarkWeight(t *testing.T) {
	store := New(filepath.Join(t.TempDir(), "weights.db"))
	defer store.db.Close()

	printerResult, err := store.db.Exec(`INSERT INTO printers (name, ip, driver_type) VALUES ('Bizerba', 'brain2', 'bizerba')`)
	if err != nil {
		t.Fatal(err)
	}
	printerID, _ := printerResult.LastInsertId()
	lineResult, err := store.db.Exec(`INSERT INTO lines (name) VALUES ('Line 1')`)
	if err != nil {
		t.Fatal(err)
	}
	lineID, _ := lineResult.LastInsertId()
	taskResult, err := store.db.Exec(`INSERT INTO tasks (line_id, template_name) VALUES (?, '1')`, lineID)
	if err != nil {
		t.Fatal(err)
	}
	taskID, _ := taskResult.LastInsertId()
	const expectedMark = "0104620031245728215?NFCZ<GS>93SwuV"
	if _, err := store.db.Exec(`
		INSERT INTO task_codes (task_id, code, status, printer_id, printer_index)
		VALUES (?, ?, 'in_buffer', ?, 41)`, taskID, expectedMark, printerID); err != nil {
		t.Fatal(err)
	}

	if err := store.SaveMarkWeight(int(printerID), 41, expectedMark, "1.248"); err != nil {
		t.Fatal(err)
	}
	var id, savedTaskID int64
	var mark, weight, createdAt string
	if err := store.db.QueryRow(`SELECT id, task_id, mark, weight, created_at FROM mark_weights`).Scan(&id, &savedTaskID, &mark, &weight, &createdAt); err != nil {
		t.Fatal(err)
	}
	if id < 1 || savedTaskID != taskID || mark != expectedMark || weight != "1.248" || createdAt == "" {
		t.Fatalf("saved weight = id:%d task_id:%d mark:%q weight:%q created_at:%q", id, savedTaskID, mark, weight, createdAt)
	}
}

func TestMarkAsPrintedIsScopedToPrinter(t *testing.T) {
	store := New(filepath.Join(t.TempDir(), "printed.db"))
	defer store.db.Close()

	lineResult, err := store.db.Exec(`INSERT INTO lines (name) VALUES ('Line 1')`)
	if err != nil {
		t.Fatal(err)
	}
	lineID, _ := lineResult.LastInsertId()
	taskResult, err := store.db.Exec(`INSERT INTO tasks (line_id, template_name) VALUES (?, '1')`, lineID)
	if err != nil {
		t.Fatal(err)
	}
	taskID, _ := taskResult.LastInsertId()
	for _, printerID := range []int{4, 7} {
		if _, err := store.db.Exec(`
			INSERT INTO task_codes (task_id, code, status, printer_id, printer_index)
			VALUES (?, ?, 'in_buffer', ?, 0)`, taskID, fmt.Sprintf("mark-%d", printerID), printerID); err != nil {
			t.Fatal(err)
		}
	}

	affected, err := store.MarkAsPrinted(int(taskID), 4, 0)
	if err != nil || affected != 1 {
		t.Fatalf("MarkAsPrinted() = %d, %v", affected, err)
	}
	for printerID, want := range map[int]string{4: "printed", 7: "in_buffer"} {
		var status string
		if err := store.db.QueryRow(`SELECT status FROM task_codes WHERE task_id = ? AND printer_id = ?`, taskID, printerID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != want {
			t.Fatalf("printer %d status = %q, want %q", printerID, status, want)
		}
	}
}

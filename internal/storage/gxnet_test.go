package storage

import (
	"path/filepath"
	"rovnoMark/internal/models"
	"testing"
	"time"
)

func TestGXNETSettingsAndResponsePersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gxnet.db")
	s := New(path)
	p := models.PrinterConfig{Name: "MV_master", IP: "brain2", DriverType: "bizerba", BCSDevice: "MV_master", IsActive: true}
	id, err := s.SavePrinter(p)
	if err != nil {
		t.Fatal(err)
	}
	printers, err := s.GetAllPrinters()
	if err != nil || len(printers) != 1 || printers[0].RecordGXNET {
		t.Fatalf("default: %+v %v", printers, err)
	}
	p.ID, p.RecordGXNET = int(id), true
	if _, err = s.SavePrinter(p); err != nil {
		t.Fatal(err)
	}
	const raw = "A!GL19|3007\r\n\x1d"
	at := time.Date(2026, 9, 7, 10, 0, 0, 123, time.FixedZone("MSK", 10800))
	if err = s.SaveGXNETResponse(int(id), p.BCSDevice, at, "A?GL19", "0", "handle", raw, 0); err != nil {
		t.Fatal(err)
	}
	s.db.Close()
	s = New(path)
	defer s.db.Close()
	printers, err = s.GetAllPrinters()
	if err != nil || len(printers) != 1 || !printers[0].RecordGXNET {
		t.Fatalf("reload: %+v %v", printers, err)
	}
	var payload, received, command, device string
	if err = s.db.QueryRow(`SELECT payload, received_at, command, device FROM bizerba_responses WHERE printer_id = ?`, id).Scan(&payload, &received, &command, &device); err != nil {
		t.Fatal(err)
	}
	if payload != raw || received != "2026-09-07 07:00:00.000000123" || command != "A?GL19" || device != p.BCSDevice {
		t.Fatalf("%q %q %q %q", payload, received, command, device)
	}
}

package scanners

import (
	"context"
	"rovnoMark/internal/core"
	"testing"
	"time"
)

func TestHandleFrameIgnoresNoReadAndEmitsSuccessfulRead(t *testing.T) {
	driver := &TCPDriver{
		events: make(chan core.ScanEvent, 2),
		ctx:    context.Background(),
	}

	driver.handleFrame([]byte("NoRead\r\n"))
	driver.handleFrame([]byte("Region1,NoRead\r\n"))
	driver.handleFrame([]byte("Region247,noread\r\n"))
	select {
	case event := <-driver.events:
		t.Fatalf("NoRead frame emitted event: %#v", event)
	default:
	}

	driver.handleFrame([]byte("010460123456789021ABC\r\n"))
	select {
	case event := <-driver.events:
		if event.Code != "010460123456789021ABC" {
			t.Fatalf("event code = %q", event.Code)
		}
	default:
		t.Fatal("successful read did not emit event")
	}

	driver.handleFrame([]byte("010460123456789021ABC\r\n"))
	select {
	case event := <-driver.events:
		t.Fatalf("immediate repeated frame emitted event: %#v", event)
	default:
	}

	status, err := driver.GetStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.TotalScans != 5 || status.NoReads != 3 || status.GoodScans != 2 {
		t.Fatalf("scanner status = %#v", status)
	}
}

func TestRegionTextWithoutNumericIDIsNotDiscarded(t *testing.T) {
	driver := &TCPDriver{
		events: make(chan core.ScanEvent, 1),
		ctx:    context.Background(),
	}

	driver.handleFrame([]byte("RegionX,NoRead\r\n"))
	select {
	case event := <-driver.events:
		if event.Code != "RegionX,NoRead" {
			t.Fatalf("event code = %q", event.Code)
		}
	default:
		t.Fatal("non-matching frame was discarded")
	}
}

func TestSuccessfulRegionFrameStoresOnlyCodePayload(t *testing.T) {
	driver := &TCPDriver{
		events: make(chan core.ScanEvent, 1),
		ctx:    context.Background(),
	}

	driver.handleFrame([]byte("Region12,010460123456789021ABC\r\n"))
	select {
	case event := <-driver.events:
		if event.Code != "010460123456789021ABC" {
			t.Fatalf("event code = %q, want code without region", event.Code)
		}
		if string(event.RawData) != "010460123456789021ABC" {
			t.Fatalf("raw data = %q, want payload without region", event.RawData)
		}
	default:
		t.Fatal("successful region frame did not emit event")
	}
}

func TestSameCodeIsEmittedAgainAfterDuplicateWindow(t *testing.T) {
	driver := &TCPDriver{
		events:          make(chan core.ScanEvent, 1),
		ctx:             context.Background(),
		lastEmittedCode: "010460123456789021ABC",
		lastEmittedAt:   time.Now().Add(-duplicateWindow - time.Second),
	}

	driver.handleFrame([]byte("Region1,010460123456789021ABC\r\n"))
	select {
	case <-driver.events:
	default:
		t.Fatal("same code was still suppressed after duplicate window")
	}
}

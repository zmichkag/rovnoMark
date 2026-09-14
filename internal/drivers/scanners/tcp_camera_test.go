package scanners

import (
	"context"
	"rovnoMark/internal/core"
	"testing"
	"time"
)

func TestHandleFrameEmitsNoreadAndSuccessfulRead(t *testing.T) {
	driver := &TCPDriver{
		events: make(chan core.ScanEvent, 5),
		ctx:    context.Background(),
	}

	driver.handleFrame([]byte("Noread\r\n"))
	driver.handleFrame([]byte("Region1,Noread\r\n"))
	driver.handleFrame([]byte("Region247,Noread\r\n"))
	for i := 0; i < 3; i++ {
		select {
		case event := <-driver.events:
			if !event.IsNoRead || event.Code != "Noread" {
				t.Fatalf("Noread event = %#v", event)
			}
		default:
			t.Fatalf("Noread frame %d did not emit event", i+1)
		}
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

	driver.handleFrame([]byte("RegionX,Noread\r\n"))
	select {
	case event := <-driver.events:
		if event.Code != "RegionX,Noread" {
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

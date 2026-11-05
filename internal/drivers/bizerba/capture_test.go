package bizerba

import (
	"fmt"
	"reflect"
	"testing"
	"time"
)

func expectResponse(t *testing.T, responses <-chan Response, payload string) {
	t.Helper()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		select {
		case r := <-responses:
			if r.Payload == payload {
				return
			}
		case <-timer.C:
			t.Fatalf("response %q not recorded", payload)
		}
	}
}

func TestCaptureStartsOnlyForJobStopsAndRestartsForNewJob(t *testing.T) {
	fake := newFakeConnection()
	responses := make(chan Response, 32)
	recorded := make(chan struct{ mark, weight string }, 1)
	d := newDriverWithProfile("MV_master", func() (bcsConnection, error) { return fake, nil }, Profile{
		RecordGXNET: true, RecordResponse: func(r Response) error { responses <- r; return nil },
		RecordWeight: func(_ int, mark, weight string) error {
			recorded <- struct{ mark, weight string }{mark, weight}
			return nil
		},
	})
	t.Cleanup(func() { _ = d.Close() })
	d.Start()
	time.Sleep(20 * time.Millisecond)
	opensIdle, _, _ := fake.snapshot()
	if len(opensIdle) != 0 {
		t.Fatalf("receiver opened without an active job: %v", opensIdle)
	}
	if err := d.InitSession("DATAMATRIX", 2, nil); err != nil {
		t.Fatal(err)
	}
	if n, err := d.PrintBatchIndexed("DATAMATRIX", 1, []string{"0104620031245728215?NFCZ<GS>93SwuV"}); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	opensBefore, _, _ := fake.snapshot()
	if _, err := d.GetStatus(); err != nil {
		t.Fatal(err)
	}
	fake.spontaneous <- "A!PW05|1"
	expectResponse(t, responses, "A!PW05|1")
	time.Sleep(30 * time.Millisecond)
	last, _ := d.GetLastPrintedIndex()
	if last != -1 {
		t.Fatalf("PW05 last printed index = %d, want -1", last)
	}
	select {
	case got := <-recorded:
		t.Fatalf("PW05 unexpectedly recorded value %#v", got)
	case <-time.After(30 * time.Millisecond):
	}
	opensAfter, _, _ := fake.snapshot()
	if !reflect.DeepEqual(opensBefore, opensAfter) {
		t.Fatal("polling opened another connection during printing")
	}
	if err := d.ClearQueue(); err != nil {
		t.Fatal(err)
	}
	d.mu.RLock()
	session, paused := d.session, d.capturePaused
	d.mu.RUnlock()
	if session != nil || !paused {
		t.Fatal("stop did not close and pause receiver")
	}
	// Both the supervisor and dashboard polling must respect explicit stop.
	time.Sleep(1100 * time.Millisecond)
	d.startMu.Lock()
	err := d.ensureCaptureLocked()
	d.startMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	opensAfter, sends, closed := fake.snapshot()
	if !reflect.DeepEqual(opensBefore, opensAfter) || closed != len(opensAfter) {
		t.Fatalf("receiver reopened after stop: %v closed=%d", opensAfter, closed)
	}
	if sends[len(sends)-1] != (fakeSend{"A!GWC3", "0"}) {
		t.Fatal("channel E not disabled")
	}
	if _, err := d.GetStatus(); err != nil {
		t.Fatal(err)
	}
	opensAfter, _, _ = fake.snapshot()
	if opensAfter[len(opensAfter)-1] {
		t.Fatal("status polling reopened spontaneous receiver after stop")
	}
	if err := d.InitSession("DATAMATRIX", 2, nil); err != nil {
		t.Fatal(err)
	}
	fake.spontaneous <- "A!PW05|2"
	expectResponse(t, responses, "A!PW05|2")
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := d.GetStatus(); err == nil {
		t.Fatal("closed driver reopened")
	}
}

type failingReceiver struct{ *fakeConnection }

func (f *failingReceiver) ReceiveOne(string, time.Duration) (string, int, error) {
	return "", 0, fmt.Errorf("connection lost")
}

func TestCaptureReconnectClosesPreviousReceiver(t *testing.T) {
	first, second := newFakeConnection(), newFakeConnection()
	responses := make(chan Response, 8)
	calls := 0
	d := newDriverWithProfile("MV_master", func() (bcsConnection, error) {
		calls++
		if calls == 1 {
			return &failingReceiver{first}, nil
		}
		_, _, closed := first.snapshot()
		if closed != 1 {
			return nil, fmt.Errorf("old receiver still open")
		}
		return second, nil
	}, Profile{RecordGXNET: true, RecordResponse: func(r Response) error { responses <- r; return nil }})
	t.Cleanup(func() { _ = d.Close() })
	d.Start()
	if err := d.InitSession("DATAMATRIX", 2, nil); err != nil {
		t.Fatal(err)
	}
	second.spontaneous <- "A!PW05|0"
	expectResponse(t, responses, "A!PW05|0")
}

func TestCaptureDisabledDoesNotOpenReceiver(t *testing.T) {
	fake := newFakeConnection()
	d := newDriver("MV_slave", func() (bcsConnection, error) { return fake, nil })
	d.Start()
	defer d.Close()
	if _, err := d.GetStatus(); err != nil {
		t.Fatal(err)
	}
	opens, _, closed := fake.snapshot()
	if !reflect.DeepEqual(opens, []bool{false}) || closed != 1 {
		t.Fatalf("opens=%v closed=%d", opens, closed)
	}
}

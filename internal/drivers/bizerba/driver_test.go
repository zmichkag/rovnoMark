package bizerba

import (
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"
)

type fakeSend struct{ header, data string }

type fakeConnection struct {
	mu          sync.Mutex
	opens       []bool
	sends       []fakeSend
	spontaneous chan string
	closed      int
	identity    string
	device      string
	plu         string
	sendErrors  map[string]error
}

func newFakeConnection() *fakeConnection {
	return &fakeConnection{spontaneous: make(chan string, 10), plu: "5047", sendErrors: make(map[string]error)}
}

func (f *fakeConnection) Open(identity, device string, spontaneous bool) error {
	f.mu.Lock()
	f.opens = append(f.opens, spontaneous)
	f.identity = identity
	f.device = device
	f.mu.Unlock()
	return nil
}

func (f *fakeConnection) openTarget() (string, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.identity, f.device
}

func (f *fakeConnection) Send(header, data string, _ time.Duration) (string, int, error) {
	f.mu.Lock()
	f.sends = append(f.sends, fakeSend{header, data})
	f.mu.Unlock()
	if err := f.sendErrors[header]; err != nil {
		return "", bcsStatusOK, err
	}
	if header == "A?GL19" {
		return "handle-plu", bcsStatusOK, nil
	}
	if header == "A?PL03" {
		return "handle-status", bcsStatusOK, nil
	}
	return "", bcsStatusOK, nil
}

func (f *fakeConnection) ReceiveOne(queue string, _ time.Duration) (string, int, error) {
	if queue == "handle-plu" {
		return "A!GL19|" + f.plu, bcsStatusOK, nil
	}
	if queue == "handle-status" {
		return "A!PL03|0", bcsStatusOK, nil
	}
	select {
	case packet := <-f.spontaneous:
		return packet, bcsStatusOK, nil
	case <-time.After(2 * time.Millisecond):
		return "", bcsStatusTimeout, nil
	}
}

func (f *fakeConnection) Close() error {
	f.mu.Lock()
	f.closed++
	f.mu.Unlock()
	return nil
}

func (f *fakeConnection) snapshot() ([]bool, []fakeSend, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]bool(nil), f.opens...), append([]fakeSend(nil), f.sends...), f.closed
}

func TestGetStatusUsesTransientNonSpontaneousConnection(t *testing.T) {
	fake := newFakeConnection()
	driver := newDriver("GLPMax", func() (bcsConnection, error) { return fake, nil })
	status, err := driver.GetStatus()
	if err != nil || status != "ГОТОВ" {
		t.Fatalf("GetStatus() = %q, %v", status, err)
	}
	status, err = driver.GetStatus()
	if err != nil || status != "ГОТОВ" {
		t.Fatalf("second GetStatus() = %q, %v", status, err)
	}
	opens, sends, closed := fake.snapshot()
	if !reflect.DeepEqual(opens, []bool{false, false}) {
		t.Fatalf("Open spontaneous flags = %v, want [false false]", opens)
	}
	if closed != 2 {
		t.Fatalf("Close count = %d, want 2", closed)
	}
	identity, device := fake.openTarget()
	if identity != "GLPMax" || device != "GLPMax" {
		t.Fatalf("Open target = (%q, %q), want (GLPMax, GLPMax)", identity, device)
	}
	if len(sends) != 2 || sends[0] != (fakeSend{"A?PL03", "0"}) || sends[1] != (fakeSend{"A?PL03", "0"}) {
		t.Fatalf("sends = %#v", sends)
	}
}

func TestConfiguredEquipmentProfile(t *testing.T) {
	fake := newFakeConnection()
	driver := newDriverWithProfile("Bizerba2", func() (bcsConnection, error) { return fake, nil }, Profile{
		Mode: MarkingModeUnique, Conveyor: true,
	})
	if driver.equipment.mode != MarkingModeUnique || !driver.equipment.conveyorEnabled {
		t.Fatalf("equipment = %#v", driver.equipment)
	}
	if !driver.equipment.unique.Enabled() {
		t.Fatal("Unique profile is not enabled")
	}
}

func TestSelectTemplateAcceptsGLPMaxRefreshErrorAfterPLUReadback(t *testing.T) {
	fake := newFakeConnection()
	fake.plu = "2644"
	fake.sendErrors["A!XV00|GL19|LX02"] = errors.New("Datensatz nicht vorhanden")
	driver := newDriver("GLPMax", func() (bcsConnection, error) { return fake, nil })

	if err := driver.SelectTemplate("2644", nil); err != nil {
		t.Fatal(err)
	}
	_, sends, _ := fake.snapshot()
	want := []fakeSend{{"A!XV00|GL19|LX02", "2644"}, {"A?GL19", "0"}}
	if !reflect.DeepEqual(sends, want) {
		t.Fatalf("sends = %#v, want %#v", sends, want)
	}
}

func TestMarkSessionOpensEAdvancesOnPV01AndClosesE(t *testing.T) {
	fake := newFakeConnection()
	recorded := make(chan struct{ mark, weight string }, 2)
	driver := newDriverWithProfile("GLPMax", func() (bcsConnection, error) { return fake, nil }, Profile{
		CaptureWeight: true,
		RecordWeight: func(_ int, mark, weight string) error {
			recorded <- struct{ mark, weight string }{mark, weight}
			return nil
		},
	})
	if err := driver.InitSession("DATAMATRIX", 10, nil); err != nil {
		t.Fatal(err)
	}
	opens, sends, closed := fake.snapshot()
	if len(opens) != 0 || len(sends) != 0 || closed != 0 {
		t.Fatalf("InitSession captured COM before marks were available: opens=%v sends=%#v closed=%d", opens, sends, closed)
	}
	free, err := driver.GetBufferFreeSpace()
	if err != nil || free != 10 {
		t.Fatalf("GetBufferFreeSpace() = %d, %v", free, err)
	}
	codes := []string{
		"0104620031245728215?NFCZ<GS>93SwuV",
		"0104620031245728215ABCDE<GS>931234",
	}
	loaded, err := driver.PrintBatchIndexed("DATAMATRIX", 41, codes)
	if err != nil || loaded != 2 {
		t.Fatalf("PrintBatchIndexed() = %d, %v", loaded, err)
	}
	waitForSend(t, fake, fakeSend{"A!GT03", "5?NFCZ"})
	waitForSend(t, fake, fakeSend{"A!GT04", "@1D93SwuV"})
	fake.spontaneous <- "A!PV01|PW02|6|PW00|0|GW09|2|GL16|0|PD00|KG;-3;200|LX02"
	waitForSend(t, fake, fakeSend{"A!GT03", "5ABCDE"})
	waitForSend(t, fake, fakeSend{"A!GT04", "@1D931234"})
	first := <-recorded
	if first.mark != "0104620031245728215?NFCZ<GS>93SwuV" || first.weight != "0.200" {
		t.Fatalf("first recorded value = %#v", first)
	}
	fake.spontaneous <- "A!PV01|PW02|6|PW00|0|GW09|2|GL16|0|PD00|KG;-3;200|LX02"
	waitForSend(t, fake, fakeSend{"A!GT03", ""})
	waitForSend(t, fake, fakeSend{"A!GT04", ""})
	second := <-recorded
	if second.mark != "0104620031245728215ABCDE<GS>931234" || second.weight != "0.200" {
		t.Fatalf("second recorded value = %#v", second)
	}

	deadline := time.Now().Add(time.Second)
	for {
		last, _ := driver.GetLastPrintedIndex()
		if last == 42 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("last printed index = %d, want 42", last)
		}
		time.Sleep(time.Millisecond)
	}
	if err := driver.ClearQueue(); err != nil {
		t.Fatal(err)
	}
	opens, sends, closed = fake.snapshot()
	if !reflect.DeepEqual(opens, []bool{true}) {
		t.Fatalf("Open spontaneous flags = %v, want [true]", opens)
	}
	if sends[0] != (fakeSend{"A!GWC3", "1"}) || sends[len(sends)-1] != (fakeSend{"A!GWC3", "0"}) {
		t.Fatalf("channel lifecycle sends = %#v", sends)
	}
	if closed != 1 {
		t.Fatalf("Close count = %d, want 1", closed)
	}
}

func TestBizerbaWeight(t *testing.T) {
	for _, test := range []struct {
		packet string
		want   string
	}{
		{"A!PV01|PD00|KG;-3;1248|LX02", "1.248"},
		{"A!PV01|PD00|KG;-3;32|LX02", "0.032"},
		{"A!PV01|PD00|1.248|LX02", "1.248"},
	} {
		got, err := bizerbaWeight(test.packet)
		if err != nil || got != test.want {
			t.Fatalf("bizerbaWeight(%q) = %q, %v; want %q", test.packet, got, err, test.want)
		}
	}
}

func waitForSend(t *testing.T, fake *fakeConnection, want fakeSend) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		_, sends, _ := fake.snapshot()
		for _, send := range sends {
			if send == want {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("send %#v was not observed", want)
}

func TestParseFieldResponseAndDate(t *testing.T) {
	value, err := ParseFieldResponse("A!GL19|5047", "GL19")
	if err != nil || value != "5047" {
		t.Fatalf("ParseFieldResponse() = %q, %v", value, err)
	}
	date, err := normalizeBizerbaDate("20.08.2026")
	if err != nil || date != "200826" {
		t.Fatalf("normalizeBizerbaDate() = %q, %v", date, err)
	}
}

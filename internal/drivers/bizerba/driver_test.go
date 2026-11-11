package bizerba

import (
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
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

func TestConfiguredEquipmentStreamGT0607AndCustomMapping(t *testing.T) {
	explicitSeparator := false
	driver := newDriverWithProfile("Bizerba2", func() (bcsConnection, error) { return newFakeConnection(), nil }, Profile{
		Mode:                MarkingModeStreamGT0607,
		ExplicitGSSeparator: &explicitSeparator,
	})
	if driver.equipment.mapping.SerialField != "GT06" || driver.equipment.mapping.CryptoField != "GT07" {
		t.Fatalf("stream_gt06_gt07 mapping = %#v", driver.equipment.mapping)
	}
	if driver.equipment.explicitGSSeparator {
		t.Fatal("explicit GS separator was enabled")
	}

	driver = newDriverWithProfile("Bizerba2", func() (bcsConnection, error) { return newFakeConnection(), nil }, Profile{
		Mode: MarkingModeStreamGT0607,
		Mapping: FieldMapping{
			SerialField: "GT51",
			CryptoField: "GT52",
		},
	})
	if driver.equipment.mapping.SerialField != "GT51" || driver.equipment.mapping.CryptoField != "GT52" {
		t.Fatalf("custom mapping was overwritten: %#v", driver.equipment.mapping)
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

func TestSelectTemplateMapsDateAndUserLabelWithoutJSONMapping(t *testing.T) {
	fake := newFakeConnection()
	driver := newDriver("GLPMax", func() (bcsConnection, error) { return fake, nil })

	if err := driver.SelectTemplate("2644", map[string]string{
		"date1":        "20.08.2026",
		"expiry_date":  "2026-09-30",
		"team":         "4",
		"label_design": "8192",
	}); err != nil {
		t.Fatal(err)
	}

	_, sends, _ := fake.snapshot()
	want := map[fakeSend]bool{
		{"A!GL06", "200826"}: true,
		{"A!GL07", "300926"}: true,
		{"A!GL15", "4"}:      true,
		{"A!GW17", "8192"}:   true,
	}
	for command := range want {
		found := false
		for _, sent := range sends {
			if sent == command {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("command %#v not found in %#v", command, sends)
		}
	}
}

func TestPrintBatchUsesGT0607WithoutExplicitGSSeparator(t *testing.T) {
	fake := newFakeConnection()
	explicitSeparator := false
	driver := newDriverWithProfile("GLPMax", func() (bcsConnection, error) { return fake, nil }, Profile{
		Mode:                MarkingModeStreamGT0607,
		ExplicitGSSeparator: &explicitSeparator,
	})
	if err := driver.InitSession("DATAMATRIX", 10, nil); err != nil {
		t.Fatal(err)
	}
	loaded, err := driver.PrintBatchIndexed("DATAMATRIX", 1, []string{
		"0104620031245728215?NFCZ<GS>93SwuV",
	})
	if err != nil || loaded != 1 {
		t.Fatalf("PrintBatchIndexed() = %d, %v", loaded, err)
	}
	waitForSend(t, fake, fakeSend{"A!GT06", "5?NFCZ"})
	waitForSend(t, fake, fakeSend{"A!GT07", "SwuV"})
	if err := driver.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestUniqueModeDoesNotFallBackToStream(t *testing.T) {
	fake := newFakeConnection()
	driver := newDriverWithProfile("GLPMax", func() (bcsConnection, error) { return fake, nil }, Profile{
		Mode: MarkingModeUnique,
	})
	codes := make([]string, uniqueDataMinRecords)
	for i := range codes {
		codes[i] = "0104620031245728215?NFCZ<GS>93SwuV"
	}
	loaded, err := driver.PrintBatchIndexed("DATAMATRIX", 1, codes)
	if err == nil || loaded != 0 || !strings.Contains(err.Error(), "FTP-загрузка ещё не настроена") {
		t.Fatalf("PrintBatchIndexed() = %d, %v", loaded, err)
	}
	opens, sends, _ := fake.snapshot()
	if len(opens) != 0 || len(sends) != 0 {
		t.Fatalf("Unique mode fell back to stream: opens=%v sends=%#v", opens, sends)
	}
}

func TestSelectTemplateRejectsInvalidUserLabelNumber(t *testing.T) {
	fake := newFakeConnection()
	driver := newDriver("GLPMax", func() (bcsConnection, error) { return fake, nil })

	err := driver.SelectTemplate("2644", map[string]string{"design": "17"})
	if err == nil {
		t.Fatal("SelectTemplate() accepted a user label number below 8192")
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
	fake.spontaneous <- "A!PV01|PW02|7|PW00|0|GW09|2|GL16|0|PD00|KG;-3;200|LX02"
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

func TestPW02DeduplicatorWindow(t *testing.T) {
	dedup := newPW02Deduplicator()
	start := time.Unix(100, 0)

	if !dedup.shouldProcess("5", "PV01", start) {
		t.Fatal("first message was rejected")
	}
	if dedup.shouldProcess("5", "PV04", start.Add(5*time.Second-time.Nanosecond)) {
		t.Fatal("paired message inside the five-second window was accepted")
	}
	if !dedup.shouldProcess("5", "PV05", start.Add(5*time.Second)) {
		t.Fatal("handle was not reusable after five seconds")
	}
	if dedup.seen["5"].msgType != "PV05" {
		t.Fatalf("stored message type = %q, want PV05", dedup.seen["5"].msgType)
	}
	dedup.reset()
	if len(dedup.seen) != 0 {
		t.Fatal("reset did not clear deduplication history")
	}
}

func TestMarkSessionMessageRules(t *testing.T) {
	tests := []struct {
		name        string
		packets     []string
		wantWeights []string
	}{
		{name: "PV01", packets: []string{"A!PV01|PW02|1|PD00|KG;-3;200|LX02"}, wantWeights: []string{"0.200"}},
		{name: "PV04", packets: []string{"A!PV04|PW02|2|PD00|KG;-3;300|LX02"}, wantWeights: []string{"0.300"}},
		{name: "PV05", packets: []string{"A!PV05|PW02|3|LX02"}, wantWeights: []string{"0"}},
		{
			name: "PV01 then PV04",
			packets: []string{
				"A!PV01|PW02|4|PD00|KG;-3;400|LX02",
				"A!PV04|PW02|4|PD00|KG;-3;999|LX02",
			},
			wantWeights: []string{"0.400"},
		},
		{
			name: "PV04 then PV01",
			packets: []string{
				"A!PV04|PW02|5|PD00|KG;-3;500|LX02",
				"A!PV01|PW02|5|PD00|KG;-3;999|LX02",
			},
			wantWeights: []string{"0.500"},
		},
		{
			name: "PV01 then PV05",
			packets: []string{
				"A!PV01|PW02|6|PD00|KG;-3;600|LX02",
				"A!PV05|PW02|6|LX02",
			},
			wantWeights: []string{"0.600"},
		},
		{
			name: "same type duplicate",
			packets: []string{
				"A!PV05|PW02|7|LX02",
				"A!PV05|PW02|7|LX02",
			},
			wantWeights: []string{"0"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake, driver, recorded := newMessageRuleSession(t, 2, nil)
			for _, packet := range test.packets {
				fake.spontaneous <- packet
			}
			for _, want := range test.wantWeights {
				got := receiveRecordedWeight(t, recorded)
				if got != want {
					t.Fatalf("recorded weight = %q, want %q", got, want)
				}
			}
			assertNoRecordedWeight(t, recorded)
			waitForLastPrinted(t, driver, len(test.wantWeights))
		})
	}
}

func TestMarkSessionRejectsInvalidMessagesWithoutAdvancing(t *testing.T) {
	tests := []struct {
		name   string
		packet string
	}{
		{name: "missing PD00", packet: "A!PV01|PW02|10|LX02"},
		{name: "invalid PD00", packet: "A!PV04|PW02|10|PD00|broken|LX02"},
		{name: "missing PW02", packet: "A!PV05|LX02"},
		{name: "empty PW02", packet: "A!PV05|PW02||LX02"},
		{name: "PW05", packet: "A!PW05|10"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake, driver, recorded := newMessageRuleSession(t, 1, nil)
			fake.spontaneous <- test.packet
			assertNoRecordedWeight(t, recorded)
			last, err := driver.GetLastPrintedIndex()
			if err != nil || last != -1 {
				t.Fatalf("last printed index = %d, %v; want -1", last, err)
			}
		})
	}
}

func TestInvalidPD00DoesNotReserveHandle(t *testing.T) {
	fake, driver, recorded := newMessageRuleSession(t, 1, nil)
	fake.spontaneous <- "A!PV01|PW02|11|PD00|broken|LX02"
	assertNoRecordedWeight(t, recorded)
	fake.spontaneous <- "A!PV04|PW02|11|PD00|KG;-3;110|LX02"
	if got := receiveRecordedWeight(t, recorded); got != "0.110" {
		t.Fatalf("recorded weight = %q, want 0.110", got)
	}
	waitForLastPrinted(t, driver, 1)
}

func TestRecordWeightFailureKeepsCurrentMarkAndAllowsRetry(t *testing.T) {
	var attempts atomic.Int32
	successful := make(chan string, 1)
	fake, driver, _ := newMessageRuleSession(t, 1, func(weight string) error {
		if attempts.Add(1) == 1 {
			return errors.New("storage unavailable")
		}
		successful <- weight
		return nil
	})

	fake.spontaneous <- "A!PV01|PW02|12|PD00|KG;-3;120|LX02"
	deadline := time.Now().Add(time.Second)
	for attempts.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("record attempts = %d, want 1", got)
	}
	last, _ := driver.GetLastPrintedIndex()
	if last != -1 {
		t.Fatalf("failed write advanced queue to %d", last)
	}

	fake.spontaneous <- "A!PV01|PW02|12|PD00|KG;-3;120|LX02"
	if got := receiveRecordedWeight(t, successful); got != "0.120" {
		t.Fatalf("recorded weight = %q, want 0.120", got)
	}
	waitForLastPrinted(t, driver, 1)
}

func newMessageRuleSession(t *testing.T, markCount int, recordOverride func(string) error) (*fakeConnection, *Driver, chan string) {
	t.Helper()
	fake := newFakeConnection()
	recorded := make(chan string, markCount+2)
	record := func(_ int, _ string, weight string) error {
		if recordOverride != nil {
			return recordOverride(weight)
		}
		recorded <- weight
		return nil
	}
	driver := newDriverWithProfile("GLPMax", func() (bcsConnection, error) { return fake, nil }, Profile{RecordWeight: record})
	if err := driver.InitSession("DATAMATRIX", markCount, nil); err != nil {
		t.Fatal(err)
	}
	allCodes := []string{
		"0104620031245728215?NFCZ<GS>93SwuV",
		"0104620031245728215ABCDE<GS>931234",
	}
	loaded, err := driver.PrintBatchIndexed("DATAMATRIX", 1, allCodes[:markCount])
	if err != nil || loaded != markCount {
		t.Fatalf("PrintBatchIndexed() = %d, %v", loaded, err)
	}
	t.Cleanup(func() { _ = driver.ClearQueue() })
	return fake, driver, recorded
}

func receiveRecordedWeight(t *testing.T, recorded <-chan string) string {
	t.Helper()
	select {
	case weight := <-recorded:
		return weight
	case <-time.After(time.Second):
		t.Fatal("weight was not recorded")
		return ""
	}
}

func assertNoRecordedWeight(t *testing.T, recorded <-chan string) {
	t.Helper()
	select {
	case weight := <-recorded:
		t.Fatalf("unexpected recorded weight %q", weight)
	case <-time.After(30 * time.Millisecond):
	}
}

func waitForLastPrinted(t *testing.T, driver *Driver, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		last, err := driver.GetLastPrintedIndex()
		if err == nil && last == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	last, err := driver.GetLastPrintedIndex()
	t.Fatalf("last printed index = %d, %v; want %d", last, err, want)
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

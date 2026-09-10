package bizerba

import (
	"fmt"
	"testing"
	"time"
)

func TestGXNETCaptureDoesNotRecordCommandReplies(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			fake := newFakeConnection()
			var responses []Response
			d := newDriverWithProfile("MV_master", func() (bcsConnection, error) { return fake, nil }, Profile{
				RecordGXNET:    enabled,
				RecordResponse: func(r Response) error { responses = append(responses, r); return nil },
			})
			plu, err := d.GetCurrentTemplate()
			defer d.Close()
			if err != nil || plu != "5047" {
				t.Fatalf("plu=%q err=%v", plu, err)
			}
			if len(responses) != 0 {
				t.Fatal(responses)
			}
		})
	}
}

func TestGXNETSpontaneousRawPayloadAndStorageFailure(t *testing.T) {
	fake := newFakeConnection()
	const raw = "A!GT03|  raw\x1dvalue\r\n"
	fake.spontaneous <- raw
	var responses []Response
	c := &recordingConnection{bcsConnection: fake, requests: make(map[string]gxnetRequest), record: func(r Response) error {
		responses = append(responses, r)
		return fmt.Errorf("database unavailable")
	}}
	payload, status, err := c.ReceiveOne(spontaneousQueue, time.Millisecond)
	if payload != raw || status != bcsStatusOK || err != nil {
		t.Fatalf("payload=%q status=%d err=%v", payload, status, err)
	}
	if len(responses) != 1 || responses[0].Payload != raw || responses[0].Command != "" || responses[0].Queue != spontaneousQueue {
		t.Fatal(responses)
	}
	_, _, _ = c.ReceiveOne(spontaneousQueue, time.Millisecond)
	if len(responses) != 1 {
		t.Fatal("empty poll was recorded")
	}
}

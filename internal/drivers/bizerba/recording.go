package bizerba

import (
	"log/slog"
	"strings"
	"time"
)

// Response preserves the GXNET payload returned by BCS without normalization.
type Response struct {
	ReceivedAt time.Time
	Command    string
	Parameter  string
	Queue      string
	Payload    string
	Status     int
}

type gxnetRequest struct{ command, parameter string }

// Like the underlying COM connection, this wrapper is owned by one OS thread.
type recordingConnection struct {
	bcsConnection
	record   func(Response) error
	requests map[string]gxnetRequest
}

func (c *recordingConnection) Send(command, parameter string, timeout time.Duration) (string, int, error) {
	handle, status, err := c.bcsConnection.Send(command, parameter, timeout)
	if err == nil && handle != "" && strings.Contains(command, "?") && (status == bcsStatusOK || status == bcsStatusMore) {
		c.requests[handle] = gxnetRequest{command, parameter}
	}
	return handle, status, err
}

func (c *recordingConnection) ReceiveOne(queue string, timeout time.Duration) (string, int, error) {
	payload, status, err := c.bcsConnection.ReceiveOne(queue, timeout)
	request := c.requests[queue]
	if status != bcsStatusMore {
		delete(c.requests, queue)
	}
	// DUSTBIN contains unsolicited messages sent by the device. Other queues
	// contain replies to commands issued by the service and are not recorded.
	if queue == spontaneousQueue && payload != "" {
		response := Response{time.Now().UTC(), request.command, request.parameter, queue, payload, status}
		if recordErr := c.record(response); recordErr != nil {
			slog.Error("Bizerba: не удалось сохранить ответ GXNET", "queue", queue, "err", recordErr)
		}
	}
	return payload, status, err
}

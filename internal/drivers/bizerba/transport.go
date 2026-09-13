package bizerba

import "time"

const (
	bcsStatusOK      = 0
	bcsStatusTimeout = 1
	bcsStatusMore    = 2
)

// COM calls must be made by one goroutine pinned to one OS thread.
type bcsConnection interface {
	Open(identity, device string, spontaneous bool) error
	Send(header, data string, timeout time.Duration) (handle string, status int, err error)
	ReceiveOne(queue string, timeout time.Duration) (payload string, status int, err error)
	Close() error
}

type connectionFactory func() (bcsConnection, error)

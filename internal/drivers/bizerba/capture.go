package bizerba

import (
	"fmt"
	"log/slog"
	"time"
)

// Start starts the reconnect supervisor. It opens the receiver only after a
// marking job calls InitSession and closes it when that job stops.
func (d *Driver) Start() {
	d.startMu.Lock()
	defer d.startMu.Unlock()
	d.mu.RLock()
	closed := d.closed
	d.mu.RUnlock()
	if !d.recordGXNET || closed || d.captureStop != nil {
		return
	}
	stop := make(chan struct{})
	d.captureStop = stop
	d.captureDone = make(chan struct{})
	go func() {
		defer close(d.captureDone)
		for {
			d.startMu.Lock()
			err := d.ensureCaptureLocked()
			d.startMu.Unlock()
			select {
			case <-stop:
				return
			default:
			}
			if err != nil {
				slog.Error("Bizerba: приём GXNET недоступен", "device", d.device, "err", err)
			}
			timer := time.NewTimer(time.Second)
			select {
			case <-stop:
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
}

// Caller holds startMu. Capture remains paused while there is no active job.
func (d *Driver) ensureCaptureLocked() error {
	d.mu.RLock()
	closed, paused, s := d.closed, d.capturePaused, d.session
	d.mu.RUnlock()
	if closed {
		return fmt.Errorf("драйвер Bizerba закрыт")
	}
	if paused {
		return nil
	}
	if s != nil {
		return nil
	}
	if d.device == "" {
		return fmt.Errorf("не задано имя устройства Bizerba в BCS")
	}
	s = &markSession{requests: make(chan sessionRequest), done: make(chan struct{}), start: make(chan error, 1)}
	d.mu.Lock()
	d.session = s
	d.mu.Unlock()
	go d.runSession(s, 0, nil)
	if err := <-s.start; err != nil {
		<-s.done
		return err
	}
	return nil
}

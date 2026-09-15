package scanners

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"rovnoMark/internal/core"
	"rovnoMark/internal/core/marking"
	"rovnoMark/internal/models"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	connectTimeout    = 2 * time.Second
	reconnectInterval = time.Second
	duplicateWindow   = 5 * time.Second
)

var regionFramePattern = regexp.MustCompile(`(?i)^Region\d+\s*,\s*(.*)$`)

type TCPDriver struct {
	config models.ScannerConfig
	events chan core.ScanEvent

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	once   sync.Once

	mu              sync.RWMutex
	status          core.ScannerStatus
	conn            net.Conn
	lastEmittedCode string
	lastEmittedAt   time.Time
}

func NewTCPCamera(config models.ScannerConfig) *TCPDriver {
	ctx, cancel := context.WithCancel(context.Background())
	driver := &TCPDriver{
		config: config,
		events: make(chan core.ScanEvent, 256),
		ctx:    ctx,
		cancel: cancel,
	}
	driver.wg.Add(1)
	go driver.run()
	return driver
}

func (driver *TCPDriver) run() {
	defer driver.wg.Done()
	defer close(driver.events)

	endpoint := net.JoinHostPort(driver.config.Address, strconv.Itoa(driver.config.Port))
	for {
		if driver.ctx.Err() != nil {
			return
		}

		conn, err := net.DialTimeout("tcp", endpoint, connectTimeout)
		if err != nil {
			driver.setOffline(err)
			if !waitContext(driver.ctx, reconnectInterval) {
				return
			}
			continue
		}
		driver.setConnection(conn)
		err = driver.readConnection(conn)
		_ = conn.Close()
		if driver.ctx.Err() != nil {
			return
		}
		driver.setOffline(err)
		if !waitContext(driver.ctx, reconnectInterval) {
			return
		}
	}
}

func (driver *TCPDriver) readConnection(conn net.Conn) error {
	reader := bufio.NewReaderSize(conn, 64*1024)
	for {
		raw, err := reader.ReadBytes('\n')
		if len(raw) > 0 {
			driver.handleFrame(raw)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return fmt.Errorf("соединение закрыто камерой")
			}
			return err
		}
	}
}

func (driver *TCPDriver) handleFrame(raw []byte) {
	now := time.Now()
	text := strings.TrimSpace(string(raw))
	payload := text
	if match := regionFramePattern.FindStringSubmatch(text); match != nil {
		payload = strings.TrimSpace(match[1])
	}
	isNoRead := payload == "Noread" || payload == "NoRead"

	driver.mu.Lock()
	driver.status.TotalScans++
	driver.status.LastHeartbeat = now
	if payload == "" {
		driver.mu.Unlock()
		return
	}
	if isNoRead {
		driver.status.NoReads++
		driver.mu.Unlock()
		event := core.ScanEvent{
			ScannerID: strconv.Itoa(driver.config.ID),
			Code:      payload,
			RawData:   []byte(payload),
			Timestamp: now,
			IsNoRead:  true,
		}
		select {
		case driver.events <- event:
		case <-driver.ctx.Done():
		}
		return
	}

	code := normalizeScannedCode(payload)
	driver.status.GoodScans++
	driver.status.LastCode = code
	driver.status.LastReadAt = now
	duplicate := code == driver.lastEmittedCode && now.Sub(driver.lastEmittedAt) < duplicateWindow
	if !duplicate {
		driver.lastEmittedCode = code
		driver.lastEmittedAt = now
	}
	driver.mu.Unlock()
	if duplicate {
		return
	}

	event := core.ScanEvent{
		ScannerID: strconv.Itoa(driver.config.ID),
		Code:      code,
		RawData:   []byte(payload),
		Timestamp: now,
	}
	select {
	case driver.events <- event:
	case <-driver.ctx.Done():
	}
}

func normalizeScannedCode(value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "]d2")
	value = strings.ReplaceAll(value, marking.ASCII_GS, "<GS>")
	if parsed, err := marking.ParseAndValidateShortGS1(value); err == nil {
		return parsed.ToDBFormat()
	}
	return value
}

func (driver *TCPDriver) setConnection(conn net.Conn) {
	driver.mu.Lock()
	driver.conn = conn
	driver.status.Online = true
	driver.status.LastError = ""
	driver.status.LastHeartbeat = time.Now()
	driver.mu.Unlock()
}

func (driver *TCPDriver) setOffline(err error) {
	driver.mu.Lock()
	driver.conn = nil
	driver.status.Online = false
	if err != nil {
		driver.status.LastError = err.Error()
	}
	driver.mu.Unlock()
}

func (driver *TCPDriver) GetStatus(ctx context.Context) (core.ScannerStatus, error) {
	select {
	case <-ctx.Done():
		return core.ScannerStatus{}, ctx.Err()
	default:
	}
	driver.mu.RLock()
	defer driver.mu.RUnlock()
	return driver.status, nil
}

func (driver *TCPDriver) Events() <-chan core.ScanEvent {
	return driver.events
}

func (driver *TCPDriver) SoftwareTrigger(context.Context) error {
	return fmt.Errorf("программный триггер TCP-камеры не настроен")
}

func (driver *TCPDriver) Close() error {
	driver.once.Do(func() {
		driver.cancel()
		driver.mu.RLock()
		conn := driver.conn
		driver.mu.RUnlock()
		if conn != nil {
			_ = conn.Close()
		}
		driver.wg.Wait()
	})
	return nil
}

func waitContext(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

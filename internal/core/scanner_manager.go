package core

import (
	"context"
	"fmt"
	"log/slog"
	"rovnoMark/internal/models"
	"rovnoMark/internal/storage"
	"sync"
	"time"
)

// ScanEvent описывает событие считывания кода камерой/сканером
type ScanEvent struct {
	ScannerID string
	Code      string
	RawData   []byte
	Timestamp time.Time
	IsNoRead  bool
}

// ScannerStatus содержит оперативную телеметрию устройства технического зрения
type ScannerStatus struct {
	Online        bool      `json:"online"`
	LastHeartbeat time.Time `json:"last_heartbeat"`
	LastReadAt    time.Time `json:"last_read_at"`
	LastCode      string    `json:"last_code"`
	LastError     string    `json:"last_error,omitempty"`
	TotalScans    int64     `json:"total_scans"`
	GoodScans     int64     `json:"good_scans"`
	NoReads       int64     `json:"no_reads"`
}

// Scanner — аппаратный контракт драйвера сканера / камеры
type Scanner interface {
	GetStatus(ctx context.Context) (ScannerStatus, error)
	Events() <-chan ScanEvent
	SoftwareTrigger(ctx context.Context) error
	Close() error
}

// ScannerManager управляет пулом подключенных сканеров и обработкой входящих кодов
type ScannerManager struct {
	mu       sync.RWMutex
	store    *storage.Store
	scanners map[int]Scanner
	configs  map[int]models.ScannerConfig
	wg       sync.WaitGroup
}

// NewScannerManager создает экземпляр менеджера сканеров
func NewScannerManager(store *storage.Store) *ScannerManager {
	return &ScannerManager{
		store:    store,
		scanners: make(map[int]Scanner),
		configs:  make(map[int]models.ScannerConfig),
	}
}

// AddScanner регистрирует сканер и запускает чтение его потока событий
func (manager *ScannerManager) AddScanner(config models.ScannerConfig, scanner Scanner) {
	manager.mu.Lock()
	if previous, exists := manager.scanners[config.ID]; exists && previous != nil {
		_ = previous.Close()
	}
	manager.scanners[config.ID] = scanner
	manager.configs[config.ID] = config
	manager.mu.Unlock()

	manager.wg.Add(1)
	go manager.consume(config, scanner)
}

// consume вычитывает события из канала сканера и фиксирует их в БД
func (manager *ScannerManager) consume(config models.ScannerConfig, scanner Scanner) {
	defer manager.wg.Done()
	for event := range scanner.Events() {
		if event.IsNoRead {
			continue
		}
		read, err := manager.store.RecordScannerRead(config, event.Code, event.RawData, event.Timestamp)
		if err != nil {
			slog.Error("Ошибка фиксации чтения сканера", "scanner", config.Name, "err", err)
			continue
		}
		slog.Debug("Код считан сканером",
			"scanner", config.Name,
			"task_id", read.TaskID,
			"match_status", read.MatchStatus,
		)
	}
}

// StartPoller запускает периодический сбор телеметрии с камер
func (manager *ScannerManager) StartPoller(ctx context.Context) {
	slog.Info("SCANNER-POLLER: Запущен мониторинг состояния камер")
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = manager.GetDashboardData()
		}
	}
}

// GetDashboardData опрашивает все зарегистрированные сканеры
func (manager *ScannerManager) GetDashboardData() map[int]ScannerStatus {
	manager.mu.RLock()
	scanners := make(map[int]Scanner, len(manager.scanners))
	for id, scanner := range manager.scanners {
		scanners[id] = scanner
	}
	manager.mu.RUnlock()

	statuses := make(map[int]ScannerStatus, len(scanners))
	for id, scanner := range scanners {
		status, err := scanner.GetStatus(context.Background())
		if err != nil {
			status.Online = false
			status.LastError = err.Error()
		}
		statuses[id] = status
	}
	return statuses
}

// Close корректно останавливает все драйверы камер и ждет завершения горутин
func (manager *ScannerManager) Close() error {
	manager.mu.RLock()
	scanners := make([]Scanner, 0, len(manager.scanners))
	for _, scanner := range manager.scanners {
		scanners = append(scanners, scanner)
	}
	manager.mu.RUnlock()

	var firstErr error
	for _, scanner := range scanners {
		if err := scanner.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	manager.wg.Wait()
	if firstErr != nil {
		return fmt.Errorf("ошибка остановки сканеров: %w", firstErr)
	}
	return nil
}

package core

import (
	"context"
	"fmt"
	"log/slog"
	"rovnoMark/internal/models"
	"rovnoMark/internal/storage"
	"sync"
)

type ScannerManager struct {
	mu       sync.RWMutex
	store    *storage.Store
	scanners map[int]Scanner
	configs  map[int]models.ScannerConfig
	wg       sync.WaitGroup
}

func NewScannerManager(store *storage.Store) *ScannerManager {
	return &ScannerManager{
		store:    store,
		scanners: make(map[int]Scanner),
		configs:  make(map[int]models.ScannerConfig),
	}
}

func (manager *ScannerManager) AddScanner(config models.ScannerConfig, scanner Scanner) {
	manager.mu.Lock()
	manager.scanners[config.ID] = scanner
	manager.configs[config.ID] = config
	manager.mu.Unlock()

	manager.wg.Add(1)
	go manager.consume(config, scanner)
}

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

package core

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"rovnoMark/internal/drivers/valentine"
	"rovnoMark/internal/models"
	"rovnoMark/internal/storage"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Printer - расширенный контракт для промышленного оборудования
type Printer interface {
	GetStatus() (string, error)
	PrintTemplate(template string, fields map[string]string) error
	PrintBatchIndexed(fieldName string, startIndex int, codes []string) (int, error)
	GetLastPrintedIndex() (int, error)
	GetTotalPrints() (int64, error)
	GetTemplates() ([]string, error)
	GetTemplateFields(templateName string) ([]string, error)
	GetRemainingRibbon() (string, error)
	GetQueueCapacity(queueName string) (string, error)
	GetPrintSpeed() (string, error)
	GetCurrentPrintCount() (string, error)
	GetCurrentTemplate() (string, error)
	ClearQueue() error
	GetBufferFreeSpace() (int, error)
	UpdateStaticFields(fields map[string]string) error
	InitSession(fieldName string, maxQueue int, staticFields map[string]string) error
	SelectTemplate(template string, fields map[string]string) error
}

type TaskProcessor struct {
	Store       *storage.Store
	Manager     *PrinterManager
	activeMu    sync.Mutex
	activeTasks map[int]bool
}

func (tp *TaskProcessor) StartPumping(lineID int, taskID int) {
	tp.activeMu.Lock()
	if tp.activeTasks == nil {
		tp.activeTasks = make(map[int]bool)
	}

	if tp.activeTasks[taskID] {
		tp.activeMu.Unlock()
		slog.Debug("Pumper: Насос для этой задачи уже работает", "task_id", taskID)
		return
	}

	tp.activeTasks[taskID] = true
	tp.activeMu.Unlock()

	printers, err := tp.Store.GetPrintersByLine(lineID)
	if err != nil || len(printers) == 0 {
		slog.Error("Pumper: Не найдены принтеры для линии", "line_id", lineID, "err", err)
		tp.stopTaskTracking(taskID)
		return
	}

	ctx := context.Background()

	// Если на линии Valentin — запускаем Fast Loop на каждый принтер с учетом роли
	hasValentin := false
	for _, pCfg := range printers {
		if pCfg.DriverType == "valentine_nice" {
			hasValentin = true
			pPrinter := tp.Manager.GetPrinter(pCfg.ID)
			if vDriver, ok := pPrinter.(*valentine.NiceLabelDriver); ok {
				slog.Info("Pumper: Запуск реактивного Valentin Fast Loop", "line_id", lineID, "printer", pCfg.Name, "role", pCfg.Role)
				go tp.RunValentinFastPumper(ctx, lineID, taskID, pCfg.ID, pCfg.Role, vDriver)
			}
		}
	}

	if hasValentin {
		return
	}

	// Для всех остальных типов (Videojet, Savema, TSC, Markem)
	slog.Info("Pumper: Запуск штатного пачечного насоса", "line_id", lineID, "task_id", taskID)
	go tp.RunDefaultPumper(ctx, lineID, taskID)
}

func (tp *TaskProcessor) RunValentinFastPumper(ctx context.Context, lineID, taskID, printerID int, role string, vDriver *valentine.NiceLabelDriver) {
	defer tp.stopTaskTracking(taskID)
	slog.Info("VALENTIN-PUMPER: Запущен реактивный насос", "line_id", lineID, "printer_id", printerID)

	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	// Первичная заправка одного кода
	_ = tp.pushSingleValentinCode(taskID, printerID, role, vDriver)

	lastPrintedCount := -1

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			status, err := tp.Store.GetTaskStatus(taskID)
			if err != nil || status == "stopped" || status == "completed" {
				return
			}

			countStr, err := vDriver.GetCurrentPrintCount()
			if err != nil {
				continue
			}
			currentCount, _ := strconv.Atoi(countStr)

			if lastPrintedCount == -1 {
				lastPrintedCount = currentCount
				continue
			}

			if currentCount > lastPrintedCount {
				delta := currentCount - lastPrintedCount
				lastPrintedCount = currentCount

				for i := 0; i < delta; i++ {
					if err := tp.pushSingleValentinCode(taskID, printerID, role, vDriver); err != nil {
						slog.Error("VALENTIN-PUMPER: Сбой дозарядки буфера", "err", err)
						break
					}
					time.Sleep(5 * time.Millisecond)
				}
			}
		}
	}
}

func (tp *TaskProcessor) pushSingleValentinCode(taskID, printerID int, role string, vDriver *valentine.NiceLabelDriver) error {
	codes, err := tp.Store.FetchAndAssignCodesAlternating(taskID, printerID, role, 1)
	if err != nil || len(codes) == 0 {
		return nil
	}

	codeObj := codes[0]
	cleanCode := strings.TrimSpace(codeObj.Code)
	if idx := strings.Index(cleanCode, "|"); idx != -1 {
		cleanCode = cleanCode[:idx]
	}

	_, err = vDriver.PrintBatchIndexed("20", codeObj.PrinterIndex, []string{cleanCode})
	if err != nil {
		return fmt.Errorf("сбой отправки КМ в Valentin: %w", err)
	}

	_ = tp.Store.UpdateCodeStatusByID(codeObj.ID, "printed", codeObj.PrinterIndex)
	return nil
}

func (tp *TaskProcessor) RunDefaultPumper(ctx context.Context, lineID, taskID int) {
	defer tp.stopTaskTracking(taskID)
	slog.Info("DEFAULT-PUMPER: Запущен пачечный цикл", "line_id", lineID, "task_id", taskID)

	ticker := time.NewTicker(1000 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			status, err := tp.Store.GetTaskStatus(taskID)
			if err != nil || status == "stopped" || status == "completed" {
				return
			}

			if status == "ready" {
				continue
			}

			printers, err := tp.Store.GetPrintersByLine(lineID)
			if err != nil || len(printers) == 0 {
				continue
			}

			for _, pCfg := range printers {
				pPrinter := tp.Manager.GetPrinter(pCfg.ID)
				if pPrinter == nil {
					continue
				}

				freeSpace, err := pPrinter.GetBufferFreeSpace()
				if err != nil || freeSpace <= 0 {
					continue
				}

				targetLoad := freeSpace
				if targetLoad > 30 {
					targetLoad = 30
				}

				// Выборка с учетом четности роли конкретного принтера
				pending, err := tp.Store.FetchAndAssignCodesAlternating(taskID, pCfg.ID, pCfg.Role, targetLoad)
				if err != nil || len(pending) == 0 {
					continue
				}

				var compositePayloads []string
				var compositeFields string

				if pCfg.DriverType == "videojet" {
					dynamicField, _ := tp.Store.GetTaskDynamicField(taskID)
					staticJSONStr, _ := tp.Store.GetTaskStaticFieldsJSON(taskID)
					var staticFields map[string]string
					if staticJSONStr != "" {
						_ = json.Unmarshal([]byte(staticJSONStr), &staticFields)
					}

					for _, item := range pending {
						fields, payload := PrepareDynamicPipeline(dynamicField, staticFields, item.Code)
						compositeFields = fields
						compositePayloads = append(compositePayloads, payload)
					}
				} else {
					compositeFields, _ = tp.Store.GetTaskDynamicField(taskID)
					if compositeFields == "" {
						compositeFields = "DATAMATRIX"
					}
					for _, item := range pending {
						compositePayloads = append(compositePayloads, item.Code)
					}
				}

				startIndex := pending[0].PrinterIndex

				loaded, err := pPrinter.PrintBatchIndexed(compositeFields, startIndex, compositePayloads)
				if err != nil {
					slog.Error("Pumper: Ошибка отправки пакета в сокет", "printer", pCfg.Name, "err", err)
				} else if loaded > 0 {
					slog.Debug("Pumper: Пачка загружена", "printer", pCfg.Name, "loaded", loaded)
				}
			}
		}
	}
}

func (tp *TaskProcessor) stopTaskTracking(taskID int) {
	tp.activeMu.Lock()
	delete(tp.activeTasks, taskID)
	tp.activeMu.Unlock()
}

type PrinterManager struct {
	mu       sync.RWMutex
	printers map[int]Printer
	configs  map[int]models.PrinterConfig
	states   map[int]models.PrinterState
	logs     []models.LogEntry
}

func NewPrinterManager() *PrinterManager {
	return &PrinterManager{
		printers: make(map[int]Printer),
		configs:  make(map[int]models.PrinterConfig),
		states:   make(map[int]models.PrinterState),
		logs:     make([]models.LogEntry, 0),
	}
}

func (pm *PrinterManager) AddPrinter(config models.PrinterConfig, p Printer) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	pm.configs[config.ID] = config
	pm.printers[config.ID] = p
	pm.states[config.ID] = models.PrinterState{Status: "INITIALIZING", Ribbon: "?", Queue: "?"}

	pID := config.ID
	pm.addLogNoLock(nil, &pID, nil, "info", fmt.Sprintf("Принтер %s добавлен (%s)", config.Name, config.IP))
}

func (pm *PrinterManager) GetPrinter(id int) Printer {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return pm.printers[id]
}

func (pm *PrinterManager) GetDashboardData() (map[int]models.PrinterState, []models.LogEntry) {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	statesCopy := make(map[int]models.PrinterState)
	for k, v := range pm.states {
		statesCopy[k] = v
	}
	logsCopy := make([]models.LogEntry, len(pm.logs))
	copy(logsCopy, pm.logs)

	return statesCopy, logsCopy
}

func (pm *PrinterManager) StartTelemetryCollector(store *storage.Store, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		for range ticker.C {
			pm.mu.RLock()
			snapshot := make(map[int]models.PrinterState)
			for id, state := range pm.states {
				snapshot[id] = state
			}
			pm.mu.RUnlock()

			for id, state := range snapshot {
				_ = store.SaveTelemetry(id, state.CurCount, state.Ribbon, state.Status, state.CurTemplate)
			}
		}
	}()
}

func (pm *PrinterManager) BackgroundPoller(store *storage.Store) {
	slog.Info("ПОЛЛЕР ПРОСНУЛСЯ")
	for {
		pm.mu.RLock()
		var ids []int
		for id := range pm.printers {
			ids = append(ids, id)
		}
		pm.mu.RUnlock()

		lineMap, _ := store.GetPrinterLineMap()

		for _, id := range ids {
			pm.mu.RLock()
			p := pm.printers[id]
			cfg := pm.configs[id]
			pm.mu.RUnlock()

			if !cfg.IsActive {
				continue
			}

			status, err := p.GetStatus()
			var ribbon, queue, speed, curCount, curTemplate string

			if err == nil {
				ribbon, _ = p.GetRemainingRibbon()

				free, errSpace := p.GetBufferFreeSpace()
				if errSpace == nil {
					queue = strconv.Itoa(free)
				} else {
					queue = "N/A"
				}

				speed, _ = p.GetPrintSpeed()
				curCount, _ = p.GetCurrentPrintCount()
				curTemplate, _ = p.GetCurrentTemplate()

				if cfg.DriverType != "valentine_nice" && lineMap != nil {
					if lineID, ok := lineMap[id]; ok {
						activeTaskID, errTask := store.GetActiveTaskByLine(lineID)
						if errTask == nil && activeTaskID > 0 {
							lastPrintedIdx, errIdx := p.GetLastPrintedIndex()
							if errIdx == nil && lastPrintedIdx >= 0 {
								affected, errMark := store.MarkAsPrinted(activeTaskID, cfg.ID, lastPrintedIdx)
								if errMark == nil && affected > 0 {
									slog.Info("[POLLER-SYNC] Коды подтверждены печатью",
										"printer", cfg.Name,
										"printer_id", cfg.ID,
										"task_id", activeTaskID,
										"last_index", lastPrintedIdx,
										"confirmed_now", affected,
									)
								}
							}
						}
					}
				}
			}

			pm.mu.Lock()
			oldState := pm.states[id]
			pID := id
			var lIDPtr *int
			if lineMap != nil {
				if lID, ok := lineMap[id]; ok {
					lIDPtr = &lID
				}
			}

			if oldState.CurTemplate != "" && oldState.CurTemplate != curTemplate && curTemplate != "N/A" {
				pm.addLogNoLock(store, &pID, lIDPtr, "info", fmt.Sprintf("СМЕНА МАКЕТА: %s -> %s", oldState.CurTemplate, curTemplate))
			}

			newState := models.PrinterState{
				LastTemplate:   oldState.LastTemplate,
				LastStaticHash: oldState.LastStaticHash,
			}

			isOfflineNow := err != nil
			wasOffline := strings.Contains(oldState.Status, "ОФФЛАЙН") || oldState.Status == "INITIALIZING"

			if isOfflineNow && !wasOffline {
				pm.addLogNoLock(store, &pID, lIDPtr, "error", fmt.Sprintf("ПОТЕРЯ СВЯЗИ: %v", err))
				newState.Status = fmt.Sprintf("ОФФЛАЙН: %v", err)
				newState.Ribbon = "N/A"
				newState.Queue = "N/A"
				newState.Speed = "N/A"
				newState.CurCount = "N/A"
				newState.CurTemplate = "N/A"
			} else if !isOfflineNow && wasOffline && oldState.Status != "INITIALIZING" {
				pm.addLogNoLock(store, &pID, lIDPtr, "success", "Связь восстановлена. Статус: "+status)
				newState.Status = status
				newState.Ribbon = ribbon
				newState.Queue = queue
				newState.Speed = speed
				newState.CurCount = curCount
				newState.CurTemplate = curTemplate
			} else if isOfflineNow {
				newState.Status = oldState.Status
			} else {
				newState.Status = status
				newState.Ribbon = ribbon
				newState.Queue = queue
				newState.Speed = speed
				newState.CurCount = curCount
				newState.CurTemplate = curTemplate
			}

			pm.states[id] = newState
			pm.mu.Unlock()
		}
		time.Sleep(2 * time.Second)
	}
}

func (pm *PrinterManager) addLogNoLock(store *storage.Store, printerID *int, lineID *int, eventType string, event string) {
	printerName := "Система"
	if printerID != nil {
		if cfg, ok := pm.configs[*printerID]; ok {
			printerName = cfg.Name
		} else {
			printerName = fmt.Sprintf("Принтер #%d", *printerID)
		}
	}

	entry := models.LogEntry{
		Time:    time.Now().Format("15:04:05"),
		Printer: printerName,
		Event:   event,
	}
	pm.logs = append([]models.LogEntry{entry}, pm.logs...)
	if len(pm.logs) > 50 {
		pm.logs = pm.logs[:50]
	}

	if store != nil {
		go func() {
			_ = store.SaveEventLog(lineID, printerID, eventType, event)
		}()
	}
}

func (pm *PrinterManager) GetPrinterState(id int) models.PrinterState {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return pm.states[id]
}

func (pm *PrinterManager) UpdatePrinterDeltaState(id int, template, staticHash string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	state := pm.states[id]
	state.LastTemplate = template
	state.LastStaticHash = staticHash
	pm.states[id] = state
}

func PrepareDynamicPipeline(dynamicFieldName string, staticFields map[string]string, czCode string) (string, string) {
	var keys []string
	for k := range staticFields {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	fieldNames := []string{dynamicFieldName}
	for _, k := range keys {
		fieldNames = append(fieldNames, k)
	}
	compositeFields := strings.Join(fieldNames, ";")

	values := []string{czCode}
	for _, k := range keys {
		cleanVal := strings.ReplaceAll(staticFields[k], "|", "")
		values = append(values, cleanVal)
	}
	compositePayload := strings.Join(values, "|")

	return compositeFields, compositePayload
}

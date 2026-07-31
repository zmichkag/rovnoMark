package core

import (
	"context"
	"fmt"
	"log"
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
	GetTemplates() ([]string, error)
	GetTemplateFields(templateName string) ([]string, error)
	GetRemainingRibbon() (string, error)
	GetQueueCapacity(queueName string) (string, error)
	GetPrintSpeed() (string, error)
	GetCurrentPrintCount() (string, error)
	GetCurrentTemplate() (string, error)
	ClearQueue() error                // Очистка очереди (команда CQI)
	GetBufferFreeSpace() (int, error) // Сколько кодов еще можно дослать
	UpdateStaticFields(fields map[string]string) error
	InitSession(fieldName string, maxQueue int, staticFields map[string]string) error
	SelectTemplate(template string, fields map[string]string) error
}

// TaskProcessor управляет фоновыми потоками отправки данных в маркираторы
type TaskProcessor struct {
	Store       *storage.Store
	Manager     *PrinterManager
	activeMu    sync.Mutex
	activeTasks map[int]bool // Реестр активных задач, чтобы не плодить дублирующие горутины
}

// StartPumping инициализирует и запускает правильный тип насоса под конкретное железо линии
func (tp *TaskProcessor) StartPumping(lineID int, taskID int) {
	tp.activeMu.Lock()
	if tp.activeTasks == nil {
		tp.activeTasks = make(map[int]bool)
	}

	if tp.activeTasks[taskID] {
		tp.activeMu.Unlock()
		slog.Debug("Pumper: Насос для этой задачи уже работает, дублирование проигнорировано", "task_id", taskID)
		return
	}

	tp.activeTasks[taskID] = true
	tp.activeMu.Unlock()

	// 1. Получаем список принтеров, привязанных к линии
	printers, err := tp.Store.GetPrintersByLine(lineID)
	if err != nil || len(printers) == 0 {
		slog.Error("Pumper: Не найдены принтеры для линии", "line_id", lineID, "err", err)
		tp.stopTaskTracking(taskID)
		return
	}

	pCfg := printers[0]
	pPrinter := tp.Manager.GetPrinter(pCfg.ID)

	if pPrinter == nil {
		slog.Error("Pumper: Принтер не найден в реестре менеджера", "printer_id", pCfg.ID)
		tp.stopTaskTracking(taskID)
		return
	}

	ctx := context.Background()

	// 2. ВЕТВЛЕНИЕ СТРАТЕГИЙ ПОДКАЧКИ
	if pCfg.DriverType == "valentine_nice" {
		if vDriver, ok := pPrinter.(*valentine.NiceLabelDriver); ok {
			slog.Info("Pumper: Запуск реактивного насоса Valentin Fast Loop", "line_id", lineID, "task_id", taskID)
			go tp.RunValentinFastPumper(ctx, lineID, taskID, vDriver)
			return
		}
	}

	// Для всех остальных типов (Videojet, Savema, TSC) запускаем штатный пачечный насос
	slog.Info("Pumper: Запуск штатного пачечного насоса (Default)", "line_id", lineID, "task_id", taskID)
	go tp.RunDefaultPumper(ctx, lineID, taskID, pPrinter)
}

// RunValentinFastPumper — реактивный насос 1-в-1 (Отправка строго по сдвигу физического счетчика)
func (tp *TaskProcessor) RunValentinFastPumper(ctx context.Context, lineID, taskID int, vDriver *valentine.NiceLabelDriver) {
	defer tp.stopTaskTracking(taskID)
	slog.Info("VALENTIN-PUMPER: Запущен реактивный насос (Синхронизация по железу)", "line_id", lineID, "task_id", taskID)

	// Опрашиваем железо каждые 150 мс
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()

	lastPrintedCount := -1

	for {
		select {
		case <-ctx.Done():
			slog.Info("VALENTIN-PUMPER: Насос остановлен", "task_id", taskID)
			return

		case <-ticker.C:
			status, err := tp.Store.GetTaskStatus(taskID)
			if err != nil || status == "stopped" || status == "completed" {
				return
			}

			// Читаем очищенный от мусора счетчик
			countStr, err := vDriver.GetCurrentPrintCount()
			if err != nil {
				continue
			}
			currentCount, _ := strconv.Atoi(countStr)

			// 1. Первичная зарядка при старте конвейера
			if lastPrintedCount == -1 {
				lastPrintedCount = currentCount
				_ = tp.pushSingleValentinCode(taskID, vDriver)
				continue
			}

			// 2. РЕАКТИВНЫЙ ТРИГГЕР: Если принтер физически отпечатал этикетку
			if currentCount > lastPrintedCount {
				slog.Info("VALENTIN-PUMPER: Зафиксирован сход этикетки, заряжаем следующий код", "total", currentCount)

				lastPrintedCount = currentCount

				// Плюем следующий код и помечаем его
				if err := tp.pushSingleValentinCode(taskID, vDriver); err != nil {
					slog.Error("VALENTIN-PUMPER: Ошибка подкачки КМ", "err", err)
				}
			}
		}
	}
}

// pushSingleValentinCode отправляет код и помечает его как printed
func (tp *TaskProcessor) pushSingleValentinCode(taskID int, vDriver *valentine.NiceLabelDriver) error {
	codes, err := tp.Store.GetPendingCodes(taskID, 1)
	if err != nil || len(codes) == 0 {
		return nil
	}

	codeObj := codes[0]
	cleanCode := strings.TrimSpace(codeObj.Code)
	if idx := strings.Index(cleanCode, "|"); idx != -1 {
		cleanCode = cleanCode[:idx]
	}

	// Отправляем такт в принтер: FD r0 -> BM[20] -> FD r1 -> FBC
	_, err = vDriver.PrintBatchIndexed("20", codeObj.ID, []string{cleanCode})
	if err != nil {
		_ = tp.Store.UpdateCodeStatusByID(codeObj.ID, "pending", 0)
		return fmt.Errorf("сбой отправки: %w", err)
	}

	// Фиксируем в БД
	if err := tp.Store.UpdateCodeStatusByID(codeObj.ID, "printed", codeObj.ID); err != nil {
		slog.Error("VALENTIN-PUMPER: Ошибка записи printed в БД", "code_id", codeObj.ID)
	}

	return nil
}

// RunDefaultPumper — стандартный пачечный насос для Videojet / Savema / TSC
func (tp *TaskProcessor) RunDefaultPumper(ctx context.Context, lineID, taskID int, p Printer) {
	defer tp.stopTaskTracking(taskID)
	slog.Info("DEFAULT-PUMPER: Запущен пачечный цикл подкачки", "line_id", lineID, "task_id", taskID)

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("DEFAULT-PUMPER: Остановлен по контексту", "task_id", taskID)
			return
		case <-ticker.C:
			status, err := tp.Store.GetTaskStatus(taskID)
			if err != nil || status == "stopped" || status == "completed" {
				slog.Info("DEFAULT-PUMPER: Задача завершена или остановлена", "task_id", taskID, "status", status)
				return
			}

			printers, err := tp.Store.GetPrintersByLine(lineID)
			if err != nil || len(printers) == 0 {
				continue
			}
			pCfg := printers[0]

			freeSpace, err := p.GetBufferFreeSpace()
			if err != nil || freeSpace <= 0 {
				continue
			}

			batchSize := freeSpace
			if batchSize > 50 {
				batchSize = 50
			}

			codes, err := tp.Store.FetchAndAssignCodes(taskID, pCfg.ID, batchSize)
			if err != nil || len(codes) == 0 {
				continue
			}

			var payload []string
			for _, c := range codes {
				payload = append(payload, c.Code)
			}

			startIndex := codes[0].PrinterIndex
			_, err = p.PrintBatchIndexed("20", startIndex, payload)
			if err != nil {
				slog.Error("DEFAULT-PUMPER: Ошибка отправки пачки в принтер", "printer", pCfg.Name, "err", err)
			} else {
				slog.Info("DEFAULT-PUMPER: Пачка успешно загружена в принтер", "printer", pCfg.Name, "count", len(payload), "start_idx", startIndex)
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

	pm.addLogNoLock(strconv.Itoa(config.ID), fmt.Sprintf("Принтер добавлен (%s)", config.IP))
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
				err := store.SaveTelemetry(id, state.CurCount, state.Ribbon, state.Status, state.CurTemplate)
				if err != nil {
					log.Printf("[STATS] Ошибка записи для принтера %d: %v", id, err)
				}
			}
		}
	}()
}

// BackgroundPoller ведет фоновый опрос железа с изоляцией реактивных принтеров
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

				// ==================== ЖИВАЯ СИНХРОНИЗАЦИЯ ПЕЧАТИ ====================
				// ИЗОЛЯЦИЯ VALENTIN: Если работает Valentin, его синхронизирует FastPumper.
				// Поллер сюда не лезет, чтобы не создавать Race Condition в БД и сокете!
				if cfg.DriverType != "valentine_nice" && lineMap != nil {
					if lineID, ok := lineMap[id]; ok {
						activeTaskID, errTask := store.GetActiveTaskByLine(lineID)
						if errTask == nil && activeTaskID > 0 {
							lastPrintedIdx, errIdx := p.GetLastPrintedIndex()
							if errIdx == nil && lastPrintedIdx >= 0 {
								affected, errMark := store.MarkAsPrinted(activeTaskID, lastPrintedIdx)
								if errMark == nil && affected > 0 {
									slog.Info("[POLLER-SYNC] Коды успешно подтверждены печатью",
										"printer", cfg.Name,
										"task_id", activeTaskID,
										"last_index", lastPrintedIdx,
										"confirmed_now", affected,
									)
								}
							}
						}
					}
				}
				// ====================================================================
			}

			pm.mu.Lock()
			oldState := pm.states[id]

			if oldState.CurTemplate != "" && oldState.CurTemplate != curTemplate && curTemplate != "N/A" {
				pm.addLogNoLock(strconv.Itoa(id), fmt.Sprintf("СМЕНА МАКЕТА: %s -> %s", oldState.CurTemplate, curTemplate))
			}

			newState := models.PrinterState{
				LastTemplate:   oldState.LastTemplate,
				LastStaticHash: oldState.LastStaticHash,
			}

			isOfflineNow := err != nil
			wasOffline := strings.Contains(oldState.Status, "ОФФЛАЙН") || oldState.Status == "INITIALIZING"

			printerIDStr := strconv.Itoa(id)

			if isOfflineNow && !wasOffline {
				pm.addLogNoLock(printerIDStr, fmt.Sprintf("ПОТЕРЯ СВЯЗИ: %v", err))
				newState.Status = fmt.Sprintf("ОФФЛАЙН: %v", err)
				newState.Ribbon = "N/A"
				newState.Queue = "N/A"
				newState.Speed = "N/A"
				newState.CurCount = "N/A"
				newState.CurTemplate = "N/A"
			} else if !isOfflineNow && wasOffline && oldState.Status != "INITIALIZING" {
				pm.addLogNoLock(printerIDStr, "Связь восстановлена. Статус: "+status)
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
		time.Sleep(5 * time.Second)
	}
}

func (pm *PrinterManager) addLogNoLock(printer, event string) {
	entry := models.LogEntry{
		Time:    time.Now().Format("15:04:05"),
		Printer: printer,
		Event:   event,
	}
	pm.logs = append([]models.LogEntry{entry}, pm.logs...)
	if len(pm.logs) > 50 {
		pm.logs = pm.logs[:50]
	}
}

func (pm *PrinterManager) addLog(printer, event string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.addLogNoLock(printer, event)
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

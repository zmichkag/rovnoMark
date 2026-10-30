package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"rovnoMark/internal/drivers/bizerba"
	"rovnoMark/internal/drivers/savema"
	"rovnoMark/internal/drivers/valentine"
	"rovnoMark/internal/models"
	"rovnoMark/internal/storage"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

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

func (pm *PrinterManager) GetPrinterConfig(id int) (models.PrinterConfig, bool) {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	cfg, ok := pm.configs[id]
	return cfg, ok
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
	hasSpecializedDriver := false

	for _, pCfg := range printers {
		switch pCfg.DriverType {
		case "bizerba":
			hasSpecializedDriver = true
			pPrinter := tp.Manager.GetPrinter(pCfg.ID)
			if bDriver, ok := pPrinter.(*bizerba.Driver); ok {
				slog.Info("Pumper: Запуск Bizerba Fast Loop", "printer", pCfg.Name, "printer_id", pCfg.ID)
				go tp.RunBizerbaFastPumper(ctx, lineID, taskID, pCfg.ID, bDriver)
			}

		case "savema":
			hasSpecializedDriver = true
			pPrinter := tp.Manager.GetPrinter(pCfg.ID)
			if sDriver, ok := pPrinter.(*savema.Driver); ok {
				slog.Info("Pumper: Запуск пакетного цикла Savema (CSV)", "printer", pCfg.Name, "printer_id", pCfg.ID)
				go tp.runSavemaBatchPumper(ctx, lineID, taskID, pCfg.ID, sDriver)
			}

		case "valentine_nice", "valentin", "valentine":
			hasSpecializedDriver = true
			pPrinter := tp.Manager.GetPrinter(pCfg.ID)
			if vDriver, ok := pPrinter.(*valentine.NiceLabelDriver); ok {
				limit := pCfg.GetEffectiveBufferLimit()
				lead := pCfg.GetEffectiveLeadLoop()
				mode := pCfg.GetEffectivePumperMode()

				slog.Info("Pumper: Запуск Valentin",
					"mode", mode,
					"printer", pCfg.Name,
					"buffer_limit", limit,
					"lead_loop", lead,
				)

				if mode == models.ModeRibbonBuffer {
					go tp.pumpValentinRibbonBuffer(ctx, lineID, taskID, pCfg.ID, pCfg.Role, vDriver, limit, lead)
				} else {
					go tp.pumpValentinFastLoop(ctx, lineID, taskID, pCfg.ID, pCfg.Role, vDriver, limit, lead)
				}
			}
		}
	}

	if hasSpecializedDriver {
		return
	}

	slog.Info("Pumper: Запуск стандартного пачечного насоса", "line_id", lineID, "task_id", taskID)
	go tp.RunDefaultPumper(ctx, lineID, taskID)
}

// ----------------------------------------------------------------------------
// РЕЖИМ 1: Классический реактивный (Fast Single) - строго 1 шт под датчик
// ----------------------------------------------------------------------------
// internal/core/manager.go

func (tp *TaskProcessor) pumpValentinFastLoop(
	ctx context.Context,
	lineID, taskID, printerID int,
	role string,
	vDriver *valentine.NiceLabelDriver,
	bufferLimit, leadLoop int,
) {
	defer tp.stopTaskTracking(taskID)
	slog.Info("VALENTIN-FAST-SINGLE: Запуск цикла строго под фотодатчик", "task_id", taskID, "printer_id", printerID)

	// Проверяем, сколько принтеров реально привязано к этой линии
	printersOnLine, err := tp.Store.GetPrintersByLine(lineID)
	isSinglePrinter := err == nil && len(printersOnLine) <= 1

	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	// Первичный код взводится строго 1 раз
	if err := tp.pushSingleValentinCode(taskID, printerID, role, vDriver, false, isSinglePrinter); err != nil {
		slog.Error("VALENTIN-FAST-SINGLE: Ошибка первичного взвода", "err", err)
		return
	}

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
			currentCount, convErr := strconv.Atoi(countStr)
			if convErr != nil {
				continue
			}

			if lastPrintedCount == -1 {
				lastPrintedCount = currentCount
				continue
			}

			if currentCount > lastPrintedCount {
				delta := currentCount - lastPrintedCount
				lastPrintedCount = currentCount

				for i := 0; i < delta; i++ {
					if err := tp.pushSingleValentinCode(taskID, printerID, role, vDriver, false, isSinglePrinter); err != nil {
						slog.Error("VALENTIN-FAST-SINGLE: Сбой дозарядки", "err", err)
						break
					}
					time.Sleep(5 * time.Millisecond)
				}
			}
		}
	}
}

// ----------------------------------------------------------------------------
// РЕЖИМ 2: Буфер на ленте (Ribbon Buffer) - программный тираж вперед
// ----------------------------------------------------------------------------
func (tp *TaskProcessor) pumpValentinRibbonBuffer(
	ctx context.Context,
	lineID, taskID, printerID int,
	role string,
	vDriver *valentine.NiceLabelDriver,
	bufferLimit, leadLoop int,
) {
	defer tp.stopTaskTracking(taskID)
	slog.Info("VALENTIN-RIBBON-BUFFER: Старт петли упреждения",
		"task_id", taskID,
		"buffer_limit", bufferLimit,
		"lead_loop", leadLoop,
	)

	// Проверяем количество активных принтеров на линии
	printersOnLine, err := tp.Store.GetPrintersByLine(lineID)
	isSinglePrinter := err == nil && len(printersOnLine) <= 1

	// ФАЗА 1: Стартовая накачка физической петли кодов на ленте (hostDriven = true)
	for i := 0; i < leadLoop; i++ {
		// ДОБАВЛЕН АРГУМЕНТ isSinglePrinter (строка 206)
		if err := tp.pushSingleValentinCode(taskID, printerID, role, vDriver, true, isSinglePrinter); err != nil {
			slog.Error("VALENTIN-RIBBON-BUFFER: Ошибка стартовой накачки петли", "err", err)
			break
		}
		time.Sleep(30 * time.Millisecond)
	}

	ticker := time.NewTicker(30 * time.Millisecond)
	defer ticker.Stop()

	lastPrintedCount := -1

	// ФАЗА 2: Цикл восполнения петли по одометру FBBC
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
			currentCount, convErr := strconv.Atoi(countStr)
			if convErr != nil {
				continue
			}

			if lastPrintedCount == -1 {
				lastPrintedCount = currentCount
				continue
			}

			if currentCount > lastPrintedCount {
				delta := currentCount - lastPrintedCount
				lastPrintedCount = currentCount

				// Аппликатор нанес этикетки: допечатываем ровно delta штук, сохраняя петлю
				for i := 0; i < delta; i++ {
					// ДОБАВЛЕН АРГУМЕНТ isSinglePrinter (строка 250)
					if err := tp.pushSingleValentinCode(taskID, printerID, role, vDriver, true, isSinglePrinter); err != nil {
						slog.Error("VALENTIN-RIBBON-BUFFER: Сбой допечатки в петлю", "err", err)
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
			}
		}
	}
}

// pushSingleValentinCode отправляет код с явным указанием hostDriven режима
func (tp *TaskProcessor) pushSingleValentinCode(
	taskID, printerID int,
	role string,
	vDriver *valentine.NiceLabelDriver,
	hostDriven bool,
	isSinglePrinter bool,
) error {
	var codes []models.TaskCode
	var err error

	// Если на линии всего один принтер — забираем ВСЕ коды подряд (без деления на чет/нечет)
	if isSinglePrinter || strings.ToUpper(strings.TrimSpace(role)) == "SINGLE" || role == "" {
		codes, err = tp.Store.FetchAndAssignCodes(taskID, printerID, 1)
	} else {
		codes, err = tp.Store.FetchAndAssignCodesAlternating(taskID, printerID, role, 1)
	}

	if err != nil {
		return fmt.Errorf("ошибка выборки кодов из БД: %w", err)
	}
	if len(codes) == 0 {
		return nil
	}

	codeObj := codes[0]
	cleanCode := strings.TrimSpace(codeObj.Code)
	if idx := strings.Index(cleanCode, "|"); idx != -1 {
		cleanCode = cleanCode[:idx]
	}

	_, err = vDriver.PrintBatchIndexedMode("20", codeObj.PrinterIndex, []string{cleanCode}, hostDriven)
	if err != nil {
		return fmt.Errorf("сбой отправки КМ в Valentin: %w", err)
	}

	_ = tp.Store.UpdateCodeStatusByID(codeObj.ID, "printed", codeObj.PrinterIndex)
	return nil
}

func (tp *TaskProcessor) runSavemaBatchPumper(ctx context.Context, lineID, taskID, printerID int, sDriver *savema.Driver) {
	defer tp.stopTaskTracking(taskID)

	for {
		status, err := tp.Store.GetTaskStatus(taskID)
		if err != nil || status == "stopped" || status == "completed" {
			_ = sDriver.ClearQueue()
			return
		}

		// Savema работает файлами ограниченного размера. Берем очередную часть задания,
		// а после ее завершения продолжаем со следующими кодами.
		fetchStarted := time.Now()
		codes, err := tp.Store.FetchAndAssignCodes(taskID, printerID, 1000)
		if err != nil || len(codes) > 0 {
			slog.Debug("SAVEMA-PUMPER TIMING", "stage", "fetch_and_assign", "task_id", taskID, "printer_id", printerID, "codes", len(codes), "elapsed_ms", time.Since(fetchStarted).Milliseconds(), "err", err)
		}
		if err != nil {
			slog.Error("SAVEMA-PUMPER: Ошибка выборки кодов", "task_id", taskID, "err", err)
			if !waitForSavemaPumper(ctx, time.Second) {
				_ = sDriver.ClearQueue()
				return
			}
			continue
		}
		if len(codes) == 0 {
			// Задание остается активным: 1С может дописать следующую часть кодов позднее.
			if !waitForSavemaPumper(ctx, 500*time.Millisecond) {
				_ = sDriver.ClearQueue()
				return
			}
			continue
		}

		payload := make([]string, 0, len(codes))
		for _, c := range codes {
			payload = append(payload, c.Code)
		}

		dynamicField, _ := tp.Store.GetTaskDynamicField(taskID)
		if dynamicField == "" {
			dynamicField = "DataMatrix"
		}

		batchStartIndex := codes[0].PrinterIndex
		uploadStarted := time.Now()
		loaded, err := sDriver.PrintBatchIndexed(dynamicField, batchStartIndex, payload)
		slog.Debug("SAVEMA-PUMPER TIMING", "stage", "upload_batch", "task_id", taskID, "printer_id", printerID, "codes", len(payload), "loaded", loaded, "elapsed_ms", time.Since(uploadStarted).Milliseconds(), "err", err)
		if err != nil || loaded == 0 {
			// Коды еще не попали в принтер, поэтому возвращаем их в pending для повтора.
			_, _ = tp.Store.ReconcileAndFinalizeTaskCodes(taskID, printerID, batchStartIndex-1)
			slog.Error("SAVEMA-PUMPER: Ошибка заливки CSV в Savema", "task_id", taskID, "err", err)
			if !waitForSavemaPumper(ctx, time.Second) {
				_ = sDriver.ClearQueue()
				return
			}
			continue
		}

		ticker := time.NewTicker(500 * time.Millisecond)
		batchComplete := false
		for !batchComplete {
			select {
			case <-ctx.Done():
				ticker.Stop()
				_ = sDriver.ClearQueue()
				return
			case <-ticker.C:
				status, err := tp.Store.GetTaskStatus(taskID)
				if err != nil || status == "stopped" || status == "completed" {
					ticker.Stop()
					_ = sDriver.ClearQueue()
					return
				}

				printedInBatch, err := sDriver.GetLastPrintedIndex()
				if err != nil {
					continue
				}
				if printedInBatch > loaded {
					printedInBatch = loaded
				}
				if printedInBatch > 0 {
					lastTaskIndex := batchStartIndex + printedInBatch - 1
					_, _ = tp.Store.MarkAsPrinted(taskID, printerID, lastTaskIndex)
				}

				if printedInBatch >= loaded {
					batchComplete = true
					slog.Info("SAVEMA-PUMPER: Часть задания завершена",
						"task_id", taskID,
						"loaded", loaded,
						"last_task_index", batchStartIndex+loaded-1,
					)
				}
			}
		}
		ticker.Stop()
	}
}

func waitForSavemaPumper(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
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

				var pending []models.TaskCode
				var assignErr error

				if len(printers) == 1 {
					pending, assignErr = tp.Store.FetchAndAssignCodes(taskID, pCfg.ID, targetLoad)
				} else {
					pending, assignErr = tp.Store.FetchAndAssignCodesAlternating(taskID, pCfg.ID, pCfg.Role, targetLoad)
				}

				if assignErr != nil || len(pending) == 0 {
					continue
				}

				var compositePayloads []string
				var compositeFields string

				if pCfg.DriverType == "videojet" {
					for _, item := range pending {
						compositePayloads = append(compositePayloads, item.Code)
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
					slog.Error("Pumper: Ошибка отправки пакета", "printer", pCfg.Name, "err", err)
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

func (tp *TaskProcessor) RunBizerbaFastPumper(ctx context.Context, lineID, taskID, printerID int, bDriver *bizerba.Driver) {
	defer tp.stopTaskTracking(taskID)
	slog.Info("BIZERBA-PUMPER: Запущен реактивный буферный насос", "line_id", lineID, "printer_id", printerID)

	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	var nextPendingProbe time.Time

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			status, err := tp.Store.GetTaskStatus(taskID)
			if err != nil || status == "stopped" || status == "completed" {
				return
			}
			if time.Now().Before(nextPendingProbe) {
				continue
			}

			// Do not enter the single BCS/COM session when the whole currently known
			// task is already in the driver's memory queue. New append portions are
			// discovered with a cheap SQLite probe instead.
			available, err := tp.Store.GetPendingCodes(taskID, 1)
			if err != nil {
				slog.Error("BIZERBA-PUMPER: Ошибка проверки новых кодов", "task_id", taskID, "err", err)
				nextPendingProbe = time.Now().Add(500 * time.Millisecond)
				continue
			}
			if len(available) == 0 {
				nextPendingProbe = time.Now().Add(500 * time.Millisecond)
				continue
			}

			bufferStarted := time.Now()
			freeSpace, err := bDriver.GetBufferFreeSpace()
			if err != nil || freeSpace > 0 {
				slog.Debug("BIZERBA-PUMPER TIMING", "stage", "buffer_free_space", "task_id", taskID, "printer_id", printerID, "free_space", freeSpace, "elapsed_ms", time.Since(bufferStarted).Milliseconds(), "err", err)
			}
			if err != nil || freeSpace <= 0 {
				continue
			}
			// Stop may arrive while GetBufferFreeSpace is waiting behind a BCS write.
			// Recheck before assigning any more database rows to the printer.
			status, err = tp.Store.GetTaskStatus(taskID)
			if err != nil || status == "stopped" || status == "completed" {
				return
			}

			targetLoad := freeSpace
			// Bizerba's driver queue is in-process memory. Enqueuing a batch sends
			// only the first mark to BCS, so small chunks merely add extra round trips
			// through the single COM session without reducing device-side load.
			if targetLoad > 1000 {
				targetLoad = 1000
			}

			fetchStarted := time.Now()
			pending, err := tp.Store.FetchAndAssignCodes(taskID, printerID, targetLoad)
			if err != nil || len(pending) > 0 {
				slog.Debug("BIZERBA-PUMPER TIMING", "stage", "fetch_and_assign", "task_id", taskID, "printer_id", printerID, "requested", targetLoad, "codes", len(pending), "elapsed_ms", time.Since(fetchStarted).Milliseconds(), "err", err)
			}
			if err != nil || len(pending) == 0 {
				if err == nil {
					nextPendingProbe = time.Now().Add(500 * time.Millisecond)
				}
				continue
			}

			var codes []string
			startIndex := pending[0].PrinterIndex
			for _, item := range pending {
				cleanCode := strings.TrimSpace(item.Code)
				if idx := strings.Index(cleanCode, "|"); idx != -1 {
					cleanCode = cleanCode[:idx]
				}
				codes = append(codes, cleanCode)
			}

			enqueueStarted := time.Now()
			loaded, enqueueErr := bDriver.PrintBatchIndexed("DATAMATRIX", startIndex, codes)
			err = enqueueErr
			slog.Debug("BIZERBA-PUMPER TIMING", "stage", "enqueue_batch", "task_id", taskID, "printer_id", printerID, "codes", len(codes), "loaded", loaded, "elapsed_ms", time.Since(enqueueStarted).Milliseconds(), "err", err)
			if err != nil {
				slog.Error("BIZERBA-PUMPER: Ошибка отправки пакета в драйвер", "err", err)
				time.Sleep(200 * time.Millisecond)
			}
		}
	}
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

	if previous := pm.printers[config.ID]; previous != nil {
		if closer, ok := previous.(interface{ Close() error }); ok {
			if err := closer.Close(); err != nil {
				slog.Error("Ошибка закрытия прежнего драйвера", "printer_id", config.ID, "err", err)
			}
		}
	}

	pm.configs[config.ID] = config
	pm.printers[config.ID] = p
	pm.states[config.ID] = models.PrinterState{Status: "INITIALIZING", Ribbon: "?", Queue: "?"}

	if config.IsActive {
		if starter, ok := p.(interface{ Start() }); ok {
			starter.Start()
		}
	}

	pID := config.ID
	pm.addLogNoLock(nil, &pID, nil, "info", fmt.Sprintf("Принтер %s добавлен (%s)", config.Name, config.IP))
}

func (pm *PrinterManager) GetPrinter(id int) Printer {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return pm.printers[id]
}

func (pm *PrinterManager) CloseAll() error {
	pm.mu.RLock()
	printers := make([]Printer, 0, len(pm.printers))
	for _, printer := range pm.printers {
		printers = append(printers, printer)
	}
	pm.mu.RUnlock()

	var closeErrors []error
	for _, printer := range printers {
		if closer, ok := printer.(interface{ Close() error }); ok {
			if err := closer.Close(); err != nil {
				closeErrors = append(closeErrors, err)
			}
		}
	}
	return errors.Join(closeErrors...)
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
	var pollsInFlight sync.Map
	for {
		pm.mu.RLock()
		var ids []int
		for id := range pm.printers {
			ids = append(ids, id)
		}
		pm.mu.RUnlock()

		lineMap, _ := store.GetPrinterLineMap()

		for _, id := range ids {
			if _, alreadyPolling := pollsInFlight.LoadOrStore(id, struct{}{}); alreadyPolling {
				continue
			}
			go func(id int, lineMap map[int]int) {
				defer pollsInFlight.Delete(id)

				pm.mu.RLock()
				p := pm.printers[id]
				cfg := pm.configs[id]
				pm.mu.RUnlock()

				if p == nil || !cfg.IsActive {
					return
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

					if cfg.DriverType != "valentine_nice" && cfg.DriverType != "savema" && lineMap != nil {
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

				if err == nil && oldState.CurTemplate != "" && curTemplate != "" && oldState.CurTemplate != curTemplate && curTemplate != "N/A" {
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
			}(id, lineMap)
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

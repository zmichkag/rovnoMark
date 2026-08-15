package core

import (
	"context"
	"encoding/json"
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

	// Для всех остальных типов (Videojet, Savema, TSC, Markem) запускаем штатный пачечный насос
	slog.Info("Pumper: Запуск штатного пачечного насоса (Default)", "line_id", lineID, "task_id", taskID)
	go tp.RunDefaultPumper(ctx, lineID, taskID, pPrinter)
}

// RunValentinFastPumper — реактивный насос подкачки для Valentin с поддержкой двойного буфера
func (tp *TaskProcessor) RunValentinFastPumper(ctx context.Context, lineID, taskID int, vDriver *valentine.NiceLabelDriver) {
	defer tp.stopTaskTracking(taskID)
	slog.Info("VALENTIN-PUMPER: Запущен реактивный насос", "line_id", lineID, "task_id", taskID)

	ticker := time.NewTicker(15 * time.Millisecond)
	defer ticker.Stop()

	// 1. СТАРТОВЫЙ ВЗВОД: Заряжаем первичные 2 кода
	slog.Info("VALENTIN-PUMPER: Первичная заправка двух кодов в RAM принтера", "task_id", taskID)

	if err := tp.pushSingleValentinCode(taskID, vDriver); err != nil {
		slog.Error("VALENTIN-PUMPER: Ошибка первичной зарядки (Код 1)", "task_id", taskID, "err", err)
		return
	}
	time.Sleep(5 * time.Millisecond)

	//if err := tp.pushSingleValentinCode(taskID, vDriver); err != nil {
	//	slog.Error("VALENTIN-PUMPER: Ошибка первичной зарядки (Код 2)", "task_id", taskID, "err", err)
	//	return
	//}

	lastPrintedCount := -1

	for {
		select {
		case <-ctx.Done():
			slog.Info("VALENTIN-PUMPER: Фоновый насос остановлен", "task_id", taskID)
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
				slog.Info("VALENTIN-PUMPER: Зафиксирован сход этикетки", "delta", delta, "total_printed", currentCount)
				lastPrintedCount = currentCount

				for i := 0; i < delta; i++ {
					if err := tp.pushSingleValentinCode(taskID, vDriver); err != nil {
						slog.Error("VALENTIN-PUMPER: Сбой дозарядки буфера", "err", err)
						break
					}
					time.Sleep(5 * time.Millisecond)
				}
			}
		}
	}
}

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

	_, err = vDriver.PrintBatchIndexed("20", codeObj.ID, []string{cleanCode})
	if err != nil {
		_ = tp.Store.UpdateCodeStatusByID(codeObj.ID, "pending", 0)
		return fmt.Errorf("сбой отправки КМ в Valentin: %w", err)
	}

	if err := tp.Store.UpdateCodeStatusByID(codeObj.ID, "printed", codeObj.ID); err != nil {
		slog.Error("VALENTIN-PUMPER: Ошибка записи printed в БД", "code_id", codeObj.ID, "err", err)
	}

	return nil
}

// RunDefaultPumper — стандартный пачечный насос
func (tp *TaskProcessor) RunDefaultPumper(ctx context.Context, lineID, taskID int, p Printer) {
	defer tp.stopTaskTracking(taskID)
	slog.Info("DEFAULT-PUMPER: Запущен пачечный цикл", "line_id", lineID, "task_id", taskID)

	ticker := time.NewTicker(200 * time.Millisecond) // Уменьшили с 5с до 200мс для скорости
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("DEFAULT-PUMPER: Остановлен по контексту", "task_id", taskID)
			return
		case <-ticker.C:
			// 1. Читаем статус задачи из БД
			status, err := tp.Store.GetTaskStatus(taskID)
			if err != nil || status == "stopped" || status == "completed" {
				return
			}

			// ⛔️ ШЛЮЗ БЛОКИРОВКИ 1С:
			// Если статус задачи 'ready' (инициализация в процессе), а НЕ 'active' — ждем!
			if status == "ready" {
				slog.Debug("PUMPER: Ожидание завершения стартовой сессии принтера...", "task_id", taskID)
				continue
			}

			printers, err := tp.Store.GetPrintersByLine(lineID)
			if err != nil || len(printers) == 0 {
				continue
			}

			for _, pCfg := range printers {

				pPrinter := tp.Manager.GetPrinter(pCfg.ID)
				if pPrinter == nil {
					slog.Warn("Pumper: Принтер привязан к линии, но отсутствует в менеджере (оффлайн)", "printer", pCfg.Name)
					continue
				}

				// 1. Устанавливаем желаемую планку буфера
				maxBuffer := 50

				// 2. Узнаем, сколько свободных слотов осталось до лимита
				freeSpace, err := pPrinter.GetBufferFreeSpace()
				if err != nil {
					slog.Warn("Pumper: Не удалось получить свободное место в буфере принтера", "printer", pCfg.Name, "err", err)
					continue
				}

				targetLoad := freeSpace
				if targetLoad > maxBuffer {
					targetLoad = maxBuffer
				}

				if targetLoad <= 0 {
					continue // Буфер полон
				}

				// 3. Забираем коды из БД в переменную pending!
				pending, err := tp.Store.FetchAndAssignCodes(taskID, pCfg.ID, targetLoad)
				if err != nil {
					slog.Error("Pumper: Ошибка БД при выборке кодов", "task_id", taskID, "err", err)
					continue
				}

				if len(pending) == 0 {
					continue // Ждем загрузки новых кодов из 1С
				}

				// 4. Подготовка данных с учетом специфики драйвера
				var compositePayloads []string
				var compositeFields string

				if pCfg.DriverType == "videojet" {
					// Достаем имена и значения статических полей задачи
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
					// Принтеры которы нужно только ЧЗ
					compositeFields, _ = tp.Store.GetTaskDynamicField(taskID)
					if compositeFields == "" {
						compositeFields = "DATAMATRIX" // Фолбэк
					}

					for _, item := range pending {
						compositePayloads = append(compositePayloads, item.Code)
					}
				}

				startIndex := pending[0].PrinterIndex

				// Информационный лог
				slog.Info("Pumper: Направляем пачку кодов в принтер",
					"printer", pCfg.Name,
					"count", len(compositePayloads),
					"start_index", startIndex,
				)

				// 5. Отправка в принтер
				loaded, err := pPrinter.PrintBatchIndexed(compositeFields, startIndex, compositePayloads)
				if err == nil && loaded > 0 {
					slog.Info("Pumper: Пачка успешно загружена в память устройства", "printer", pCfg.Name, "loaded_count", loaded)
				} else if err != nil {
					slog.Error("Pumper: Критическая ошибка отправки SID пакета в сокет", "printer", pCfg.Name, "err", err)
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
				err := store.SaveTelemetry(id, state.CurCount, state.Ribbon, state.Status, state.CurTemplate)
				if err != nil {
					log.Printf("[STATS] Ошибка записи для принтера %d: %v", id, err)
				}
			}
		}
	}()
}

// BackgroundPoller опрашивает железки и сохраняет логи как в RAM, так и в БД
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

				// Синхронизация печати
				if cfg.DriverType != "valentine_nice" && lineMap != nil {
					if lineID, ok := lineMap[id]; ok {
						activeTaskID, errTask := store.GetActiveTaskByLine(lineID)
						if errTask == nil && activeTaskID > 0 {
							lastPrintedIdx, errIdx := p.GetLastPrintedIndex()
							if errIdx == nil && lastPrintedIdx >= 0 {
								affected, errMark := store.MarkAsPrinted(activeTaskID, lastPrintedIdx)
								if errMark == nil && affected > 0 {
									slog.Info("[POLLER-SYNC] Коды подтверждены печатью",
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
		time.Sleep(5 * time.Second)
	}
}

// addLogNoLock универсальный метод для записи логов в RAM и в БД SQLite
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

	// Если передан Store, пишем также в базу данных SQLite
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

package core

import (
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"rovnoMark/internal/models"
	"rovnoMark/internal/storage"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Printer - расширенный контракт для железа
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

// TaskProcessor Добавляем возможность управления задачами
type TaskProcessor struct {
	Store       *storage.Store
	Manager     *PrinterManager
	activeMu    sync.Mutex
	activeTasks map[int]bool // Тут храним ID задач, у которых насос УЖЕ крутится
}

func (tp *TaskProcessor) StartPumping(lineID int, taskID int) {
	tp.activeMu.Lock()
	if tp.activeTasks == nil {
		tp.activeTasks = make(map[int]bool)
	}
	// Если насос для этой задачи уже запущен — тихо выходим, не плодим горутины!
	if tp.activeTasks[taskID] {
		tp.activeMu.Unlock()
		slog.Debug("Pumper: Насос для этой задачи уже работает, дублирование проигнорировано", "task_id", taskID)
		return
	}
	// Регистрируем запуск
	tp.activeTasks[taskID] = true
	tp.activeMu.Unlock()

	// Достаем имя динамического поля для этой задачи ИЗ БАЗЫ
	dynamicField, err := tp.Store.GetTaskDynamicField(taskID)
	if err != nil || dynamicField == "" {
		slog.Error("Накачка отменена: не найдено или пустое динамическое поле", "task_id", taskID)

		tp.activeMu.Lock()
		delete(tp.activeTasks, taskID) // Снимаем регистрацию при ошибке
		tp.activeMu.Unlock()
		return
	}

	go func() {
		slog.Info("=== [PUMPER-RUN] Внутри горутины, проверяем запуск ===", "task_id", taskID)

		defer func() {
			tp.activeMu.Lock()
			delete(tp.activeTasks, taskID)
			tp.activeMu.Unlock()
		}()

		// Очищаем регистрацию, когда горутина завершит работу (при stop)
		defer func() {
			tp.activeMu.Lock()
			delete(tp.activeTasks, taskID)
			tp.activeMu.Unlock()
		}()

		slog.Info("=== [PUMPER] Насос кодов успешно запущен в фоне ===",
			"task_id", taskID,
			"line_id", lineID,
			"dynamic_field", dynamicField,
		)

		for {
			// 1. Проверяем, не остановлена ли задача (Graceful exit из горутины)
			status, err := tp.Store.GetTaskStatus(taskID)
			if err != nil || (status != "active" && status != "ready") {
				slog.Info("=== [PUMPER] Выключаем насос ===", "task_id", taskID, "final_status", status)
				return
			}

			// 2. Получаем активные принтеры на линии
			printers, err := tp.Store.GetPrintersByLine(lineID)
			if err != nil {
				slog.Error("Pumper: Ошибка получения принтеров из БД", "line_id", lineID, "err", err)
				time.Sleep(2 * time.Second)
				continue
			}

			for _, pCfg := range printers {
				p := tp.Manager.GetPrinter(pCfg.ID)
				if p == nil {
					slog.Warn("Pumper: Принтер привязан к линии, но отсутствует в менеджере (оффлайн)", "printer", pCfg.Name)
					continue
				}

				// 1. Устанавливаем желаемую планку буфера (для промышленной стабильности ставим 50-100)
				maxBuffer := 50

				// 2. Узнаем, сколько свободных слотов осталось до лимита
				freeSpace, err := p.GetBufferFreeSpace()
				if err != nil {
					slog.Warn("Pumper: Не удалось получить свободное место в буфере принтера", "printer", pCfg.Name, "err", err)
					continue
				}

				targetLoad := freeSpace
				if targetLoad > maxBuffer {
					targetLoad = maxBuffer
				}

				slog.Debug("Pumper: мониторинг буфера",
					"printer", pCfg.Name,
					"in_printer", maxBuffer-freeSpace,
					"free_space", freeSpace,
				)

				// 3. Если место есть — забираем коды из базы
				if targetLoad <= 0 {
					continue // Буфер полон
				}

				pending, err := tp.Store.FetchAndAssignCodes(taskID, pCfg.ID, targetLoad)
				if err != nil {
					slog.Error("Pumper: Ошибка БД при выборке кодов", "task_id", taskID, "err", err)
					continue
				}

				if len(pending) == 0 {
					slog.Debug("Pumper: Новые коды в базе данных отсутствуют (ожидание аппенда)", "task_id", taskID)
					continue
				}

				// 4. Достаем сохраненные статические поля задачи из БД
				staticJSONStr, errFields := tp.Store.GetTaskStaticFieldsJSON(taskID)
				var staticFields map[string]string
				if errFields == nil && staticJSONStr != "" {
					_ = json.Unmarshal([]byte(staticJSONStr), &staticFields)
				} else {
					slog.Warn("Pumper: Статические поля не найдены в БД для задачи", "task_id", taskID, "err", errFields)
				}

				// 5. Подготовка пачки composite-данных для SID
				var compositePayloads []string
				var compositeFields string

				for _, item := range pending {
					// PrepareDynamicPipeline возвращает:
					// compositeFields: "dm_data0;date01;date02"
					// payload: "0104600840...|20.10.2026|20.10.3026"
					fields, payload := PrepareDynamicPipeline(dynamicField, staticFields, item.Code)
					compositeFields = fields // Поля одинаковы для всей пачки, просто сохраняем последнее
					compositePayloads = append(compositePayloads, payload)
				}

				startIndex := pending[0].PrinterIndex

				// Информационный лог перед отправкой в сокет
				slog.Info("Pumper: Направляем пачку кодов и статики в принтер",
					"printer", pCfg.Name,
					"count", len(compositePayloads),
					"start_index", startIndex,
				)

				// 6. Отправка в принтер составной строки полей и полезной нагрузки
				loaded, err := p.PrintBatchIndexed(compositeFields, startIndex, compositePayloads)

				if err == nil && loaded > 0 {
					slog.Info("Pumper: Пачка успешно загружена в память устройства", "printer", pCfg.Name, "loaded_count", loaded)
				} else if err != nil {
					slog.Error("Pumper: Критическая ошибка отправки SID пакета в сокет", "printer", pCfg.Name, "err", err)
				}
			}

			time.Sleep(5 * time.Second)
		}
	}()
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

// StartTelemetryCollector запускает фоновый процесс сбора статистики
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

func (pm *PrinterManager) BackgroundPoller(store *storage.Store) {
	slog.Info("ПОЛЛЕР ПРОСНУЛСЯ")
	for {
		pm.mu.RLock()
		var ids []int
		for id := range pm.printers {
			ids = append(ids, id)
		}
		pm.mu.RUnlock()

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
				// Обращаемся напрямую к store
				lineMap, errMap := store.GetPrinterLineMap()
				if errMap == nil {
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

// GetPrinterState возвращает копию текущего состояния принтера
func (pm *PrinterManager) GetPrinterState(id int) models.PrinterState {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return pm.states[id]
}

// UpdatePrinterDeltaState сохраняет последние успешно отправленные параметры
// Это нужно, чтобы алгоритм Delta понимал, что данные в принтере уже обновлены
func (pm *PrinterManager) UpdatePrinterDeltaState(id int, template, staticHash string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	state := pm.states[id]
	state.LastTemplate = template
	state.LastStaticHash = staticHash
	pm.states[id] = state
}

func PrepareDynamicPipeline(dynamicFieldName string, staticFields map[string]string, czCode string) (string, string) {
	// 1. Сортируем ключи статики по алфавиту для 100% стабильного порядка полей
	var keys []string
	for k := range staticFields {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// 2. Собираем строку имен полей для InitSession (SHO) -> "dm_data0;date01;date02;text01"
	fieldNames := []string{dynamicFieldName}
	for _, k := range keys {
		fieldNames = append(fieldNames, k)
	}
	compositeFields := strings.Join(fieldNames, ";")

	// 3. Собираем строку значений для конкретной записи (SID) -> "01046...|20.10.2026|20.10.3026"
	values := []string{czCode}
	for _, k := range keys {
		cleanVal := strings.ReplaceAll(staticFields[k], "|", "") // Экранируем разделитель протокола
		values = append(values, cleanVal)
	}
	compositePayload := strings.Join(values, "|")

	return compositeFields, compositePayload
}

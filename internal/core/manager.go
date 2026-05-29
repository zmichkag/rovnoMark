package core

import (
	"fmt"
	"log"
	"log/slog"
	"rovnoMark/internal/models"
	"rovnoMark/internal/storage"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Printer - расширенный контракт для железа
type Printer interface {
	GetStatus() (string, error)
	PrintTemplate(template string, fields map[string]string) error
	PrintBatch(fieldName string, codes []string) (int, error)
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
	InitSession(fieldName string, maxQueue int) error
	SelectTemplate(template string, fields map[string]string) error
}

// TaskProcessor Добавляем возможность управления задачами
type TaskProcessor struct {
	Store   *storage.Store
	Manager *PrinterManager
}

func (tp *TaskProcessor) StartPumping(lineID int, taskID int) {
	// Достаем имя динамического поля для этой задачи ИЗ БАЗЫ
	dynamicField, err := tp.Store.GetTaskDynamicField(taskID)
	if err != nil || dynamicField == "" {
		slog.Error("Накачка отменена: не найдено или пустое динамическое поле", "task_id", taskID)
		return
	}

	// Запускаем асинхронный насос
	go func() {
		// Четкий информационный лог о фактическом старте горутины
		slog.Info("=== [PUMPER] Насос кодов успешно запущен в фоне ===",
			"task_id", taskID,
			"line_id", lineID,
			"dynamic_field", dynamicField,
		)

		for {
			// 1. Проверяем, не остановлена ли задача (Graceful exit из горутины)
			status, err := tp.Store.GetTaskStatus(taskID)
			if err != nil || status != "active" {
				slog.Info("=== [PUMPER] Работа завершена или принудительно остановлена, выключаем насос ===",
					"task_id", taskID,
					"final_status", status,
				)
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

				// 1. Устанавливаем желаемую планку буфера
				maxBuffer := 10

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

				// Меняем уровень лога с Info на Debug, чтобы не спамить каждые 5 секунд пустой информацией,
				// но если место ЕСТЬ и мы РЕАЛЬНО что-то добавляем — выведем как Info ниже.
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
					// Лог о том, что база пуста, тоже переводим в Debug, чтобы не засорять экран
					slog.Debug("Pumper: Новые коды в базе данных отсутствуют (ожидание аппенда)", "task_id", taskID)
					continue
				}

				// 4. Подготовка пачки
				var codesOnly []string
				for _, item := range pending {
					codesOnly = append(codesOnly, item.Code)
				}

				startIndex := pending[0].PrinterIndex

				// Информационный лог перед отправкой в сокет
				slog.Info("Pumper: Направляем пачку кодов в принтер",
					"printer", pCfg.Name,
					"count", len(codesOnly),
					"start_index", startIndex,
				)

				// 5. Отправка в принтер
				loaded, err := p.PrintBatchIndexed(dynamicField, startIndex, codesOnly)

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

func (pm *PrinterManager) BackgroundPoller() {
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

				// ИСПРАВЛЕНИЕ: Безопасное получение очереди без хардкода поля
				free, errSpace := p.GetBufferFreeSpace()
				if errSpace == nil {
					queue = strconv.Itoa(free)
				} else {
					queue = "N/A"
				}

				speed, _ = p.GetPrintSpeed()
				curCount, _ = p.GetCurrentPrintCount()
				curTemplate, _ = p.GetCurrentTemplate()
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

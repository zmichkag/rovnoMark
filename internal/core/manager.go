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

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// Метрики оборудования
	printerRibbon = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "rovno_printer_ribbon_remaining",
		Help: "Остаток риббона в принтере (числовое значение)",
	}, []string{"printer_id", "printer_name"})

	printerSpeed = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "rovno_printer_speed_ppm",
		Help: "Текущая скорость печати принтера кодов/мин",
	}, []string{"printer_id", "printer_name"})

	// Метрики конкретного задания (растут от 0 до максимума)
	taskTotalCodes = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "rovno_task_total_codes",
		Help: "Общее количество кодов, присланных 1С в рамках текущего задания",
	}, []string{"task_id", "line_name"})

	taskPrintedCodes = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "rovno_task_printed_codes",
		Help: "Сколько кодов уже успешно отпечатано внутри этого задания",
	}, []string{"task_id", "line_name"})
)

// Вспомогательный парсер: очищает строки типа "75%", "N/A", "120.5" до чистого float64
func parseNumeric(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" || s == "N/A" || s == "?" {
		return 0
	}
	var sb strings.Builder
	for _, ch := range s {
		if (ch >= '0' && ch <= '9') || ch == '.' {
			sb.WriteRune(ch)
		}
	}
	val, err := strconv.ParseFloat(sb.String(), 64)
	if err != nil {
		return 0
	}
	return val
}

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
		slog.Error("Накачка отменена: не найдено динамическое поле", "task_id", taskID)
		return
	}
	slog.Info("Начинаем шоу")

	go func() {
		for {
			// 1. Проверяем, не остановлена ли задача (Graceful exit из горутины)
			status, err := tp.Store.GetTaskStatus(taskID)
			if err != nil || status != "active" {
				slog.Info("Задача завершена, останавливаем насос", "task_id", taskID)
				return
			}

			// 2. Получаем активные принтеры на линии
			printers, err := tp.Store.GetPrintersByLine(lineID)
			if err != nil {
				time.Sleep(2 * time.Second)
				continue
			}

			for _, pCfg := range printers {
				p := tp.Manager.GetPrinter(pCfg.ID)
				if p == nil {
					continue
				}

				// 1. Устанавливаем желаемую планку буфера
				maxBuffer := 10

				// 2. Узнаем, сколько свободных слотов осталось до лимита
				// (Драйвер возвращает SGM - SRC, т.е. сколько еще влезет)
				freeSpace, err := p.GetBufferFreeSpace()
				if err != nil {
					continue
				}

				// Считаем, сколько кодов сейчас в принтере
				// ВАЖНО: Это сработает корректно, если в InitSession (main.go) вы указали лимит 10.
				// Если там лимит 2000, то freeSpace будет ~1990.
				// Поэтому для логики "держать 10" мы берем freeSpace, но не даем ему превысить наш макс.

				targetLoad := freeSpace
				if targetLoad > maxBuffer {
					// Если в принтере пусто, не заливаем больше 10 за раз
					targetLoad = maxBuffer
				}

				// Выводим лог для контроля "наполненности"
				slog.Info("Pumper: состояние буфера",
					"printer", pCfg.Name,
					"in_printer", maxBuffer-freeSpace, // Сколько примерно занято
					"adding", targetLoad)

				// 3. Если место есть — забираем коды из базы
				if targetLoad <= 0 {
					continue // Буфер полон
				}

				pending, err := tp.Store.FetchAndAssignCodes(taskID, pCfg.ID, targetLoad)
				if err != nil {
					slog.Error("Pumper: Ошибка БД", "task_id", taskID, "err", err)
					continue
				}

				if len(pending) == 0 {
					continue // Коды в базе кончились
				}

				// 4. Подготовка пачки
				var codesOnly []string
				for _, item := range pending {
					codesOnly = append(codesOnly, item.Code)
				}

				startIndex := pending[0].PrinterIndex

				// 5. Отправка в принтер
				loaded, err := p.PrintBatchIndexed(dynamicField, startIndex, codesOnly)

				if err == nil && loaded > 0 {
					slog.Debug("Pumper: дозагрузка выполнена", "printer", pCfg.Name, "count", loaded)
				} else if err != nil {
					slog.Warn("Pumper: ошибка SID", "printer", pCfg.Name, "err", err)
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

			// 1. Сразу объявляем ID строкой, так как он понадобится и для логов, и для метрик
			printerIDStr := strconv.Itoa(id)

			if err == nil {
				// 2. СНАЧАЛА получаем реальные значения от принтера
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

				// 3. ПОСЛЕ ТОГО как получили, записываем их в метрики Prometheus
				printerRibbon.WithLabelValues(printerIDStr, cfg.Name).Set(parseNumeric(ribbon))
				printerSpeed.WithLabelValues(printerIDStr, cfg.Name).Set(parseNumeric(speed))
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

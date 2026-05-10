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
}

// Добавляем возможность управления задачами
type TaskProcessor struct {
	Store   *storage.Store
	Manager *PrinterManager
}

func (tp *TaskProcessor) StartPumping(lineID int, taskID int) {
	go func() {
		for {
			// 1. Проверяем, активна ли еще задача в БД
			// (Здесь должна быть проверка статуса task из Store)

			// 2. Получаем принтеры линии
			printers, _ := tp.Store.GetPrintersByLine(lineID)
			for _, pCfg := range printers {
				p := tp.Manager.GetPrinter(pCfg.ID)
				if p == nil {
					continue
				}

				// 3. Сколько места в буфере принтера?
				freeSpace, err := p.GetBufferFreeSpace()
				if err != nil || freeSpace < 10 {
					continue
				} // Ждем, если буфер забит или принтер оффлайн

				// Держим в принтере не более 100 кодов для маневренности
				targetLoad := 100 - (100 - freeSpace)
				if targetLoad <= 0 {
					continue
				}

				// 4. Берем коды из БД
				pending, _ := tp.Store.GetNextPendingCodes(taskID, targetLoad)
				if len(pending) == 0 {
					continue
				}

				var codesOnly []string
				for _, item := range pending {
					codesOnly = append(codesOnly, item.Code)
				}

				// 5. Отправляем в принтер через SID
				startIndex := int(time.Now().UnixNano() / 1e6) // Уникальный индекс для пачки
				loaded, err := p.PrintBatchIndexed("code", startIndex, codesOnly)

				if err == nil && loaded > 0 {
					// 6. Обновляем статусы в БД: теперь они 'in_buffer' с привязкой к индексу
					for i, item := range pending[:loaded] {
						tp.Store.UpdateCodeStatus(item.ID, "in_buffer", startIndex+i)
					}
				}
			}
			time.Sleep(1 * time.Second)
		}
	}()
}

type PrinterManager struct {
	mu       sync.RWMutex
	printers map[int]Printer
	configs  map[int]models.PrinterConfig // ИСПРАВЛЕНО
	states   map[int]models.PrinterState  // ИСПРАВЛЕНО
	logs     []models.LogEntry            // ИСПРАВЛЕНО
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

func (pm *PrinterManager) telemetryFlusher(store *storage.Store) {
	ticker := time.NewTicker(1 * time.Minute)
	for range ticker.C {
		pm.mu.RLock()
		// Делаем снимок текущих состояний
		snap := make(map[int]models.PrinterState)
		for id, state := range pm.states {
			snap[id] = state
		}
		pm.mu.RUnlock()

		for id, state := range snap {
			// Просто передаем всё как есть — строками.
			// Принтер не умеет в риббон? Придет "N/A". Всё честно.
			err := store.SaveTelemetry(id, state.CurCount, state.Ribbon, state.Status)
			if err != nil {
				log.Printf("[STATS] Ошибка записи: %v", err)
			}
		}
	}
}

// StartTelemetryCollector запускает фоновый процесс сбора статистики
func (pm *PrinterManager) StartTelemetryCollector(store *storage.Store, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		// ticker.C — это канал, который будет "стрелять" раз в указанный интервал
		for range ticker.C {
			// ШАГ 1: Быстро делаем "снимок" состояний под защитой RLock
			pm.mu.RLock()
			snapshot := make(map[int]models.PrinterState)
			for id, state := range pm.states {
				snapshot[id] = state
			}
			pm.mu.RUnlock()
			// С этого момента m.states может меняться другими горутинами,
			// а мы работаем со своей копией 'snapshot' в спокойном темпе.

			// ШАГ 2: Записываем данные из снимка в базу данных
			for id, state := range snapshot {
				// Передаем всё строками, как мы договорились (CurCount, Ribbon, Status)
				err := store.SaveTelemetry(id, state.CurCount, state.Ribbon, state.Status)
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
			cfg := pm.configs[id] // Получаем конфиг для проверки статуса
			pm.mu.RUnlock()

			// Если принтер деактивирован в настройках — пропускаем опрос
			if !cfg.IsActive {
				continue
			}

			status, err := p.GetStatus()

			var ribbon, queue, speed, curCount, curTemplate string

			if err == nil {
				ribbon, _ = p.GetRemainingRibbon()
				queue, _ = p.GetQueueCapacity("code")
				speed, _ = p.GetPrintSpeed()
				curCount, _ = p.GetCurrentPrintCount()
				curTemplate, _ = p.GetCurrentTemplate()
			}

			pm.mu.Lock()
			oldState := pm.states[id]
			newState := models.PrinterState{
				LastTemplate:   oldState.LastTemplate,
				LastStaticHash: oldState.LastStaticHash,
			}

			isOfflineNow := err != nil
			wasOffline := strings.Contains(oldState.Status, "ОФФЛАЙН") || oldState.Status == "INITIALIZING"

			printerIDStr := strconv.Itoa(id) // Для логов

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
		slog.Debug("Итерация опроса завершена", "printers_count", len(ids))
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

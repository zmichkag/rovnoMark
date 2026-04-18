package core

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// Printer - расширенный контракт для железа
type Printer interface {
	GetStatus() (string, error)
	PrintTemplate(template string, fields map[string]string) error
	GetRemainingRibbon() (string, error)
	GetQueueCapacity(queueName string) (string, error)
	GetPrintSpeed() (string, error)
	GetCurrentPrintCount() (string, error)
}
type PrinterConfig struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	IP         string `json:"ip"`
	DriverType string `json:"driver_type"`
}

// PrinterState хранит полную телеметрию
type PrinterState struct {
	Status   string `json:"status"`
	Ribbon   string `json:"ribbon"`
	Queue    string `json:"queue"`
	Speed    string `json:"speed"`
	CurCount string `json:"cur_count"`
}

// LogEntry - запись для журнала событий
type LogEntry struct {
	Time    string `json:"time"`
	Printer string `json:"printer"`
	Event   string `json:"event"`
}

type PrinterManager struct {
	mu       sync.RWMutex
	printers map[string]Printer
	configs  map[string]PrinterConfig
	states   map[string]PrinterState
	logs     []LogEntry // Наш бортовой самописец
}

func NewPrinterManager() *PrinterManager {
	pm := &PrinterManager{
		printers: make(map[string]Printer),
		configs:  make(map[string]PrinterConfig),
		states:   make(map[string]PrinterState),
		logs:     make([]LogEntry, 0),
	}
	// Добавляем стартовую запись
	pm.addLog("СИСТЕМА", "Ядро РОВНО v1.0 успешно запущено")
	go pm.backgroundPoller()
	return pm
}

func (pm *PrinterManager) AddPrinter(config PrinterConfig, p Printer) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.configs[config.ID] = config
	pm.printers[config.ID] = p
	pm.states[config.ID] = PrinterState{Status: "INITIALIZING", Ribbon: "?", Queue: "?"}
	pm.addLogNoLock(config.ID, fmt.Sprintf("Принтер добавлен в пул наблюдения (%s)", config.IP))
}

func (pm *PrinterManager) GetPrinter(id string) Printer {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return pm.printers[id]
}

// GetDashboardData отдает всё сразу для UI
func (pm *PrinterManager) GetDashboardData() (map[string]PrinterState, []LogEntry) {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	statesCopy := make(map[string]PrinterState)
	for k, v := range pm.states {
		statesCopy[k] = v
	}
	// Отдаем последние 50 логов
	logsCopy := make([]LogEntry, len(pm.logs))
	copy(logsCopy, pm.logs)

	return statesCopy, logsCopy
}

// backgroundPoller - опрашивает железо и генерирует события
func (pm *PrinterManager) backgroundPoller() {
	for {
		pm.mu.RLock()
		var ids []string
		for id := range pm.printers {
			ids = append(ids, id)
		}
		pm.mu.RUnlock()

		for _, id := range ids {
			pm.mu.RLock()
			p := pm.printers[id]
			pm.mu.RUnlock()

			// Собираем телеметрию
			status, err := p.GetStatus()

			// 1. Объявляем переменные ДО проверки, чтобы они были доступны везде

			var ribbon, queue, speed, curCount string

			// 2. Запрашиваем данные, если нет ошибки
			if err == nil {
				ribbon, _ = p.GetRemainingRibbon()
				queue, _ = p.GetQueueCapacity("code") // Пока хардкодим поле 'code'
				speed, _ = p.GetPrintSpeed()
				curCount, _ = p.GetCurrentPrintCount() // Твоя новая скорость
			}

			// 3. Блокируем память для записи
			pm.mu.Lock()
			oldState := pm.states[id]
			newState := PrinterState{}

			// 4. STATE MACHINE: Детектор событий
			isOfflineNow := err != nil
			wasOffline := strings.Contains(oldState.Status, "ОФФЛАЙН") || oldState.Status == "INITIALIZING"

			if isOfflineNow && !wasOffline {
				pm.addLogNoLock(id, fmt.Sprintf("ПОТЕРЯ СВЯЗИ: %v", err))
				newState.Status = fmt.Sprintf("ОФФЛАЙН: %v", err)
				newState.Ribbon = "N/A"
				newState.Queue = "N/A"
				newState.Speed = "N/A" // Сбрасываем скорость
				newState.CurCount = "N/A"
			} else if !isOfflineNow && wasOffline && oldState.Status != "INITIALIZING" {
				pm.addLogNoLock(id, "Связь восстановлена. Статус: "+status)
				newState.Status = status
				newState.Ribbon = ribbon
				newState.Queue = queue
				newState.Speed = speed
				newState.CurCount = curCount
			} else if isOfflineNow {
				newState.Status = oldState.Status // Оставляем старую ошибку
			} else {
				// Все хорошо, просто обновляем данные
				newState.Status = status
				newState.Ribbon = ribbon
				newState.Queue = queue
				newState.Speed = speed
				newState.CurCount = curCount
			}

			// 5. Сохраняем и открываем замок
			pm.states[id] = newState
			pm.mu.Unlock()
		}
		time.Sleep(2 * time.Second)
	}
}

// Внутренние методы логирования
func (pm *PrinterManager) addLogNoLock(printer, event string) {
	entry := LogEntry{
		Time:    time.Now().Format("15:04:05"),
		Printer: printer,
		Event:   event,
	}
	// Добавляем в начало списка (последние сверху)
	pm.logs = append([]LogEntry{entry}, pm.logs...)
	if len(pm.logs) > 50 {
		pm.logs = pm.logs[:50] // Храним только 50 последних событий
	}
}

func (pm *PrinterManager) addLog(printer, event string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.addLogNoLock(printer, event)
}

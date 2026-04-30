package core

import (
	"fmt"
	"strconv" // Добавляем для конвертации ID в строку для логов
	"strings"
	"sync"
	"time"
)

// Printer - расширенный контракт для железа
type Printer interface {
	GetStatus() (string, error)
	PrintTemplate(template string, fields map[string]string) error
	PrintBatch(fieldName string, codes []string) (int, error)
	GetRemainingRibbon() (string, error)
	GetQueueCapacity(queueName string) (string, error)
	GetPrintSpeed() (string, error)
	GetCurrentPrintCount() (string, error)
	GetCurrentTemplate() (string, error)
}

//LineConfig

type LineConfig struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	IsActive    bool   `json:"is_active"`
	IsDeleted   bool   `json:"is_deleted"`
}

// PrinterConfig - физическое устройство
type PrinterConfig struct {
	ID         int    `json:"id"`
	Name       string `json:"name"`
	IP         string `json:"ip"`
	Port       int    `json:"port"`
	DriverType string `json:"driver_type"`
	IsActive   bool   `json:"is_active"`
	IsDeleted  bool   `json:"is_deleted"`
}

// PrinterState хранит полную телеметрию
type PrinterState struct {
	Status      string `json:"status"`
	Ribbon      string `json:"ribbon"`
	Queue       string `json:"queue"`
	Speed       string `json:"speed"`
	CurCount    string `json:"cur_count"`
	CurTemplate string `json:"cur_template"`
}

type LogEntry struct {
	Time    string `json:"time"`
	Printer string `json:"printer"`
	Event   string `json:"event"`
}

type PrinterManager struct {
	mu       sync.RWMutex
	printers map[int]Printer
	configs  map[int]PrinterConfig
	states   map[int]PrinterState
	logs     []LogEntry
}

func NewPrinterManager() *PrinterManager {
	pm := &PrinterManager{
		printers: make(map[int]Printer),
		configs:  make(map[int]PrinterConfig),
		states:   make(map[int]PrinterState),
		logs:     make([]LogEntry, 0),
	}

	go pm.backgroundPoller()

	return pm
}

func (pm *PrinterManager) AddPrinter(config PrinterConfig, p Printer) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	pm.configs[config.ID] = config
	pm.printers[config.ID] = p
	pm.states[config.ID] = PrinterState{Status: "INITIALIZING", Ribbon: "?", Queue: "?"}

	// Конвертируем int ID в строку для метода лога
	pm.addLogNoLock(strconv.Itoa(config.ID), fmt.Sprintf("Принтер добавлен (%s)", config.IP))
}

func (pm *PrinterManager) GetPrinter(id int) Printer {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return pm.printers[id]
}

func (pm *PrinterManager) GetDashboardData() (map[int]PrinterState, []LogEntry) {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	statesCopy := make(map[int]PrinterState)
	for k, v := range pm.states {
		statesCopy[k] = v
	}
	logsCopy := make([]LogEntry, len(pm.logs))
	copy(logsCopy, pm.logs)

	return statesCopy, logsCopy
}

func (pm *PrinterManager) backgroundPoller() {
	for {
		pm.mu.RLock()
		// Срез ids теперь должен быть []int, так как ключи в мапе - числа
		var ids []int
		for id := range pm.printers {
			ids = append(ids, id)
		}
		pm.mu.RUnlock()

		for _, id := range ids {
			pm.mu.RLock()
			p := pm.printers[id]
			pm.mu.RUnlock()

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
			newState := PrinterState{}

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
		time.Sleep(2 * time.Second)
	}
}

func (pm *PrinterManager) addLogNoLock(printer, event string) {
	entry := LogEntry{
		Time:    time.Now().Format("15:04:05"),
		Printer: printer,
		Event:   event,
	}
	pm.logs = append([]LogEntry{entry}, pm.logs...)
	if len(pm.logs) > 50 {
		pm.logs = pm.logs[:50]
	}
}

func (pm *PrinterManager) addLog(printer, event string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.addLogNoLock(printer, event)
}

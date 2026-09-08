package models

import (
	"encoding/json"
	"strings"
	"time"
)

// PrinterConfig описывает конфигурацию физического печатающего устройства
type PrinterConfig struct {
	ID         int    `json:"id"`
	Name       string `json:"name"`
	IP         string `json:"ip"`
	Port       int    `json:"port"`
	DriverType string `json:"driver_type"`
	Role       string `json:"role"`
	IsActive   bool   `json:"is_active"`
	IsDeleted  bool   `json:"is_deleted"`
}

// LineConfig описывает производственную линию
type LineConfig struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	IsActive    bool   `json:"is_active"`
	IsDeleted   bool   `json:"is_deleted"`
}

// PrinterState хранит оперативное состояние и телеметрию принтера в ОЗУ
type PrinterState struct {
	LastTemplate   string
	LastStaticHash string
	Status         string `json:"status"`
	Ribbon         string `json:"ribbon"`
	Queue          string `json:"queue"`
	Speed          string `json:"speed"`
	CurCount       string `json:"cur_count"`
	CurTemplate    string `json:"cur_template"`
}

// LogEntry представляет строковый лог для веб-интерфейса и дашборда
type LogEntry struct {
	Time    string `json:"time"`
	Printer string `json:"printer"`
	Event   string `json:"event"`
}

// InboundCodeItem представляет универсальный элемент кода от 1С
type InboundCodeItem struct {
	Code  string `json:"code"`
	ExtID string `json:"ext_id"`
}

// UnmarshalJSON безопасно читает ext_id как число (11), строку ("11") или null
func (item *InboundCodeItem) UnmarshalJSON(data []byte) error {
	var raw struct {
		Code  string          `json:"code"`
		ExtID json.RawMessage `json:"ext_id"`
	}

	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	item.Code = raw.Code

	if len(raw.ExtID) > 0 {
		val := strings.TrimSpace(string(raw.ExtID))
		if val != "null" {
			// Отрезаем кавычки, если пришла строка, или оставляем число как строку
			item.ExtID = strings.Trim(val, `"`)
		}
	}

	return nil
}

// TaskCode представляет единицу маркировки в шарде БД
type TaskCode struct {
	ID           int       `json:"id"`
	TaskID       int       `json:"task_id"`
	PrinterID    int       `json:"printer_id"`
	Code         string    `json:"code"`
	ExternalID   string    `json:"ext_id"`        // Идентификатор/порядковый номер из 1C (может быть пустым)
	Status       string    `json:"status"`        // 'pending', 'in_buffer', 'printed'[cite: 1, 2, 3]
	PrinterIndex int       `json:"printer_index"` // Индекс партии/пакета для принтера
	PrintedAt    time.Time `json:"printed_at"`
}

// EventLogItem представляет запись системного или аппаратного события в Master DB
type EventLogItem struct {
	ID        int       `json:"id"`
	Timestamp time.Time `json:"timestamp"`
	LineID    *int      `json:"line_id,omitempty"`
	LineName  string    `json:"line_name,omitempty"`
	PrinterID *int      `json:"printer_id,omitempty"`
	Printer   string    `json:"printer_name,omitempty"`
	EventType string    `json:"event_type"` // 'error', 'warn', 'info', 'success'
	Message   string    `json:"message"`
}

// LogFilter содержит параметры фильтрации истории событий
type LogFilter struct {
	LineID    int       `json:"line_id"`
	PrinterID int       `json:"printer_id"`
	EventType string    `json:"event_type"` // 'error', 'warn', 'info', 'success'
	DateFrom  time.Time `json:"date_from"`
	DateTo    time.Time `json:"date_to"`
	Limit     int       `json:"limit"`
	Offset    int       `json:"offset"`
}

type TaskState string

const (
	TaskStateCreated      TaskState = "created"
	TaskStateInitializing TaskState = "ready"
	TaskStateActive       TaskState = "active"
	TaskStateCompleted    TaskState = "completed"
	TaskStateStopped      TaskState = "stopped"
	TaskStateFailed       TaskState = "failed"
)

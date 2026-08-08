package models

import "time"

// Физическое устройство
type PrinterConfig struct {
	ID         int    `json:"id"`
	Name       string `json:"name"`
	IP         string `json:"ip"`
	Port       int    `json:"port"`
	DriverType string `json:"driver_type"`
	IsActive   bool   `json:"is_active"`
	IsDeleted  bool   `json:"is_deleted"`
}

// Конфигурация линии
type LineConfig struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	IsActive    bool   `json:"is_active"`
	IsDeleted   bool   `json:"is_deleted"`
}

// Состояние принтера (Телеметрия)
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

type LogEntry struct {
	Time    string `json:"time"`
	Printer string `json:"printer"`
	Event   string `json:"event"`
}

// TaskCode представляет одну единицу маркировки в базе данных
type TaskCode struct {
	ID           int       `json:"id"`
	TaskID       int       `json:"task_id"`
	PrinterID    int       `json:"printer_id"`
	Code         string    `json:"code"`
	Status       string    `json:"status"`        // 'pending', 'in_buffer', 'printed'
	PrinterIndex int       `json:"printer_index"` // Индекс SID от принтера
	PrintedAt    time.Time `json:"printed_at"`
}

// EventLogItem представляет запись системного или аппаратного события в БД
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

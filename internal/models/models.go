package models

import (
	"encoding/json"
	"strings"
	"time"
)

// PumperMode определяет алгоритм прокачки кодов в принтер
type PumperMode string

const (
	ModeFastSingle   PumperMode = "fast_single"   // Реактивный под фотодатчик (по 1 шт)
	ModeRibbonBuffer PumperMode = "ribbon_buffer" // Программная петля упреждения на ленте
)

// Константы буферизации по умолчанию
const (
	DefaultBufferLimit = 30 // Стандартный лимит аппаратного буфера принтера
	DefaultLeadLoop    = 5  // Стандартная глубина стартовой петли упреждения
)

// PrinterConfig описывает конфигурацию физического печатающего устройства
type PrinterConfig struct {
	ID          int             `json:"id" db:"id"`
	Name        string          `json:"name" db:"name"`
	IP          string          `json:"ip" db:"ip"`
	Port        int             `json:"port" db:"port"`
	DriverType  string          `json:"driver_type" db:"driver_type"`
	Role        string          `json:"role" db:"role"`
	PumperMode  PumperMode      `json:"pumper_mode,omitempty" db:"pumper_mode"`   // fast_single или ribbon_buffer
	BufferLimit int             `json:"buffer_limit,omitempty" db:"buffer_limit"` // Глубина очереди / шаг дозарядки
	LeadLoop    int             `json:"lead_loop,omitempty" db:"lead_loop"`       // Стартовая петля опережения триггера
	IsActive    bool            `json:"is_active" db:"is_active"`
	IsDeleted   bool            `json:"is_deleted" db:"is_deleted"`
	Settings    json.RawMessage `json:"settings,omitempty" db:"settings_json"`
}

// GetEffectiveBufferLimit возвращает безопасный размер буфера с фоллбэком на дефолт
func (p *PrinterConfig) GetEffectiveBufferLimit() int {
	if p.BufferLimit <= 0 {
		return DefaultBufferLimit
	}
	return p.BufferLimit
}

// GetEffectiveLeadLoop возвращает безопасный размер стартовой петли
func (p *PrinterConfig) GetEffectiveLeadLoop() int {
	if p.LeadLoop <= 0 {
		return DefaultLeadLoop
	}
	if limit := p.GetEffectiveBufferLimit(); p.LeadLoop > limit {
		return limit
	}
	return p.LeadLoop
}

// GetEffectivePumperMode возвращает режим работы с фоллбэком на классический реактивный
func (p *PrinterConfig) GetEffectivePumperMode() PumperMode {
	if p.PumperMode == ModeRibbonBuffer {
		return ModeRibbonBuffer
	}
	return ModeFastSingle
}

type ScannerConfig struct {
	ID             int         `json:"id"`
	LineID         int         `json:"line_id"`
	Name           string      `json:"name"`
	DriverType     string      `json:"driver_type"` // tcp_camera
	Address        string      `json:"address"`     // IP / Hostname
	Port           int         `json:"port"`
	Role           ScannerRole `json:"role,omitempty"`
	TargetDeviceID *int        `json:"target_device_id,omitempty"`
	SettingsJSON   string      `json:"settings_json,omitempty"`
	IsActive       bool        `json:"is_active"`
	IsDeleted      bool        `json:"is_deleted"`
	CreatedAt      string      `json:"created_at,omitempty"`
	UpdatedAt      string      `json:"updated_at,omitempty"`
}

type ScannerRole string

const (
	ScannerRoleInlineVerifier ScannerRole = "INLINE_VERIFIER"
	ScannerRoleAuditCheck     ScannerRole = "AUDIT_CHECK"
	ScannerRoleAggregator     ScannerRole = "AGGREGATOR"
)

type ScannerRead struct {
	ID          int       `json:"id"`
	ScannerID   int       `json:"scanner_id"`
	LineID      int       `json:"line_id"`
	TaskID      *int      `json:"task_id,omitempty"`
	TaskCodeID  *int      `json:"task_code_id,omitempty"`
	Code        string    `json:"code"`
	MatchStatus string    `json:"match_status"`
	ReadAt      time.Time `json:"read_at"`
}

type LineConfig struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	IsActive    bool   `json:"is_active"`
	IsDeleted   bool   `json:"is_deleted"`
}

type PrinterState struct {
	LastTemplate   string
	LastStaticHash string
	Status         string `json:"status"`
	Ribbon         string `json:"ribbon"`
	Queue          string `json:"queue"`
	Speed          string `json:"speed"`
	CurCount       string `json:"cur_count"`
	CurTemplate    string `json:"cur_template"`
	LastWeight     string `json:"last_weight,omitempty"`
}

type LogEntry struct {
	Time    string `json:"time"`
	Printer string `json:"printer"`
	Event   string `json:"event"`
}

type InboundCodeItem struct {
	Code  string `json:"code"`
	ExtID string `json:"ext_id"`
}

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
			item.ExtID = strings.Trim(val, `"`)
		}
	}

	return nil
}

type TaskCode struct {
	ID           int       `json:"id"`
	TaskID       int       `json:"task_id"`
	PrinterID    int       `json:"printer_id"`
	Code         string    `json:"code"`
	Weight       string    `json:"weight,omitempty"`
	ExternalID   string    `json:"ext_id"`
	Status       string    `json:"status"`
	PrinterIndex int       `json:"printer_index"`
	PrintedAt    time.Time `json:"printed_at"`
}

type EventLogItem struct {
	ID        int       `json:"id"`
	Timestamp time.Time `json:"timestamp"`
	LineID    *int      `json:"line_id,omitempty"`
	LineName  string    `json:"line_name,omitempty"`
	PrinterID *int      `json:"printer_id,omitempty"`
	Printer   string    `json:"printer_name,omitempty"`
	EventType string    `json:"event_type"`
	Message   string    `json:"message"`
}

type LogFilter struct {
	LineID    int       `json:"line_id"`
	PrinterID int       `json:"printer_id"`
	EventType string    `json:"event_type"`
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

package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"rovnoMark/internal/models"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db       *sql.DB      // Master DB (метаданные, задачи, линии)
	codesMu  sync.RWMutex // Защита дескриптора активного шарда кодов
	codesDB  *sql.DB      // Активный месячный шард для task_codes
	curMonth string       // YYYY_MM текущего активного шарда
	dataDir  string       // Папка для хранения файлов баз
}

// ============================================================================
// 1. Инициализация и ротация хранилища
// ============================================================================

func New(baseDir string) *Store {
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		log.Fatalf("Не удалось создать каталог БД %s: %v", baseDir, err)
	}

	masterPath := filepath.Join(baseDir, "rovnoMark_master.db")
	db, err := sql.Open("sqlite", masterPath)
	if err != nil {
		log.Fatal("Ошибка открытия Master БД:", err)
	}

	db.SetMaxOpenConns(1)
	db.Exec("PRAGMA journal_mode = WAL;")
	db.Exec("PRAGMA busy_timeout = 5000;")
	db.Exec("PRAGMA synchronous = NORMAL;")
	db.Exec("PRAGMA foreign_keys = ON;")

	createMasterTables(db)

	addColumnIfNotExists(db, "tasks", "rnd_text", "TEXT DEFAULT ''")
	addColumnIfNotExists(db, "printers", "raw_body", "TEXT DEFAULT ''")

	store := &Store{
		db:      db,
		dataDir: baseDir,
	}

	store.rotateCodesDBIfNeeded()
	return store
}

func (s *Store) getCodesDB() *sql.DB {
	s.rotateCodesDBIfNeeded()
	s.codesMu.RLock()
	defer s.codesMu.RUnlock()
	return s.codesDB
}

func (s *Store) rotateCodesDBIfNeeded() {
	monthKey := time.Now().Format("2006_01")

	s.codesMu.RLock()
	if s.codesDB != nil && s.curMonth == monthKey {
		s.codesMu.RUnlock()
		return
	}
	s.codesMu.RUnlock()

	s.codesMu.Lock()
	defer s.codesMu.Unlock()

	if s.codesDB != nil && s.curMonth == monthKey {
		return
	}

	if s.codesDB != nil {
		slog.Info("Ротация хранилища кодов: закрываем шард", "prev_month", s.curMonth)
		_ = s.codesDB.Close()
	}

	shardPath := filepath.Join(s.dataDir, fmt.Sprintf("codes_%s.db", monthKey))
	shardDB, err := sql.Open("sqlite", shardPath)
	if err != nil {
		log.Fatalf("Критическая ошибка открытия шарда кодов %s: %v", shardPath, err)
	}

	shardDB.SetMaxOpenConns(1)
	shardDB.Exec("PRAGMA journal_mode = WAL;")
	shardDB.Exec("PRAGMA busy_timeout = 5000;")
	shardDB.Exec("PRAGMA synchronous = NORMAL;")
	shardDB.Exec("PRAGMA cache_size = -64000;")
	shardDB.Exec("PRAGMA temp_store = MEMORY;")
	shardDB.Exec("PRAGMA wal_autocheckpoint = 4000;")
	shardDB.Exec("PRAGMA mmap_size = 1073741824;")

	createCodesTables(shardDB)
	addColumnIfNotExists(shardDB, "task_codes", "ext_id", "TEXT DEFAULT ''")

	s.codesDB = shardDB
	s.curMonth = monthKey
	slog.Info("Активный шард кодов подключен", "month", monthKey, "path", shardPath)
}

// ============================================================================
// 2. Схемы таблиц и вспомогательные функции
// ============================================================================

func createMasterTables(db *sql.DB) {
	db.Exec(`CREATE TABLE IF NOT EXISTS lines (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL,
		description TEXT,
		is_active BOOLEAN DEFAULT 1,
		is_deleted BOOLEAN DEFAULT 0
	);`)

	db.Exec(`CREATE TABLE IF NOT EXISTS printers (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL,
		ip TEXT NOT NULL,
		port INTEGER,
		driver_type TEXT,
		raw_body TEXT DEFAULT '',
		is_active BOOLEAN DEFAULT 1,
		is_deleted BOOLEAN DEFAULT 0
	);`)

	db.Exec(`CREATE TABLE IF NOT EXISTS line_printers (
		line_id INTEGER,
		printer_id INTEGER,
		role TEXT,
		PRIMARY KEY (line_id, printer_id)
	);`)

	db.Exec(`CREATE TABLE IF NOT EXISTS event_log (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
		line_id INTEGER,
		printer_id INTEGER,
		event_type TEXT,
		message TEXT
	);`)

	db.Exec(`CREATE TABLE IF NOT EXISTS tasks (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		line_id INTEGER,
		template_name TEXT,
		dynamic_field_name TEXT,
		rnd_text TEXT, 
		status TEXT DEFAULT 'active',
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		static_fields_json TEXT
	);`)

	db.Exec(`CREATE TABLE IF NOT EXISTS task_printer_counters (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		task_id INTEGER NOT NULL,
		line_id INTEGER NOT NULL,
		printer_id INTEGER NOT NULL,
		event_type TEXT NOT NULL,
		counter_value INTEGER NOT NULL,
		recorded_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);`)

	db.Exec(`CREATE TABLE IF NOT EXISTS printer_telemetry (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
		printer_id INTEGER,
		cur_count TEXT,
		ribbon TEXT,
		status TEXT,
		template TEXT
	);`)

	db.Exec(`CREATE INDEX IF NOT EXISTS idx_telemetry_time ON printer_telemetry(timestamp);`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_event_log_composite ON event_log(line_id, event_type, timestamp);`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_task_counters_task_printer ON task_printer_counters(task_id, printer_id);`)
}

func createCodesTables(db *sql.DB) {
	db.Exec(`CREATE TABLE IF NOT EXISTS task_codes (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		task_id INTEGER,
		code TEXT NOT NULL,
		ext_id TEXT DEFAULT '',
		status TEXT DEFAULT 'pending',
		printer_id INTEGER,           
		printer_index INTEGER,          
		printed_at DATETIME,
		CONSTRAINT unq_task_code UNIQUE (task_id, code)
	);`)

	// Уникальный индекс гарантирует отсечение дублей от 1С на уровне B-Tree
	db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_task_codes_unique_code 
		ON task_codes(task_id, code);`)

	// Частичный индекс для Pumper
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_task_codes_active_queue 
		ON task_codes(task_id, printer_id, printer_index) 
		WHERE status IN ('pending', 'in_buffer');`)

	db.Exec(`CREATE INDEX IF NOT EXISTS idx_task_codes_status ON task_codes(task_id, status);`)
}

func addColumnIfNotExists(db *sql.DB, tableName, columnName, colType string) {
	query := fmt.Sprintf("PRAGMA table_info(%s)", tableName)
	rows, err := db.Query(query)
	if err != nil {
		return
	}
	defer rows.Close()

	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dfltValue interface{}
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dfltValue, &pk); err == nil {
			if strings.EqualFold(name, columnName) {
				return
			}
		}
	}

	alterQuery := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", tableName, columnName, colType)
	_, _ = db.Exec(alterQuery)
}

// ============================================================================
// 3. Боевая работа с кодами (Месячный шард)
// ============================================================================

func (s *Store) AppendTaskCodes(taskID int, items []models.InboundCodeItem) error {
	db := s.getCodesDB()
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
		INSERT INTO task_codes (task_id, code, ext_id, status, printer_id, printer_index) 
		VALUES (?, ?, ?, 'pending', NULL, NULL)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, item := range items {
		if _, err := stmt.Exec(taskID, item.Code, item.ExtID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// FetchAndAssignCodesAlternating выбирает коды строго под чётность роли принтера
func (s *Store) FetchAndAssignCodesAlternating(taskID int, printerID int, role string, limit int) ([]models.TaskCode, error) {
	db := s.getCodesDB()
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// 1. Определяем требуемый остаток от деления (0 для чётных, 1 для нечётных)
	targetModulo := 1 // ODD / PRIMARY по умолчанию
	uRole := strings.ToUpper(strings.TrimSpace(role))
	if uRole == "EVEN" || uRole == "SECONDARY" || uRole == "LANE_2" {
		targetModulo = 0
	}

	// 2. Получаем последний индекс конкретного принтера
	var lastIndex int
	tx.QueryRow(`SELECT COALESCE(MAX(printer_index), 0) FROM task_codes 
	             WHERE task_id = ? AND printer_id = ?`, taskID, printerID).Scan(&lastIndex)

	// 3. Выборка кодов:
	query := `
		SELECT id, code, COALESCE(ext_id, '') 
		FROM task_codes 
		WHERE task_id = ? 
		  AND status = 'pending' 
		  AND printer_id IS NULL 
		  AND (
		      (CASE 
		          WHEN ext_id GLOB '[0-9]*' AND ext_id != '' THEN CAST(ext_id AS INTEGER) % 2 
		          ELSE id % 2 
		       END) = ?
		  )
		ORDER BY id ASC 
		LIMIT ?`

	rows, err := tx.Query(query, taskID, targetModulo, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []models.TaskCode
	for rows.Next() {
		var tc models.TaskCode
		if err := rows.Scan(&tc.ID, &tc.Code, &tc.ExternalID); err != nil {
			return nil, err
		}
		list = append(list, tc)
	}

	if len(list) == 0 {
		return nil, nil
	}

	// 4. Фиксируем захват кодов принтером и генерируем ext_id, если 1С его не прислала
	stmtUpdate, err := tx.Prepare(`
		UPDATE task_codes 
		SET printer_id = ?, 
		    printer_index = ?, 
		    status = 'in_buffer',
		    ext_id = CASE WHEN ext_id = '' THEN ? ELSE ext_id END
		WHERE id = ?`)
	if err != nil {
		return nil, err
	}
	defer stmtUpdate.Close()

	for i, tc := range list {
		nextIdx := lastIndex + 1 + i
		list[i].PrinterIndex = nextIdx

		fallbackExtID := tc.ExternalID
		if fallbackExtID == "" {
			fallbackExtID = strconv.Itoa(tc.ID)
			list[i].ExternalID = fallbackExtID
		}

		if _, errExec := stmtUpdate.Exec(printerID, nextIdx, fallbackExtID, tc.ID); errExec != nil {
			return nil, errExec
		}
	}

	return list, tx.Commit()
}

// FetchAndAssignCodes — базовый метод раздачи (без разделения на чет/нечет)
func (s *Store) FetchAndAssignCodes(taskID int, printerID int, limit int) ([]models.TaskCode, error) {
	db := s.getCodesDB()
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var lastIndex int
	tx.QueryRow(`SELECT COALESCE(MAX(printer_index), 0) FROM task_codes 
	             WHERE task_id = ? AND printer_id = ?`, taskID, printerID).Scan(&lastIndex)

	rows, err := tx.Query(`
		SELECT id, code, COALESCE(ext_id, '') FROM task_codes 
		WHERE task_id = ? AND status = 'pending' AND printer_id IS NULL 
		ORDER BY id ASC LIMIT ?`, taskID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []models.TaskCode
	for rows.Next() {
		var tc models.TaskCode
		if err := rows.Scan(&tc.ID, &tc.Code, &tc.ExternalID); err != nil {
			return nil, err
		}
		list = append(list, tc)
	}

	if len(list) == 0 {
		return nil, nil
	}

	for i, tc := range list {
		nextIdx := lastIndex + 1 + i
		_, errExec := tx.Exec(`UPDATE task_codes SET printer_id = ?, printer_index = ?, status = 'in_buffer' 
		                       WHERE id = ?`, printerID, nextIdx, tc.ID)
		if errExec != nil {
			return nil, errExec
		}
		list[i].PrinterIndex = nextIdx
	}

	return list, tx.Commit()
}

func (s *Store) GetPendingCodes(taskID int, limit int) ([]models.TaskCode, error) {
	db := s.getCodesDB()
	query := `
		SELECT id, task_id, code, COALESCE(ext_id, ''), status, COALESCE(printer_id, 0), COALESCE(printer_index, 0) 
		FROM task_codes 
		WHERE task_id = ? AND status = 'pending' 
		ORDER BY id ASC 
		LIMIT ?`

	rows, err := db.Query(query, taskID, limit)
	if err != nil {
		return nil, fmt.Errorf("ошибка выборки pending кодов: %w", err)
	}
	defer rows.Close()

	var codes []models.TaskCode
	for rows.Next() {
		var c models.TaskCode
		if err := rows.Scan(&c.ID, &c.TaskID, &c.Code, &c.ExternalID, &c.Status, &c.PrinterID, &c.PrinterIndex); err != nil {
			return nil, fmt.Errorf("ошибка сканирования строки task_codes: %w", err)
		}
		codes = append(codes, c)
	}
	return codes, nil
}

func (s *Store) MarkAsPrinted(taskID int, printerID int, lastIndex int) (int64, error) {
	db := s.getCodesDB()
	res, err := db.Exec(`
		UPDATE task_codes 
		SET status = 'printed', printed_at = CURRENT_TIMESTAMP 
		WHERE task_id = ? AND printer_id = ? AND printer_index <= ? AND status = 'in_buffer'`,
		taskID, printerID, lastIndex)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *Store) UpdateCodeStatusByID(id int, status string, printerIndex int) error {
	db := s.getCodesDB()
	query := `UPDATE task_codes SET status = ?, printer_index = ?, printed_at = CURRENT_TIMESTAMP WHERE id = ?`
	_, err := db.Exec(query, status, printerIndex, id)
	if err != nil {
		return fmt.Errorf("сбой обновления статуса кода ID=%d: %w", id, err)
	}
	return nil
}

func (s *Store) GetCodePassport(code string) (map[string]interface{}, error) {
	cleanCode := strings.ReplaceAll(code, "\x1d", "<GS>")

	activeDB := s.getCodesDB()
	var taskID int
	var status, printedAt string

	query := `SELECT task_id, status, COALESCE(printed_at, 'Не отпечатан') 
	          FROM task_codes WHERE code = ? OR code LIKE ? ORDER BY id DESC LIMIT 1`

	err := activeDB.QueryRow(query, cleanCode, "%"+cleanCode+"%").Scan(&taskID, &status, &printedAt)

	if errors.Is(err, sql.ErrNoRows) {
		files, _ := filepath.Glob(filepath.Join(s.dataDir, "codes_*.db"))
		for _, f := range files {
			if strings.Contains(f, s.curMonth) {
				continue
			}
			arcDB, errArc := sql.Open("sqlite", f+"?mode=ro")
			if errArc != nil {
				continue
			}
			errScan := arcDB.QueryRow(query, cleanCode, "%"+cleanCode+"%").Scan(&taskID, &status, &printedAt)
			arcDB.Close()
			if errScan == nil {
				err = nil
				break
			}
		}
	}

	if err != nil {
		return nil, err
	}

	var templateName, lineName string
	masterQuery := `
		SELECT t.template_name, COALESCE(l.name, '—') 
		FROM tasks t 
		LEFT JOIN lines l ON t.line_id = l.id 
		WHERE t.id = ?`
	_ = s.db.QueryRow(masterQuery, taskID).Scan(&templateName, &lineName)

	statusRu := "Загружен в очередь"
	isValid := false
	if status == "printed" {
		statusRu = "Нанесен на упаковку"
		isValid = true
	} else if status == "in_buffer" {
		statusRu = "В буфере печати"
		isValid = true
	}

	return map[string]interface{}{
		"batch":     fmt.Sprintf("%d", taskID),
		"product":   templateName,
		"line":      lineName,
		"printTime": printedAt,
		"status":    statusRu,
		"valid":     isValid,
	}, nil
}

// ============================================================================
// 4. Методы Master DB (Задачи, линии, лог, телеметрия)
// ============================================================================

func (s *Store) CreateTask(lineID int, template, dynamicField, staticJSON string, rndText string) (int64, error) {
	res, err := s.db.Exec(`
		INSERT INTO tasks (line_id, template_name, dynamic_field_name, static_fields_json, rnd_text, status) 
		VALUES (?, ?, ?, ?, ?, 'ready')`,
		lineID, template, dynamicField, staticJSON, rndText)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) GetActiveTaskByLine(lineID int) (int, error) {
	var taskID int
	query := `SELECT id FROM tasks WHERE line_id = ? AND status IN ('active', 'ready') LIMIT 1`
	err := s.db.QueryRow(query, lineID).Scan(&taskID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return taskID, err
}

func (s *Store) GetAllLines() ([]models.LineConfig, error) {
	query := `SELECT id, name, description, is_active FROM lines WHERE is_deleted = 0 AND is_active = 1 ORDER BY name ASC`
	rows, err := s.db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []models.LineConfig
	for rows.Next() {
		var l models.LineConfig
		if err := rows.Scan(&l.ID, &l.Name, &l.Description, &l.IsActive); err == nil {
			list = append(list, l)
		}
	}
	return list, nil
}

func (s *Store) GetAllPrinters() ([]models.PrinterConfig, error) {
	query := `SELECT id, name, ip, port, driver_type, is_active FROM printers WHERE is_deleted = 0`
	rows, err := s.db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []models.PrinterConfig
	for rows.Next() {
		var p models.PrinterConfig
		if err := rows.Scan(&p.ID, &p.Name, &p.IP, &p.Port, &p.DriverType, &p.IsActive); err == nil {
			list = append(list, p)
		}
	}
	return list, nil
}

func (s *Store) SavePrinter(p models.PrinterConfig) (int64, error) {
	query := `INSERT OR REPLACE INTO printers (id, name, ip, port, driver_type, is_active) VALUES (?, ?, ?, ?, ?, ?)`
	var id interface{} = p.ID
	if p.ID == 0 {
		id = nil
	}
	res, err := s.db.Exec(query, id, p.Name, p.IP, p.Port, p.DriverType, p.IsActive)
	if err != nil {
		return 0, err
	}
	if p.ID == 0 {
		return res.LastInsertId()
	}
	return int64(p.ID), nil
}

func (s *Store) SaveLine(l models.LineConfig) error {
	query := `INSERT OR REPLACE INTO lines (id, name, description, is_active) VALUES (?, ?, ?, ?)`
	var id interface{} = l.ID
	if l.ID == 0 {
		id = nil
	}
	_, err := s.db.Exec(query, id, l.Name, l.Description, l.IsActive)
	return err
}

func (s *Store) AssignPrinterToLine(lineID, printerID int, role string) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO line_printers (line_id, printer_id, role) VALUES (?, ?, ?)`, lineID, printerID, role)
	return err
}

func (s *Store) GetPrintersByLine(lineID int) ([]models.PrinterConfig, error) {
	query := `SELECT p.id, p.name, p.ip, p.port, p.driver_type, COALESCE(lp.role, 'PRIMARY') 
		FROM printers p
		JOIN line_printers lp ON p.id = lp.printer_id
		WHERE lp.line_id = ? AND p.is_active = 1`
	rows, err := s.db.Query(query, lineID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []models.PrinterConfig
	for rows.Next() {
		var p models.PrinterConfig
		// Сканируем все 6 колонок, включая Role:
		if err := rows.Scan(&p.ID, &p.Name, &p.IP, &p.Port, &p.DriverType, &p.Role); err != nil {
			slog.Error("GetPrintersByLine scan error", "err", err)
			continue
		}
		list = append(list, p)
	}
	return list, nil
}

func (s *Store) GetPrinterLineMap() (map[int]int, error) {
	rows, err := s.db.Query("SELECT printer_id, line_id FROM line_printers")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	m := make(map[int]int)
	for rows.Next() {
		var pid, lid int
		rows.Scan(&pid, &lid)
		m[pid] = lid
	}
	return m, nil
}

func (s *Store) GetAssignments() ([]map[string]interface{}, error) {
	query := `
		SELECT l.id, l.name, p.id, p.name, lp.role 
		FROM line_printers lp
		JOIN lines l ON lp.line_id = l.id
		JOIN printers p ON lp.printer_id = p.id`
	rows, err := s.db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []map[string]interface{}
	for rows.Next() {
		var lineID, printerID int
		var lName, pName, role string
		if err := rows.Scan(&lineID, &lName, &printerID, &pName, &role); err == nil {
			result = append(result, map[string]interface{}{
				"line_id":      lineID,
				"line_name":    lName,
				"printer_id":   printerID,
				"printer_name": pName,
				"role":         role,
			})
		}
	}
	return result, nil
}

func (s *Store) SetTaskStatus(taskID int, status models.TaskState) error {
	_, err := s.db.Exec(`UPDATE tasks SET status = ? WHERE id = ?`, status, taskID)
	return err
}

func (s *Store) GetTaskStatus(taskID int) (string, error) {
	var status string
	err := s.db.QueryRow("SELECT status FROM tasks WHERE id = ?", taskID).Scan(&status)
	return status, err
}

func (s *Store) GetLineIDByTask(taskID int) (int, error) {
	var lineID int
	err := s.db.QueryRow("SELECT line_id FROM tasks WHERE id = ?", taskID).Scan(&lineID)
	return lineID, err
}

func (s *Store) GetTaskDynamicField(taskID int) (string, error) {
	var field string
	err := s.db.QueryRow("SELECT dynamic_field_name FROM tasks WHERE id = ?", taskID).Scan(&field)
	return field, err
}

func (s *Store) GetTaskStaticFieldsJSON(taskID int) (string, error) {
	var staticJSON string
	err := s.db.QueryRow("SELECT static_fields_json FROM tasks WHERE id = ?", taskID).Scan(&staticJSON)
	return staticJSON, err
}

func (s *Store) GetRndTextByTask(taskID int) (string, error) {
	var rndText string
	err := s.db.QueryRow("SELECT rnd_text FROM tasks WHERE id = ?", taskID).Scan(&rndText)
	return rndText, err
}

func (s *Store) SaveEventLog(lineID *int, printerID *int, eventType string, message string) error {
	query := `INSERT INTO event_log (line_id, printer_id, event_type, message) VALUES (?, ?, ?, ?)`
	var lID, pID interface{}
	if lineID != nil && *lineID > 0 {
		lID = *lineID
	}
	if printerID != nil && *printerID > 0 {
		pID = *printerID
	}
	_, err := s.db.Exec(query, lID, pID, eventType, message)
	return err
}

func (s *Store) GetEventLogsHistory(filter models.LogFilter) ([]models.EventLogItem, error) {
	query := `
		SELECT e.id, e.timestamp, e.line_id, COALESCE(l.name, ''), e.printer_id, COALESCE(p.name, 'Система'), e.event_type, e.message
		FROM event_log e
		LEFT JOIN lines l ON e.line_id = l.id
		LEFT JOIN printers p ON e.printer_id = p.id
		WHERE 1=1`

	var args []interface{}
	if filter.LineID > 0 {
		query += " AND e.line_id = ?"
		args = append(args, filter.LineID)
	}
	if filter.PrinterID > 0 {
		query += " AND e.printer_id = ?"
		args = append(args, filter.PrinterID)
	}
	if filter.EventType != "" {
		query += " AND e.event_type = ?"
		args = append(args, filter.EventType)
	}
	if !filter.DateFrom.IsZero() {
		query += " AND e.timestamp >= ?"
		args = append(args, filter.DateFrom.Format("2006-01-02 15:04:05"))
	}
	if !filter.DateTo.IsZero() {
		query += " AND e.timestamp <= ?"
		args = append(args, filter.DateTo.Format("2006-01-02 15:04:05"))
	}

	query += " ORDER BY e.id DESC"
	if filter.Limit <= 0 {
		filter.Limit = 100
	}
	query += " LIMIT ?"
	args = append(args, filter.Limit)

	if filter.Offset > 0 {
		query += " OFFSET ?"
		args = append(args, filter.Offset)
	}

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var logs []models.EventLogItem
	for rows.Next() {
		var item models.EventLogItem
		var lID, pID sql.NullInt64
		if err := rows.Scan(&item.ID, &item.Timestamp, &lID, &item.LineName, &pID, &item.Printer, &item.EventType, &item.Message); err == nil {
			if lID.Valid {
				id := int(lID.Int64)
				item.LineID = &id
			}
			if pID.Valid {
				id := int(pID.Int64)
				item.PrinterID = &id
			}
			logs = append(logs, item)
		}
	}
	if logs == nil {
		logs = make([]models.EventLogItem, 0)
	}
	return logs, nil
}

func (s *Store) SaveTelemetry(printerID int, count string, ribbon string, status string, template string) error {
	_, err := s.db.Exec(`INSERT INTO printer_telemetry (printer_id, cur_count, ribbon, status, template) VALUES (?, ?, ?, ?, ?)`,
		printerID, count, ribbon, status, template)
	return err
}

func (s *Store) GetTelemetry(printerID int, limit int) ([]map[string]interface{}, error) {
	query := `SELECT timestamp, cur_count, ribbon, status, template FROM printer_telemetry WHERE printer_id = ? ORDER BY timestamp DESC LIMIT ?`
	rows, err := s.db.Query(query, printerID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []map[string]interface{}
	for rows.Next() {
		var ts, count, ribbon, status, template string
		rows.Scan(&ts, &count, &ribbon, &status, &template)
		result = append(result, map[string]interface{}{
			"time":     ts,
			"count":    count,
			"ribbon":   ribbon,
			"status":   status,
			"template": template,
		})
	}
	return result, nil
}

func (s *Store) RecordPrinterCounterSnapshot(taskID, lineID, printerID int, eventType string, counterValue int64) error {
	query := `INSERT INTO task_printer_counters (task_id, line_id, printer_id, event_type, counter_value, recorded_at) VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP)`
	_, err := s.db.Exec(query, taskID, lineID, printerID, eventType, counterValue)
	return err
}

func (s *Store) GetActiveTasks(lineID, printerID int) ([]map[string]interface{}, error) {
	query := `
		SELECT t.id, t.line_id, COALESCE(l.name, 'Неизвестная линия'), t.template_name, COALESCE(t.dynamic_field_name, ''), t.status, t.created_at, COALESCE(t.rnd_text, '')
		FROM tasks t
		LEFT JOIN lines l ON t.line_id = l.id
		WHERE t.status IN ('active', 'ready')`

	var args []interface{}
	if lineID > 0 {
		query += " AND t.line_id = ?"
		args = append(args, lineID)
	}
	if printerID > 0 {
		query += " AND t.line_id IN (SELECT line_id FROM line_printers WHERE printer_id = ?)"
		args = append(args, printerID)
	}
	query += " ORDER BY t.id DESC"

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	codesDB := s.getCodesDB()
	var result []map[string]interface{}

	for rows.Next() {
		var id, lID int
		var lName, template, dynamic, status, created, rndText string
		if err := rows.Scan(&id, &lID, &lName, &template, &dynamic, &status, &created, &rndText); err == nil {
			var total, printed, buffered int
			codesDB.QueryRow(`
				SELECT COUNT(*), 
				       COUNT(CASE WHEN status = 'printed' THEN 1 END), 
				       COUNT(CASE WHEN status = 'in_buffer' THEN 1 END) 
				FROM task_codes WHERE task_id = ?`, id).Scan(&total, &printed, &buffered)

			result = append(result, map[string]interface{}{
				"task_id":            id,
				"line_id":            lID,
				"line_name":          lName,
				"template_name":      template,
				"dynamic_field_name": dynamic,
				"status":             status,
				"created_at":         created,
				"rnd_text":           rndText,
				"stats": map[string]int{
					"total":    total,
					"printed":  printed,
					"buffered": buffered,
				},
			})
		}
	}
	if result == nil {
		result = make([]map[string]interface{}, 0)
	}
	return result, nil
}

func (s *Store) GetLiveDashboardData() (map[string]interface{}, error) {
	lines, err := s.GetAllLines()
	if err != nil {
		return nil, err
	}
	assignments, err := s.GetAssignments()
	if err != nil {
		return nil, err
	}

	linePrintersMap := make(map[int][]int)
	for _, a := range assignments {
		lID := a["line_id"].(int)
		pID := a["printer_id"].(int)
		linePrintersMap[lID] = append(linePrintersMap[lID], pID)
	}

	activeTasks, _ := s.GetActiveTasks(0, 0)
	taskByLineMap := make(map[int]map[string]interface{})
	for _, t := range activeTasks {
		lID := t["line_id"].(int)
		taskByLineMap[lID] = t
	}

	var linesData []map[string]interface{}
	totalActive := 0
	for _, l := range lines {
		lineObj := map[string]interface{}{
			"line_id":     l.ID,
			"line_name":   l.Name,
			"description": l.Description,
			"is_active":   l.IsActive,
			"printers":    linePrintersMap[l.ID],
		}
		if task, exists := taskByLineMap[l.ID]; exists {
			lineObj["current_task"] = task
			lineObj["status"] = task["status"]
			totalActive++
		} else {
			lineObj["current_task"] = nil
			lineObj["status"] = "IDLE"
		}
		linesData = append(linesData, lineObj)
	}

	return map[string]interface{}{
		"timestamp": time.Now().Format(time.RFC3339),
		"summary": map[string]interface{}{
			"total_lines":  len(lines),
			"active_tasks": totalActive,
			"idle_lines":   len(lines) - totalActive,
		},
		"lines": linesData,
	}, nil
}

func (s *Store) GetTaskInfo(ctx context.Context, taskID int) (map[string]interface{}, error) {
	queryMaster := `
		SELECT t.id, t.line_id, COALESCE(l.name, 'Неизвестная линия'), t.template_name, t.status, t.created_at,
		(SELECT e.timestamp FROM event_log e WHERE e.line_id = t.line_id AND e.message LIKE '%' || CAST(t.id AS TEXT) || '%' AND (e.message LIKE '%stopped%' OR e.message LIKE '%остановк%') ORDER BY e.id DESC LIMIT 1)
		FROM tasks t
		LEFT JOIN lines l ON t.line_id = l.id
		WHERE t.id = ?`

	var tID, lineID int
	var lineName, templateName, taskStatus, startedAt string
	var stopEventAt sql.NullString

	err := s.db.QueryRowContext(ctx, queryMaster, taskID).Scan(&tID, &lineID, &lineName, &templateName, &taskStatus, &startedAt, &stopEventAt)
	if err != nil {
		return nil, err
	}

	codesDB := s.getCodesDB()
	var lastPrintedAt sql.NullString
	var totalCodes, printedCount, inBufferCount, pendingCount int

	_ = codesDB.QueryRowContext(ctx, `
		SELECT MAX(printed_at),
		       COUNT(id),
		       COUNT(CASE WHEN status = 'printed' THEN 1 END),
		       COUNT(CASE WHEN status = 'in_buffer' THEN 1 END),
		       COUNT(CASE WHEN status = 'pending' THEN 1 END)
		FROM task_codes WHERE task_id = ?`, taskID).Scan(&lastPrintedAt, &totalCodes, &printedCount, &inBufferCount, &pendingCount)

	return map[string]interface{}{
		"task_id":              tID,
		"line_id":              lineID,
		"line_name":            lineName,
		"template_name":        templateName,
		"task_status":          taskStatus,
		"started_at":           startedAt,
		"last_code_printed_at": lastPrintedAt.String,
		"stop_event_at":        stopEventAt.String,
		"total_codes":          totalCodes,
		"printed_count":        printedCount,
		"in_buffer_count":      inBufferCount,
		"pending_count":        pendingCount,
	}, nil
}

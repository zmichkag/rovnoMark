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
	"strings"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"
)

type Store struct {
	db       *sqlx.DB
	codesMu  sync.RWMutex
	codesDB  *sql.DB
	curMonth string
	dataDir  string
}

type Printer struct {
	ID          int64  `db:"id"`
	Name        string `db:"name"`
	IP          string `db:"ip"`
	Port        int    `db:"port"`
	DriverType  string `db:"driver_type"`
	RawBody     string `db:"raw_body"`
	IsActive    bool   `db:"is_active"`
	IsDeleted   bool   `db:"is_deleted"`
	BufferLimit int    `db:"buffer_limit"`
	LeadLoop    int    `db:"lead_loop"`
}

type ReconcileResult struct {
	TotalPrinted  int `json:"total_printed"`
	ReturnedCodes int `json:"returned_codes"`
}

const (
	TargetMasterSchemaVersion = 2
	TargetCodesSchemaVersion  = 1
)

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

	if err := MigrateMaster(db); err != nil {
		log.Fatalf("Критическая ошибка миграции Master БД: %v", err)
	}

	sqlxDB := sqlx.NewDb(db, "sqlite")

	store := &Store{
		db:      sqlxDB,
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
		slog.Info("Ротация хранилища кодов: контрольная точка и закрытие", "prev_month", s.curMonth)
		_, _ = s.codesDB.Exec("PRAGMA wal_checkpoint(TRUNCATE);")
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

	if err := MigrateCodesShard(shardDB); err != nil {
		log.Fatalf("Критическая ошибка миграции шарда кодов %s: %v", shardPath, err)
	}

	s.codesDB = shardDB
	s.curMonth = monthKey
	slog.Info("Активный шард кодов подключен", "month", monthKey, "path", shardPath)
}

func MigrateMaster(db *sql.DB) error {
	var currentVersion int
	if err := db.QueryRow("PRAGMA user_version;").Scan(&currentVersion); err != nil {
		return fmt.Errorf("ошибка чтения PRAGMA user_version Master БД: %w", err)
	}

	if currentVersion >= TargetMasterSchemaVersion {
		slog.Debug("Схема Master БД актуальна", "user_version", currentVersion)
		return nil
	}

	slog.Info("Обновление схемы Master БД", "from", currentVersion, "target", TargetMasterSchemaVersion)

	migrations := map[int]string{
		1: `
		CREATE TABLE IF NOT EXISTS lines (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			description TEXT,
			is_active BOOLEAN DEFAULT 1,
			is_deleted BOOLEAN DEFAULT 0
		);

		CREATE TABLE IF NOT EXISTS printers (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			ip TEXT NOT NULL,
			port INTEGER,
			driver_type TEXT,
			raw_body TEXT DEFAULT '',
			is_active BOOLEAN DEFAULT 1,
			is_deleted BOOLEAN DEFAULT 0
		);

		CREATE TABLE IF NOT EXISTS line_printers (
			line_id INTEGER,
			printer_id INTEGER,
			role TEXT,
			PRIMARY KEY (line_id, printer_id)
		);

		CREATE TABLE IF NOT EXISTS event_log (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
			line_id INTEGER,
			printer_id INTEGER,
			event_type TEXT,
			message TEXT
		);

		CREATE TABLE IF NOT EXISTS tasks (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			line_id INTEGER,
			template_name TEXT,
			dynamic_field_name TEXT,
			rnd_text TEXT DEFAULT '', 
			status TEXT DEFAULT 'active',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			static_fields_json TEXT
		);

		CREATE TABLE IF NOT EXISTS task_printer_counters (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			task_id INTEGER NOT NULL,
			line_id INTEGER NOT NULL,
			printer_id INTEGER NOT NULL,
			event_type TEXT NOT NULL,
			counter_value INTEGER NOT NULL,
			recorded_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);

		CREATE TABLE IF NOT EXISTS printer_telemetry (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
			printer_id INTEGER,
			cur_count TEXT,
			ribbon TEXT,
			status TEXT,
			template TEXT
		);

		CREATE INDEX IF NOT EXISTS idx_telemetry_time ON printer_telemetry(timestamp);
		CREATE INDEX IF NOT EXISTS idx_event_log_composite ON event_log(line_id, event_type, timestamp);
		CREATE INDEX IF NOT EXISTS idx_task_counters_task_printer ON task_printer_counters(task_id, printer_id);
		`,

		2: `
		ALTER TABLE printers ADD COLUMN buffer_limit INTEGER DEFAULT 30;
		ALTER TABLE printers ADD COLUMN lead_loop INTEGER DEFAULT 5;
		`,
	}

	for v := currentVersion + 1; v <= TargetMasterSchemaVersion; v++ {
		sqlStep, ok := migrations[v]
		if !ok {
			return fmt.Errorf("отсутствует DDL для Master версии %d", v)
		}

		// 1. Открываем транзакцию чисто под DDL (создание/изменение таблиц)
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("ошибка открытия транзакции миграции Master v%d: %w", v, err)
		}

		if _, err := tx.Exec(sqlStep); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("сбой применения миграции Master v%d: %w", v, err)
		}

		// 2. Коммитим изменения схемы
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("ошибка коммита миграции Master v%d: %w", v, err)
		}

		// 3. Фиксируем новую версию PRAGMA вне транзакции, напрямую через db
		if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d;", v)); err != nil {
			return fmt.Errorf("сбой фиксации Master user_version=%d: %w", v, err)
		}

		slog.Info("Успешно применена миграция Master БД", "version", v)
	}

	return nil
}

func MigrateCodesShard(db *sql.DB) error {
	var currentVersion int
	if err := db.QueryRow("PRAGMA user_version;").Scan(&currentVersion); err != nil {
		return fmt.Errorf("ошибка чтения PRAGMA user_version шарда кодов: %w", err)
	}

	if currentVersion >= TargetCodesSchemaVersion {
		slog.Debug("Схема шарда кодов актуальна", "user_version", currentVersion)
		return nil
	}

	slog.Info("Обновление схемы шарда кодов", "from", currentVersion, "target", TargetCodesSchemaVersion)

	migrations := map[int]string{
		1: `
		CREATE TABLE IF NOT EXISTS task_codes (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			task_id INTEGER,
			code TEXT NOT NULL,
			ext_id TEXT DEFAULT '',
			status TEXT DEFAULT 'pending',
			printer_id INTEGER,           
			printer_index INTEGER,          
			printed_at DATETIME,
			CONSTRAINT unq_task_code UNIQUE (task_id, code)
		);

		CREATE UNIQUE INDEX IF NOT EXISTS idx_task_codes_unique_code 
			ON task_codes(task_id, code);

		CREATE INDEX IF NOT EXISTS idx_task_codes_active_queue 
			ON task_codes(task_id, printer_id, printer_index) 
			WHERE status IN ('pending', 'in_buffer');

		CREATE INDEX IF NOT EXISTS idx_task_codes_status 
			ON task_codes(task_id, status);
		`,
	}

	for v := currentVersion + 1; v <= TargetCodesSchemaVersion; v++ {
		sqlStep, ok := migrations[v]
		if !ok {
			return fmt.Errorf("отсутствует DDL для шарда кодов версии %d", v)
		}

		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("ошибка открытия транзакции миграции шарда v%d: %w", v, err)
		}

		if _, err := tx.Exec(sqlStep); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("сбой применения миграции шарда v%d: %w", v, err)
		}

		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d;", v)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("сбой фиксации user_version=%d шарда: %w", v, err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("ошибка коммита миграции шарда v%d: %w", v, err)
		}
		slog.Info("Успешно применена миграция шарда кодов", "version", v)
	}

	return nil
}

func (s *Store) Close() error {
	s.codesMu.Lock()
	if s.codesDB != nil {
		_, _ = s.codesDB.Exec("PRAGMA wal_checkpoint(TRUNCATE);")
		_ = s.codesDB.Close()
		s.codesDB = nil
	}
	s.codesMu.Unlock()

	if s.db != nil {
		_, _ = s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE);")
		err := s.db.Close()
		s.db = nil
		return err
	}
	return nil
}

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

func (s *Store) FetchAndAssignCodesAlternating(taskID int, printerID int, role string, limit int) ([]models.TaskCode, error) {
	db := s.getCodesDB()
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	targetModulo := 1
	uRole := strings.ToUpper(strings.TrimSpace(role))
	if uRole == "EVEN" || uRole == "SECONDARY" || uRole == "LANE_2" {
		targetModulo = 0
	}

	var lastIndex int
	tx.QueryRow(`SELECT COALESCE(MAX(printer_index), 0) FROM task_codes 
	             WHERE task_id = ? AND printer_id = ?`, taskID, printerID).Scan(&lastIndex)

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

	stmtUpdate, err := tx.Prepare(`
		UPDATE task_codes 
		SET printer_id = ?, 
			printer_index = ?, 
			status = 'in_buffer'
		WHERE id = ?`)
	if err != nil {
		return nil, err
	}
	defer stmtUpdate.Close()

	for i := range list {
		nextIdx := lastIndex + 1 + i
		list[i].PrinterIndex = nextIdx

		if _, errExec := stmtUpdate.Exec(printerID, nextIdx, list[i].ID); errExec != nil {
			return nil, errExec
		}
	}

	return list, tx.Commit()
}

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

// ReconcileAndFinalizeTaskCodes атомарно фиксирует напечатанное и возвращает остатки буфера в очередь
func (s *Store) ReconcileAndFinalizeTaskCodes(taskID int, printerID int, lastIndex int) (*ReconcileResult, error) {
	db := s.getCodesDB()
	tx, err := db.Begin()
	if err != nil {
		return nil, fmt.Errorf("ошибка открытия транзакции сверки: %w", err)
	}
	defer tx.Rollback()

	// 1. Фиксируем как напечатанные те коды, индекс которых подтвержден одометром железки
	if lastIndex > 0 {
		_, err = tx.Exec(`
			UPDATE task_codes 
			SET status = 'printed', 
			    printed_at = CURRENT_TIMESTAMP 
			WHERE task_id = ? 
			  AND printer_id = ? 
			  AND printer_index <= ? 
			  AND status = 'in_buffer'`,
			taskID, printerID, lastIndex)
		if err != nil {
			return nil, fmt.Errorf("ошибка фиксации printed кодов: %w", err)
		}
	}

	// 2. Все коды, которые принтер захватил в буфер, но НЕ успел напечатать,
	// возвращаем обратно в статус 'pending' и снимаем привязку к принтеру!
	resReverted, err := tx.Exec(`
		UPDATE task_codes 
		SET status = 'pending', 
		    printer_id = NULL, 
		    printer_index = NULL 
		WHERE task_id = ? 
		  AND printer_id = ? 
		  AND status = 'in_buffer'`,
		taskID, printerID)
	if err != nil {
		return nil, fmt.Errorf("ошибка возврата неотпечатанного буфера в pending: %w", err)
	}

	returnedCount, _ := resReverted.RowsAffected()

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("ошибка фиксации транзакции сверки: %w", err)
	}

	// 3. Получаем честное итоговое количество фактически напечатанных кодов по задаче из БД
	var totalPrinted int
	err = db.QueryRow(`
		SELECT COUNT(id) 
		FROM task_codes 
		WHERE task_id = ? AND status = 'printed'`,
		taskID).Scan(&totalPrinted)
	if err != nil {
		return nil, fmt.Errorf("ошибка подсчета итоговых printed: %w", err)
	}

	return &ReconcileResult{
		TotalPrinted:  totalPrinted,
		ReturnedCodes: int(returnedCount),
	}, nil
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
	query := `SELECT id, name, ip, port, driver_type, is_active, 
	                 COALESCE(buffer_limit, 30) AS buffer_limit, 
	                 COALESCE(lead_loop, 5) AS lead_loop 
	          FROM printers WHERE is_active = 1 and is_deleted = 0`

	var list []models.PrinterConfig
	err := s.db.Select(&list, query)
	return list, err
}

func (s *Store) SavePrinter(p models.PrinterConfig) (int64, error) {
	// Задаем значения по умолчанию
	if p.BufferLimit <= 0 {
		p.BufferLimit = 30
	}
	if p.LeadLoop < 0 {
		p.LeadLoop = 5
	}

	// Если ID равен 0, передаем nil, чтобы SQLite сам выдал новый номер
	var idVal any = p.ID
	if p.ID == 0 {
		idVal = nil
	}

	// Собираем данные в карту
	params := map[string]any{
		"id":           idVal,
		"name":         p.Name,
		"ip":           p.IP,
		"port":         p.Port,
		"driver_type":  p.DriverType,
		"is_active":    p.IsActive,
		"buffer_limit": p.BufferLimit,
		"lead_loop":    p.LeadLoop,
	}

	// Запрос с именованными параметрами
	query := `INSERT OR REPLACE INTO printers 
		(id, name, ip, port, driver_type, is_active, buffer_limit, lead_loop) 
		VALUES (:id, :name, :ip, :port, :driver_type, :is_active, :buffer_limit, :lead_loop)`
	res, err := s.db.NamedExec(query, params)
	if err != nil {
		return 0, err
	}

	//  Возвращаем ID
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
	query := `SELECT p.id, p.name, p.ip, p.port, p.driver_type, COALESCE(lp.role, 'PRIMARY') AS role, 
       COALESCE(buffer_limit, 30) AS buffer_limit, 
	   COALESCE(lead_loop, 5) AS lead_loop  
		FROM printers p
		JOIN line_printers lp ON p.id = lp.printer_id
		WHERE lp.line_id = ? AND p.is_active = 1`

	var list []models.PrinterConfig
	err := s.db.Select(&list, query, lineID)
	return list, err
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

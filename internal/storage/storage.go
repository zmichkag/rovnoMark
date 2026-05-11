package storage

import (
	"database/sql"
	"fmt"
	"log"
	"rovnoMark/internal/models"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

// UpdateCodeStatus переводит код из 'pending' в 'in_buffer' и присваивает ему индекс принтера
func (s *Store) UpdateCodeStatus(codeID int, status string, printerIndex int) error {
	_, err := s.db.Exec(`
		UPDATE task_codes 
		SET status = ?, printer_index = ? 
		WHERE id = ?`, status, printerIndex, codeID)
	return err
}

// SetTaskStatus меняет статус всей партии (например, на 'completed' или 'stopped')
func (s *Store) SetTaskStatus(taskID int, status string) error {
	_, err := s.db.Exec(`UPDATE tasks SET status = ? WHERE id = ?`, status, taskID)
	return err
}

// CreateTask Создание задачи и возврат ID
func (s *Store) CreateTask(lineID int, template string) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO tasks (line_id, template_name, status) VALUES (?, ?, 'active')`, lineID, template)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// AppendCodes Массовая дозаливка кодов в статусе pending
func (s *Store) AppendCodes(taskID int64, codes []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	stmt, _ := tx.Prepare(`INSERT INTO task_codes (task_id, code, status) VALUES (?, ?, 'pending')`)
	for _, c := range codes {
		stmt.Exec(taskID, c)
	}
	return tx.Commit()
}

// GetNextPendingCodes выбирает порцию кодов, ожидающих печати.
// Используется для наполнения внутреннего буфера принтера.
func (s *Store) GetNextPendingCodes(taskID int, limit int) ([]models.TaskCode, error) {
	// 1. Выполняем запрос
	rows, err := s.db.Query(`
        SELECT id, code 
        FROM task_codes 
        WHERE task_id = ? AND status = 'pending' 
        LIMIT ?`, taskID, limit)
	if err != nil {
		return nil, fmt.Errorf("ошибка запроса кодов: %w", err)
	}
	defer rows.Close()

	var list []models.TaskCode

	// 2. Итерируемся по результатам
	for rows.Next() {
		var tc models.TaskCode
		// Обязательно проверяем ошибку Scan!
		if err := rows.Scan(&tc.ID, &tc.Code); err != nil {
			return nil, fmt.Errorf("ошибка сканирования строки: %w", err)
		}
		list = append(list, tc)
	}

	// 3. Проверяем, не случилась ли ошибка в процессе итерации
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("ошибка после чтения всех строк: %w", err)
	}

	return list, nil
}

// Синхронизация статуса 'printed' на основе индекса SID от принтера
func (s *Store) MarkAsPrinted(taskID int, lastIndex int) (int64, error) {
	res, err := s.db.Exec(`
		UPDATE task_codes 
		SET status = 'printed', printed_at = CURRENT_TIMESTAMP 
		WHERE task_id = ? AND printer_index <= ? AND status = 'in_buffer'`,
		taskID, lastIndex)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// GetAllActivePrinters (для инициализации менеджера при запуске)
func (s *Store) GetAllPrinters() ([]models.PrinterConfig, error) { // Замена core -> models
	rows, err := s.db.Query("SELECT id, name, ip, port, driver_type, is_active FROM printers WHERE is_deleted = 0")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []models.PrinterConfig
	for rows.Next() {
		var p models.PrinterConfig
		if err := rows.Scan(&p.ID, &p.Name, &p.IP, &p.Port, &p.DriverType, &p.IsActive); err != nil {
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

// Сохранить или обновить линию
func (s *Store) SaveLine(l models.LineConfig) error {
	query := `INSERT OR REPLACE INTO lines (id, name, description, is_active) VALUES (?, ?, ?, ?)`
	var id interface{} = l.ID
	if l.ID == 0 {
		id = nil
	}
	_, err := s.db.Exec(query, id, l.Name, l.Description, l.IsActive)
	return err
}

// Получить все активные линии
func (s *Store) GetAllLines() ([]models.LineConfig, error) {
	rows, err := s.db.Query("SELECT id, name, description, is_active FROM lines WHERE is_deleted = 0")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []models.LineConfig
	for rows.Next() {
		var l models.LineConfig
		rows.Scan(&l.ID, &l.Name, &l.Description, &l.IsActive)
		list = append(list, l)
	}
	return list, nil
}

// AssignPrinterToLine Привязать принтер к линии
func (s *Store) AssignPrinterToLine(lineID, printerID int, role string) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO line_printers (line_id, printer_id, role) VALUES (?, ?, ?)`,
		lineID, printerID, role)
	return err
}

// GetPrintersByLine Показывает привязаные  принтеры
func (s *Store) GetPrintersByLine(lineID int) ([]models.PrinterConfig, error) {
	query := `
        SELECT p.id, p.name, p.ip, p.port, p.driver_type 
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
		rows.Scan(&p.ID, &p.Name, &p.IP, &p.Port, &p.DriverType)
		list = append(list, p)
	}
	return list, nil
}

// GetAssignments возвращает список всех привязок линий к принтерам с их ролями
func (s *Store) GetAssignments() ([]map[string]interface{}, error) {
	query := `
		SELECT l.name as line_name, p.name as printer_name, lp.role 
		FROM line_printers lp
		JOIN lines l ON lp.line_id = l.id
		JOIN printers p ON lp.printer_id = p.id
	`
	rows, err := s.db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []map[string]interface{}
	for rows.Next() {
		var lName, pName, role string

		if err := rows.Scan(&lName, &pName, &role); err != nil {
			log.Printf("ОШИБКА SCAN В ПРИВЯЗКАХ: %v", err)
			continue
		}
		result = append(result, map[string]interface{}{"line_name": lName, "printer_name": pName, "role": role})
	}
	return result, nil
}

// GetTelemetry возвращает историю состояния принтера
func (s *Store) GetTelemetry(printerID int, limit int) ([]map[string]interface{}, error) {
	query := `
		SELECT timestamp, cur_count, ribbon, status, template 
		FROM printer_telemetry 
		WHERE printer_id = ? 
		ORDER BY timestamp DESC 
		LIMIT ?`

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

// New запускаемся, чекаем базу на предмет актуальности версии и наличия нужных таблиц.
func New(path string) *Store {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		log.Fatal("Ошибка открытия БД:", err)
	}

	// Включаем внешние ключи
	db.Exec("PRAGMA foreign_keys = ON;")

	// --- 1. ПРОВЕРКА НА МИГРАЦИЮ СО СТАРОЙ ВЕРСИИ ---
	var oldTableExists int
	db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='printers'").Scan(&oldTableExists)

	var linesTableExists int
	db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='lines'").Scan(&linesTableExists)

	// Если есть старая таблица, но нет новой (lines) - делаем миграцию
	if oldTableExists > 0 && linesTableExists == 0 {
		log.Println("=== ОБНАРУЖЕНА СТАРАЯ БАЗА. ЗАПУСК МИГРАЦИИ НА ВЕРСИЮ 1.3 ===")
		db.Exec("ALTER TABLE printers RENAME TO printers_v1_backup;")
	}

	// --- 2. СОЗДАНИЕ НОВОЙ СТРУКТУРЫ ---
	createTables(db)

	// --- 3. ЗАВЕРШЕНИЕ МИГРАЦИИ (ПЕРЕНОС ДАННЫХ) ---
	if oldTableExists > 0 && linesTableExists == 0 {
		runMigration(db)
	}

	return &Store{db: db}
}

// SaveTelemetry метод для сохранения среза данных
func (s *Store) SaveTelemetry(printerID int, count string, ribbon string, status string, template string) error {
	_, err := s.db.Exec(`
        INSERT INTO printer_telemetry (printer_id, cur_count, ribbon, status, template)
        VALUES (?, ?, ?, ?, ?)`,
		printerID, count, ribbon, status, template)
	return err
}

// createTables Создает таблички которых не хватает
func createTables(db *sql.DB) {
	// Линии
	db.Exec(`CREATE TABLE IF NOT EXISTS lines (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL,
		description TEXT,
		is_active BOOLEAN DEFAULT 1,
		is_deleted BOOLEAN DEFAULT 0
	);`)

	// Принтеры (только физика)
	db.Exec(`CREATE TABLE IF NOT EXISTS printers (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL,
		ip TEXT NOT NULL,
		port INTEGER,
		driver_type TEXT,
		is_active BOOLEAN DEFAULT 1,
		is_deleted BOOLEAN DEFAULT 0
	);`)

	// Матрица связей
	db.Exec(`CREATE TABLE IF NOT EXISTS line_printers (
		line_id INTEGER,
		printer_id INTEGER,
		role TEXT,
		PRIMARY KEY (line_id, printer_id),
		FOREIGN KEY(line_id) REFERENCES lines(id),
		FOREIGN KEY(printer_id) REFERENCES printers(id)
	);`)

	// Журнал событий
	db.Exec(`CREATE TABLE IF NOT EXISTS event_log (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
		line_id INTEGER,
		printer_id INTEGER,
		event_type TEXT,
		message TEXT,
		FOREIGN KEY(line_id) REFERENCES lines(id),
		FOREIGN KEY(printer_id) REFERENCES printers(id)
	);`)

	// Таблица задач (Партий)
	db.Exec(`CREATE TABLE IF NOT EXISTS tasks (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        line_id INTEGER,
        template_name TEXT,
        dynamic_field_name TEXT, -- Добавлено
        status TEXT DEFAULT 'active', -- 'active', 'completed', 'stopped'
        created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
        FOREIGN KEY(line_id) REFERENCES lines(id)
    );`)

	// Таблица кодов с расширенными статусами и индексами SID
	db.Exec(`CREATE TABLE IF NOT EXISTS task_codes (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        task_id INTEGER,
        code TEXT NOT NULL,
        status TEXT DEFAULT 'pending', -- 'pending', 'in_buffer', 'printed'
        printer_index INTEGER,          -- Индекс, присвоенный в очереди принтера (SID)
        printed_at DATETIME,
        FOREIGN KEY(task_id) REFERENCES tasks(id)
    );`)

	// Таблица периодических снимков состояния
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS printer_telemetry (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
		printer_id INTEGER,
		cur_count TEXT,
		ribbon TEXT,
		status TEXT,
		template TEXT, -- Добавлено для аналитики смен
		FOREIGN KEY(printer_id) REFERENCES printers(id)
	);`)
	if err != nil {
		log.Printf("[DB ERROR] Таблица телеметрии не создана: %v", err)
	}

	// Индекс для быстрых отчетов по времени
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_telemetry_time ON printer_telemetry(timestamp);`)

	db.Exec(`CREATE INDEX IF NOT EXISTS idx_task_codes_status ON task_codes(task_id, status);`)
}

// runMigration перетаскивает данные из бэкапа прозрачно для пользователя
func runMigration(db *sql.DB) {
	// 1. Создаем дефолтную линию
	res, err := db.Exec(`INSERT INTO lines (name, description) VALUES ('Линия 1 (Авто-миграция)', 'Создана при обновлении системы')`)
	if err != nil {
		log.Printf("Ошибка создания дефолтной линии: %v", err)
		return
	}

	defaultLineID, err := res.LastInsertId()
	if err != nil {
		log.Printf("Ошибка получения ID линии: %v", err)
		return
	}

	// 2. Читаем старые принтеры
	rows, err := db.Query("SELECT name, ip, port, driver_type FROM printers_v1_backup")
	if err != nil {
		log.Printf("Ошибка чтения бэкапа: %v", err)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var name, ip, driver string
		var port int
		if err := rows.Scan(&name, &ip, &port, &driver); err != nil {
			continue // Пропускаем битую запись
		}

		// 3. Записываем в новую таблицу принтеров
		pRes, err := db.Exec(`INSERT INTO printers (name, ip, port, driver_type) VALUES (?, ?, ?, ?)`, name, ip, port, driver)
		if err != nil {
			log.Printf("Ошибка переноса принтера %s: %v", name, err)
			continue
		}

		newPrinterID, _ := pRes.LastInsertId()

		// 4. Вяжем принтер к дефолтной линии
		_, err = db.Exec(`INSERT INTO line_printers (line_id, printer_id, role) VALUES (?, ?, ?)`, defaultLineID, newPrinterID, "PRIMARY")
		if err != nil {
			log.Printf("Ошибка привязки принтера к линии: %v", err)
		}
	}

	// 5. Удаляем бэкап
	db.Exec("DROP TABLE printers_v1_backup")
	log.Println("=== МИГРАЦИЯ УСПЕШНО ЗАВЕРШЕНА ===")
}

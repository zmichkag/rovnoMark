package storage

import (
	"database/sql"
	"log"
	"log/slog"
	"rovnoMark/internal/models"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

// CreateTask Создание задачи с поддержкой динамических полей и статики
func (s *Store) CreateTask(lineID int, template, dynamicField, staticJSON string) (int64, error) {
	res, err := s.db.Exec(`
        INSERT INTO tasks (line_id, template_name, dynamic_field_name, static_fields_json, status) 
        VALUES (?, ?, ?, ?, 'ready')`,
		lineID, template, dynamicField, staticJSON)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateCodeStatus переводит код из 'pending' в 'in_buffer' и присваивает ему индекс принтера
func (s *Store) UpdateCodeStatus(taskID int, status string, printerID int, limit int) error {
	// Мы обновляем статус только для конкретной задачи и конкретного принтера,
	// строго соблюдая порядок индексов (printer_index).
	query := `
		UPDATE task_codes 
		SET status = ? 
		WHERE id IN (
			SELECT id FROM task_codes 
			WHERE task_id = ? AND printer_id = ? AND status = 'pending' 
			ORDER BY printer_index ASC 
			LIMIT ?
		)`

	_, err := s.db.Exec(query, status, taskID, printerID, limit)
	if err != nil {
		slog.Error("SQL: Ошибка обновления статуса кодов", "err", err, "task", taskID, "printer", printerID)
	}
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

// GetAssignments возвращает список всех привязок линий к принтерам с их ролями и ID
func (s *Store) GetAssignments() ([]map[string]interface{}, error) {
	query := `
		SELECT 
			l.id as line_id, 
			l.name as line_name, 
			p.id as printer_id, 
			p.name as printer_name, 
			lp.role 
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
		var lineID, printerID int
		var lName, pName, role string

		// Сканируем 5 полей в том же порядке, что и в SELECT
		if err := rows.Scan(&lineID, &lName, &printerID, &pName, &role); err != nil {
			log.Printf("ОШИБКА SCAN В ПРИВЯЗКАХ: %v", err)
			continue
		}

		result = append(result, map[string]interface{}{
			"line_id":      lineID,
			"line_name":    lName,
			"printer_id":   printerID,
			"printer_name": pName,
			"role":         role,
		})
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

// GetActiveTaskByLine возвращает ID активной задачи для линии, если она есть
func (s *Store) GetActiveTaskByLine(lineID int) (int, error) {
	var taskID int
	query := `SELECT id FROM tasks WHERE line_id = ? AND status = 'active' LIMIT 1`
	err := s.db.QueryRow(query, lineID).Scan(&taskID)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return taskID, err
}

// GetTaskStatus возвращает текущий статус задачи для контроля остановки накачки
func (s *Store) GetTaskStatus(taskID int) (string, error) {
	var status string
	err := s.db.QueryRow("SELECT status FROM tasks WHERE id = ?", taskID).Scan(&status)
	return status, err
}

// GetTaskDynamicField возвращает имя динамического поля для сериализации
func (s *Store) GetTaskDynamicField(taskID int) (string, error) {
	var field string
	err := s.db.QueryRow("SELECT dynamic_field_name FROM tasks WHERE id = ?", taskID).Scan(&field)
	return field, err
}

// GetLineIDByTask потом добавтиь метод проверки задачи, чтобы не мучить main запросами
func (s *Store) GetLineIDByTask(taskID int) (int, error) {
	var lineID int
	err := s.db.QueryRow("SELECT line_id FROM tasks WHERE id = ?", taskID).Scan(&lineID)
	return lineID, err
}

// GetActiveTasks возвращает расширенный список задач с фильтрацией и полной статистикой
func (s *Store) GetActiveTasks(lineID, printerID int) ([]map[string]interface{}, error) {
	// Базовый запрос со всеми полями и подзапросами для счетчиков
	query := `
		SELECT 
			t.id, 
			t.line_id, 
			COALESCE(l.name, 'Неизвестная линия') as line_name, 
			t.template_name, 
			COALESCE(t.dynamic_field_name, '') as dynamic_field_name, 
			t.status, 
			t.created_at,
			(SELECT COUNT(*) FROM task_codes WHERE task_id = t.id) as total_codes,
			(SELECT COUNT(*) FROM task_codes WHERE task_id = t.id AND status = 'printed') as printed_codes,
			(SELECT COUNT(*) FROM task_codes WHERE task_id = t.id AND status = 'in_buffer') as buffered_codes
		FROM tasks t
		LEFT JOIN lines l ON t.line_id = l.id
		WHERE t.status IN ('active', 'ready')`

	var args []interface{}

	// Добавляем фильтры динамически
	if lineID > 0 {
		query += " AND t.line_id = ?"
		args = append(args, lineID)
	}

	if printerID > 0 {
		// Ищем задачи на линии, к которой привязан данный принтер
		query += " AND t.line_id IN (SELECT line_id FROM line_printers WHERE printer_id = ?)"
		args = append(args, printerID)
	}

	query += " ORDER BY t.id DESC"

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []map[string]interface{}
	for rows.Next() {
		var id, lID, total, printed, buffered int
		var lName, template, dynamic, status, created string

		if err := rows.Scan(&id, &lID, &lName, &template, &dynamic, &status, &created, &total, &printed, &buffered); err != nil {
			continue
		}

		// Собираем полный JSON-объект обратно
		result = append(result, map[string]interface{}{
			"task_id":            id,
			"line_id":            lID,
			"line_name":          lName,
			"template_name":      template,
			"dynamic_field_name": dynamic,
			"status":             status,
			"created_at":         created,
			"stats": map[string]int{
				"total":    total,
				"printed":  printed,
				"buffered": buffered, // Тот самый параметр, который важен для контроля "насоса"
			},
		})
	}

	if result == nil {
		result = make([]map[string]interface{}, 0)
	}

	return result, nil
}

// TryActivateTask проверяет, находится ли задача в статусе 'ready',
// и если да — переводит её в 'active'. Возвращает true, если активация произошла.
func (s *Store) TryActivateTask(taskID int) (bool, error) {
	res, err := s.db.Exec(`
		UPDATE tasks 
		SET status = 'active' 
		WHERE id = ? AND status = 'ready'`, taskID)
	if err != nil {
		return false, err
	}
	affected, _ := res.RowsAffected()
	return affected > 0, nil
}

// AppendTaskCodes вставляет пачку кодов в статусе 'pending' без привязки к принтеру
func (s *Store) AppendTaskCodes(taskID int, codes []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
		INSERT INTO task_codes (task_id, code, status, printer_id, printer_index) 
		VALUES (?, ?, 'pending', NULL, NULL)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, code := range codes {
		if _, err := stmt.Exec(taskID, code); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// FetchAndAssignCodes атомарно забирает коды, назначает им ID принтера, порядковый индекс и ставит статус 'in_buffer'
func (s *Store) FetchAndAssignCodes(taskID int, printerID int, limit int) ([]models.TaskCode, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// 1. Узнаем последний индекс ИМЕННО ЭТОГО принтера в этой задаче
	var lastIndex int
	tx.QueryRow(`SELECT COALESCE(MAX(printer_index), -1) FROM task_codes 
	             WHERE task_id = ? AND printer_id = ?`, taskID, printerID).Scan(&lastIndex)

	// 2. Выбираем свободные коды
	rows, err := tx.Query(`
		SELECT id, code FROM task_codes 
		WHERE task_id = ? AND status = 'pending' AND printer_id IS NULL 
		ORDER BY id ASC LIMIT ?`, taskID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []models.TaskCode
	for rows.Next() {
		var tc models.TaskCode
		rows.Scan(&tc.ID, &tc.Code)
		list = append(list, tc)
	}

	if len(list) == 0 {
		return nil, nil
	}

	// 3. Присваиваем этим кодам ID принтера и индексы
	for i, tc := range list {
		nextIdx := lastIndex + 1 + i
		tx.Exec(`UPDATE task_codes SET printer_id = ?, printer_index = ?, status = 'in_buffer' 
		         WHERE id = ?`, printerID, nextIdx, tc.ID)
		list[i].PrinterIndex = nextIdx
	}

	return list, tx.Commit()
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
func (s *Store) GetAllPrinters() ([]models.PrinterConfig, error) {
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

// New запускаемся, чекаем базу на предмет актуальности версии и наличия нужных таблиц.
func New(path string) *Store {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		log.Fatal("Ошибка открытия БД:", err)
	}
	// --- АНТИ-БЛОКИРОВОЧНАЯ МАГИЯ SQLITE ---

	// 1. Ограничиваем пул (все запросы идут через одно соединение, строго в очередь)
	db.SetMaxOpenConns(1)

	// 2. Включаем WAL (Write-Ahead Logging) для быстрой работы
	db.Exec("PRAGMA journal_mode = WAL;")

	// 3. Если база заблокирована на долю секунды, ждем до 5 секунд вместо ошибки 500
	db.Exec("PRAGMA busy_timeout = 5000;")

	// 4. Оптимизация записи на диск (в связке с WAL дает прирост скорости)
	db.Exec("PRAGMA synchronous = NORMAL;")

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

// SetTaskStatus меняет статус всей партии (например, на 'completed' или 'stopped')
func (s *Store) SetTaskStatus(taskID int, status string) error {
	_, err := s.db.Exec(`UPDATE tasks SET status = ? WHERE id = ?`, status, taskID)
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

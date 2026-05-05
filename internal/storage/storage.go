package storage

import (
	"database/sql"
	"log"
	"rovnoMark/internal/core"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

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

	// Индексы
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_event_log_time ON event_log(timestamp);`)
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

// GetAllActivePrinters (для инициализации менеджера при запуске)
func (s *Store) GetAllPrinters() ([]core.PrinterConfig, error) {
	rows, err := s.db.Query("SELECT id, name, ip, port, driver_type, is_active FROM printers WHERE is_deleted = 0 and is_active = 1")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []core.PrinterConfig
	for rows.Next() {
		var p core.PrinterConfig
		if err := rows.Scan(&p.ID, &p.Name, &p.IP, &p.Port, &p.DriverType, &p.IsActive); err != nil {
			log.Printf("ОШИБКА ЧТЕНИЯ ПРИНТЕРА ИЗ БД: %v", err)
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

func (s *Store) SavePrinter(p core.PrinterConfig) (int64, error) {
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
func (s *Store) SaveLine(l core.LineConfig) error {
	query := `INSERT OR REPLACE INTO lines (id, name, description, is_active) VALUES (?, ?, ?, ?)`
	var id interface{} = l.ID
	if l.ID == 0 {
		id = nil
	}
	_, err := s.db.Exec(query, id, l.Name, l.Description, l.IsActive)
	return err
}

// Получить все активные линии
func (s *Store) GetAllLines() ([]core.LineConfig, error) {
	rows, err := s.db.Query("SELECT id, name, description, is_active FROM lines WHERE is_deleted = 0")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []core.LineConfig
	for rows.Next() {
		var l core.LineConfig
		rows.Scan(&l.ID, &l.Name, &l.Description, &l.IsActive)
		list = append(list, l)
	}
	return list, nil
}

// Привязать принтер к линии
func (s *Store) AssignPrinterToLine(lineID, printerID int, role string) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO line_printers (line_id, printer_id, role) VALUES (?, ?, ?)`,
		lineID, printerID, role)
	return err
}

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

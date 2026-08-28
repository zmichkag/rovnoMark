package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"rovnoMark/internal/models"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

// CreateTask Создание задачи с поддержкой динамических полей и статики
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

// GetActiveTaskByLine возвращает ID активной или ожидающей задачи для линии
func (s *Store) GetActiveTaskByLine(lineID int) (int, error) {
	var taskID int
	query := `SELECT id FROM tasks WHERE line_id = ? AND status IN ('active', 'ready') LIMIT 1`
	err := s.db.QueryRow(query, lineID).Scan(&taskID)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return taskID, err
}

// GetAllLines возвращает только активные и не удаленные линии, отсортированные по имени
func (s *Store) GetAllLines() ([]models.LineConfig, error) {
	// Добавлен фильтр is_active = 1 и сортировка ORDER BY name ASC
	query := `
		SELECT id, name, description, is_active 
		FROM lines 
		WHERE is_deleted = 0 AND is_active = 1 
		ORDER BY name ASC`

	rows, err := s.db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []models.LineConfig
	for rows.Next() {
		var l models.LineConfig
		if err := rows.Scan(&l.ID, &l.Name, &l.Description, &l.IsActive); err != nil {
			continue
		}
		list = append(list, l)
	}
	return list, nil
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

// GetEventLogsHistory извлекает события с учетом фильтрации по дате, типу и линии
func (s *Store) GetEventLogsHistory(filter models.LogFilter) ([]models.EventLogItem, error) {
	query := `
		SELECT 
			e.id, 
			e.timestamp, 
			e.line_id, 
			COALESCE(l.name, '') as line_name,
			e.printer_id, 
			COALESCE(p.name, 'Система') as printer_name, 
			e.event_type, 
			e.message
		FROM event_log e
		LEFT JOIN lines l ON e.line_id = l.id
		LEFT JOIN printers p ON e.printer_id = p.id
		WHERE 1=1`

	var args []interface{}

	// Фильтр по линии
	if filter.LineID > 0 {
		query += " AND e.line_id = ?"
		args = append(args, filter.LineID)
	}

	// Фильтр по принтеру
	if filter.PrinterID > 0 {
		query += " AND e.printer_id = ?"
		args = append(args, filter.PrinterID)
	}

	// Фильтр по типу события (error, warn, info, success)
	if filter.EventType != "" {
		query += " AND e.event_type = ?"
		args = append(args, filter.EventType)
	}

	// Фильтр по диапазону дат (смене)
	if !filter.DateFrom.IsZero() {
		query += " AND e.timestamp >= ?"
		args = append(args, filter.DateFrom.Format("2006-01-02 15:04:05"))
	}
	if !filter.DateTo.IsZero() {
		query += " AND e.timestamp <= ?"
		args = append(args, filter.DateTo.Format("2006-01-02 15:04:05"))
	}

	query += " ORDER BY e.id DESC"

	// Лимиты пагинации
	if filter.Limit <= 0 {
		filter.Limit = 100 // По умолчанию 100 записей
	}
	query += " LIMIT ?"
	args = append(args, filter.Limit)

	if filter.Offset > 0 {
		query += " OFFSET ?"
		args = append(args, filter.Offset)
	}

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("ошибка получения истории логов: %w", err)
	}
	defer rows.Close()

	var logs []models.EventLogItem
	for rows.Next() {
		var item models.EventLogItem
		var lID, pID sql.NullInt64

		err := rows.Scan(
			&item.ID,
			&item.Timestamp,
			&lID,
			&item.LineName,
			&pID,
			&item.Printer,
			&item.EventType,
			&item.Message,
		)
		if err != nil {
			slog.Error("Scan error event_log", "err", err)
			continue
		}

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

	if logs == nil {
		logs = make([]models.EventLogItem, 0)
	}

	return logs, nil
}

// GetLiveDashboardData собирает актуальное состояние завода для планшетов
func (s *Store) GetLiveDashboardData() (map[string]interface{}, error) {
	// 1. Извлекаем только активные и не удаленные линии (благодаря обновленному GetAllLines)
	lines, err := s.GetAllLines()
	if err != nil {
		return nil, fmt.Errorf("ошибка получения линий: %w", err)
	}

	// 2. Получаем карту привязок принтеров
	assignments, err := s.GetAssignments()
	if err != nil {
		return nil, fmt.Errorf("ошибка получения привязок: %w", err)
	}

	linePrintersMap := make(map[int][]int)
	for _, a := range assignments {
		lID := a["line_id"].(int)
		pID := a["printer_id"].(int)
		linePrintersMap[lID] = append(linePrintersMap[lID], pID)
	}

	// 3. Получаем все активные задачи
	activeTasks, _ := s.GetActiveTasks(0, 0)
	taskByLineMap := make(map[int]map[string]interface{})
	for _, t := range activeTasks {
		lID := t["line_id"].(int)
		taskByLineMap[lID] = t
	}

	// 4. Формируем список линий
	var linesData []map[string]interface{}
	totalActiveTasks := 0

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
			lineObj["status"] = task["status"] // 'active' или 'ready'
			totalActiveTasks++
		} else {
			lineObj["current_task"] = nil
			lineObj["status"] = "IDLE" // Простой
		}

		linesData = append(linesData, lineObj)
	}

	summary := map[string]interface{}{
		"total_lines":  len(lines),
		"active_tasks": totalActiveTasks,
		"idle_lines":   len(lines) - totalActiveTasks,
	}

	return map[string]interface{}{
		"timestamp": time.Now().Format(time.RFC3339),
		"summary":   summary,
		"lines":     linesData,
	}, nil
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

// GetPendingCodes извлекает заданное количество неотпечатанных кодов для конкретной задачи
func (s *Store) GetPendingCodes(taskID int, limit int) ([]models.TaskCode, error) {
	query := `
		SELECT id, task_id, code, status, COALESCE(printer_id, 0), COALESCE(printer_index, 0) 
		FROM task_codes 
		WHERE task_id = ? AND status = 'pending' 
		ORDER BY id ASC 
		LIMIT ?`

	rows, err := s.db.Query(query, taskID, limit)
	if err != nil {
		return nil, fmt.Errorf("ошибка выборки pending кодов из БД: %w", err)
	}
	defer rows.Close()

	var codes []models.TaskCode
	for rows.Next() {
		var c models.TaskCode
		if err := rows.Scan(&c.ID, &c.TaskID, &c.Code, &c.Status, &c.PrinterID, &c.PrinterIndex); err != nil {
			return nil, fmt.Errorf("ошибка сканирования строки task_codes: %w", err)
		}
		codes = append(codes, c)
	}
	return codes, nil
}

func (s *Store) GetTaskStaticFieldsJSON(taskID int) (string, error) {
	var staticJSON string
	err := s.db.QueryRow("SELECT static_fields_json FROM tasks WHERE id = ?", taskID).Scan(&staticJSON)
	return staticJSON, err
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

func (s *Store) GetRndTextByTask(taskID int) (string, error) {
	var rndText string
	err := s.db.QueryRow("SELECT rnd_text FROM tasks WHERE id = ?", taskID).Scan(&rndText)
	return rndText, err
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
			COALESCE(t.rnd_text, '') as rnd_text, 
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
		var lName, template, dynamic, status, created, rndText string // Добавлена переменная rndText

		if err := rows.Scan(&id, &lID, &lName, &template, &dynamic, &status, &created, &rndText, &total, &printed, &buffered); err != nil {
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
			"rnd_text":           rndText, // Отдаем в JSON
			"stats": map[string]int{
				"total":    total,
				"printed":  printed,
				"buffered": buffered,
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

func (s *Store) FetchAndAssignCodes(taskID int, printerID int, limit int) ([]models.TaskCode, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// Узнаем последний индекс конкретного принтера в этой задаче
	var lastIndex int
	tx.QueryRow(`SELECT COALESCE(MAX(printer_index), 0) FROM task_codes 
	             WHERE task_id = ? AND printer_id = ?`, taskID, printerID).Scan(&lastIndex)

	// Выбираем свободные коды
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

	// Присваиваем независимые индексы каждому принтеру отдельно
	for i, tc := range list {
		nextIdx := lastIndex + 1 + i
		tx.Exec(`UPDATE task_codes SET printer_id = ?, printer_index = ?, status = 'in_buffer' 
		         WHERE id = ?`, printerID, nextIdx, tc.ID)
		list[i].PrinterIndex = nextIdx
	}

	return list, tx.Commit()
}

// Синхронизация статуса 'printed' на основе индекса от принтера
func (s *Store) MarkAsPrinted(taskID int, printerID int, lastIndex int) (int64, error) {
	res, err := s.db.Exec(`
		UPDATE task_codes 
		SET status = 'printed', printed_at = CURRENT_TIMESTAMP 
		WHERE task_id = ? AND printer_id = ? AND printer_index <= ? AND status = 'in_buffer'`,
		taskID, printerID, lastIndex)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
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

// AssignPrinterToLine Привязать принтер к линии
func (s *Store) AssignPrinterToLine(lineID, printerID int, role string) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO line_printers (line_id, printer_id, role) VALUES (?, ?, ?)`,
		lineID, printerID, role)
	return err
}

// GetCodePassport возвращает паспорт отпечатанного или загруженного кода для терминала ОКК
func (s *Store) GetCodePassport(code string) (map[string]interface{}, error) {
	// Нормализуем спецсимволы
	cleanCode := strings.ReplaceAll(code, "\x1d", "<GS>")

	query := `
		SELECT 
			tc.task_id, 
			tc.status, 
			COALESCE(tc.printed_at, 'Не отпечатан') as printed_at,
			t.template_name, 
			COALESCE(l.name, '—') as line_name
		FROM task_codes tc
		JOIN tasks t ON tc.task_id = t.id
		LEFT JOIN lines l ON t.line_id = l.id
		WHERE tc.code = ? OR tc.code LIKE ?
		ORDER BY tc.id DESC LIMIT 1`

	var taskID int
	var status, printedAt, templateName, lineName string

	err := s.db.QueryRow(query, cleanCode, "%"+cleanCode+"%").Scan(&taskID, &status, &printedAt, &templateName, &lineName)
	if err != nil {
		return nil, err
	}

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

	db.Exec("ALTER TABLE tasks ADD COLUMN rnd_text TEXT DEFAULT '';")

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

// SaveEventLog сохраняет инцидент или системный шаг в SQLite
func (s *Store) SaveEventLog(lineID *int, printerID *int, eventType string, message string) error {
	query := `
		INSERT INTO event_log (line_id, printer_id, event_type, message) 
		VALUES (?, ?, ?, ?)`

	var lID, pID interface{}
	if lineID != nil && *lineID > 0 {
		lID = *lineID
	}
	if printerID != nil && *printerID > 0 {
		pID = *printerID
	}

	_, err := s.db.Exec(query, lID, pID, eventType, message)
	if err != nil {
		slog.Error("SQL Error: Ошибка записи в event_log", "err", err)
	}
	return err
}

// SetTaskStatus меняет статус всей партии (например, на 'completed' или 'stopped')
func (s *Store) SetTaskStatus(taskID int, status models.TaskState) error {
	_, err := s.db.Exec(`UPDATE tasks SET status = ? WHERE id = ?`, status, taskID)
	return err
}

// GetTaskInfo возвращает агрегированную информацию по задаче маркировки
func (s *Store) GetTaskInfo(ctx context.Context, taskID int) (map[string]interface{}, error) {
	query := `
		SELECT 
			t.id AS task_id,
			t.line_id,
			COALESCE(l.name, 'Неизвестная линия') AS line_name,
			t.template_name,
			t.status AS task_status,
			t.created_at AS started_at,
			
			MAX(tc.printed_at) AS last_code_printed_at,
			
			(
				SELECT e.timestamp 
				FROM event_log e 
				WHERE e.line_id = t.line_id 
				  AND e.message LIKE '%' || CAST(t.id AS TEXT) || '%' 
				  AND (e.message LIKE '%stopped%' OR e.message LIKE '%остановк%')
				ORDER BY e.id DESC 
				LIMIT 1
			) AS stop_event_at,

			COUNT(tc.id) AS total_codes,
			COUNT(CASE WHEN tc.status = 'printed' THEN 1 END) AS printed_count,
			COUNT(CASE WHEN tc.status = 'in_buffer' THEN 1 END) AS in_buffer_count,
			COUNT(CASE WHEN tc.status = 'pending' THEN 1 END) AS pending_count

		FROM tasks t
		LEFT JOIN lines l ON t.line_id = l.id
		LEFT JOIN task_codes tc ON t.id = tc.task_id
		WHERE t.id = ?;`

	var (
		tID, lineID                                           int
		lineName, templateName, taskStatus, startedAt         string
		lastPrintedAt, stopEventAt                            sql.NullString
		totalCodes, printedCount, inBufferCount, pendingCount int
	)

	err := s.db.QueryRowContext(ctx, query, taskID).Scan(
		&tID, &lineID, &lineName, &templateName, &taskStatus, &startedAt,
		&lastPrintedAt, &stopEventAt,
		&totalCodes, &printedCount, &inBufferCount, &pendingCount,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("task %d not found", taskID)
		}
		return nil, err
	}

	result := map[string]interface{}{
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
	}

	return result, nil
}

// UpdateCodeStatusByID атомарно обновляет статус и индекс конкретного кода по его уникальному ID (для Valentin)
func (s *Store) UpdateCodeStatusByID(id int, status string, printerIndex int) error {
	query := `UPDATE task_codes SET status = ?, printer_index = ? WHERE id = ?`
	_, err := s.db.Exec(query, status, printerIndex, id)
	if err != nil {
		return fmt.Errorf("сбой обновления статуса кода ID=%d: %w", id, err)
	}
	return nil
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
        dynamic_field_name TEXT,
        rnd_text TEXT, 
        status TEXT DEFAULT 'active', -- 'active', 'completed', 'stopped'
        created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
        static_fields_json TEXT, 
        FOREIGN KEY(line_id) REFERENCES lines(id)
    );`)

	// Таблица кодов с расширенными статусами и индексами SID
	db.Exec(`CREATE TABLE IF NOT EXISTS task_codes (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		task_id INTEGER,
		code TEXT NOT NULL,
		status TEXT DEFAULT 'pending',
		printer_id INTEGER,           
		printer_index INTEGER,          
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
		template TEXT, 
		FOREIGN KEY(printer_id) REFERENCES printers(id)
	);`)

	if err != nil {
		log.Printf("[DB ERROR] Таблица телеметрии не создана: %v", err)
	}

	// Индекс для быстрых отчетов по времени
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_telemetry_time ON printer_telemetry(timestamp);`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_task_codes_status ON task_codes(task_id, status);`)
	// Составные индексы для ускорения работы пагинации и поиска по фильтрам
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_event_log_composite ON event_log(line_id, event_type, timestamp);`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_event_log_time ON event_log(timestamp DESC);`)
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

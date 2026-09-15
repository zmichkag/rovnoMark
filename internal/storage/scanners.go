package storage

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"rovnoMark/internal/models"
	"strings"
	"time"
)

// SaveLineScanner создаёт конфигурацию сканера или обновляет существующую по ID.
func (s *Store) SaveLineScanner(scanner models.ScannerConfig) (int64, error) {
	if err := normalizeAndValidateScanner(&scanner); err != nil {
		return 0, err
	}

	if scanner.ID == 0 {
		result, err := s.db.Exec(`
			INSERT INTO line_scanners (
				line_id, name, driver_type, address, port, role,
				target_device_id, settings_json, is_active
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			scanner.LineID, scanner.Name, scanner.DriverType, scanner.Address,
			nullablePort(scanner.Port), scanner.Role, scanner.TargetDeviceID,
			scanner.SettingsJSON, scanner.IsActive,
		)
		if err != nil {
			return 0, fmt.Errorf("ошибка сохранения сканера %q: %w", scanner.Name, err)
		}
		return result.LastInsertId()
	}

	result, err := s.db.Exec(`
		UPDATE line_scanners
		SET line_id = ?, name = ?, driver_type = ?, address = ?, port = ?, role = ?,
			target_device_id = ?, settings_json = ?, is_active = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = ?`,
		scanner.LineID, scanner.Name, scanner.DriverType, scanner.Address,
		nullablePort(scanner.Port), scanner.Role, scanner.TargetDeviceID,
		scanner.SettingsJSON, scanner.IsActive, scanner.ID,
	)
	if err != nil {
		return 0, fmt.Errorf("ошибка обновления сканера ID=%d: %w", scanner.ID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("ошибка проверки обновления сканера ID=%d: %w", scanner.ID, err)
	}
	if affected == 0 {
		return 0, sql.ErrNoRows
	}
	return int64(scanner.ID), nil
}

// GetScannersByLine возвращает активные сканеры линии в стабильном порядке.
func (s *Store) GetScannersByLine(lineID int) ([]models.ScannerConfig, error) {
	if lineID <= 0 {
		return nil, fmt.Errorf("line_id должен быть положительным")
	}

	return s.queryScanners(`
		SELECT id, line_id, name, driver_type, address, COALESCE(port, 0), role,
			target_device_id, settings_json, is_active, created_at, updated_at
		FROM line_scanners
		WHERE line_id = ? AND is_active = 1
		ORDER BY id`, lineID)

}

// GetAllScanners возвращает все конфигурации для экрана настроек, включая выключенные.
func (s *Store) GetAllScanners() ([]models.ScannerConfig, error) {
	return s.queryScanners(`
		SELECT id, line_id, name, driver_type, address, COALESCE(port, 0), role,
			target_device_id, settings_json, is_active, created_at, updated_at
		FROM line_scanners
		ORDER BY line_id, id`)
}

func (s *Store) queryScanners(query string, args ...interface{}) ([]models.ScannerConfig, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("ошибка получения конфигураций сканеров: %w", err)
	}
	defer rows.Close()

	var scanners []models.ScannerConfig
	for rows.Next() {
		var scanner models.ScannerConfig
		if err := rows.Scan(
			&scanner.ID, &scanner.LineID, &scanner.Name, &scanner.DriverType,
			&scanner.Address, &scanner.Port, &scanner.Role, &scanner.TargetDeviceID,
			&scanner.SettingsJSON, &scanner.IsActive, &scanner.CreatedAt, &scanner.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("ошибка чтения конфигурации сканера: %w", err)
		}
		scanners = append(scanners, scanner)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ошибка обхода конфигураций сканеров: %w", err)
	}
	return scanners, nil
}

func normalizeAndValidateScanner(scanner *models.ScannerConfig) error {
	scanner.Name = strings.TrimSpace(scanner.Name)
	scanner.DriverType = strings.TrimSpace(scanner.DriverType)
	scanner.Address = strings.TrimSpace(scanner.Address)
	scanner.Role = models.ScannerRole(strings.ToUpper(strings.TrimSpace(string(scanner.Role))))
	scanner.SettingsJSON = strings.TrimSpace(scanner.SettingsJSON)
	if scanner.SettingsJSON == "" {
		scanner.SettingsJSON = "{}"
	}

	if scanner.LineID <= 0 {
		return fmt.Errorf("line_id должен быть положительным")
	}
	if scanner.Name == "" {
		return fmt.Errorf("имя сканера не задано")
	}
	if scanner.DriverType == "" {
		return fmt.Errorf("тип драйвера сканера не задан")
	}
	if scanner.Address == "" {
		return fmt.Errorf("адрес сканера не задан")
	}
	if scanner.Port < 0 || scanner.Port > 65535 {
		return fmt.Errorf("порт сканера должен быть в диапазоне 0..65535")
	}
	if scanner.TargetDeviceID != nil && *scanner.TargetDeviceID <= 0 {
		return fmt.Errorf("target_device_id должен быть положительным")
	}
	if scanner.Role == models.ScannerRoleInlineVerifier && scanner.TargetDeviceID == nil {
		return fmt.Errorf("для роли INLINE_VERIFIER требуется target_device_id")
	}
	switch scanner.Role {
	case models.ScannerRoleInlineVerifier, models.ScannerRoleAuditCheck, models.ScannerRoleAggregator:
	default:
		return fmt.Errorf("неподдерживаемая роль сканера %q", scanner.Role)
	}
	if !json.Valid([]byte(scanner.SettingsJSON)) {
		return fmt.Errorf("settings_json содержит некорректный JSON")
	}
	return nil
}

func nullablePort(port int) interface{} {
	if port == 0 {
		return nil
	}
	return port
}

// RecordScannerRead сохраняет успешное чтение в отдельном журнале сканера.
func (s *Store) RecordScannerRead(scanner models.ScannerConfig, code string, rawData []byte, readAt time.Time) (*models.ScannerRead, error) {
	code = strings.TrimSpace(strings.ReplaceAll(code, "\x1d", "<GS>"))
	if code == "" {
		return nil, fmt.Errorf("пустой код сканирования")
	}
	if readAt.IsZero() {
		readAt = time.Now()
	}
	readAt = readAt.UTC()
	if rawData == nil {
		rawData = []byte{}
	}

	taskID, err := s.GetActiveTaskByLine(scanner.LineID)
	if err != nil {
		return nil, fmt.Errorf("ошибка поиска активной задачи линии ID=%d: %w", scanner.LineID, err)
	}
	var nullableTaskID interface{}
	if taskID > 0 {
		nullableTaskID = taskID
	}

	result, err := s.db.Exec(`
		INSERT INTO scanner_reads (
			scanner_id, line_id, task_id, code, raw_data, match_status, read_at
		) VALUES (?, ?, ?, ?, ?, 'unmatched', ?)`,
		scanner.ID, scanner.LineID, nullableTaskID, code, rawData, readAt)
	if err != nil {
		return nil, fmt.Errorf("ошибка сохранения чтения сканера ID=%d: %w", scanner.ID, err)
	}
	readID, err := result.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("ошибка получения ID чтения сканера: %w", err)
	}

	read := &models.ScannerRead{
		ID:          int(readID),
		ScannerID:   scanner.ID,
		LineID:      scanner.LineID,
		Code:        code,
		MatchStatus: "unmatched",
		ReadAt:      readAt,
	}
	if taskID > 0 {
		read.TaskID = &taskID
	}
	return read, nil
}

// GetRecentScannerReads возвращает последние успешные чтения сканера.
func (s *Store) GetRecentScannerReads(scannerID, limit int) ([]models.ScannerRead, error) {
	if scannerID <= 0 {
		return nil, fmt.Errorf("scanner_id должен быть положительным")
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.Query(`
		SELECT id, scanner_id, line_id, task_id, task_code_id, code, match_status, read_at
		FROM scanner_reads
		WHERE scanner_id = ? AND match_status <> 'no_read'
		ORDER BY id DESC LIMIT ?`, scannerID, limit)
	if err != nil {
		return nil, fmt.Errorf("ошибка получения чтений сканера ID=%d: %w", scannerID, err)
	}
	defer rows.Close()

	reads := make([]models.ScannerRead, 0)
	for rows.Next() {
		var read models.ScannerRead
		if err := rows.Scan(&read.ID, &read.ScannerID, &read.LineID, &read.TaskID,
			&read.TaskCodeID, &read.Code, &read.MatchStatus, &read.ReadAt); err != nil {
			return nil, fmt.Errorf("ошибка чтения события сканера: %w", err)
		}
		reads = append(reads, read)
	}
	return reads, rows.Err()
}

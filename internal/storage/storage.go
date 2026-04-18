package storage

import (
	"database/sql"
	"log"
	"rovnoMark/internal/core" // Импортируем конфиг из ядра

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

func New(path string) *Store {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		log.Fatal(err)
	}

	// Создаем таблицу для парка принтеров
	query := `
	CREATE TABLE IF NOT EXISTS printers (
		id TEXT PRIMARY KEY,
		name TEXT,
		ip TEXT,
		port INTEGER,
		driver_type TEXT
	);`
	db.Exec(query)

	return &Store{db: db}
}

// SavePrinter сохраняет или обновляет принтер
func (s *Store) SavePrinter(p core.PrinterConfig) error {
	query := `INSERT OR REPLACE INTO printers (id, name, ip, driver_type) VALUES (?, ?, ?, ?)`
	_, err := s.db.Exec(query, p.ID, p.Name, p.IP, p.DriverType)
	return err
}

// GetAllPrinters вычитывает весь список для инициализации системы
func (s *Store) GetAllPrinters() ([]core.PrinterConfig, error) {
	rows, err := s.db.Query("SELECT id, name, ip, driver_type FROM printers")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []core.PrinterConfig
	for rows.Next() {
		var p core.PrinterConfig
		if err := rows.Scan(&p.ID, &p.Name, &p.IP, &p.DriverType); err == nil {
			list = append(list, p)
		}
	}
	return list, nil
}

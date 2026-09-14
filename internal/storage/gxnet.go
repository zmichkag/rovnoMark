package storage

import "time"

// SaveGXNETResponse сохраняет сырой ответ очереди DUSTBIN при включенном аудите
func (s *Store) SaveGXNETResponse(printerID int, device string, receivedAt time.Time, command, parameter, queue, payload string, status int) error {
	query := `INSERT INTO bizerba_responses (printer_id, device, received_at, command, parameter, queue, payload, status) 
	          VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
	_, err := s.db.Exec(query, printerID, device, receivedAt.UTC().Format("2006-01-02 15:04:05.000000000"), command, parameter, queue, payload, status)
	return err
}

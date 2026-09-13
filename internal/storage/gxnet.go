package storage

import "time"

// SaveGXNETResponse records the original BCS reply and its request context.
func (s *Store) SaveGXNETResponse(printerID int, device string, receivedAt time.Time, command, parameter, queue, payload string, status int) error {
	_, err := s.db.Exec(`INSERT INTO bizerba_responses
		(printer_id, device, received_at, command, parameter, queue, payload, status)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, printerID, device,
		receivedAt.UTC().Format("2006-01-02 15:04:05.000000000"), command, parameter, queue, payload, status)
	return err
}

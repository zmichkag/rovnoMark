package savema

import (
	"fmt"
	"net"
	"strings" // Обязательно добавляем пакет для работы со строками
	"time"
)

type Driver struct {
	Address string
	Timeout time.Duration
}

func New(ip string) *Driver {
	return &Driver{
		Address: ip + ":9100",
		Timeout: 3 * time.Second,
	}
}

func (d *Driver) SendCommand(cmdBody string) (string, error) {
	conn, err := net.DialTimeout("tcp", d.Address, d.Timeout)
	if err != nil {
		return "", err
	}
	defer conn.Close()

	conn.Write([]byte(fmt.Sprintf("~%s^", cmdBody)))
	conn.SetReadDeadline(time.Now().Add(d.Timeout))
	buffer := make([]byte, 1024)
	n, err := conn.Read(buffer)
	if err != nil {
		return "", err
	}
	return string(buffer[:n]), nil
}

// CleanResponse — наш "очиститель" протокола
func CleanResponse(raw string) string {
	// 1. Убираем пробелы и переносы каретки (\r, \n)
	clean := strings.TrimSpace(raw)

	start := strings.Index(clean, ":")
	end := strings.Index(clean, "}")
	if start != -1 && end != -1 {
		clean = clean[start+1 : end]
		return clean
	}

	return clean
}

// GetStatus запрашивает статус и переводит его на русский
func (d *Driver) GetStatus() (string, error) {
	raw, err := d.SendCommand("SPPSTA")
	if err != nil {
		return "", err
	}

	clean := CleanResponse(raw)
	upperStatus := strings.ToUpper(clean)

	// Маппинг (Словарь) статусов принтера INIT, WAITING, RUNNING and ERROR
	switch {
	case strings.Contains(upperStatus, "INIT") || clean == "00":
		return "ЗАПУСК", nil
	case strings.Contains(upperStatus, "RUNNING") || clean == "01":
		return "ПЕЧАТАЕТ", nil
	case strings.Contains(upperStatus, "WAITING"):
		return "ВНИМАНИЕ (ПРЕДУПРЕЖДЕНИЕ)", nil
	case strings.Contains(upperStatus, "ERROR") || strings.Contains(upperStatus, "ERROR"):
		return "ОШИБКА ОБОРУДОВАНИЯ", nil
	default:
		// Если статус неизвестен, возвращаем его очищенным
		return clean, nil
	}
}

// GetRemainingRibbon возвращает только чистое число (метры)
func (d *Driver) GetRemainingRibbon() (string, error) {
	raw, err := d.SendCommand("SPGGRR")
	if err != nil {
		return "", err
	}
	return CleanResponse(raw), nil
}

// GetQueueCapacity возвращает только чистое число кодов
func (d *Driver) GetQueueCapacity(queueName string) (string, error) {
	raw, err := d.SendCommand(fmt.Sprintf("SPLGMQ{%s}", queueName))
	if err != nil {
		return "", err
	}
	return CleanResponse(raw), nil
}

func (d *Driver) PrintTemplate(template string, fields map[string]string) error {
	_, err := d.SendCommand(fmt.Sprintf("SPLLTF{%s}", template))
	if err != nil {
		return err
	}
	for k, v := range fields {
		d.SendCommand(fmt.Sprintf("SPMCTV{%s~gt~%s}", k, v))
	}
	return nil
}

func (d *Driver) GetPrintSpeed() (string, error) { // Скобки для параметров, потом типы возврата
	raw, err := d.SendCommand("SPCGPS")
	if err != nil {
		return "", err
	}
	return CleanResponse(raw), nil
}

func (d *Driver) GetCurrentPrintCount() (string, error) {
	raw, err := d.SendCommand("SPGGCP")
	if err != nil {
		return "", err
	}
	return CleanResponse(raw), nil
}

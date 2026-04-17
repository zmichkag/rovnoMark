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
	// 2. Отрезаем тильду и крышку (~ ... ^)
	clean = strings.TrimPrefix(clean, "~")
	clean = strings.TrimSuffix(clean, "^")

	// 3. В SPPL успешный ответ часто идет в формате "00|Данные"
	parts := strings.SplitN(clean, "|", 2)
	if len(parts) == 2 {
		return parts[1] // Возвращаем только полезные данные после пайпа
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

	// Маппинг (Словарь) статусов принтера
	switch {
	case strings.Contains(upperStatus, "READY") || clean == "00":
		return "ГОТОВ", nil
	case strings.Contains(upperStatus, "PRINTING") || clean == "01":
		return "ПЕЧАТАЕТ", nil
	case strings.Contains(upperStatus, "WARNING"):
		return "ВНИМАНИЕ (ПРЕДУПРЕЖДЕНИЕ)", nil
	case strings.Contains(upperStatus, "FAULT") || strings.Contains(upperStatus, "ERROR"):
		return "ОШИБКА ОБОРУДОВАНИЯ", nil
	default:
		// Если статус неизвестен, возвращаем его очищенным,
		// чтобы ты мог увидеть его в UI и потом добавить в этот switch
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

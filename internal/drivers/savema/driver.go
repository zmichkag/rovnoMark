package savema

import (
	"fmt"
	"net"
	"time"
)

// Driver реализует интерфейс core.Printer
type Driver struct {
	Address string
	Timeout time.Duration
}

func New(ip string) *Driver {
	return &Driver{
		Address: ip + ":9100", // Savema обычно работает по порту 9100
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

// GetStatus запрашивает статус (реализует интерфейс)
func (d *Driver) GetStatus() (string, error) {
	// Используем команду SPPSTA из протокола SPPL
	return d.SendCommand("SPPSTA")
}

// PrintTemplate реализует печать (реализует интерфейс)
func (d *Driver) PrintTemplate(template string, fields map[string]string) error {
	// 1. Загрузка шаблона
	_, err := d.SendCommand(fmt.Sprintf("SPLLTF{%s}", template))
	if err != nil {
		return err
	}
	// 2. Установка полей
	for k, v := range fields {
		d.SendCommand(fmt.Sprintf("SPMCTV{%s~gt~%s}", k, v))
	}
	return nil
}

// GetRemainingRibbon запрашивает остаток риббона
func (d *Driver) GetRemainingRibbon() (string, error) {
	// Возвращает число (обычно в метрах)
	return d.SendCommand("SPGGRR")
}

// GetQueueCapacity запрашивает количество кодов в очереди
func (d *Driver) GetQueueCapacity(queueName string) (string, error) {
	// Синтаксис: SPLGMQ{ИмяПоля}
	return d.SendCommand(fmt.Sprintf("SPLGMQ{%s}", queueName))
}

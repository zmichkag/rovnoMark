package videojet

import (
	"bufio"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Driver struct {
	Address string
	Port    int
	Timeout time.Duration
	mu      sync.Mutex // Защита TCP-канала от одновременных запросов
}

func New(ip string, port int) *Driver {
	return &Driver{
		Address: ip,
		Port:    port,
		Timeout: 3 * time.Second,
	}
}

// sendRaw
func (d *Driver) sendRaw(cmd string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	// Склеиваем IP и Port. %d — формат для целого числа.
	address := net.JoinHostPort(d.Address, strconv.Itoa(d.Port))

	conn, err := net.DialTimeout("tcp", address, d.Timeout)
	if err != nil {
		return "", err
	}
	defer conn.Close()

	conn.Write([]byte(cmd + "\r"))

	// Videojet требует терминатор \r
	_, err = conn.Write([]byte(cmd + "\r"))
	if err != nil {
		return "", err
	}

	conn.SetReadDeadline(time.Now().Add(d.Timeout))
	// Читаем до символа \r (терминатор ответа)
	reader := bufio.NewReader(conn)
	reply, err := reader.ReadString('\r')
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(reply), nil
}

// GetStatus запрашивает GST и разбирает состояние [cite: 426]
func (d *Driver) GetStatus() (string, error) {
	// Ответ выглядит так: STS |overall|error|job|batch|total|
	raw, err := d.sendRaw("GST")
	if err != nil {
		return "", err
	}

	parts := strings.Split(raw, "|")
	if len(parts) < 2 {
		return "ОШИБКА ПРОТОКОЛА", nil
	}

	// overallstate: 0-Shutdown, 3-Running, 4-Offline [cite: 1256-1260]
	// errorstate: 0-No errors, 1-Warnings, 2-Faults [cite: 1300-1302]
	stateCode := parts[1]
	errorCode := ""
	if len(parts) > 2 {
		errorCode = parts[2]
	}

	switch stateCode {
	case "0":
		return "ВЫКЛЮЧЕН", nil
	case "1":
		return "ЗАПУСК", nil
	case "2":
		return "ОСТАНОВКА", nil
	case "3":
		if errorCode == "2" {
			return "ПЕЧАТЬ (ОШИБКА)", nil
		}
		return "ГОТОВ / ПЕЧАТЬ", nil
	case "4":
		return "ОФФЛАЙН", nil
	default:
		return "НЕИЗВЕСТНО", nil
	}
}

// GetRemainingRibbon использует команду GCL (Consumable Levels) [cite: 1086]
func (d *Driver) GetRemainingRibbon() (string, error) {
	raw, err := d.sendRaw("GCL")
	if err != nil {
		return "", err
	}
	// Формат ответа: GCL <уровень> [cite: 1102]
	return strings.TrimPrefix(raw, "GCL "), nil
}

// GetQueueCapacity запрашивает QSZ (Queue Size) [cite: 673]
func (d *Driver) GetQueueCapacity(queueName string) (string, error) {
	raw, err := d.sendRaw("QSZ")
	if err != nil {
		return "", err
	}
	// Ответ: QSZ | <nn> | <s> | [cite: 678]
	parts := strings.Split(raw, "|")
	if len(parts) >= 2 {
		return strings.TrimSpace(parts[1]), nil
	}
	return "0", nil
}

// PrintTemplate выполняет выбор задания (SLA) и команду печати (PRN) [cite: 123, 347]
func (d *Driver) PrintTemplate(template string, fields map[string]string) error {
	// 1. Формируем команду выбора задания с полями [cite: 123]
	// SLA |имя|поле1=значение|поле2=значение|
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("SLA|%s", template))
	for k, v := range fields {
		sb.WriteString(fmt.Sprintf("|%s=%s", k, v))
	}
	sb.WriteString("|")

	res, err := d.sendRaw(sb.String())
	if err != nil || res == "ERR" {
		return fmt.Errorf("ошибка выбора задания: %v", err)
	}

	// 2. Команда на физическую печать [cite: 347]
	_, err = d.sendRaw("PRN")
	return err
}

func (d *Driver) GetPrintSpeed() (string, error) {
	return "N/A", nil // В текстовом протоколе нет прямой команды скорости
}

func (d *Driver) GetCurrentPrintCount() (string, error) {
	raw, err := d.sendRaw("GPC") // Get Counts [cite: 567]
	if err != nil {
		return "", err
	}
	// PCS <success>|<fail>|<missed>|<remaining> [cite: 572]
	parts := strings.Split(raw, "|")
	if len(parts) >= 1 {
		return strings.TrimPrefix(parts[0], "PCS "), nil
	}
	return "0", nil
}

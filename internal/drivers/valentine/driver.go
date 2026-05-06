package valentin

import (
	"fmt"
	"log"
	"net"
	"rovnoMark/internal/core"
	"strings"
	"sync"
	"time"
)

const (
	STX = "\x02" // Начало кадра
	ETX = "\x03" // Конец кадра
)

type Driver struct {
	address string
	mu      sync.Mutex
}

func (d *Driver) GetRemainingRibbon() (string, error) {
	//TODO implement me
	panic("implement me")
}

func (d *Driver) GetQueueCapacity(queueName string) (string, error) {
	//TODO implement me
	panic("implement me")
}

func (d *Driver) GetPrintSpeed() (string, error) {
	//TODO implement me
	panic("implement me")
}

func (d *Driver) GetCurrentPrintCount() (string, error) {
	//TODO implement me
	panic("implement me")
}

func (d *Driver) GetCurrentTemplate() (string, error) {
	//TODO implement me
	panic("implement me")
}

func New(ip string, port int) core.Printer {
	return &Driver{
		address: fmt.Sprintf("%s:%d", ip, port),
	}
}

func (d *Driver) sendRaw(command string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	conn, err := net.DialTimeout("tcp", d.address, 3*time.Second)
	if err != nil {
		return "", fmt.Errorf("связь потеряна: %w", err)
	}
	defer conn.Close()

	conn.SetReadDeadline(time.Now().Add(3 * time.Second))

	fullCommand := STX + command + ETX
	_, err = conn.Write([]byte(fullCommand))
	if err != nil {
		return "", err
	}

	buf := make([]byte, 1024)
	n, err := conn.Read(buf)
	if err != nil {
		// Некоторые команды Валентина не возвращают текст, если всё Ок.
		// Возвращаем пустую строку вместо ошибки таймаута чтения.
		return "", nil
	}

	response := string(buf[:n])
	response = strings.Trim(response, STX+ETX+"\r\n")

	log.Printf("[Valentin %s] -> %s | <- %s", d.address, command, response)
	return response, nil
}

// GetStatus возвращает понятный статус: ГОТОВ, ПЕЧАТЬ, ОШИБКА
func (d *Driver) GetStatus() (string, error) {
	resp, err := d.sendRaw("S")
	if err != nil {
		return "ОШИБКА СВЯЗИ", err
	}

	switch {
	case strings.Contains(resp, "WAITING"):
		return "ГОТОВ", nil
	case strings.Contains(resp, "PRINTING"):
		return "ПЕЧАТЬ", nil
	case strings.Contains(resp, "ERROR"):
		return "ОШИБКА", nil
	default:
		return "НЕИЗВЕСТНО: " + resp, nil
	}
}

func (d *Driver) PrintTemplate(name string, fields map[string]string) error {
	// Очищаем буфер от старых ошибок
	_, _ = d.sendRaw("~C")

	cmd := fmt.Sprintf("^A%s", name)
	for k, v := range fields {
		cmd += fmt.Sprintf("^F%s=%s", k, v)
	}
	cmd += "^Z"

	_, err := d.sendRaw(cmd)
	return err
}

// PrintBatch загружает массив кодов. Возвращает количество кодов.
func (d *Driver) PrintBatch(fieldName string, codes []string) (int, error) {
	// Склеиваем коды через \r\n, как требует протокол
	batchData := strings.Join(codes, "\r\n")

	// Формируем команду батч-печати
	cmd := fmt.Sprintf("~B%s\r\n%s", fieldName, batchData)

	// Используем _, чтобы показать компилятору: ответ получили, но он нам не нужен
	_, err := d.sendRaw(cmd)
	if err != nil {
		return 0, err
	}

	return len(codes), nil
}

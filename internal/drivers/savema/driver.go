package savema

import (
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
	mu      sync.Mutex
}

func New(ip string, port int) *Driver {
	return &Driver{
		Address: ip,
		Port:    port,
		Timeout: 3 * time.Second,
	}
}

// sendRaw — базовый метод обмена данными[cite: 1]
func (d *Driver) sendRaw(cmdBody string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	address := net.JoinHostPort(d.Address, strconv.Itoa(d.Port))
	conn, err := net.DialTimeout("tcp", address, d.Timeout)
	if err != nil {
		return "", err
	}
	defer conn.Close()

	// Оборачиваем команду в спецсимволы Savema[cite: 1]
	fmt.Fprintf(conn, "~%s^", cmdBody)

	conn.SetReadDeadline(time.Now().Add(d.Timeout))
	buffer := make([]byte, 1024)
	n, err := conn.Read(buffer)
	if err != nil {
		return "", err
	}
	return string(buffer[:n]), nil
}

// PrintBatch — загрузка очереди Честного Знака[cite: 1]
func (d *Driver) PrintBatch(fieldName string, codes []string) (int, error) {
	allCodes := strings.Join(codes, "\n")
	// Команда SPLAQD добавляет данные в очередь[cite: 1]
	cmd := fmt.Sprintf("SPLAQD{%s~gt~%s}", fieldName, allCodes)
	resp, err := d.sendRaw(cmd)
	if err != nil {
		return 0, err
	}
	if strings.Contains(resp, "OK") {
		return len(codes), nil
	}
	return 0, fmt.Errorf("принтер не принял пачку: %s", resp)
}

func (d *Driver) PrintTemplate(template string, fields map[string]string) error {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("SPLLTF{%s}", template))
	// Используем разделитель | для отправки нескольких команд за раз[cite: 1]
	for k, v := range fields {
		sb.WriteString(fmt.Sprintf("|SPMCTV{%s~gt~%s}", k, v))
	}
	_, err := d.sendRaw(sb.String())
	return err
}

func (d *Driver) GetStatus() (string, error) {
	raw, err := d.sendRaw("SPPSTA")
	if err != nil {
		return "", err
	}
	clean := CleanResponse(raw)
	// Маппинг статусов согласно документации Rev.12[cite: 1]
	switch strings.ToUpper(clean) {
	case "WAITING":
		return "ГОТОВ", nil
	case "RUNNING":
		return "ПЕЧАТЬ", nil
	case "INIT":
		return "ЗАПУСК", nil
	case "ERROR":
		return "ОШИБКА", nil
	default:
		return clean, nil
	}
}

func CleanResponse(raw string) string {
	clean := strings.TrimSpace(raw)
	start := strings.Index(clean, ":")
	end := strings.LastIndex(clean, "}")
	if start != -1 && end != -1 && end > start {
		return strings.TrimSpace(clean[start+1 : end])
	}
	return clean
}

// Заглушки для интерфейса Printer[cite: 4]
func (d *Driver) GetRemainingRibbon() (string, error) {
	r, e := d.sendRaw("SPGGRR")
	return CleanResponse(r), e
}
func (d *Driver) GetQueueCapacity(q string) (string, error) {
	r, e := d.sendRaw(fmt.Sprintf("SPLGQC{%s}", q))
	return CleanResponse(r), e
}
func (d *Driver) GetPrintSpeed() (string, error) {
	r, e := d.sendRaw("SPCGPS")
	return CleanResponse(r), e
}
func (d *Driver) GetCurrentPrintCount() (string, error) {
	r, e := d.sendRaw("SPGGCP")
	return CleanResponse(r), e
}
func (d *Driver) GetCurrentTemplate() (string, error) {
	r, e := d.sendRaw("SPLGAT")
	return CleanResponse(r), e
}

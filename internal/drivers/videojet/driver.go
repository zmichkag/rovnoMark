package videojet

import (
	"bufio"
	"fmt"
	"log"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Driver struct {
	Address     string
	Port        int
	Timeout     time.Duration
	mu          sync.Mutex
	currstate   string
	CurTemplate string
}

// sendRaw — низкоуровневый обмен данными
func (d *Driver) sendRaw(cmd string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	address := net.JoinHostPort(d.Address, strconv.Itoa(d.Port))
	conn, err := net.DialTimeout("tcp", address, d.Timeout)
	if err != nil {
		// Ошибка подключения — это уровень Error или Warn
		slog.Error("VIDEOJET Connect Error", "ip", d.Address, "err", err)
		return "", err
	}
	defer conn.Close()

	// Логируем исходящую команду в Debug
	slog.Debug("VIDEOJET IO Out", "ip", d.Address, "cmd", cmd)

	_, err = conn.Write([]byte(cmd + "\r"))
	if err != nil {
		slog.Error("VIDEOJET Write Error", "ip", d.Address, "cmd", cmd, "err", err)
		return "", err
	}

	conn.SetReadDeadline(time.Now().Add(d.Timeout))
	reader := bufio.NewReader(conn)
	reply, err := reader.ReadString('\r')
	if err != nil {
		slog.Error("VIDEOJET Read Error", "ip", d.Address, "cmd", cmd, "err", err)
		return "", err
	}

	cleanReply := strings.TrimSpace(reply)
	// Логируем входящий ответ в Debug
	slog.Debug("VIDEOJET IO In", "ip", d.Address, "cmd", cmd, "resp", cleanReply)

	return cleanReply, nil
}

// InitSession
func (d *Driver) InitSession(fieldName string, maxQueue int) error {
	address := net.JoinHostPort(d.Address, strconv.Itoa(d.Port))
	conn, err := net.DialTimeout("tcp", address, 10*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()

	reader := bufio.NewReader(conn)
	conn.Write([]byte("\r"))

	send := func(c string) (string, error) {
		fmt.Fprint(conn, c+"\r")
		return reader.ReadString('\r')
	}

	send("SCB")
	send(fmt.Sprintf("SMR|%d|", maxQueue))

	resp, _ := send(fmt.Sprintf("SHO|%s|", fieldName))
	if strings.Contains(resp, "ERR") {
		return fmt.Errorf("поле %s не поддерживает сериализацию", fieldName)
	}
	return nil
}

// UpdateStaticFields обновляет статические поля в режиме сериализации (команда SCF)
func (d *Driver) UpdateStaticFields(fields map[string]string) error {
	if len(fields) == 0 {
		return nil
	}

	var sb strings.Builder
	sb.WriteString("SCF")
	for name, value := range fields {
		cleanValue := strings.ReplaceAll(value, "|", "")
		sb.WriteString(fmt.Sprintf("|%s=%s", name, cleanValue))
	}
	sb.WriteString("|")

	resp, err := d.sendRaw(sb.String())
	if err != nil {
		slog.Error("VIDEOJET SCF Failed", "ip", d.Address, "err", err)
		return err
	}

	if strings.Contains(resp, "ERR") {
		slog.Warn("VIDEOJET SCF Rejected", "ip", d.Address, "resp", resp, "fields", fields)
		return fmt.Errorf("принтер отклонил SCF: %s", resp)
	}

	slog.Info("VIDEOJET Static Updated", "ip", d.Address, "count", len(fields))
	return nil
}

func (d *Driver) ClearQueue() error {
	_, err := d.sendRaw("CQI") // Очищает все элементы очереди [cite: 610]
	return err
}

func (d *Driver) GetBufferFreeSpace() (int, error) {
	raw, err := d.sendRaw("SFS")
	if err != nil {
		return 0, err
	}
	// Ожидаем SFS|952368| [cite: 990]
	parts := strings.Split(raw, "|")
	if len(parts) >= 2 {
		space, _ := strconv.Atoi(strings.TrimSpace(parts[1]))
		return space, nil
	}
	return 0, fmt.Errorf("bad SFS response: %s", raw)
}

func New(ip string, port int) *Driver {
	return &Driver{
		Address: ip,
		Port:    port,
		Timeout: 3 * time.Second,
	}
}

// GetStatus запрашивает GST и разбирает состояние
func (d *Driver) GetStatus() (string, error) {
	raw, err := d.sendRaw("GST")
	if err != nil {
		return "", err
	}

	d.currstate = raw
	parts := strings.Split(raw, "|")
	if len(parts) < 2 {
		return "ОШИБКА ПРОТОКОЛА", nil
	}

	stateCode := parts[1]
	errorCode := ""
	if len(parts) > 2 {
		errorCode = parts[2]
	}

	// Если есть ошибка в статусе — логируем это как Warn
	if errorCode != "0" && errorCode != "" {
		slog.Warn("VIDEOJET Device Warning", "ip", d.Address, "state", stateCode, "error_code", errorCode)
	}

	switch stateCode {
	case "0":
		return "ВЫКЛЮЧЕН", nil
	case "1":
		return "НЕ ГОТОВ", nil
	case "2":
		return "ГОТОВ", nil
	case "3":
		return "ПЕЧАТЬ", nil
	case "4":
		return "ГОТОВ", nil
	default:
		return "НЕИЗВЕСТНО", nil
	}
}

// GetRemainingRibbon использует команду GCL (Consumable Levels)
func (d *Driver) GetRemainingRibbon(queueName string) (string, error) {
	raw, err := d.sendRaw("QSZ")
	if err != nil {
		return "", err
	}
	slog.Debug("VIDEOJET IO",
		"ip", d.Address,
		"reply", raw,
	)
	// Ответ: GCL | 30 | 50 |
	parts := strings.Split(raw, "|")
	if len(parts) >= 2 {
		return strings.TrimSpace(parts[1]), nil
	}

	return "0", nil
}

// GetQueueCapacity запрашивает QSZ (Queue Size) [cite: 673]
func (d *Driver) GetQueueCapacity(queueName string) (string, error) {
	raw, err := d.sendRaw("QSZ")
	if err != nil {
		return "", err
	}
	slog.Debug("VIDEOJET IO",
		"ip", d.Address,
		"reply", raw,
	)
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
	return "N/A", nil
}

func (d *Driver) GetCurrentPrintCount() (string, error) {
	if d.currstate == "" {
		return "0", nil
	}

	parts := strings.Split(d.currstate, "|")
	if len(parts) < 5 {
		return "0", nil
	}

	return strings.TrimSpace(parts[4]), nil
}

func (d *Driver) GetCurrentTemplate() (string, error) {
	if d.currstate == "" {
		return "0", nil
	}

	parts := strings.Split(d.currstate, "|")
	if len(parts) < 5 {
		return "0", nil
	}

	return strings.TrimSpace(parts[3]), nil
}

// GetTemplateFields запрашивает список переменных в указанном макете
func (d *Driver) GetTemplateFields(templateName string) ([]string, error) {
	// Формируем команду GJF (Get Job Fields)
	cmd := fmt.Sprintf("GJF|%s|", templateName)
	raw, err := d.sendRaw(cmd)
	if err != nil {
		return nil, err
	}

	// Ожидаемый ответ принтера: GJF|<JobName>|<Field1>|<Field2>|...|
	if strings.HasPrefix(raw, "ERR") {
		return nil, fmt.Errorf("принтер вернул ошибку на запрос полей макета %s: %s", templateName, raw)
	}

	parts := strings.Split(raw, "|")
	var fields []string

	// parts[0] == "GJF", parts[1] == templateName
	// Сами переменные начинаются с индекса 2. Последний элемент может быть пустым из-за финального "|"
	for i := 2; i < len(parts); i++ {
		field := strings.TrimSpace(parts[i])
		if field != "" {
			fields = append(fields, field)
		}
	}

	return fields, nil
}

// GetLastPrintedIndex запрашивает последний напечатанный индекс (команда SLR)
func (d *Driver) GetLastPrintedIndex() (int, error) {
	// Отправляем команду SLR
	raw, err := d.sendRaw("SLR")
	if err != nil {
		return 0, err
	}

	// Ожидаемый ответ: SLR|<index>|
	parts := strings.Split(raw, "|")
	if len(parts) >= 2 {
		indexStr := strings.TrimSpace(parts[1])
		if indexStr == "" {
			return 0, nil // Буфер пуст или еще ничего не напечатано
		}

		idx, err := strconv.Atoi(indexStr)
		if err != nil {
			return 0, fmt.Errorf("ошибка парсинга индекса %q: %v", indexStr, err)
		}
		return idx, nil
	}

	// Если принтер вернул ошибку выполнения (default failure response)
	return 0, fmt.Errorf("неизвестный ответ на команду SLR: %s", raw)
}

// GetTemplates запрашивает список шаблонов из памяти принтера
func (d *Driver) GetTemplates() ([]string, error) {
	// Команда GJL (Job List) запрашивает список доступных макетов
	raw, err := d.sendRaw("GJL")
	if err != nil {
		return nil, err
	}

	parts := strings.Split(raw, "|")
	var templates []string
	log.Printf("[VIDEOJET %s] >: %v", d.Address, raw)

	// Начинаем с индекса 1, так как parts[0] — это эхо команды "JLI"
	for i := 1; i < len(parts); i++ {
		name := strings.TrimSpace(parts[i])
		if name != "" && name != "ERR" {
			templates = append(templates, name)
		}
	}

	if len(templates) == 0 {
		return nil, fmt.Errorf("шаблоны не найдены или принтер вернул ошибку: %s", raw)
	}

	return templates, nil
}

// PrintBatchIndexed загружает пачку кодов через SID (с индексами)
func (d *Driver) PrintBatchIndexed(fieldName string, startIndex int, codes []string) (int, error) {
	slog.Info("VIDEOJET Batch Start", "ip", d.Address, "field", fieldName, "count", len(codes), "start_idx", startIndex)

	// Мы открываем соединение вручную, так как здесь пакетная передача
	address := net.JoinHostPort(d.Address, strconv.Itoa(d.Port))
	conn, err := net.DialTimeout("tcp", address, 10*time.Second)
	if err != nil {
		return 0, err
	}
	defer conn.Close()

	reader := bufio.NewReader(conn)
	conn.Write([]byte("\r"))

	// Вспомогательная функция для чистых логов внутри цикла
	send := func(c string) (string, error) {
		fmt.Fprint(conn, c+"\r")
		return reader.ReadString('\r')
	}

	//send("SCB")
	//send(fmt.Sprintf("SMR|%d|", len(codes)+10))

	//resp, _ := send(fmt.Sprintf("SHO|%s|", fieldName))
	//if strings.Contains(resp, "ERR") {
	//	slog.Error("VIDEOJET Serialization Error", "ip", d.Address, "field", fieldName, "resp", resp)
	//	return 0, fmt.Errorf("поле %s не поддерживает сериализацию", fieldName)
	//}

	successCount := 0
	for i, code := range codes {
		cleanCode := strings.ReplaceAll(code, "\x1d", "~1")
		currIdx := startIndex + i

		r, err := send(fmt.Sprintf("SID|%d|%s|", currIdx, cleanCode))
		if err != nil || strings.Contains(r, "ERR") {
			slog.Warn("VIDEOJET SID Item Failed", "ip", d.Address, "idx", currIdx, "err", err, "resp", r)
			break
		}
		successCount++
	}

	slog.Info("VIDEOJET Batch Finished", "ip", d.Address, "loaded", successCount, "total", len(codes))
	return successCount, nil
}

func (d *Driver) PrintBatch(fieldName string, codes []string) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	address := net.JoinHostPort(d.Address, strconv.Itoa(d.Port))
	conn, err := net.DialTimeout("tcp", address, 10*time.Second)
	if err != nil {
		return 0, err
	}
	defer conn.Close()

	reader := bufio.NewReader(conn)
	conn.Write([]byte("\r")) // Сброс парсера

	// 1. Очищаем старый буфер сериализации (команда SCB)
	fmt.Fprint(conn, "SCB\r")
	reader.ReadString('\r')
	log.Printf("[VIDEOJET %s] raw >: %s, %s", d.Address, conn.RemoteAddr(), strings.Join(codes, "|"))

	// 2. Устанавливаем лимит записей (например, до 2000), чтобы не упереться в память
	fmt.Fprintf(conn, "SMR|%d|\r", 2000)
	reader.ReadString('\r')

	// 3. Объявляем, для какого поля мы будем слать данные (команда SHO)
	// Синтаксис: SHO | <имя_поля> |
	fmt.Fprintf(conn, "SHO|%s|\r", fieldName)
	resp, _ := reader.ReadString('\r')
	if strings.Contains(resp, "ERR") {
		return 0, fmt.Errorf("поле %s не поддерживает сериализацию", fieldName)
	}

	successCount := 0
	for _, code := range codes {
		// Подготовка кода для GS1 (замена \x1d на ~1)
		cleanCode := strings.ReplaceAll(code, "\x1d", "~1")

		// 4. Заливаем данные в буфер (команда SDO)
		// Синтаксис: SDO | <данные> |
		fmt.Fprintf(conn, "SDO|%s|\r", cleanCode)

		// При успехе SDO возвращает количество свободного места (SFS)
		resp, err := reader.ReadString('\r')
		if err != nil || strings.Contains(resp, "ERR") {
			log.Printf("[SERIAL %s] Ошибка загрузки кода %d", d.Address, successCount)
			break
		}
		log.Printf("[VIDEOJET %s] RAW > %q", d.Address, resp)
		successCount++
	}

	return successCount, nil
}

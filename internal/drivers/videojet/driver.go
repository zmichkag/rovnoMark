package videojet

import (
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
	conn        net.Conn
}

func (d *Driver) PrintTemplate(template string, fields map[string]string) error {
	//TODO implement me
	panic("implement me")
}

// sendRaw — низкоуровневый обмен данными
func (d *Driver) sendRaw(cmd string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	// 1. Если соединения нет, создаем его (один раз!)
	if d.conn == nil {
		address := net.JoinHostPort(d.Address, strconv.Itoa(d.Port))
		conn, err := net.DialTimeout("tcp", address, d.Timeout)
		if err != nil {
			slog.Error("VIDEOJET Connect Error", "ip", d.Address, "err", err)
			return "", err
		}
		d.conn = conn

		// При новом подключении "чистим горло" парсеру
		d.conn.Write([]byte("\r"))
		time.Sleep(10 * time.Millisecond)
	}

	d.conn.SetReadDeadline(time.Now().Add(d.Timeout))

	slog.Debug("VIDEOJET IO Out", "ip", d.Address, "cmd", cmd)

	// 2. Отправляем саму команду
	_, err := d.conn.Write([]byte(cmd + "\r"))
	if err != nil {
		d.conn.Close()
		d.conn = nil // Сбрасываем соединение при ошибке
		return "", err
	}

	// 3. Читаем ответ побайтно (как sock.recv(1) в Python)
	var reply []byte
	buf := make([]byte, 1)
	for {
		_, err := d.conn.Read(buf)
		if err != nil {
			d.conn.Close()
			d.conn = nil // При таймауте закроем сокет, в следующий раз переподключится
			return "", err
		}

		if buf[0] == '\r' {
			// Если дошли до \r, но ничего не прочитали (эхо пустого сброса) — читаем дальше
			if len(reply) == 0 {
				continue
			}
			break // Конец ответа
		}

		if buf[0] != '\n' { // Игнорируем \n
			reply = append(reply, buf[0])
		}
	}

	cleanReply := strings.TrimSpace(string(reply))
	slog.Debug("VIDEOJET IO In", "ip", d.Address, "cmd", cmd, "resp", cleanReply)

	return cleanReply, nil
}

// InitSession
func (d *Driver) InitSession(compositeFields string, maxQueue int, staticFields map[string]string) error {
	slog.Info("VIDEOJET: Инициализация динамической сессии", "fields", compositeFields, "max_queue", maxQueue)

	// 1. Очищаем старый буфер сериализации
	if _, err := d.sendRaw("SCB"); err != nil {
		return fmt.Errorf("SCB failed: %v", err)
	}

	// 2. Устанавливаем лимит записей в очереди
	if _, err := d.sendRaw(fmt.Sprintf("SMR|%d|", maxQueue)); err != nil {
		return fmt.Errorf("SMR failed: %v", err)
	}

	// 3. Формируем команду SHO из составной строки полей
	// Заменяем наш внутренний разделитель ";" на протокольный "|"
	protocolFields := strings.ReplaceAll(compositeFields, ";", "|")
	cmdSHO := fmt.Sprintf("SHO|%s|", protocolFields)

	resp, err := d.sendRaw(cmdSHO)
	if err != nil {
		return fmt.Errorf("ошибка сокета при отправке SHO: %v", err)
	}
	if strings.Contains(resp, "ERR") {
		return fmt.Errorf("принтер отклонил SHO (%s): %s", cmdSHO, resp)
	}

	// 4. Сбрасываем внутренний указатель принтера
	//if _, err := d.sendRaw("SID|0|0|"); err != nil {
	//	return fmt.Errorf("ошибка сброса SID: %v", err)
	//}

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
	slog.Debug("VIDEOJET", "ip", d.Address, "SCF Reply", resp)

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

// GetBufferFreeSpace возвращает количество свободных слотов для записей (SGM - SRC)
func (d *Driver) GetBufferFreeSpace() (int, error) {
	// 1. Узнаем лимит записей в буфере (SGM)
	rawMax, err := d.sendRaw("SGM")
	if err != nil {
		return 0, fmt.Errorf("ошибка SGM: %v", err)
	}
	partsMax := strings.Split(rawMax, "|")
	if len(partsMax) < 2 {
		return 0, fmt.Errorf("неверный ответ SGM: %s", rawMax)
	}
	maxRecords, err := strconv.Atoi(strings.TrimSpace(partsMax[1]))
	if err != nil {
		return 0, fmt.Errorf("ошибка парсинга SGM: %v", err)
	}

	// 2. Узнаем, сколько записей уже лежит в буфере (SRC)
	rawBusy, err := d.sendRaw("SRC")
	if err != nil {
		return 0, fmt.Errorf("ошибка SRC: %v", err)
	}
	partsBusy := strings.Split(rawBusy, "|")
	if len(partsBusy) < 2 {
		return 0, fmt.Errorf("неверный ответ SRC: %s", rawBusy)
	}
	busyRecords, err := strconv.Atoi(strings.TrimSpace(partsBusy[1]))
	if err != nil {
		return 0, fmt.Errorf("ошибка парсинга SRC: %v", err)
	}

	// 3. Вычисляем свободное место (Слоты)
	freeSpace := maxRecords - busyRecords

	// Защита от отрицательных значений на всякий случай
	if freeSpace < 0 {
		freeSpace = 0
	}

	return freeSpace, nil
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

// GetQueueCapacity запрашивает QSZ (Queue Size) [cite: 673]
func (d *Driver) GetQueueCapacity(queueName string) (string, error) {
	//raw, err := d.sendRaw("QSZ")
	//if err != nil {
	//	return "", err
	//}
	//slog.Debug("VIDEOJET IO",
	//	"ip", d.Address,
	//	"reply", raw,
	//)
	//// Ответ: QSZ | <nn> | <s> | [cite: 678]
	//parts := strings.Split(raw, "|")
	//if len(parts) >= 2 {
	//	return strings.TrimSpace(parts[1]), nil
	//}
	return "0", nil
}

func (d *Driver) GetRemainingRibbon() (string, error) {
	raw, err := d.sendRaw("GCL")
	if err != nil {
		return "", err
	}
	slog.Debug("VIDEOJET IO",
		"ip", d.Address,
		"reply", raw,
	)
	parts := strings.Split(raw, "|")
	if len(parts) >= 2 {
		return strings.TrimSpace(parts[1]), nil
	}
	return strings.TrimSpace(parts[1]), nil
}

// SelectTemplate поддерживает опциональную передачу статических полей (Job with Data)
func (d *Driver) SelectTemplate(name string, fields map[string]string) error {
	var sb strings.Builder
	sb.WriteString("SEL|")
	sb.WriteString(name)
	sb.WriteString("|")

	// Если поля переданы, добавляем их прямо в команду SEL
	for k, v := range fields {
		cleanValue := strings.ReplaceAll(v, "|", "")
		sb.WriteString(fmt.Sprintf("%s=%s|", k, cleanValue))
	}

	res, err := d.sendRaw(sb.String())
	if err != nil || strings.Contains(res, "ERR") {
		return fmt.Errorf("ошибка выбора макета %s: %s", name, res)
	}
	return nil
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

// PrintBatchIndexed отправляет пачку данных с жесткой привязкой к индексам.
func (d *Driver) PrintBatchIndexed(compositeFields string, startIndex int, codes []string) (int, error) {
	slog.Info("VIDEOJET: Загрузка пачки SID", "ip", d.Address, "count", len(codes), "start_idx", startIndex)

	successCount := 0
	for i, payload := range codes {
		// Подготовка GS1-разделителя (замена <GS> или \x1d на ASCII 29)
		cleanPayload := strings.ReplaceAll(payload, "<GS>", "\x1d")

		//// 2. Вырезаем символ '|', чтобы защитить протокол CLARiTY от поломки
		//cleanPayload = strings.ReplaceAll(cleanPayload, "|", "")

		// 3. СТАРТОВЫЙ FNC1 (ASCII 232) В НАЧАЛО
		if !strings.HasPrefix(cleanPayload, "~") {
			cleanPayload = "~" + cleanPayload
		}

		// 4. Формируем команду и отправляем в сокет
		currIdx := startIndex + i
		cmdSID := fmt.Sprintf("SID|%d|%s|", currIdx, cleanPayload)

		resp, err := d.sendRaw(cmdSID)
		if err != nil || strings.Contains(resp, "ERR") {
			slog.Warn("VIDEOJET: Ошибка загрузки записи SID", "idx", currIdx, "payload", cleanPayload, "err", err, "resp", resp)
			break
		}
		successCount++
	}

	slog.Info("VIDEOJET: Пачка успешно загружена", "ip", d.Address, "loaded", successCount, "total", len(codes))
	return successCount, nil
}

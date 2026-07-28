package markem

import (
	"bytes"
	"fmt"
	"log/slog"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
)

const (
	MaxQueueLimit = 200 // Аппаратный лимит буфера кодов SmartDate
)

type Driver struct {
	Address     string
	Port        int
	ActorName   string // Имя принтера в DCP (обычно "Actor1" или "SmartDate5")
	SenderName  string // Наше имя ("RovnoMarkGo")
	Timeout     time.Duration
	mu          sync.Mutex
	conn        net.Conn
	actCounter  int
	curTemplate string

	// Внутренние счетчики для автономного контроля буфера
	totalSent    int
	totalPrinted int
}

func New(ip string, port int, actorName string) *Driver {
	if actorName == "" {
		actorName = "Actor1" // Дефолт, подтвержденный на практике
	}
	return &Driver{
		Address:    ip,
		Port:       port,
		ActorName:  actorName,
		SenderName: "RovnoMarkGo",
		Timeout:    3 * time.Second,
		actCounter: 50000,
	}
}

// --- НИЗКОУРОВНЕВЫЙ СЛОЙ (UTF-16LE & SOAP) ---

func encodeUTF16LE(s string) []byte {
	runes := utf16.Encode([]rune(s))
	bytes := make([]byte, len(runes)*2)
	for i, r := range runes {
		bytes[i*2] = byte(r)
		bytes[i*2+1] = byte(r >> 8)
	}
	return bytes
}

func decodeUTF16LE(b []byte) string {
	if len(b)%2 != 0 {
		b = b[:len(b)-1]
	}
	u16s := make([]uint16, len(b)/2)
	for i := 0; i < len(u16s); i++ {
		u16s[i] = uint16(b[i*2]) | uint16(b[i*2+1])<<8
	}
	return string(utf16.Decode(u16s))
}

func escapeXML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, "\"", "&quot;")
	s = strings.ReplaceAll(s, "'", "&apos;")
	return s
}

func (d *Driver) getNextAct() int {
	d.actCounter++
	if d.actCounter > 59999 {
		d.actCounter = 50000
	}
	return d.actCounter
}

func (d *Driver) closeConn() {
	if d.conn != nil {
		d.conn.Close()
		d.conn = nil
	}
}

// sendSOAP отправляет XML-команду и вычитывает поток до закрывающего тега </Envelope>
func (d *Driver) sendSOAP(bodyXML string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.conn == nil {
		address := net.JoinHostPort(d.Address, strconv.Itoa(d.Port))
		conn, err := net.DialTimeout("tcp", address, d.Timeout)
		if err != nil {
			slog.Error("MARKEM Connect Error", "ip", d.Address, "err", err)
			return "", err
		}
		d.conn = conn
	}

	act := d.getNextAct()
	formattedBody := fmt.Sprintf(bodyXML, act)

	soapMsg := fmt.Sprintf(`<Envelope><Header sender="%s" receiver="%s"/><Body>%s</Body></Envelope>`,
		d.SenderName, d.ActorName, formattedBody)

	payload := encodeUTF16LE(soapMsg)
	_ = d.conn.SetDeadline(time.Now().Add(d.Timeout))

	slog.Debug("MARKEM Out", "ip", d.Address, "act", act, "body", formattedBody)

	if _, err := d.conn.Write(payload); err != nil {
		d.closeConn()
		return "", fmt.Errorf("socket write error: %w", err)
	}

	// Читаем ответ из сокета в цикле (защита от склейки пакетов и CmdPending)
	var buffer bytes.Buffer
	chunk := make([]byte, 4096)
	for {
		n, err := d.conn.Read(chunk)
		if err != nil {
			d.closeConn()
			return "", fmt.Errorf("socket read error: %w", err)
		}
		buffer.Write(chunk[:n])

		decoded := decodeUTF16LE(buffer.Bytes())
		if strings.Contains(decoded, "</Envelope>") {
			// Проверяем на наличие ошибок в теле SOAP
			if strings.Contains(decoded, "CmdFailed") || strings.Contains(decoded, "Fault") {
				slog.Warn("MARKEM Rejected Command", "ip", d.Address, "resp", decoded)
				return decoded, fmt.Errorf("printer rejected command (CmdFailed/Fault)")
			}
			return decoded, nil
		}
	}
}

// --- РЕАЛИЗАЦИЯ КОНТРАКТА core.Printer ---

// 1. ВЫЗОВ ИЗ ПАМЯТИ ПО PLU / ИМЕНИ + 2. ПЕРЕДАЧА СТАТИЧЕСКИХ ДАТ
func (d *Driver) SelectTemplate(template string, fields map[string]string) error {
	slog.Info("MARKEM: Выбор шаблона и загрузка статики", "ip", d.Address, "template", template, "fields", len(fields))

	// Обеспечиваем расширение .job, если 1С прислала просто имя
	jobName := template
	if !strings.HasSuffix(strings.ToLower(jobName), ".job") {
		jobName += ".job"
	}

	// Шаг 1: Вызов макета из памяти принтера
	cmdSelect := `<SelectLocalJob act="%d"><JobFileName>` + escapeXML(jobName) + `</JobFileName></SelectLocalJob>`
	if _, err := d.sendSOAP(cmdSelect); err != nil {
		return fmt.Errorf("ошибка активации задания %s: %w", jobName, err)
	}
	d.curTemplate = template

	// Сброс виртуальной очереди при смене задания
	d.totalSent = 0
	d.totalPrinted = 0

	// Шаг 2: Если есть статические поля (даты) — сразу шлем их в макет
	if len(fields) > 0 {
		return d.UpdateStaticFields(fields)
	}
	return nil
}

func (d *Driver) UpdateStaticFields(fields map[string]string) error {
	if len(fields) == 0 {
		return nil
	}

	var sb strings.Builder
	sb.WriteString(`<UpdateSelectedJob act="%d">`)
	for name, val := range fields {
		sb.WriteString(fmt.Sprintf(`<FieldData><FieldName>%s</FieldName><FieldValue>%s</FieldValue></FieldData>`,
			escapeXML(name), escapeXML(val)))
	}
	sb.WriteString(`</UpdateSelectedJob>`)

	_, err := d.sendSOAP(sb.String())
	if err != nil {
		return fmt.Errorf("ошибка передачи статических полей: %w", err)
	}
	slog.Info("MARKEM: Статические даты успешно применены", "ip", d.Address, "count", len(fields))
	return nil
}

// 3. НАКАЧКА ПАЧКАМИ ДО 200 ШТУК (QueuePackData)
func (d *Driver) PrintBatchIndexed(fieldName string, startIndex int, codes []string) (int, error) {
	if len(codes) == 0 {
		return 0, nil
	}

	slog.Info("MARKEM: Отправка пачки в буфер (QueuePackData)", "ip", d.Address, "field", fieldName, "count", len(codes))

	var sb strings.Builder
	sb.WriteString(`<QueuePackData act="%d">`)

	for _, code := range codes {
		// Подготовка GS1 (замена символа Group Separator \x1d на стандартный ASCII 29 / &#x1D;)
		cleanCode := strings.ReplaceAll(code, "<GS>", "&#x1D;")
		cleanCode = strings.ReplaceAll(cleanCode, "\x1d", "&#x1D;")
		cleanCode = escapeXML(cleanCode)

		sb.WriteString(fmt.Sprintf(`<PackData><FieldData><FieldName>%s</FieldName><FieldValue>%s</FieldValue></FieldData></PackData>`,
			escapeXML(fieldName), cleanCode))
	}
	sb.WriteString(`</QueuePackData>`)

	_, err := d.sendSOAP(sb.String())
	if err != nil {
		slog.Error("MARKEM: Сбой загрузки очереди", "ip", d.Address, "err", err)
		return 0, err
	}

	// Увеличиваем счетчик успешно закинутых в буфер кодов
	d.totalSent += len(codes)
	return len(codes), nil
}

// 4. КОНТРОЛЬ БУФЕРА (ЗАЩИТА ОТ ПЕРЕПОЛНЕНИЯ 200 ШТ)
func (d *Driver) GetBufferFreeSpace() (int, error) {
	// Актуализируем текущее количество отпечатанных кодов
	countStr, err := d.GetCurrentPrintCount()
	if err != nil {
		return 0, err
	}

	printed, _ := strconv.Atoi(countStr)
	if printed > d.totalPrinted {
		d.totalPrinted = printed
	}

	// Сколько кодов сейчас физически лежит в буфере принтера
	inBuffer := d.totalSent - d.totalPrinted
	if inBuffer < 0 {
		inBuffer = 0
	}

	freeSpace := MaxQueueLimit - inBuffer
	if freeSpace < 0 {
		freeSpace = 0
	}

	slog.Debug("MARKEM Buffer Status", "ip", d.Address, "in_buffer", inBuffer, "free_space", freeSpace)
	return freeSpace, nil
}

// 4. ПОДКАЧКА ПО СЧЕТЧИКУ (RequestCounts -> тег Batch)
func (d *Driver) GetCurrentPrintCount() (string, error) {
	resp, err := d.sendSOAP(`<RequestCounts act="%d"/>`)
	if err != nil {
		return strconv.Itoa(d.totalPrinted), err
	}

	// Ищем тег <Batch><Value>123</Value></Batch> или <Total>
	re := regexp.MustCompile(`<Batch>.*?<Value>(\d+)</Value>.*?</Batch>`)
	matches := re.FindStringSubmatch(resp)
	if len(matches) > 1 {
		return matches[1], nil
	}

	// Фоллбэк на Total, если Batch пуст
	reTotal := regexp.MustCompile(`<Total>.*?<Value>(\d+)</Value>.*?</Total>`)
	matchesTotal := reTotal.FindStringSubmatch(resp)
	if len(matchesTotal) > 1 {
		return matchesTotal[1], nil
	}

	return strconv.Itoa(d.totalPrinted), nil
}

// GetStatus для BackgroundPoller
func (d *Driver) GetStatus() (string, error) {
	resp, err := d.sendSOAP(`<RequestPackMLStatus act="%d"/>`)
	if err != nil {
		return "ОФФЛАЙН", err
	}

	if strings.Contains(resp, "<State>4</State>") || strings.Contains(resp, "<State>5</State>") {
		return "ГОТОВ", nil
	}
	if strings.Contains(resp, "<State>6</State>") {
		return "ПЕЧАТЬ", nil
	}
	if strings.Contains(resp, "<State>2</State>") {
		return "ОСТАНОВЛЕН", nil
	}
	if strings.Contains(resp, "<State>8</State>") || strings.Contains(resp, "<State>9</State>") {
		return "ОШИБКА", nil
	}

	return "ОНЛАЙН", nil
}

// ClearQueue сброс буфера кодов (при остановке линии в 1С / MES)
func (d *Driver) ClearQueue() error {
	slog.Info("MARKEM: Очистка очереди кодов", "ip", d.Address)
	_, err := d.sendSOAP(`<ClearPackDataQueue act="%d"/>`)
	if err == nil {
		d.totalSent = 0
		d.totalPrinted = 0
	}
	return err
}

func (d *Driver) InitSession(fieldName string, maxQueue int, staticFields map[string]string) error {
	return d.ClearQueue()
}

func (d *Driver) PrintTemplate(template string, fields map[string]string) error {
	return d.SelectTemplate(template, fields)
}

func (d *Driver) GetTemplates() ([]string, error) {
	resp, err := d.sendSOAP(`<RequestFileDirectoryListing act="%d"><Filter>job</Filter></RequestFileDirectoryListing>`)
	if err != nil {
		return nil, err
	}
	re := regexp.MustCompile(`<FileName>(.*?)</FileName>`)
	matches := re.FindAllStringSubmatch(resp, -1)
	var list []string
	for _, m := range matches {
		if len(m) > 1 {
			list = append(list, m[1])
		}
	}
	return list, nil
}

func (d *Driver) GetTemplateFields(templateName string) ([]string, error) {
	// Возвращаем стандартные имена для разметки (в CoLOS их называют именно так)
	return []string{"DATAMATRIX", "date01", "date02", "BATCH"}, nil
}

func (d *Driver) GetRemainingRibbon() (string, error) { return "N/A", nil }
func (d *Driver) GetQueueCapacity(queueName string) (string, error) {
	return strconv.Itoa(MaxQueueLimit), nil
}
func (d *Driver) GetPrintSpeed() (string, error)    { return "N/A", nil }
func (d *Driver) GetLastPrintedIndex() (int, error) { return d.totalPrinted, nil }
func (d *Driver) GetCurrentTemplate() (string, error) {
	if d.curTemplate != "" {
		return d.curTemplate, nil
	}
	return "N/A", nil
}

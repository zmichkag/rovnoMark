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
	MaxQueueLimit = 200 // Аппаратный лимит буфера кодов SmartDate X60
)

type Driver struct {
	Address     string
	Port        int
	ActorName   string
	SenderName  string
	Timeout     time.Duration
	mu          sync.Mutex // Защищает сетевые операции
	stateMu     sync.Mutex // Защищает счетчики (избегаем dead-lock'ов)
	conn        net.Conn
	actCounter  int
	curTemplate string

	// Внутренние счетчики
	totalSent int
	baseCount int // Стартовое значение счетчика принтера при начале задания
}

func New(ip string, port int, actorName string) *Driver {
	if actorName == "" {
		actorName = "Actor1"
	}
	return &Driver{
		Address:    ip,
		Port:       port,
		ActorName:  actorName,
		SenderName: "RovnoMarkGo",
		Timeout:    5 * time.Second,
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

// sendSOAPWaitACK отправляет XML с умным ожиданием и АВТОРЕТРАЕМ при обрыве
func (d *Driver) sendSOAPWaitACK(bodyXML string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	act := d.getNextAct()
	formattedBody := fmt.Sprintf(bodyXML, act)
	soapMsg := fmt.Sprintf(`<?xml version="1.0" encoding="utf-16"?><Envelope><Header sender="%s" receiver="%s"/><Body>%s</Body></Envelope>`,
		d.SenderName, d.ActorName, formattedBody)
	payload := encodeUTF16LE(soapMsg)

	// Делаем максимум 2 попытки (оригинальная + 1 ретрай при обрыве)
	for attempt := 1; attempt <= 2; attempt++ {
		if d.conn == nil {
			address := net.JoinHostPort(d.Address, strconv.Itoa(d.Port))
			conn, err := net.DialTimeout("tcp", address, d.Timeout)
			if err != nil {
				if attempt == 2 {
					slog.Error("MARKEM Connect Error (Retry Failed)", "ip", d.Address, "err", err)
					return "", err
				}
				continue // Пробуем еще раз
			}
			d.conn = conn
		}

		_ = d.conn.SetDeadline(time.Now().Add(8 * time.Second))
		slog.Debug("MARKEM Out", "ip", d.Address, "act", act, "body", formattedBody)

		if _, err := d.conn.Write(payload); err != nil {
			d.closeConn()
			if attempt == 2 {
				return "", fmt.Errorf("socket write error: %w", err)
			}
			continue // Сокет отвалился при записи, уходим на 2-ю попытку
		}

		var buffer bytes.Buffer
		chunk := make([]byte, 4096)
		actTagSingle := fmt.Sprintf("act='%d'", act)
		actTagDouble := fmt.Sprintf("act=\"%d\"", act)

		readSuccess := false
		for {
			_ = d.conn.SetDeadline(time.Now().Add(5 * time.Second))
			n, err := d.conn.Read(chunk)
			if err != nil {
				d.closeConn()
				break // Выходим из цикла чтения, пойдет на 2-ю попытку (если attempt==1)
			}
			buffer.Write(chunk[:n])
			decoded := decodeUTF16LE(buffer.Bytes())

			if strings.Contains(decoded, actTagSingle) || strings.Contains(decoded, actTagDouble) {
				if strings.Contains(decoded, "CmdPending") {
					continue // Ждем финального ответа
				}
				if strings.Contains(decoded, "CmdFailed") || strings.Contains(decoded, "Fault") {
					slog.Warn("MARKEM Command Failed", "ip", d.Address, "resp", decoded)
					return decoded, fmt.Errorf("printer rejected command (CmdFailed)")
				}
				readSuccess = true
				return decoded, nil
			}
		}

		if readSuccess {
			break
		}
	}
	return "", fmt.Errorf("failed to communicate with printer after retries")
}

// --- РЕАЛИЗАЦИЯ КОНТРАКТА core.Printer ---

func (d *Driver) SelectTemplate(template string, fields map[string]string) error {
	slog.Info("MARKEM: Выбор шаблона и загрузка статики", "ip", d.Address, "template", template)

	jobName := template
	if !strings.HasSuffix(strings.ToLower(jobName), ".job") {
		jobName += ".job"
	}

	cmdSelect := `<SelectLocalJob act="%d"><JobFileName>` + escapeXML(jobName) + `</JobFileName></SelectLocalJob>`
	if _, err := d.sendSOAPWaitACK(cmdSelect); err != nil {
		return fmt.Errorf("ошибка активации задания %s: %w", jobName, err)
	}
	d.curTemplate = template
	time.Sleep(100 * time.Millisecond)

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
		cleanVal := strings.ReplaceAll(val, "|", "")
		cleanVal = escapeXML(cleanVal)

		sb.WriteString(fmt.Sprintf(`<FieldData><FieldName>%s</FieldName><FieldValue>%s</FieldValue></FieldData>`,
			escapeXML(name), cleanVal))

		upper := strings.ToUpper(name)
		if upper == "DATE01" || upper == "DATE1" {
			sb.WriteString(fmt.Sprintf(`<FieldData><FieldName>date1</FieldName><FieldValue>%s</FieldValue></FieldData>`, cleanVal))
		}
		if upper == "DATE02" || upper == "DATE2" {
			sb.WriteString(fmt.Sprintf(`<FieldData><FieldName>date2</FieldName><FieldValue>%s</FieldValue></FieldData>`, cleanVal))
		}
	}
	sb.WriteString(`</UpdateSelectedJob>`)

	_, err := d.sendSOAPWaitACK(sb.String())
	if err != nil {
		return fmt.Errorf("ошибка передачи статики: %w", err)
	}
	return nil
}

func (d *Driver) PrintBatchIndexed(fieldName string, startIndex int, codes []string) (int, error) {
	if len(codes) == 0 {
		return 0, nil
	}

	// Жестко форсируем DATAMATRIX, чтобы избежать "Syntax error parsing command"
	targetField := "DATAMATRIX"

	var sb strings.Builder
	sb.WriteString(`<QueuePackData act="%d">`)

	for _, code := range codes {
		cleanCode := escapeXML(code)
		cleanCode = strings.ReplaceAll(cleanCode, "&lt;GS&gt;", "&#x1D;")
		cleanCode = strings.ReplaceAll(cleanCode, "<GS>", "&#x1D;")
		cleanCode = strings.ReplaceAll(cleanCode, "\x1d", "&#x1D;")

		sb.WriteString(fmt.Sprintf(`<PackData><FieldData><FieldName>%s</FieldName><FieldValue>%s</FieldValue></FieldData></PackData>`,
			targetField, cleanCode))
	}
	sb.WriteString(`</QueuePackData>`)

	_, err := d.sendSOAPWaitACK(sb.String())
	if err != nil {
		slog.Error("MARKEM: Сбой загрузки очереди", "ip", d.Address, "err", err)
		return 0, err
	}

	// Безопасно обновляем счетчик отправленных кодов
	d.stateMu.Lock()
	d.totalSent += len(codes)
	d.stateMu.Unlock()

	return len(codes), nil
}

func (d *Driver) GetCurrentPrintCount() (string, error) {
	resp, err := d.sendSOAPWaitACK(`<RequestCounts act="%d"/>`)
	if err != nil {
		return "0", err
	}

	re := regexp.MustCompile(`<Batch>.*?<Value>(\d+)</Value>.*?</Batch>`)
	matches := re.FindStringSubmatch(resp)
	if len(matches) > 1 {
		return matches[1], nil
	}

	reTotal := regexp.MustCompile(`<Total>.*?<Value>(\d+)</Value>.*?</Total>`)
	matchesTotal := reTotal.FindStringSubmatch(resp)
	if len(matchesTotal) > 1 {
		return matchesTotal[1], nil
	}

	return "0", nil
}

func (d *Driver) GetBufferFreeSpace() (int, error) {
	countStr, err := d.GetCurrentPrintCount()
	if err != nil {
		return 0, err
	}

	hwCount, _ := strconv.Atoi(countStr)

	d.stateMu.Lock()
	defer d.stateMu.Unlock()

	// Вычисляем, сколько РЕАЛЬНО напечатано с момента инициализации (InitSession)
	printedSinceStart := hwCount - d.baseCount
	if printedSinceStart < 0 {
		printedSinceStart = 0 // На случай сброса счетчиков на самом принтере
	}

	// Вычисляем, сколько кодов сейчас висит в буфере принтера
	inBuffer := d.totalSent - printedSinceStart
	if inBuffer < 0 {
		inBuffer = 0
	}

	freeSpace := MaxQueueLimit - inBuffer
	if freeSpace < 0 {
		freeSpace = 0
	}

	slog.Debug("MARKEM Buffer", "ip", d.Address, "in_buffer", inBuffer, "free_space", freeSpace, "total_sent", d.totalSent, "printed", printedSinceStart)
	return freeSpace, nil
}

func (d *Driver) GetStatus() (string, error) {
	resp, err := d.sendSOAPWaitACK(`<RequestPackMLStatus act="%d"/>`)
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

func (d *Driver) ClearQueue() error {
	slog.Info("MARKEM: Очистка очереди кодов", "ip", d.Address)
	_, err := d.sendSOAPWaitACK(`<ClearPackDataQueue act="%d"/>`)

	// Привязываем базовый счетчик к текущему аппаратному счетчику
	countStr, _ := d.GetCurrentPrintCount()
	hwCount, _ := strconv.Atoi(countStr)

	d.stateMu.Lock()
	d.baseCount = hwCount
	d.totalSent = 0
	d.stateMu.Unlock()

	return err
}

func (d *Driver) InitSession(fieldName string, maxQueue int, staticFields map[string]string) error {
	return d.ClearQueue() // Очистка очереди автоматически сбрасывает счетчики расчета буфера
}

func (d *Driver) PrintTemplate(template string, fields map[string]string) error {
	return d.SelectTemplate(template, fields)
}

func (d *Driver) GetTemplates() ([]string, error) {
	resp, err := d.sendSOAPWaitACK(`<RequestFileDirectoryListing act="%d"><Filter>job</Filter></RequestFileDirectoryListing>`)
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
	return []string{"DATAMATRIX", "date1", "date2", "PLU"}, nil
}

func (d *Driver) GetRemainingRibbon() (string, error) { return "N/A", nil }
func (d *Driver) GetQueueCapacity(queueName string) (string, error) {
	return strconv.Itoa(MaxQueueLimit), nil
}
func (d *Driver) GetPrintSpeed() (string, error) { return "N/A", nil }
func (d *Driver) GetLastPrintedIndex() (int, error) {
	d.stateMu.Lock()
	defer d.stateMu.Unlock()
	return d.totalSent, nil // Упрощенный возврат для Markem, так как индексы тут условные
}
func (d *Driver) GetCurrentTemplate() (string, error) {
	if d.curTemplate != "" {
		return d.curTemplate, nil
	}
	return "N/A", nil
}

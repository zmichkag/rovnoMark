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
	MaxQueueLimit  = 150 // Аппаратный лимит буфера кодов SmartDate X60
	SafeQueueLimit = 100 // 💡 Увеличили софтовый лимит до 100, чтобы Pumper не застревал
)

type Driver struct {
	Address        string
	Port           int
	ActorName      string
	SenderName     string
	Timeout        time.Duration
	mu             sync.Mutex // Защищает сетевые операции
	stateMu        sync.Mutex // Защищает счетчики
	conn           net.Conn
	actCounter     int
	curTemplate    string
	lastCountCheck time.Time

	// Внутренние счетчики
	totalSent             int
	baseCount             int // Значение countBatchGood при начале задания
	lastPrintedCalculated int // Кэш отпечатанных кодов для защиты буфера
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
		Timeout:    10 * time.Second,
		actCounter: 50000,
	}
}

// --- НИЗКОУРОВНЕВЫЙ СЛОЙ (UTF-16LE & SOAP) ---

func encodeUTF16LE(s string) []byte {
	runes := utf16.Encode([]rune(s))
	b := make([]byte, len(runes)*2)
	for i, r := range runes {
		b[i*2] = byte(r)
		b[i*2+1] = byte(r >> 8)
	}
	return b
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
		_ = d.conn.Close()
		d.conn = nil
	}
}

// sendSOAPWaitACK отправляет XML с МЯГКИМ ожиданием ответа
func (d *Driver) sendSOAPWaitACK(bodyXML string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	act := d.getNextAct()
	formattedBody := strings.ReplaceAll(bodyXML, "{act}", strconv.Itoa(act))
	soapMsg := fmt.Sprintf(`<?xml version="1.0" encoding="utf-16"?><Envelope><Header sender="%s" receiver="%s"/><Body>%s</Body></Envelope>`,
		d.SenderName, d.ActorName, formattedBody)
	payload := encodeUTF16LE(soapMsg)

	for attempt := 1; attempt <= 2; attempt++ {
		if d.conn == nil {
			address := net.JoinHostPort(d.Address, strconv.Itoa(d.Port))
			conn, err := net.DialTimeout("tcp", address, d.Timeout)
			if err != nil {
				if attempt == 2 {
					slog.Error("MARKEM Connect Error (Retry Failed)", "ip", d.Address, "err", err)
					return "", err
				}
				time.Sleep(300 * time.Millisecond)
				continue
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
			time.Sleep(300 * time.Millisecond)
			continue
		}

		var buffer bytes.Buffer
		chunk := make([]byte, 8192)
		actTagSingle := fmt.Sprintf("act='%d'", act)
		actTagDouble := fmt.Sprintf("act=\"%d\"", act)

		startTime := time.Now()
		for time.Since(startTime) < 8*time.Second {
			_ = d.conn.SetDeadline(time.Now().Add(3 * time.Second))
			n, err := d.conn.Read(chunk)
			if err != nil {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					time.Sleep(100 * time.Millisecond)
					continue
				}
				d.closeConn()
				break
			}

			buffer.Write(chunk[:n])
			decoded := decodeUTF16LE(buffer.Bytes())

			if strings.Contains(decoded, actTagSingle) || strings.Contains(decoded, actTagDouble) {
				if strings.Contains(decoded, "CmdPending") {
					continue
				}

				if strings.Contains(decoded, "CmdFailed") || strings.Contains(decoded, "Fault") {
					if strings.Contains(bodyXML, "RequestCounts") || strings.Contains(bodyXML, "RequestPackMLStatus") {
						return "", fmt.Errorf("telemetry command rejected")
					}
					slog.Warn("MARKEM Command Failed", "ip", d.Address, "resp", decoded)
					return decoded, fmt.Errorf("printer rejected command (CmdFailed)")
				}

				return decoded, nil
			}

			if strings.Contains(decoded, "countBatchGood") {
				d.extractAndSaveHwCount(decoded)
			}
		}

		d.closeConn()
		time.Sleep(200 * time.Millisecond)
	}

	return "", fmt.Errorf("failed to communicate with printer after retries")
}

func (d *Driver) extractAndSaveHwCount(xmlStr string) {
	reBatchGood := regexp.MustCompile(`<StringID>countBatchGood</StringID>\s*<Value>(\d+)</Value>`)
	matches := reBatchGood.FindStringSubmatch(xmlStr)
	if len(matches) > 1 {
		current, _ := strconv.Atoi(matches[1])
		d.stateMu.Lock()
		if current >= d.baseCount {
			d.lastPrintedCalculated = current - d.baseCount
		}
		d.stateMu.Unlock()
	}
}

// --- РЕАЛИЗАЦИЯ КОНТРАКТА core.Printer ---

func (d *Driver) SelectTemplate(template string, fields map[string]string) error {
	slog.Info("MARKEM: Выбор шаблона и загрузка статики", "ip", d.Address, "template", template)

	jobName := template
	if !strings.HasSuffix(strings.ToLower(jobName), ".job") {
		jobName += ".job"
	}

	//  Если этот макет уже выбран на Маркеме, пропускаем сетевой вызов SelectLocalJob!
	if d.curTemplate != template {
		cmdSelect := `<SelectLocalJob act="{act}"><JobFileName>` + escapeXML(jobName) + `</JobFileName></SelectLocalJob>`
		if _, err := d.sendSOAPWaitACK(cmdSelect); err != nil {
			slog.Warn("MARKEM: Ошибка смены макета (пропускаем, если принтер уже печатает)", "ip", d.Address, "err", err)
		}
		d.curTemplate = template
	}

	time.Sleep(150 * time.Millisecond)

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
	sb.WriteString(`<UpdateSelectedJob act="{act}">`)

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

	targetField := "DATAMATRIX"

	var sb strings.Builder
	sb.WriteString(`<QueuePackData act="{act}">`)

	for _, code := range codes {
		cleanCode := escapeXML(code)
		cleanCode = strings.ReplaceAll(cleanCode, "&lt;GS&gt;", "&#x1D;")
		cleanCode = strings.ReplaceAll(cleanCode, "<GS>", "&#x1D;")
		cleanCode = strings.ReplaceAll(cleanCode, "\x1d", "&#x1D;")
		cleanCode = "~1" + cleanCode

		sb.WriteString(fmt.Sprintf(`<PackData><FieldData><FieldName>%s</FieldName><FieldValue>%s</FieldValue></FieldData></PackData>`,
			targetField, cleanCode))
	}
	sb.WriteString(`</QueuePackData>`)

	_, err := d.sendSOAPWaitACK(sb.String())
	if err != nil {
		slog.Error("MARKEM: Сбой загрузки очереди", "ip", d.Address, "err", err)
		return 0, err
	}

	d.stateMu.Lock()
	d.totalSent += len(codes)
	d.stateMu.Unlock()

	time.Sleep(50 * time.Millisecond)

	return len(codes), nil
}

func (d *Driver) GetCurrentPrintCount() (string, error) {
	d.stateMu.Lock()

	if time.Since(d.lastCountCheck) < 1500*time.Millisecond {
		lastValid := d.baseCount + d.lastPrintedCalculated
		d.stateMu.Unlock()
		return strconv.Itoa(lastValid), nil
	}
	d.stateMu.Unlock()

	resp, err := d.sendSOAPWaitACK(`<RequestCounts act="{act}"/>`)

	d.stateMu.Lock()
	d.lastCountCheck = time.Now()
	d.stateMu.Unlock()

	if err != nil {
		d.stateMu.Lock()
		defer d.stateMu.Unlock()
		lastValid := d.baseCount + d.lastPrintedCalculated
		return strconv.Itoa(lastValid), nil
	}

	reBatchGood := regexp.MustCompile(`<StringID>countBatchGood</StringID>\s*<Value>(\d+)</Value>`)
	matches := reBatchGood.FindStringSubmatch(resp)

	var current int
	if len(matches) > 1 {
		current, _ = strconv.Atoi(matches[1])
	} else {
		reAlloc := regexp.MustCompile(`<StringID>countAllocCurrent</StringID>\s*<Value>(\d+)</Value>`)
		matchesAlloc := reAlloc.FindStringSubmatch(resp)
		if len(matchesAlloc) > 1 {
			current, _ = strconv.Atoi(matchesAlloc[1])
		}
	}

	d.stateMu.Lock()
	defer d.stateMu.Unlock()

	if current < d.baseCount && current != 0 {
		d.baseCount = current
	}

	return strconv.Itoa(current), nil
}

func (d *Driver) GetBufferFreeSpace() (int, error) {
	countStr, err := d.GetCurrentPrintCount()

	d.stateMu.Lock()
	defer d.stateMu.Unlock()

	hwCount, _ := strconv.Atoi(countStr)
	if err == nil && hwCount >= d.baseCount {
		printedSinceStart := hwCount - d.baseCount
		d.lastPrintedCalculated = printedSinceStart
	}

	inBuffer := d.totalSent - d.lastPrintedCalculated
	if inBuffer < 0 {
		inBuffer = 0
	}

	freeSpace := SafeQueueLimit - inBuffer
	if freeSpace < 0 {
		freeSpace = 0
	}

	slog.Debug("MARKEM Software Buffer",
		"ip", d.Address,
		"sent", d.totalSent,
		"printed", d.lastPrintedCalculated,
		"in_buffer", inBuffer,
		"free_space", freeSpace,
	)

	return freeSpace, nil
}

func (d *Driver) GetStatus() (string, error) {
	resp, err := d.sendSOAPWaitACK(`<RequestPackMLStatus act="{act}"/>`)
	if err != nil {
		return "ОНЛАЙН", nil
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
	_, err := d.sendSOAPWaitACK(`<ClearPackDataQueue act="{act}"/>`)
	if err != nil {
		slog.Warn("MARKEM: Очистка очереди не вернула ACK, сбрасываем счетчики локально", "ip", d.Address, "err", err)
	}

	time.Sleep(150 * time.Millisecond)

	countStr, _ := d.GetCurrentPrintCount()
	hwCount, _ := strconv.Atoi(countStr)

	d.stateMu.Lock()
	d.baseCount = hwCount
	d.totalSent = 0
	d.lastPrintedCalculated = 0
	d.stateMu.Unlock()

	slog.Info("MARKEM: Очередь очищена, новый baseCount зафиксирован", "baseCount", hwCount)
	return nil
}

func (d *Driver) InitSession(fieldName string, maxQueue int, staticFields map[string]string) error {
	return d.ClearQueue()
}

func (d *Driver) PrintTemplate(template string, fields map[string]string) error {
	return d.SelectTemplate(template, fields)
}

func (d *Driver) GetTemplates() ([]string, error) {
	resp, err := d.sendSOAPWaitACK(`<RequestFileDirectoryListing act="{act}"><Filter>job</Filter></RequestFileDirectoryListing>`)
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
	countStr, _ := d.GetCurrentPrintCount()
	hwCount, _ := strconv.Atoi(countStr)

	d.stateMu.Lock()
	defer d.stateMu.Unlock()

	printedSinceStart := hwCount - d.baseCount
	if printedSinceStart < 0 {
		printedSinceStart = 0
	}

	return printedSinceStart, nil
}

func (d *Driver) GetCurrentTemplate() (string, error) {
	if d.curTemplate != "" {
		return d.curTemplate, nil
	}
	return "N/A", nil
}

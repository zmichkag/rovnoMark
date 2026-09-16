package markem_async

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf16"
)

const (
	MaxQueueLimit  = 150 // Аппаратный лимит SmartDate
	SafeQueueLimit = 100 // Софтовый лимит для Pumper
)

type Driver struct {
	Address    string
	Port       int
	ActorName  string
	SenderName string
	Timeout    time.Duration

	writeMu sync.Mutex
	conn    net.Conn

	// Диспетчер асинхронных ответов по токену act
	pendingMu sync.Mutex
	pending   map[int]chan string

	// Генератор act (диапазон 50000-59999 для 3rd Party Control Apps)
	actSeq int32

	// Кэш состояния в RAM (мгновенная неблокирующая отдача в Poller)
	stateMu               sync.RWMutex
	curStatus             string
	curRibbon             string
	curTemplate           string
	totalSent             int
	baseCount             int
	lastPrintedCalculated int
	hwBatchCount          int
	hwTotalPrints         int64
}

func New(ip string, port int, actorName string) *Driver {
	if actorName == "" {
		actorName = "Actor1"
	}
	d := &Driver{
		Address:       ip,
		Port:          port,
		ActorName:     actorName,
		SenderName:    "AstraPrint",
		Timeout:       5 * time.Second,
		pending:       make(map[int]chan string),
		curStatus:     "INITIALIZING",
		curRibbon:     "N/A",
		hwTotalPrints: -1,
	}
	atomic.StoreInt32(&d.actSeq, 50000)

	// Фоновый подъем сокета
	go func() {
		_ = d.ensureConnection()
	}()

	return d
}

// --- НИЗКОУРОВНЕВЫЙ СЛОЙ (UTF-16LE & XML) ---

func (d *Driver) getNextAct() int {
	for {
		cur := atomic.LoadInt32(&d.actSeq)
		next := cur + 1
		if next > 59999 {
			next = 50000
		}
		if atomic.CompareAndSwapInt32(&d.actSeq, cur, next) {
			return int(cur)
		}
	}
}

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

func (d *Driver) ensureConnection() error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	if d.conn != nil {
		return nil
	}

	address := net.JoinHostPort(d.Address, strconv.Itoa(d.Port))
	conn, err := net.DialTimeout("tcp", address, d.Timeout)
	if err != nil {
		d.stateMu.Lock()
		d.curStatus = "ОФФЛАЙН: " + err.Error()
		d.stateMu.Unlock()
		return err
	}

	d.conn = conn
	go d.readLoop(conn)

	// Инициализация диалога по спецификации DCP (п. 3.9)
	go func() {
		_ = d.rawSendNoWait(`<RequestPackMLStatus act="{act}"/>`)
		_ = d.rawSendNoWait(`<RequestSuppliesUpdate act="{act}"/>`)
		_ = d.rawSendNoWait(`<RequestCounts act="{act}"/>`)
	}()

	return nil
}

func (d *Driver) closeConnection() {
	d.writeMu.Lock()
	if d.conn != nil {
		_ = d.conn.Close()
		d.conn = nil
	}
	d.writeMu.Unlock()

	d.stateMu.Lock()
	d.curStatus = "ОФФЛАЙН"
	d.stateMu.Unlock()

	d.pendingMu.Lock()
	for act, ch := range d.pending {
		close(ch)
		delete(d.pending, act)
	}
	d.pendingMu.Unlock()
}

// readLoop ведет непрерывное чтение и парсинг потока пакетов
func (d *Driver) readLoop(conn net.Conn) {
	defer d.closeConnection()

	buf := make([]byte, 4096)
	var rawBuffer bytes.Buffer

	for {
		n, err := conn.Read(buf)
		if err != nil {
			if err != io.EOF {
				slog.Debug("MARKEM-ASYNC: сокет закрыт", "ip", d.Address, "err", err)
			}
			return
		}

		rawBuffer.Write(buf[:n])

		for {
			decoded := decodeUTF16LE(rawBuffer.Bytes())
			endIdx := strings.Index(decoded, "</Envelope>")
			if endIdx == -1 {
				break
			}

			envEnd := endIdx + len("</Envelope>")
			msgXML := decoded[:envEnd]

			processedBytes := len(encodeUTF16LE(msgXML))
			if processedBytes <= rawBuffer.Len() {
				rawBuffer.Next(processedBytes)
			} else {
				rawBuffer.Reset()
			}

			d.handleIncomingDCPMessage(msgXML)
		}
	}
}

func (d *Driver) handleIncomingDCPMessage(rawXML string) {
	// 1. Немедленный ответ на Keep-Alive Ping (п. 2.3)
	if strings.Contains(rawXML, "<Ping") {
		act := extractAttr(rawXML, "act")
		_ = d.sendRawResponse(fmt.Sprintf(`<CmdOK act="%s"/>`, act))
		return
	}

	// 2. Асинхронные события от принтера (Unsolicited Events)
	if strings.Contains(rawXML, "<CountsUpdateEvent") {
		d.parseCountsEvent(rawXML)
		return
	}
	if strings.Contains(rawXML, "<PackMLStatusUpdateEvent") {
		d.parsePackMLEvent(rawXML)
		return
	}
	if strings.Contains(rawXML, "<SuppliesUpdateEvent") {
		d.parseSuppliesEvent(rawXML)
		return
	}

	// 3. Маршрутизация ответов на синхронные команды
	actStr := extractAttr(rawXML, "act")
	if actStr != "" {
		if actVal, err := strconv.Atoi(actStr); err == nil {
			d.pendingMu.Lock()
			ch, exists := d.pending[actVal]
			d.pendingMu.Unlock()

			if exists {
				if strings.Contains(rawXML, "<CmdPending") {
					return
				}
				ch <- rawXML
			}
		}
	}
}

func (d *Driver) parseCountsEvent(xmlStr string) {
	d.stateMu.Lock()
	defer d.stateMu.Unlock()

	// countBatchGood
	if idx := strings.Index(xmlStr, "countBatchGood"); idx != -1 {
		valPart := xmlStr[idx:]
		if valStart := strings.Index(valPart, "<Value>"); valStart != -1 {
			valEnd := strings.Index(valPart[valStart:], "</Value>")
			if valEnd != -1 {
				numStr := valPart[valStart+7 : valStart+valEnd]
				if count, err := strconv.Atoi(strings.TrimSpace(numStr)); err == nil {
					d.hwBatchCount = count
					if count >= d.baseCount {
						d.lastPrintedCalculated = count - d.baseCount
					}
				}
			}
		}
	}

	// countTotalGood (абсолютный одометр)
	if idx := strings.Index(xmlStr, "countTotalGood"); idx != -1 {
		valPart := xmlStr[idx:]
		if valStart := strings.Index(valPart, "<Value>"); valStart != -1 {
			valEnd := strings.Index(valPart[valStart:], "</Value>")
			if valEnd != -1 {
				numStr := valPart[valStart+7 : valStart+valEnd]
				if total, err := strconv.ParseInt(strings.TrimSpace(numStr), 10, 64); err == nil {
					d.hwTotalPrints = total
				}
			}
		}
	}
}

func (d *Driver) parsePackMLEvent(xmlStr string) {
	d.stateMu.Lock()
	defer d.stateMu.Unlock()

	// Значения согласно PackML (п. 3.2.1)
	if strings.Contains(xmlStr, "<State>4</State>") || strings.Contains(xmlStr, "<State>5</State>") {
		d.curStatus = "ГОТОВ"
	} else if strings.Contains(xmlStr, "<State>6</State>") {
		d.curStatus = "ПЕЧАТЬ"
	} else if strings.Contains(xmlStr, "<State>2</State>") {
		d.curStatus = "ОСТАНОВЛЕН"
	} else if strings.Contains(xmlStr, "<State>8</State>") || strings.Contains(xmlStr, "<State>9</State>") {
		d.curStatus = "ОШИБКА"
	} else {
		d.curStatus = "ОНЛАЙН"
	}
}

func (d *Driver) parseSuppliesEvent(xmlStr string) {
	d.stateMu.Lock()
	defer d.stateMu.Unlock()

	if valStart := strings.Index(xmlStr, "<PercentageRemaining>"); valStart != -1 {
		valEnd := strings.Index(xmlStr[valStart:], "</PercentageRemaining>")
		if valEnd != -1 {
			d.curRibbon = strings.TrimSpace(xmlStr[valStart+21:valStart+valEnd]) + "%"
		}
	}
}

func extractAttr(xmlStr, attrName string) string {
	patternSingle := attrName + "='"
	patternDouble := attrName + "=\""

	var startIdx int
	if idx := strings.Index(xmlStr, patternSingle); idx != -1 {
		startIdx = idx + len(patternSingle)
	} else if idx := strings.Index(xmlStr, patternDouble); idx != -1 {
		startIdx = idx + len(patternDouble)
	} else {
		return ""
	}

	quote := xmlStr[startIdx-1]
	endIdx := strings.IndexByte(xmlStr[startIdx:], quote)
	if endIdx == -1 {
		return ""
	}
	return xmlStr[startIdx : startIdx+endIdx]
}

func (d *Driver) sendSOAPWaitACK(bodyXML string) (string, error) {
	if err := d.ensureConnection(); err != nil {
		return "", err
	}

	act := d.getNextAct()
	formattedBody := strings.ReplaceAll(bodyXML, "{act}", strconv.Itoa(act))
	soapMsg := fmt.Sprintf(`<?xml version="1.0" encoding="utf-16"?><Envelope><Header sender="%s" receiver="%s"/><Body>%s</Body></Envelope>`,
		d.SenderName, d.ActorName, formattedBody)
	payload := encodeUTF16LE(soapMsg)

	respChan := make(chan string, 1)

	d.pendingMu.Lock()
	d.pending[act] = respChan
	d.pendingMu.Unlock()

	defer func() {
		d.pendingMu.Lock()
		delete(d.pending, act)
		d.pendingMu.Unlock()
	}()

	d.writeMu.Lock()
	if d.conn == nil {
		d.writeMu.Unlock()
		return "", fmt.Errorf("нет соединения")
	}
	_ = d.conn.SetWriteDeadline(time.Now().Add(d.Timeout))
	_, err := d.conn.Write(payload)
	d.writeMu.Unlock()

	if err != nil {
		d.closeConnection()
		return "", fmt.Errorf("ошибка записи в сокет: %w", err)
	}

	select {
	case respXML, ok := <-respChan:
		if !ok {
			return "", fmt.Errorf("соединение разорвано до получения ответа")
		}
		if strings.Contains(respXML, "CmdFailed") || strings.Contains(respXML, "Fault") {
			return respXML, fmt.Errorf("принтер отклонил команду (CmdFailed)")
		}
		return respXML, nil

	case <-time.After(5 * time.Second):
		return "", fmt.Errorf("таймаут ожидания ответа act=%d", act)
	}
}

func (d *Driver) rawSendNoWait(bodyXML string) error {
	act := d.getNextAct()
	formattedBody := strings.ReplaceAll(bodyXML, "{act}", strconv.Itoa(act))
	soapMsg := fmt.Sprintf(`<?xml version="1.0" encoding="utf-16"?><Envelope><Header sender="%s" receiver="%s"/><Body>%s</Body></Envelope>`,
		d.SenderName, d.ActorName, formattedBody)
	payload := encodeUTF16LE(soapMsg)

	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	if d.conn == nil {
		return fmt.Errorf("нет соединения")
	}
	_, err := d.conn.Write(payload)
	return err
}

func (d *Driver) sendRawResponse(bodyXML string) error {
	soapMsg := fmt.Sprintf(`<?xml version="1.0" encoding="utf-16"?><Envelope><Header sender="%s" receiver="%s"/><Body>%s</Body></Envelope>`,
		d.SenderName, d.ActorName, bodyXML)
	payload := encodeUTF16LE(soapMsg)

	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	if d.conn == nil {
		return fmt.Errorf("нет соединения")
	}
	_, err := d.conn.Write(payload)
	return err
}

// --- РЕАЛИЗАЦИЯ КОНТРАКТА core.Printer ---

func (d *Driver) SelectTemplate(template string, fields map[string]string) error {
	slog.Info("MARKEM-ASYNC: Выбор шаблона", "ip", d.Address, "template", template)

	jobName := template
	if !strings.HasSuffix(strings.ToLower(jobName), ".job") {
		jobName += ".job"
	}

	d.stateMu.Lock()
	needSelect := d.curTemplate != template
	d.stateMu.Unlock()

	if needSelect {
		cmdSelect := `<SelectLocalJob act="{act}"><JobFileName>` + escapeXML(jobName) + `</JobFileName></SelectLocalJob>`
		if _, err := d.sendSOAPWaitACK(cmdSelect); err != nil {
			slog.Warn("MARKEM-ASYNC: Смена макета вернула предупреждение", "ip", d.Address, "err", err)
		}
		d.stateMu.Lock()
		d.curTemplate = template
		d.stateMu.Unlock()
	}

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
	return err
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
		slog.Error("MARKEM-ASYNC: Сбой загрузки очереди кодов", "ip", d.Address, "err", err)
		return 0, err
	}

	d.stateMu.Lock()
	d.totalSent += len(codes)
	d.stateMu.Unlock()

	return len(codes), nil
}

// GetStatus отдает статус мгновенно из памяти
func (d *Driver) GetStatus() (string, error) {
	if err := d.ensureConnection(); err != nil {
		return "ОФФЛАЙН", err
	}
	d.stateMu.RLock()
	defer d.stateMu.RUnlock()
	return d.curStatus, nil
}

// GetCurrentPrintCount мгновенно отдает последний подтвержденный батч
func (d *Driver) GetCurrentPrintCount() (string, error) {
	d.stateMu.RLock()
	defer d.stateMu.RUnlock()
	return strconv.Itoa(d.hwBatchCount), nil
}

// GetTotalPrints мгновенно отдает одометр, обновляемый событиями CountsUpdateEvent
func (d *Driver) GetTotalPrints() (int64, error) {
	d.stateMu.RLock()
	total := d.hwTotalPrints
	d.stateMu.RUnlock()

	if total >= 0 {
		return total, nil
	}

	// Если событие еще не прилетало — делаем разовый фоллбэк
	resp, err := d.sendSOAPWaitACK(`<RequestCounts act="{act}"/>`)
	if err != nil {
		return 0, err
	}
	d.parseCountsEvent(resp)

	d.stateMu.RLock()
	defer d.stateMu.RUnlock()
	return d.hwTotalPrints, nil
}

func (d *Driver) GetBufferFreeSpace() (int, error) {
	d.stateMu.RLock()
	defer d.stateMu.RUnlock()

	inBuffer := d.totalSent - d.lastPrintedCalculated
	if inBuffer < 0 {
		inBuffer = 0
	}
	freeSpace := SafeQueueLimit - inBuffer
	if freeSpace < 0 {
		freeSpace = 0
	}
	return freeSpace, nil
}

func (d *Driver) ClearQueue() error {
	slog.Info("MARKEM-ASYNC: Очистка очереди кодов", "ip", d.Address)
	_, _ = d.sendSOAPWaitACK(`<ClearPackDataQueue act="{act}"/>`)

	d.stateMu.Lock()
	d.baseCount = d.hwBatchCount
	d.totalSent = 0
	d.lastPrintedCalculated = 0
	d.stateMu.Unlock()
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

	var list []string
	dec := xml.NewDecoder(strings.NewReader(resp))
	for {
		t, err := dec.Token()
		if err != nil {
			break
		}
		if se, ok := t.(xml.StartElement); ok && se.Name.Local == "FileName" {
			var fn string
			if err := dec.DecodeElement(&fn, &se); err == nil {
				list = append(list, fn)
			}
		}
	}
	return list, nil
}

func (d *Driver) GetTemplateFields(templateName string) ([]string, error) {
	return []string{"DATAMATRIX", "date1", "date2", "PLU"}, nil
}

func (d *Driver) GetRemainingRibbon() (string, error) {
	d.stateMu.RLock()
	defer d.stateMu.RUnlock()
	return d.curRibbon, nil
}

func (d *Driver) GetQueueCapacity(queueName string) (string, error) {
	return strconv.Itoa(MaxQueueLimit), nil
}

func (d *Driver) GetPrintSpeed() (string, error) {
	return "N/A", nil
}

func (d *Driver) GetLastPrintedIndex() (int, error) {
	d.stateMu.RLock()
	defer d.stateMu.RUnlock()
	return d.lastPrintedCalculated, nil
}

func (d *Driver) GetCurrentTemplate() (string, error) {
	d.stateMu.RLock()
	defer d.stateMu.RUnlock()
	if d.curTemplate != "" {
		return d.curTemplate, nil
	}
	return "N/A", nil
}

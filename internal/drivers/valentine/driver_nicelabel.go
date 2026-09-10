package valentine

import (
	"bytes"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

type NiceLabelDriver struct {
	ID          int           // ID принтера в БД
	Address     string        // IP-адрес
	Port        int           // RAW TCP порт (9100)
	Timeout     time.Duration // Сетевой таймаут сокета
	curTemplate string        // Текущий выбранный макет

	mu   sync.Mutex // Защита монопольного доступа к сокету
	conn net.Conn   // Активный сокет

	lastCount   int // Виртуальный одометр
	lastRawFBBC int // Последнее сырое значение из FBBC
}

func NewNiceLabelDriver(id int, ip string, port int) *NiceLabelDriver {
	return &NiceLabelDriver{
		ID:          id,
		Address:     ip,
		Port:        port,
		Timeout:     2 * time.Second,
		conn:        nil,
		curTemplate: "",
		lastCount:   0,
		lastRawFBBC: 0,
	}
}

// InitSession поднимает соединение и сбрасывает счетчики
func (d *NiceLabelDriver) InitSession(fieldName string, maxQueue int, staticFields map[string]string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if err := d.ensureConnectionLocked(); err != nil {
		return fmt.Errorf("valentin [%s]: ошибка инициализации сокета: %w", d.Address, err)
	}

	d.lastCount = 0
	d.lastRawFBBC = 0

	slog.Info("VALENTIN: Сессия успешно открыта", "printer_id", d.ID, "addr", d.Address)
	return nil
}

// SelectTemplate выбирает макет из памяти (FMB) и обновляет статические поля
func (d *NiceLabelDriver) SelectTemplate(template string, staticFields map[string]string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if template != "" {
		d.curTemplate = template
	} else if d.curTemplate == "" {
		return fmt.Errorf("valentin [%d]: имя макета не задано", d.ID)
	}

	if err := d.ensureConnectionLocked(); err != nil {
		return fmt.Errorf("valentin [%s]: сбой связи: %w", d.Address, err)
	}

	// Пакетируем выбор макета (FMB) и установку статических полей в один сетевой буфер
	var buf bytes.Buffer

	// 1. Выбор файла из Flash-карты/ОЗУ: SOH + FMB---r + template + ETB
	buf.WriteByte(SOH)
	buf.WriteString(fmt.Sprintf("FMB---r%s", d.curTemplate))
	buf.WriteByte(ETB)

	// 2. Запись полей (поддерживаются как BV[Name], так и BM[Index])
	for k, v := range staticFields {
		cleanVal := strings.ReplaceAll(v, "|", "")
		buf.WriteByte(SOH)
		if _, err := strconv.Atoi(k); err == nil {
			buf.WriteString(fmt.Sprintf("BM[%s]%s", k, cleanVal))
		} else {
			buf.WriteString(fmt.Sprintf("BV[%s]%s", k, cleanVal))
		}
		buf.WriteByte(ETB)
	}

	// 3. Первичный взвод в готовность к приему триггера (FBC)
	buf.WriteByte(SOH)
	buf.WriteString("FBC---r--------")
	buf.WriteByte(ETB)

	_ = d.conn.SetWriteDeadline(time.Now().Add(d.Timeout))
	if _, err := d.conn.Write(buf.Bytes()); err != nil {
		d.closeConnLocked()
		return fmt.Errorf("valentin [%d]: сбой загрузки шаблона %s: %w", d.ID, d.curTemplate, err)
	}

	d.lastCount = 0
	d.lastRawFBBC = 0

	slog.Info("VALENTIN: Макет и статика успешно загружены", "printer_id", d.ID, "template", d.curTemplate)
	return nil
}

// PrintBatchIndexed осуществляет атомарную запись DataMatrix и взвод триггера
func (d *NiceLabelDriver) PrintBatchIndexed(fieldName string, startIndex int, codes []string) (int, error) {
	if len(codes) == 0 {
		return 0, nil
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	if err := d.ensureConnectionLocked(); err != nil {
		return 0, fmt.Errorf("valentin [%s]: ошибка сокета перед отправкой кода: %w", d.Address, err)
	}

	rawCode := codes[0]
	// Отсечение метаданных 1С (хвост после '|')
	if idx := strings.Index(rawCode, "|"); idx != -1 {
		rawCode = rawCode[:idx]
	}

	// Замена текстового маркера на бинарный ASCII 29 (GS)
	cleanCode := strings.ReplaceAll(rawCode, "<GS>", "\x1d")
	cleanCode = strings.TrimSpace(cleanCode)

	// Если динамическое поле не передано, по умолчанию берем поле "20"
	targetField := fieldName
	if targetField == "" {
		targetField = "20"
	}

	// Формируем атомарный фрейм: обновление поля + мгновенный взвод FBC
	var frame bytes.Buffer

	frame.WriteByte(SOH)
	if _, err := strconv.Atoi(targetField); err == nil {
		frame.WriteString(fmt.Sprintf("BM[%s]%s", targetField, cleanCode))
	} else {
		frame.WriteString(fmt.Sprintf("BV[%s]%s", targetField, cleanCode))
	}
	frame.WriteByte(ETB)

	frame.WriteByte(SOH)
	frame.WriteString("FBC---r--------")
	frame.WriteByte(ETB)

	_ = d.conn.SetWriteDeadline(time.Now().Add(d.Timeout))
	if _, err := d.conn.Write(frame.Bytes()); err != nil {
		d.closeConnLocked()
		return 0, fmt.Errorf("valentin [%d]: ошибка записи пакета в сокет: %w", d.ID, err)
	}

	return 1, nil
}

// GetCurrentPrintCount опрашивает регистр отпечатанных этикеток (FBBC)
func (d *NiceLabelDriver) GetCurrentPrintCount() (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if err := d.ensureConnectionLocked(); err != nil {
		return strconv.Itoa(d.lastCount), err
	}

	// Запрос регистра отпечатанных этикеток: SOH + FBBC--w + ETB
	cmd := []byte{SOH, 'F', 'B', 'B', 'C', '-', '-', 'w', ETB}
	_ = d.conn.SetDeadline(time.Now().Add(d.Timeout))

	if _, err := d.conn.Write(cmd); err != nil {
		d.closeConnLocked()
		return strconv.Itoa(d.lastCount), err
	}

	resp, err := d.readFrameLocked()
	if err != nil {
		if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
			return strconv.Itoa(d.lastCount), nil
		}
		d.closeConnLocked()
		return strconv.Itoa(d.lastCount), err
	}

	// Ответ принтера: SOH + A + NNNN + ... + ETB
	aIdx := strings.Index(resp, "A")
	if aIdx == -1 {
		return strconv.Itoa(d.lastCount), nil
	}

	var sb strings.Builder
	for _, ch := range resp[aIdx+1:] {
		if ch >= '0' && ch <= '9' {
			sb.WriteRune(ch)
		} else if sb.Len() > 0 {
			break
		}
	}

	if sb.Len() == 0 {
		return strconv.Itoa(d.lastCount), nil
	}

	rawCount, err := strconv.Atoi(sb.String())
	if err != nil {
		return strconv.Itoa(d.lastCount), nil
	}

	// Логика расчета дельты и защита от сброса внутреннего счетчика
	if rawCount < d.lastRawFBBC {
		d.lastRawFBBC = rawCount
	} else if rawCount > d.lastRawFBBC {
		delta := rawCount - d.lastRawFBBC
		d.lastCount += delta
		d.lastRawFBBC = rawCount
	}

	return strconv.Itoa(d.lastCount), nil
}

// GetStatus производит опрос состояния принтера через регистр ошибок FCMH (стр. 92)
func (d *NiceLabelDriver) GetStatus() (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if err := d.ensureConnectionLocked(); err != nil {
		return "ОФФЛАЙН", err
	}

	// Запрос регистра ошибок: SOH + FCMH--w + ETB (Мануал, стр. 92)
	cmd := []byte{SOH, 'F', 'C', 'M', 'H', '-', '-', 'w', ETB}
	_ = d.conn.SetDeadline(time.Now().Add(d.Timeout))

	if _, err := d.conn.Write(cmd); err != nil {
		d.closeConnLocked()
		return "ОФФЛАЙН", err
	}

	resp, err := d.readFrameLocked()
	if err != nil {
		d.closeConnLocked()
		return "ОФФЛАЙН", err
	}

	// Ожидаемый ответ: (SOH)ANNNN0000...(ETB)
	aIdx := strings.Index(resp, "A")
	if aIdx == -1 || len(resp) < aIdx+5 {
		// Если принтер ответил, но специфичный кадр не распарсился — не бракуем принтер
		return "ГОТОВ", nil
	}

	errCode := resp[aIdx+1 : aIdx+5]

	// 0000 означает отсутствие активных ошибок
	if errCode == "0000" {
		return "ГОТОВ", nil
	}

	// Известные критические коды ошибок Valentin CVPL:
	switch errCode {
	case "0001", "0020":
		return "ОШИБКА: РИББОН", nil
	case "0002", "0021":
		return "ОШИБКА: МАТЕРИАЛ", nil
	case "0004", "0035":
		return "ОШИБКА: ТЕРМОГОЛОВКА", nil
	default:
		slog.Warn("VALENTIN: Получен код предупреждения/ошибки", "printer_id", d.ID, "code", errCode)
		// Если это не фатальный отказ оборудования, даем работать
		return "ГОТОВ", nil
	}
}

// readFrameLocked читает байты до разделителя ETB
func (d *NiceLabelDriver) readFrameLocked() (string, error) {
	var buf bytes.Buffer
	b := make([]byte, 1)

	for {
		n, err := d.conn.Read(b)
		if err != nil {
			return buf.String(), err
		}
		if n == 0 {
			continue
		}

		if b[0] == ETB {
			break
		}
		if b[0] != SOH && b[0] != '\r' && b[0] != '\n' {
			buf.WriteByte(b[0])
		}
	}
	return buf.String(), nil
}

func (d *NiceLabelDriver) ensureConnectionLocked() error {
	if d.conn != nil {
		return nil
	}
	addr := net.JoinHostPort(d.Address, strconv.Itoa(d.Port))
	conn, err := net.DialTimeout("tcp", addr, d.Timeout)
	if err != nil {
		return err
	}

	if tcpConn, ok := conn.(*net.TCPConn); ok {
		_ = tcpConn.SetNoDelay(true)
		_ = tcpConn.SetKeepAlive(true)
		_ = tcpConn.SetKeepAlivePeriod(5 * time.Second)
	}

	d.conn = conn
	return nil
}

func (d *NiceLabelDriver) closeConnLocked() {
	if d.conn != nil {
		_ = d.conn.Close()
		d.conn = nil
	}
}

func (d *NiceLabelDriver) ClearQueue() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.lastCount = 0
	d.lastRawFBBC = 0
	return nil
}

func (d *NiceLabelDriver) GetLastPrintedIndex() (int, error) {
	return d.lastCount, nil
}

func (d *NiceLabelDriver) GetBufferFreeSpace() (int, error) {
	return 1, nil
}

func (d *NiceLabelDriver) GetTemplates() ([]string, error) {
	return []string{d.curTemplate}, nil
}

func (d *NiceLabelDriver) GetTemplateFields(t string) ([]string, error) {
	return []string{"20", "date01", "date02", "text01"}, nil
}

func (d *NiceLabelDriver) GetRemainingRibbon() (string, error)       { return "N/A", nil }
func (d *NiceLabelDriver) GetQueueCapacity(q string) (string, error) { return "N/A", nil }
func (d *NiceLabelDriver) GetPrintSpeed() (string, error)            { return "N/A", nil }
func (d *NiceLabelDriver) GetCurrentTemplate() (string, error)       { return d.curTemplate, nil }
func (d *NiceLabelDriver) UpdateStaticFields(f map[string]string) error {
	return d.SelectTemplate("", f)
}
func (d *NiceLabelDriver) PrintTemplate(t string, f map[string]string) error {
	return d.SelectTemplate(t, f)
}

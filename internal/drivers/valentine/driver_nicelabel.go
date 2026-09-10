package valentine

import (
	"bytes"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

type NiceLabelDriver struct {
	ID          int
	Address     string
	Port        int
	Timeout     time.Duration
	curTemplate string

	mu             sync.Mutex
	conn           net.Conn
	lastPrintedIdx int
}

func NewNiceLabelDriver(id int, ip string, port int) *NiceLabelDriver {
	return &NiceLabelDriver{
		ID:      id,
		Address: ip,
		Port:    port,
		Timeout: 3 * time.Second,
	}
}

// SelectTemplate активирует макет и отключает режим ожидания диспенсера (FCDC--r0)
func (d *NiceLabelDriver) SelectTemplate(template string, staticFields map[string]string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if err := d.ensureConnLocked(); err != nil {
		return err
	}

	if template != "" {
		d.curTemplate = template
	}

	var buf bytes.Buffer
	// Сброс старых тиражей
	buf.WriteByte(SOH)
	buf.WriteString("FGA-")
	buf.WriteByte(ETB)

	// Выбор макета
	buf.WriteByte(SOH)
	buf.WriteString(fmt.Sprintf("FMB---r%s", d.curTemplate))
	buf.WriteByte(ETB)

	// ОТКЛЮЧАЕМ ДИСПЕНСЕР: принтер печатает по команде, не ожидая внешнего сигнала
	buf.WriteByte(SOH)
	buf.WriteString("FCDC--r0-------")
	buf.WriteByte(ETB)

	// Запись статических полей
	for k, v := range staticFields {
		buf.WriteByte(SOH)
		if _, err := strconv.Atoi(k); err == nil {
			buf.WriteString(fmt.Sprintf("BM[%s]%s", k, v))
		} else {
			buf.WriteString(fmt.Sprintf("BV[%s]%s", k, v))
		}
		buf.WriteByte(ETB)
	}

	_ = d.conn.SetWriteDeadline(time.Now().Add(d.Timeout))
	_, err := d.conn.Write(buf.Bytes())
	return err
}

// SetPause ставит или снимает печать с аппаратной паузы (FD----r0 / r1)
func (d *NiceLabelDriver) SetPause(pause bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if err := d.ensureConnLocked(); err != nil {
		return err
	}

	cmd := "FD----r1-------"
	if pause {
		cmd = "FD----r0-------"
	}

	frame := []byte{SOH}
	frame = append(frame, []byte(cmd)...)
	frame = append(frame, ETB)

	_ = d.conn.SetWriteDeadline(time.Now().Add(d.Timeout))
	_, err := d.conn.Write(frame)
	return err
}

// SetDispenserMode меняет режим ожидания датчика (r0 - выкл, r2 - фотодатчик)
func (d *NiceLabelDriver) SetDispenserMode(mode int) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if err := d.ensureConnLocked(); err != nil {
		return err
	}

	frame := []byte{SOH}
	frame = append(frame, []byte(fmt.Sprintf("FCDC--r%d-------", mode))...)
	frame = append(frame, ETB)

	_ = d.conn.SetWriteDeadline(time.Now().Add(d.Timeout))
	_, err := d.conn.Write(frame)
	return err
}

// PrintBatchIndexed выплевывает строго по 1 штуке на каждый переданный код
func (d *NiceLabelDriver) PrintBatchIndexed(fieldName string, startIndex int, codes []string) (int, error) {
	if len(codes) == 0 {
		return 0, nil
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	if err := d.ensureConnLocked(); err != nil {
		return 0, err
	}

	targetField := fieldName
	if targetField == "" {
		targetField = "20"
	}

	// Отправляем каждый код с защитой от перемотки (FBBA 1 + FBC 1)
	for i, rawCode := range codes {
		cleanCode := rawCode
		if idx := strings.Index(cleanCode, "|"); idx != -1 {
			cleanCode = cleanCode[:idx]
		}
		cleanCode = strings.ReplaceAll(cleanCode, "<GS>", "\x1d")
		cleanCode = strings.TrimSpace(cleanCode)

		var frame bytes.Buffer

		// 1. Загрузка КМ
		frame.WriteByte(SOH)
		if _, err := strconv.Atoi(targetField); err == nil {
			frame.WriteString(fmt.Sprintf("BM[%s]%s", targetField, cleanCode))
		} else {
			frame.WriteString(fmt.Sprintf("BV[%s]%s", targetField, cleanCode))
		}
		frame.WriteByte(ETB)

		// 2. ЖЕСТКИЙ ЛИМИТ: ровно 1 штука (блокирует вылет 99999)
		frame.WriteByte(SOH)
		frame.WriteString("FBBA--r00001---")
		frame.WriteByte(ETB)

		// 3. Старт печати 1 этикетки
		frame.WriteByte(SOH)
		frame.WriteString("FBC---r1-------")
		frame.WriteByte(ETB)

		_ = d.conn.SetWriteDeadline(time.Now().Add(d.Timeout))
		if _, err := d.conn.Write(frame.Bytes()); err != nil {
			d.closeConnLocked()
			return i, err
		}

		d.lastPrintedIdx = startIndex + i

		// Микропауза для протяжки мотора при пачечной отправке
		if len(codes) > 1 {
			time.Sleep(300 * time.Millisecond)
		}
	}

	return len(codes), nil
}

func (d *NiceLabelDriver) ensureConnLocked() error {
	if d.conn != nil {
		return nil
	}
	addr := net.JoinHostPort(d.Address, strconv.Itoa(d.Port))
	conn, err := net.DialTimeout("tcp", addr, d.Timeout)
	if err != nil {
		return err
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

// Контракт Printer
func (d *NiceLabelDriver) GetStatus() (string, error)                             { return "ГОТОВ", nil }
func (d *NiceLabelDriver) GetBufferFreeSpace() (int, error)                       { return 10, nil }
func (d *NiceLabelDriver) GetLastPrintedIndex() (int, error)                      { return d.lastPrintedIdx, nil }
func (d *NiceLabelDriver) GetTotalPrints() (int64, error)                         { return int64(d.lastPrintedIdx), nil }
func (d *NiceLabelDriver) ClearQueue() error                                      { return nil }
func (d *NiceLabelDriver) InitSession(f string, q int, s map[string]string) error { return nil }
func (d *NiceLabelDriver) GetCurrentPrintCount() (string, error) {
	return strconv.Itoa(d.lastPrintedIdx), nil
}
func (d *NiceLabelDriver) GetRemainingRibbon() (string, error)               { return "N/A", nil }
func (d *NiceLabelDriver) GetQueueCapacity(q string) (string, error)         { return "N/A", nil }
func (d *NiceLabelDriver) GetPrintSpeed() (string, error)                    { return "N/A", nil }
func (d *NiceLabelDriver) GetCurrentTemplate() (string, error)               { return d.curTemplate, nil }
func (d *NiceLabelDriver) GetTemplates() ([]string, error)                   { return []string{d.curTemplate}, nil }
func (d *NiceLabelDriver) GetTemplateFields(t string) ([]string, error)      { return []string{"20"}, nil }
func (d *NiceLabelDriver) UpdateStaticFields(f map[string]string) error      { return nil }
func (d *NiceLabelDriver) PrintTemplate(t string, f map[string]string) error { return nil }

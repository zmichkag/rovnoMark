package valentine

import (
	"fmt"
	"log/slog"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

var fbbcRegex = regexp.MustCompile(`A(\d+)`)

const (
	// Жесткий размер буфера FIFO в RAM принтера для Режима 1
	fifoBufferSize = 10
)

type NativeDriver struct {
	Address     string
	Port        int
	Timeout     time.Duration
	conn        net.Conn
	mu          sync.Mutex
	curTemplate string

	totalSent    int // Сколько всего кодов улетело в FBC
	totalPrinted int // Сколько этикеток реально сошло с ножа
	lastRawFBBC  int // Сырое значение одометра FBBC
}

func NewNativeDriver(ip string, port int) *NativeDriver {
	if port <= 0 {
		port = 9100
	}
	return &NativeDriver{
		Address: ip,
		Port:    port,
		Timeout: 3 * time.Second,
	}
}

func (d *NativeDriver) InitSession(fieldName string, maxQueue int, staticFields map[string]string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if err := d.reconnectNoLock(); err != nil {
		return err
	}

	// Отключаем спам дисплея и включаем Режим 1
	_ = d.writeCmdNoLock("CTE---r0-------")
	_ = d.writeCmdNoLock("FCDC--r1-------")

	d.totalSent = 0
	d.totalPrinted = 0
	d.lastRawFBBC = 0

	slog.Info("VALENTIN-NATIVE: Инициализация выполнена (Mode 1, CTE 0)", "addr", d.Address)
	return nil
}

func (d *NativeDriver) SelectTemplate(template string, staticFields map[string]string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.curTemplate = template
	if err := d.reconnectNoLock(); err != nil {
		return err
	}

	if err := d.writeCmdNoLock(fmt.Sprintf("FMB---r%s", d.curTemplate)); err != nil {
		return err
	}
	time.Sleep(30 * time.Millisecond)

	// Статика (BM18, BM19, BM21)
	if v := staticFields["date01"]; v != "" {
		_ = d.writeCmdNoLock(fmt.Sprintf("BM[18]%s", v))
	}
	if v := staticFields["date02"]; v != "" {
		_ = d.writeCmdNoLock(fmt.Sprintf("BM[19]%s", v))
	}
	if v := staticFields["text01"]; v != "" {
		_ = d.writeCmdNoLock(fmt.Sprintf("BM[21]%s", v))
	}

	return nil
}

// GetBufferFreeSpace отдаёт ядру размер доступного слота под пачку
func (d *NativeDriver) GetBufferFreeSpace() (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	inFlight := d.totalSent - d.totalPrinted
	free := fifoBufferSize - inFlight
	if free <= 0 {
		return 0, nil
	}
	return free, nil
}

// PrintBatchIndexed принимает пачку от pumperRoutine и шлёт в сокет
func (d *NativeDriver) PrintBatchIndexed(fieldName string, startIndex int, codes []string) (int, error) {
	if len(codes) == 0 {
		return 0, nil
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	if err := d.reconnectNoLock(); err != nil {
		return 0, err
	}

	targetField := "20"
	if fieldName != "" {
		targetField = fieldName
	}

	sent := 0
	for _, raw := range codes {
		clean := d.normalizeDM(raw)

		_ = d.writeCmdNoLock(fmt.Sprintf("BM[%s]%s", targetField, clean))
		_ = d.writeCmdNoLock("FBBA--r00001---")
		_ = d.writeCmdNoLock("FBC---r--------")

		d.totalSent++
		sent++
	}

	return sent, nil
}

// GetCurrentPrintCount опрашивается тикером pumperRoutine для фиксации прогресса
func (d *NativeDriver) GetCurrentPrintCount() (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.conn == nil {
		if err := d.reconnectNoLock(); err != nil {
			return strconv.Itoa(d.totalPrinted), err
		}
	}

	_ = d.conn.SetDeadline(time.Now().Add(45 * time.Millisecond))
	query := fmt.Sprintf("%cFBBC--w        %c", SOH, ETB)
	if _, err := d.conn.Write([]byte(query)); err != nil {
		d.closeConnNoLock()
		return strconv.Itoa(d.totalPrinted), err
	}

	buf := make([]byte, 128)
	n, err := d.conn.Read(buf)
	if err != nil {
		return strconv.Itoa(d.totalPrinted), nil
	}

	resp := string(buf[:n])
	if strings.Contains(resp, "TD\"") && !strings.Contains(resp, "A") {
		return strconv.Itoa(d.totalPrinted), nil
	}

	if match := fbbcRegex.FindStringSubmatch(resp); len(match) > 1 {
		val, _ := strconv.Atoi(match[1])
		if val > d.lastRawFBBC {
			d.totalPrinted += (val - d.lastRawFBBC)
		} else if val < d.lastRawFBBC && val > 0 {
			// При занулении одометра новым кадром
			d.totalPrinted += val
		}
		d.lastRawFBBC = val
	}

	return strconv.Itoa(d.totalPrinted), nil
}

func (d *NativeDriver) writeCmdNoLock(cmd string) error {
	frame := []byte(fmt.Sprintf("%c%s%c", SOH, cmd, ETB))
	_ = d.conn.SetWriteDeadline(time.Now().Add(d.Timeout))
	if _, err := d.conn.Write(frame); err != nil {
		d.closeConnNoLock()
		return err
	}
	time.Sleep(15 * time.Millisecond)
	return nil
}

func (d *NativeDriver) normalizeDM(raw string) string {
	code := raw
	if idx := strings.Index(code, "|"); idx != -1 {
		code = code[:idx]
	}
	code = strings.ReplaceAll(code, "<GS>", "\x1d")
	if !strings.HasPrefix(code, "~1") {
		code = "~1" + code
	}
	return strings.TrimSpace(code)
}

func (d *NativeDriver) reconnectNoLock() error {
	if d.conn != nil {
		return nil
	}
	addr := net.JoinHostPort(d.Address, strconv.Itoa(d.Port))
	conn, err := net.DialTimeout("tcp", addr, d.Timeout)
	if err != nil {
		return err
	}
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetNoDelay(true)
		_ = tcp.SetKeepAlive(true)
	}
	d.conn = conn
	return nil
}

func (d *NativeDriver) closeConnNoLock() {
	if d.conn != nil {
		_ = d.conn.Close()
		d.conn = nil
	}
}

func (d *NativeDriver) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closeConnNoLock()
	return nil
}

func (d *NativeDriver) ClearQueue() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_ = d.writeCmdNoLock("FD----r0-------")
	_ = d.writeCmdNoLock("FGA---r--------")
	d.totalSent = 0
	d.totalPrinted = 0
	d.lastRawFBBC = 0
	return nil
}

// Заглушки под интерфейс core.Printer
func (d *NativeDriver) GetStatus() (string, error)                        { return "ГОТОВ", nil }
func (d *NativeDriver) GetLastPrintedIndex() (int, error)                 { return d.totalPrinted, nil }
func (d *NativeDriver) GetTotalPrints() (int64, error)                    { return int64(d.totalPrinted), nil }
func (d *NativeDriver) UpdateStaticFields(f map[string]string) error      { return nil }
func (d *NativeDriver) PrintTemplate(t string, f map[string]string) error { return nil }
func (d *NativeDriver) GetTemplates() ([]string, error)                   { return []string{d.curTemplate}, nil }
func (d *NativeDriver) GetTemplateFields(t string) ([]string, error)      { return []string{"20"}, nil }
func (d *NativeDriver) GetRemainingRibbon() (string, error)               { return "N/A", nil }
func (d *NativeDriver) GetQueueCapacity(q string) (string, error)         { return "N/A", nil }
func (d *NativeDriver) GetPrintSpeed() (string, error)                    { return "N/A", nil }
func (d *NativeDriver) GetCurrentTemplate() (string, error)               { return d.curTemplate, nil }
func (d *NativeDriver) SetDispenserMode(mode int) error                   { return nil }
func (d *NativeDriver) SetPause(pause bool) error                         { return nil }

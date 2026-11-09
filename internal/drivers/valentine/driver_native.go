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

var fbbcMatchRegex = regexp.MustCompile(`A(\d+)`)

type NativeDriver struct {
	Name        string        // Имя макета / PLU
	ID          int           // ID принтера в базе данных
	Address     string        // IP-адрес принтера (например, 192.168.x.x)
	Port        int           // RAW TCP порт (9100)
	Timeout     time.Duration // Сетевой таймаут сокета
	conn        net.Conn      // Активная монопольная TCP-сессия
	mu          sync.Mutex    // Мьютекс для защиты сокета при многопоточном вызове
	curTemplate string        // Активный выбранный шаблон

	// Конфигурация скользящего окна
	BatchSizeS int // Размер доливки S (по умолчанию 10 шт.)
	ThresholdN int // Нижний порог N (по умолчанию 3 шт.)

	// Метрики конвейера
	totalSent    int // Всего передано в принтер
	totalPrinted int // Всего сошло физически с ножа
	lastRawFBBC  int // Последнее сырое значение регистра FBBC

	cmdDelay time.Duration
}

func NewNativeDriver(ip string, port int) *NativeDriver {
	if port <= 0 {
		port = 9100
	}
	return &NativeDriver{
		Address:      ip,
		Port:         port,
		Timeout:      3 * time.Second,
		cmdDelay:     20 * time.Millisecond,
		BatchSizeS:   10,
		ThresholdN:   3,
		totalSent:    0,
		totalPrinted: 0,
		lastRawFBBC:  0,
	}
}

// InitSession поднимает соединение, отключает терминальный спам и переводит трак в Режим 1
func (d *NativeDriver) InitSession(fieldName string, maxQueue int, staticFields map[string]string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if err := d.reconnectNoLock(); err != nil {
		return fmt.Errorf("native init: ошибка сокета %s: %w", d.Address, err)
	}

	//// 1. Отключаем асинхронный спам TD"
	//if err := d.writeCmdNoLock("CTE---r0-------", "Disable TD Spam"); err != nil {
	//	return err
	//}

	// 2. Включаем Режим 1 (Внешний статический диспенсер)
	if err := d.writeCmdNoLock("FCDC--r1-------", "Set Mode 1"); err != nil {
		return err
	}

	d.totalSent = 0
	d.totalPrinted = 0
	d.lastRawFBBC = 0

	slog.Info("VALENTIN-NATIVE: Сессия инициализирована в Режиме 1 (FCDC 1)", "addr", d.Address)
	return nil
}

// SelectTemplate выбирает макет и записывает статические поля дат
func (d *NativeDriver) SelectTemplate(template string, staticFields map[string]string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if template != "" {
		d.curTemplate = template
	} else if d.curTemplate == "" {
		return fmt.Errorf("native: пустой код макета (PLU)")
	}

	if err := d.reconnectNoLock(); err != nil {
		return err
	}

	// Загружаем макет
	if err := d.writeCmdNoLock(fmt.Sprintf("FMB---r%s", d.curTemplate), "Select Template"); err != nil {
		return err
	}
	time.Sleep(40 * time.Millisecond)

	// Статика
	dateProd := staticFields["date01"]
	dateExp := staticFields["date02"]
	//text01 := staticFields["text01"]

	if strings.TrimSpace(dateProd) == "" {
		dateProd = staticFields["18"]
	}
	if strings.TrimSpace(dateExp) == "" {
		dateExp = staticFields["19"]
	}
	//if strings.TrimSpace(text01) == "" {
	//	text01 = staticFields["21"]
	//}

	if dateProd != "" {
		_ = d.writeCmdNoLock(fmt.Sprintf("BM[18]%s", dateProd), "Set DateProd")
	}
	if dateExp != "" {
		_ = d.writeCmdNoLock(fmt.Sprintf("BM[19]%s", dateExp), "Set DateExp")
	}
	//if text01 != "" {
	//	_ = d.writeCmdNoLock(fmt.Sprintf("BM[21]%s", text01), "Set Batch")
	//}

	return nil
}

// PrintBatchIndexed наливает пачку кодов S поштучно
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

		// BM -> FBBA 1 -> FBC
		if err := d.writeCmdNoLock(fmt.Sprintf("BM[21]%s", targetField, clean), "Set DM"); err != nil {
			return sent, err
		}
		if err := d.writeCmdNoLock("FBBA--r00001---", "Set Qty 1"); err != nil {
			return sent, err
		}
		if err := d.writeCmdNoLock("FBC---r--------", "Arm Next Print"); err != nil {
			return sent, err
		}

		d.totalSent++
		sent++
	}

	slog.Info("VALENTIN-NATIVE: Долита пачка кодов",
		"added", sent,
		"total_sent", d.totalSent,
		"in_flight", d.totalSent-d.totalPrinted,
	)
	return sent, nil
}

// GetBufferFreeSpace управляет скоростью накачки ядра: сообщает ядру, есть ли место под пачку S
func (d *NativeDriver) GetBufferFreeSpace() (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.updateOdometerNoLock()

	inFlight := d.totalSent - d.totalPrinted
	if inFlight <= d.ThresholdN {
		// Буфер просел ниже порога N: просим у ядра долить пачку размера S
		return d.BatchSizeS, nil
	}

	// В тракте еще достаточно этикеток (> N), ядро должно подождать схода
	return 0, nil
}

// updateOdometerNoLock опрашивает FBBC и корректирует виртуальный счетчик
func (d *NativeDriver) updateOdometerNoLock() {
	if d.conn == nil {
		if err := d.reconnectNoLock(); err != nil {
			return
		}
	}

	// Сброс старых ответов
	_ = d.conn.SetReadDeadline(time.Now().Add(15 * time.Millisecond))
	drain := make([]byte, 256)
	for {
		if n, _ := d.conn.Read(drain); n == 0 {
			break
		}
	}

	_ = d.conn.SetDeadline(time.Now().Add(d.Timeout))
	query := fmt.Sprintf("%cFBBC--w        %c", SOH, ETB)
	if _, err := d.conn.Write([]byte(query)); err != nil {
		d.closeConnNoLock()
		return
	}

	buf := make([]byte, 256)
	start := time.Now()
	for time.Since(start) < 150*time.Millisecond {
		n, err := d.conn.Read(buf)
		if err != nil {
			break
		}
		resp := string(buf[:n])
		if strings.Contains(resp, "TD\"") && !strings.Contains(resp, "A") {
			continue
		}
		if match := fbbcMatchRegex.FindStringSubmatch(resp); len(match) > 1 {
			val, _ := strconv.Atoi(match[1])
			if val > d.lastRawFBBC {
				d.totalPrinted += (val - d.lastRawFBBC)
			} else if val < d.lastRawFBBC && val > 0 {
				// Обработка сброса одометра при переподаче кадра
				d.totalPrinted += val
			}
			d.lastRawFBBC = val
			return
		}
	}
}

func (d *NativeDriver) writeCmdNoLock(cmd, desc string) error {
	frame := []byte(fmt.Sprintf("%c%s%c", SOH, cmd, ETB))
	_ = d.conn.SetWriteDeadline(time.Now().Add(d.Timeout))
	if _, err := d.conn.Write(frame); err != nil {
		d.closeConnNoLock()
		return fmt.Errorf("сбой отправки [%s]: %w", desc, err)
	}
	time.Sleep(d.cmdDelay)
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
	_ = d.writeCmdNoLock("FD----r0-------", "Stop")
	_ = d.writeCmdNoLock("FGA---r--------", "Clear FGA")
	d.totalSent = 0
	d.totalPrinted = 0
	d.lastRawFBBC = 0
	return nil
}

// Реализация контракта core.Printer для интеграции с manager.go
func (d *NativeDriver) GetStatus() (string, error)        { return "ГОТОВ", nil }
func (d *NativeDriver) GetLastPrintedIndex() (int, error) { return d.totalPrinted, nil }
func (d *NativeDriver) GetTotalPrints() (int64, error)    { return int64(d.totalPrinted), nil }
func (d *NativeDriver) GetCurrentPrintCount() (string, error) {
	return strconv.Itoa(d.totalPrinted), nil
}
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

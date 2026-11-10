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

var fbbcNativeRegex = regexp.MustCompile(`A(\d+)`)

type NativeDriver struct {
	ID          int           // ID принтера
	Address     string        // IP-адрес принтера
	Port        int           // Порт CVPL (9100)
	Timeout     time.Duration // Сетевой таймаут сокета
	conn        net.Conn      // Активная TCP-сессия
	mu          sync.Mutex    // Мьютекс сокета
	curTemplate string        // Активный выбранный макет

	totalSent    int // Сквозной счетчик отправленных заданий
	totalPrinted int // Физический одометр (нарастающий итог)
	lastRawFBBC  int // Последнее значение аппаратного регистра FBBC
}

func NewNativeDriver(ip string, port int) *NativeDriver {
	if port <= 0 {
		port = 9100
	}
	return &NativeDriver{
		Address:      ip,
		Port:         port,
		Timeout:      3 * time.Second,
		conn:         nil,
		curTemplate:  "",
		totalSent:    0,
		totalPrinted: 0,
		lastRawFBBC:  0,
	}
}

// InitSession поднимает сокет, глушит спам дисплея и переводит тракт в Режим 1
func (d *NativeDriver) InitSession(fieldName string, maxQueue int, staticFields map[string]string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.conn == nil {
		addr := net.JoinHostPort(d.Address, strconv.Itoa(d.Port))
		conn, err := net.DialTimeout("tcp", addr, d.Timeout)
		if err != nil {
			return fmt.Errorf("native init: ошибка подключения к %s: %w", addr, err)
		}
		d.optimizeSocket(conn)
		d.conn = conn
	}

	// 1. Отключаем асинхронный спам строк TD" в сокет
	_ = d.writeRawCmdNoLock("CTE---r0-------")
	time.Sleep(20 * time.Millisecond)

	// 2. Включаем Режим 1: внешний статический диспенсер
	_ = d.writeRawCmdNoLock("FCDC--r1-------")
	time.Sleep(20 * time.Millisecond)

	d.totalSent = 0
	d.totalPrinted = 0
	d.lastRawFBBC = 0

	slog.Info("VALENTIN-NATIVE: Сессия инициализирована (FCDC 1, CTE 0)", "addr", d.Address)
	return nil
}

// SelectTemplate повторяет логику NiceLabel: строгая фильтрация полей без лишнего мусора
func (d *NativeDriver) SelectTemplate(template string, staticFields map[string]string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if template != "" {
		d.curTemplate = template
	} else if d.curTemplate == "" {
		return fmt.Errorf("native select: передан пустой код макета (PLU)")
	}

	if d.conn == nil {
		if err := d.reconnectNoLock(); err != nil {
			return fmt.Errorf("native select: ошибка реконнекта: %w", err)
		}
	}

	// Вытягиваем только строго разрешенные поля (как в driver_nicelabel)
	dateProd := staticFields["date01"]
	dateExp := staticFields["date02"]
	text01 := staticFields["text01"]

	if strings.TrimSpace(dateProd) == "" {
		dateProd = staticFields["18"]
	}
	if strings.TrimSpace(dateExp) == "" {
		dateExp = staticFields["19"]
	}
	if strings.TrimSpace(text01) == "" {
		text01 = staticFields["21"]
	}

	slog.Info("VALENTIN-NATIVE: Покомандная загрузка макета и статики",
		"template", d.curTemplate,
		"date_prod", dateProd,
		"date_exp", dateExp,
		"text01", text01,
	)

	// ШАГ 1: Загрузка макета из Flash (FMB)
	cmdFMB := fmt.Sprintf("FMB---r%s", d.curTemplate)
	if err := d.writeRawCmdNoLock(cmdFMB); err != nil {
		return fmt.Errorf("сбой отправки FMB: %w", err)
	}
	time.Sleep(35 * time.Millisecond) // Пауза на переключение буфера в RAM

	// ШАГ 2: Запись даты производства строго в поле 18 (BM[18])
	if dateProd != "" {
		if err := d.writeRawCmdNoLock(fmt.Sprintf("BM[18]%s", dateProd)); err != nil {
			return fmt.Errorf("сбой отправки BM[18]: %w", err)
		}
		time.Sleep(15 * time.Millisecond)
	}

	// ШАГ 3: Запись даты годности строго в поле 19 (BM[19])
	if dateExp != "" {
		if err := d.writeRawCmdNoLock(fmt.Sprintf("BM[19]%s", dateExp)); err != nil {
			return fmt.Errorf("сбой отправки BM[19]: %w", err)
		}
		time.Sleep(15 * time.Millisecond)
	}

	// ШАГ 4: Запись текста строго в поле 21 (BM[21])
	if text01 != "" {
		if err := d.writeRawCmdNoLock(fmt.Sprintf("BM[21]%s", text01)); err != nil {
			return fmt.Errorf("сбой отправки BM[21]: %w", err)
		}
		time.Sleep(15 * time.Millisecond)
	}

	// ШАГ 5: Первичный взвод ожидания датчика
	if err := d.writeRawCmdNoLock("FBC---r--------"); err != nil {
		return fmt.Errorf("сбой отправки FBC: %w", err)
	}

	d.totalSent = 0
	d.totalPrinted = 0
	d.lastRawFBBC = 0

	return nil
}

// PrintBatchIndexed строго использует поле [20], игнорируя посторонние имена из ядра
func (d *NativeDriver) PrintBatchIndexed(fieldName string, startIndex int, codes []string) (int, error) {
	if len(codes) == 0 {
		return 0, nil
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	if d.conn == nil {
		if err := d.reconnectNoLock(); err != nil {
			return 0, fmt.Errorf("native print: ошибка сокета: %w", err)
		}
	}

	sent := 0
	for _, rawCode := range codes {
		cleanCode := rawCode

		// 1. Отрезаем метаданные 1С (после '|')
		if idx := strings.Index(cleanCode, "|"); idx != -1 {
			cleanCode = cleanCode[:idx]
		}

		// 2. Преобразуем <GS> в байт 0x1D и добавляем префикс ~1 для FNC1
		cleanCode = strings.ReplaceAll(cleanCode, "<GS>", "\x1d")
		if !strings.HasPrefix(cleanCode, "~1") {
			cleanCode = "~1" + cleanCode
		}
		cleanCode = strings.TrimSpace(cleanCode)

		// 3. СТРОГО ПОЛЕ [20] (никаких dm_data0 и других алиасов!)
		cmdBM20 := fmt.Sprintf("BM[20]%s", cleanCode)
		if err := d.writeRawCmdNoLock(cmdBM20); err != nil {
			return sent, fmt.Errorf("сбой отправки BM[20]: %w", err)
		}

		// 🛑 Обязательная технологическая пауза 15мс на растеризацию 2D-кода в RAM
		time.Sleep(15 * time.Millisecond)

		// 4. ТИРАЖ: строго 1 оттиск
		if err := d.writeRawCmdNoLock("FBBA--r00001---"); err != nil {
			return sent, fmt.Errorf("сбой отправки FBBA: %w", err)
		}
		time.Sleep(5 * time.Millisecond)

		// 5. ВЗВОД ТРИГГЕРА ПОД ДАТЧИК
		if err := d.writeRawCmdNoLock("FBC---r--------"); err != nil {
			return sent, fmt.Errorf("сбой отправки FBC: %w", err)
		}
		time.Sleep(10 * time.Millisecond)

		d.totalSent++
		sent++
	}

	return sent, nil
}

// GetCurrentPrintCount опрашивает FBBC с коротким дедлайном и фильтрацией TD"
func (d *NativeDriver) GetCurrentPrintCount() (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.conn == nil {
		if err := d.reconnectNoLock(); err != nil {
			return strconv.Itoa(d.totalPrinted), fmt.Errorf("сокет закрыт: %w", err)
		}
	}

	// Короткий дедлайн для исключения блокировки тикера
	_ = d.conn.SetDeadline(time.Now().Add(45 * time.Millisecond))
	query := fmt.Sprintf("%cFBBC--w        %c", SOH, ETB)

	if _, err := d.conn.Write([]byte(query)); err != nil {
		d.closeConnNoLock()
		return strconv.Itoa(d.totalPrinted), err
	}

	buf := make([]byte, 128)
	n, err := d.conn.Read(buf)
	if err != nil {
		// При таймауте возвращаем текущее накопленное значение
		return strconv.Itoa(d.totalPrinted), nil
	}

	rawResponse := string(buf[:n])
	if strings.Contains(rawResponse, "TD\"") || !strings.Contains(rawResponse, "A") {
		return strconv.Itoa(d.totalPrinted), nil
	}

	cleanResp := strings.Trim(rawResponse, string([]byte{SOH, byte(ETB), '\r', '\n', ' '}))
	aIdx := strings.Index(cleanResp, "A")
	if aIdx == -1 {
		return strconv.Itoa(d.totalPrinted), nil
	}

	numStr := ""
	for _, char := range cleanResp[aIdx+1:] {
		if char >= '0' && char <= '9' {
			numStr += string(char)
		} else {
			break
		}
	}

	if numStr == "" {
		return strconv.Itoa(d.totalPrinted), nil
	}

	rawCount, _ := strconv.Atoi(numStr)

	// Защита и учет дельты
	if rawCount > d.lastRawFBBC {
		delta := rawCount - d.lastRawFBBC
		if delta < 10 {
			d.totalPrinted += delta
		}
		d.lastRawFBBC = rawCount
	} else if rawCount < d.lastRawFBBC && rawCount > 0 {
		// Сброс одометра прошивкой при обновлении поля
		d.totalPrinted += rawCount
		d.lastRawFBBC = rawCount
	}

	return strconv.Itoa(d.totalPrinted), nil
}

// GetBufferFreeSpace сообщает ядру о готовности принять следующую этикетку
func (d *NativeDriver) GetBufferFreeSpace() (int, error) {
	// Как и в NiceLabelDriver, отдаем 1, чтобы пампер ядра работал в своем штатном ритме
	return 1, nil
}

func (d *NativeDriver) writeRawCmdNoLock(cmd string) error {
	frame := []byte(fmt.Sprintf("%c%s%c", SOH, cmd, ETB))
	_ = d.conn.SetWriteDeadline(time.Now().Add(d.Timeout))
	if _, err := d.conn.Write(frame); err != nil {
		d.closeConnNoLock()
		return err
	}
	return nil
}

func (d *NativeDriver) optimizeSocket(conn net.Conn) {
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		_ = tcpConn.SetNoDelay(true)
		_ = tcpConn.SetKeepAlive(true)
		_ = tcpConn.SetKeepAlivePeriod(5 * time.Second)
	}
}

func (d *NativeDriver) reconnectNoLock() error {
	addr := net.JoinHostPort(d.Address, strconv.Itoa(d.Port))
	conn, err := net.DialTimeout("tcp", addr, d.Timeout)
	if err != nil {
		return err
	}
	d.optimizeSocket(conn)
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
	_ = d.writeRawCmdNoLock("FD----r0-------")
	_ = d.writeRawCmdNoLock("FGA---r--------")
	d.totalSent = 0
	d.totalPrinted = 0
	d.lastRawFBBC = 0
	return nil
}

// Реализация контракта core.Printer
func (d *NativeDriver) GetStatus() (string, error)                        { return "ГОТОВ", nil }
func (d *NativeDriver) GetLastPrintedIndex() (int, error)                 { return d.totalPrinted, nil }
func (d *NativeDriver) GetTotalPrints() (int64, error)                    { return int64(d.totalPrinted), nil }
func (d *NativeDriver) UpdateStaticFields(f map[string]string) error      { return nil }
func (d *NativeDriver) PrintTemplate(t string, f map[string]string) error { return nil }
func (d *NativeDriver) GetTemplates() ([]string, error)                   { return []string{d.curTemplate}, nil }
func (d *NativeDriver) GetTemplateFields(t string) ([]string, error) {
	return []string{"18", "19", "20", "21"}, nil
}
func (d *NativeDriver) GetRemainingRibbon() (string, error)       { return "N/A", nil }
func (d *NativeDriver) GetQueueCapacity(q string) (string, error) { return "N/A", nil }
func (d *NativeDriver) GetPrintSpeed() (string, error)            { return "N/A", nil }
func (d *NativeDriver) GetCurrentTemplate() (string, error)       { return d.curTemplate, nil }
func (d *NativeDriver) SetDispenserMode(mode int) error           { return nil }
func (d *NativeDriver) SetPause(pause bool) error                 { return nil }

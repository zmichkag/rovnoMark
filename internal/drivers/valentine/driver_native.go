package valentine

import (
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

type NativeDriver struct {
	Address     string
	Port        int
	Timeout     time.Duration
	conn        net.Conn
	mu          sync.Mutex
	curTemplate string

	// Монотонный счетчик отправленных в буфер кодов
	totalSent int

	// Ограничитель очереди "в тракте"
	maxInFlight int
	inFlight    int
}

func NewNativeDriver(ip string, port int) *NativeDriver {
	if port <= 0 {
		port = 9100
	}
	return &NativeDriver{
		Address:     ip,
		Port:        port,
		Timeout:     3 * time.Second,
		conn:        nil,
		totalSent:   0,
		maxInFlight: 2, // Не больше 2 этикеток в тракте одновременно
		inFlight:    0,
	}
}

// InitSession только поднимает сокет и глушит спам дисплея
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

	// 1. Отключаем асинхронный спам TD"
	//_ = d.writeRawCmdNoLock("CTE---r0-------", "Disable TD Spam")

	d.totalSent = 0
	d.inFlight = 0

	slog.Info("VALENTIN-NATIVE: Сокет поднят, спам дисплея заглушен", "addr", d.Address)
	return nil
}

// SelectTemplate загружает макет, заполняет статику и ТОЛЬКО ПОТОМ переводит в Режим 1
func (d *NativeDriver) SelectTemplate(template string, staticFields map[string]string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if template != "" {
		d.curTemplate = template
	} else if d.curTemplate == "" {
		return fmt.Errorf("native select: передан пустой PLU")
	}

	if d.conn == nil {
		if err := d.reconnectNoLock(); err != nil {
			return fmt.Errorf("native select: ошибка реконнекта: %w", err)
		}
	}

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
	if err := d.writeRawCmdNoLock(cmdFMB, "FMB"); err != nil {
		return fmt.Errorf("сбой FMB: %w", err)
	}
	slog.Info("VALENTIN-NATIVE: FMB---r")
	time.Sleep(10 * time.Second)

	// ШАГ 2: Перевод диспенсера в Режим 1 (строго после вызова макета, как в ручном тесте)
	if err := d.writeRawCmdNoLock("FCDC--r1-------", "FCDC 1"); err != nil {
		return fmt.Errorf("сбой установки FCDC 1: %w", err)
	}
	slog.Info("VALENTIN-NATIVE: FCDC--r1")
	time.Sleep(10 * time.Second)

	// ШАГ 2: Дата производства строго в поле 18
	if dateProd != "" {
		if err := d.writeRawCmdNoLock(fmt.Sprintf("BM[18]%s", dateProd), "BM18"); err != nil {
			return fmt.Errorf("сбой BM[18]: %w", err)
		}
		slog.Info("VALENTIN-NATIVE: 18")
		time.Sleep(10 * time.Second)
	}

	// ШАГ 3: Дата годности строго в поле 19
	if dateExp != "" {
		if err := d.writeRawCmdNoLock(fmt.Sprintf("BM[19]%s", dateExp), "BM19"); err != nil {
			return fmt.Errorf("сбой BM[19]: %w", err)
		}
		slog.Info("VALENTIN-NATIVE: 19")
		time.Sleep(10 * time.Second)
	}

	// ШАГ 4: Текст партии строго в поле 21
	if text01 != "" {
		if err := d.writeRawCmdNoLock(fmt.Sprintf("BM[21]%s", text01), "BM21"); err != nil {
			return fmt.Errorf("сбой BM[21]: %w", err)
		}
		slog.Info("VALENTIN-NATIVE: 21")
		time.Sleep(10 * time.Second)
	}

	d.totalSent = 0
	d.inFlight = 0
	return nil
}

// PrintBatchIndexed отправляет кадры под нож
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
		if idx := strings.Index(cleanCode, "|"); idx != -1 {
			cleanCode = cleanCode[:idx]
		}
		cleanCode = strings.ReplaceAll(cleanCode, "<GS>", "\x1d")
		if !strings.HasPrefix(cleanCode, "~1") {
			cleanCode = "~1" + cleanCode
		}
		cleanCode = strings.TrimSpace(cleanCode)

		// 1. Строго поле BM[20]
		if err := d.writeRawCmdNoLock(fmt.Sprintf("BM[20]%s", cleanCode), "BM20 DataMatrix"); err != nil {
			return sent, fmt.Errorf("сбой отправки BM[20]: %w", err)
		}
		time.Sleep(5 * time.Second) // Пауза на растеризацию 2D матрицы

		// 2. Тираж 1 копия
		if err := d.writeRawCmdNoLock("FBBA--r00001---", "FBBA 1"); err != nil {
			return sent, fmt.Errorf("сбой отправки FBBA: %w", err)
		}
		time.Sleep(5 * time.Millisecond)

		// 3. Взвод печати под нож
		if err := d.writeRawCmdNoLock("FBC---r--------", "FBC Arm"); err != nil {
			return sent, fmt.Errorf("сбой отправки FBC: %w", err)
		}

		d.totalSent++
		d.inFlight++
		sent++
	}

	return sent, nil
}

// GetBufferFreeSpace управляет скоростью: не дает лить бесконечно вперед
func (d *NativeDriver) GetBufferFreeSpace() (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	// Опрашиваем статус готовности ножа/диспенсера
	d.checkDispenserReadyNoLock()

	// Если в тракте уже висит максимальное число этикеток — тормозим ядро
	if d.inFlight >= d.maxInFlight {
		return 0, nil
	}

	return 1, nil
}

// checkDispenserReadyNoLock проверяет освобождение диспенсера
func (d *NativeDriver) checkDispenserReadyNoLock() {
	if d.conn == nil || d.inFlight == 0 {
		return
	}

	// Запрашиваем краткий статус готовности диспенсера STA
	_ = d.conn.SetDeadline(time.Now().Add(25 * time.Millisecond))
	query := fmt.Sprintf("%cSTA---w        %c", SOH, ETB)
	if _, err := d.conn.Write([]byte(query)); err != nil {
		return
	}

	buf := make([]byte, 64)
	n, err := d.conn.Read(buf)
	if err != nil || n == 0 {
		return
	}

	resp := string(buf[:n])
	// Если принтер сообщает о готовности принять следующее задание (нет флага Label Waiting):
	// декрементируем inFlight
	if !strings.Contains(resp, "W") && d.inFlight > 0 {
		d.inFlight--
	}
}

// GetCurrentPrintCount отдает подтвержденное количество
func (d *NativeDriver) GetCurrentPrintCount() (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	// Считаем напечатанными те, что уже сошли из inFlight
	confirmed := d.totalSent - d.inFlight
	if confirmed < 0 {
		confirmed = 0
	}
	return strconv.Itoa(confirmed), nil
}

func (d *NativeDriver) GetLastPrintedIndex() (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	confirmed := d.totalSent - d.inFlight
	if confirmed < 0 {
		confirmed = 0
	}
	return confirmed, nil
}

func (d *NativeDriver) GetTotalPrints() (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	confirmed := int64(d.totalSent - d.inFlight)
	if confirmed < 0 {
		confirmed = 0
	}
	return confirmed, nil
}

func (d *NativeDriver) writeRawCmdNoLock(cmd string, desc ...string) error {
	frame := []byte(fmt.Sprintf("%c%s%c", SOH, cmd, ETB))
	_ = d.conn.SetWriteDeadline(time.Now().Add(d.Timeout))

	tag := "CMD"
	if len(desc) > 0 && desc[0] != "" {
		tag = desc[0]
	}
	slog.Debug("VALENTIN-RAW-OUT", "desc", tag, "payload", cmd)

	if _, err := d.conn.Write(frame); err != nil {
		d.closeConnNoLock()
		return fmt.Errorf("сбой отправки [%s]: %w", tag, err)
	}

	time.Sleep(15 * time.Millisecond)
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

	if d.conn == nil {
		if err := d.reconnectNoLock(); err != nil {
			return err
		}
	}

	_ = d.writeRawCmdNoLock("FD----r0-------", "Stop")
	_ = d.writeRawCmdNoLock("FBA---r--------", "Abort Job")
	_ = d.writeRawCmdNoLock("FCA---r--------", "Clear Spooler")
	_ = d.writeRawCmdNoLock("FGA---r--------", "Clear Layout")
	_ = d.writeRawCmdNoLock("FCDC--r0-------", "Mode 0")

	d.totalSent = 0
	d.inFlight = 0
	return nil
}

// Заглушки core.Printer
func (d *NativeDriver) GetStatus() (string, error)                        { return "ГОТОВ", nil }
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

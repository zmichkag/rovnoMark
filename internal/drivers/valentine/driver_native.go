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
}

func NewNativeDriver(ip string, port int) *NativeDriver {
	if port <= 0 {
		port = 9100
	}
	return &NativeDriver{
		Address:   ip,
		Port:      port,
		Timeout:   3 * time.Second,
		conn:      nil,
		totalSent: 0,
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

	// 1. Отключаем асинхронный спам TD"
	_ = d.writeRawCmdNoLock("CTE---r0-------")
	time.Sleep(20 * time.Millisecond)

	// 2. Включаем Режим 1: внешний статический диспенсер
	_ = d.writeRawCmdNoLock("FCDC--r1-------")
	time.Sleep(20 * time.Millisecond)

	d.totalSent = 0

	slog.Info("VALENTIN-NATIVE: Сессия инициализирована (FCDC 1, CTE 0, Push-Accounting)", "addr", d.Address)
	return nil
}

// SelectTemplate загружает макет и строго фильтрует статические поля (18, 19, 21)
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
	if err := d.writeRawCmdNoLock(cmdFMB); err != nil {
		return fmt.Errorf("сбой FMB: %w", err)
	}
	time.Sleep(35 * time.Millisecond)

	// ШАГ 2: Дата производства строго в поле 18
	if dateProd != "" {
		if err := d.writeRawCmdNoLock(fmt.Sprintf("BM[18]%s", dateProd)); err != nil {
			return fmt.Errorf("сбой BM[18]: %w", err)
		}
		time.Sleep(15 * time.Millisecond)
	}

	// ШАГ 3: Дата годности строго в поле 19
	if dateExp != "" {
		if err := d.writeRawCmdNoLock(fmt.Sprintf("BM[19]%s", dateExp)); err != nil {
			return fmt.Errorf("сбой BM[19]: %w", err)
		}
		time.Sleep(15 * time.Millisecond)
	}

	// ШАГ 4: Текст партии строго в поле 21
	if text01 != "" {
		if err := d.writeRawCmdNoLock(fmt.Sprintf("BM[21]%s", text01)); err != nil {
			return fmt.Errorf("сбой BM[21]: %w", err)
		}
		time.Sleep(15 * time.Millisecond)
	}

	// ШАГ 5: Взвод триггера FBC
	if err := d.writeRawCmdNoLock("FBC---r--------"); err != nil {
		return fmt.Errorf("сбой FBC: %w", err)
	}

	d.totalSent = 0
	return nil
}

// PrintBatchIndexed отправляет кадры под нож и сразу инкрементирует счетчик
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
		cmdBM20 := fmt.Sprintf("BM[20]%s", cleanCode)
		if err := d.writeRawCmdNoLock(cmdBM20); err != nil {
			return sent, fmt.Errorf("сбой отправки BM[20]: %w", err)
		}
		time.Sleep(15 * time.Millisecond) // Пауза на растеризацию матрицы в ОЗУ

		// 2. Тираж 1 копия
		if err := d.writeRawCmdNoLock("FBBA--r00001---"); err != nil {
			return sent, fmt.Errorf("сбой отправки FBBA: %w", err)
		}
		time.Sleep(5 * time.Millisecond)

		// 3. Взвод печати под нож
		if err := d.writeRawCmdNoLock("FBC---r--------"); err != nil {
			return sent, fmt.Errorf("сбой отправки FBC: %w", err)
		}
		time.Sleep(10 * time.Millisecond)

		// Инкрементируем счетчик отправленных
		d.totalSent++
		sent++
	}

	slog.Info("VALENTIN-NATIVE: Код принят принтером",
		"batch_size", sent,
		"total_sent", d.totalSent,
	)

	return sent, nil
}

// GetCurrentPrintCount отдает монотонно растущий totalSent строкой (для manager.go)
func (d *NativeDriver) GetCurrentPrintCount() (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return strconv.Itoa(d.totalSent), nil
}

// GetLastPrintedIndex отдает totalSent числом (для контракта core.Printer)
func (d *NativeDriver) GetLastPrintedIndex() (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.totalSent, nil
}

// GetTotalPrints контракт core.Printer
func (d *NativeDriver) GetTotalPrints() (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return int64(d.totalSent), nil
}

// GetBufferFreeSpace отдаем 1: пампер ядра берет по 1 коду в такт и не захлебывается
func (d *NativeDriver) GetBufferFreeSpace() (int, error) {
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

// ClearQueue производит безусловный сброс всей очереди заданий в RAM принтера
// и выводит контроллер из заблокированного состояния ожидания триггеров
func (d *NativeDriver) ClearQueue() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.conn == nil {
		if err := d.reconnectNoLock(); err != nil {
			return fmt.Errorf("clear queue: ошибка сокета: %w", err)
		}
	}

	// 1. Аппаратный Стоп / Пауза (останавливает двигатель и блокирует новые такты)
	if err := d.writeRawCmdNoLock("FD----r0-------"); err != nil {
		return fmt.Errorf("сбой отправки FD (Stop): %w", err)
	}
	time.Sleep(15 * time.Millisecond)

	// 2. Сброс активного тиража FBBA (обнуляет регистр R)
	if err := d.writeRawCmdNoLock("FBA---r--------"); err != nil {
		return fmt.Errorf("сбой отправки FBA (Abort Job): %w", err)
	}
	time.Sleep(15 * time.Millisecond)

	// 3. Очистка спулера/очереди печати в памяти принтера (снимает все взведенные FBC)
	if err := d.writeRawCmdNoLock("FCA---r--------"); err != nil {
		return fmt.Errorf("сбой отправки FCA (Clear Spooler): %w", err)
	}
	time.Sleep(15 * time.Millisecond)

	// 4. Очистка графического буфера макета и переменных BM
	if err := d.writeRawCmdNoLock("FGA---r--------"); err != nil {
		return fmt.Errorf("сбой отправки FGA (Clear Layout): %w", err)
	}
	time.Sleep(15 * time.Millisecond)

	// 5. Сброс режима диспенсера в нейтраль (Mode 0), чтобы снять зажатый триггер
	_ = d.writeRawCmdNoLock("FCDC--r0-------")
	time.Sleep(15 * time.Millisecond)

	// Сброс локальных счетчиков драйвера
	d.totalSent = 0

	slog.Info("VALENTIN-NATIVE: Аппаратный спулер и очередь печати полностью очищены",
		"printer_addr", d.Address,
	)

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

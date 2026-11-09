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

type NativeDriver struct {
	ID          int           // ID принтера в системе
	Address     string        // IP-адрес
	Port        int           // Порт CVPL (9100)
	Timeout     time.Duration // Сетевой таймаут
	conn        net.Conn      // Активный сокет
	mu          sync.Mutex    // Мьютекс сокета и очередей
	curTemplate string        // Текущий загруженный макет

	// Параметры алгоритма скользящего буфера
	BatchSizeS int // Размер заливки S (порция кодов)
	ThresholdN int // Нижний порог N (когда в буфере <= N, доливаем еще S)

	// Метрики конвейера
	totalSent    int // Всего кодов отправлено в принтер
	totalPrinted int // Всего этикеток сошло физически
	lastRawFBBC  int // Последнее сырое значение регистра FBBC

	cmdDelay time.Duration // Задержка между одиночными командами
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
		BatchSizeS:   10, // S: доливаем по 10 штук
		ThresholdN:   3,  // N: порог доливки
		totalSent:    0,
		totalPrinted: 0,
		lastRawFBBC:  0,
	}
}

// InitSession поднимает сокет, глушит спам терминала и активирует Режим 1
func (d *NativeDriver) InitSession(fieldName string, maxQueue int, staticFields map[string]string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if err := d.ensureConnectionNoLock(); err != nil {
		return err
	}

	// 1. Отключаем паразитный спам дисплея (Terminal Emulation)
	if err := d.sendSingleCmdNoLock("CTE---r0-------", "Disable TD Spam"); err != nil {
		return err
	}

	// 2. Включаем Режим 1: внешний статический диспенсер
	if err := d.sendSingleCmdNoLock("FCDC--r1-------", "Set Dispenser Mode 1"); err != nil {
		return err
	}

	// Сброс счетчиков сессии
	d.totalSent = 0
	d.totalPrinted = 0
	d.lastRawFBBC = 0

	slog.Info("VALENTIN-NATIVE: Инициализация выполнена (FCDC 1, CTE 0)", "addr", d.Address)
	return nil
}

// SelectTemplate загружает макет с карты CF и записывает даты
func (d *NativeDriver) SelectTemplate(template string, staticFields map[string]string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if template != "" {
		d.curTemplate = template
	} else if d.curTemplate == "" {
		return fmt.Errorf("ошибка: не указан макет PLU")
	}

	if err := d.ensureConnectionNoLock(); err != nil {
		return err
	}

	// Загружаем макет в RAM
	if err := d.sendSingleCmdNoLock(fmt.Sprintf("FMB---r%s", d.curTemplate), "Select Template FMB"); err != nil {
		return err
	}
	time.Sleep(40 * time.Millisecond)

	// Запись статических дат (BM18, BM19, BM21) при наличии
	if val, ok := staticFields["date01"]; ok && val != "" {
		_ = d.sendSingleCmdNoLock(fmt.Sprintf("BM[18]%s", val), "DateProd")
	}
	if val, ok := staticFields["date02"]; ok && val != "" {
		_ = d.sendSingleCmdNoLock(fmt.Sprintf("BM[19]%s", val), "DateExp")
	}
	if val, ok := staticFields["text01"]; ok && val != "" {
		_ = d.sendSingleCmdNoLock(fmt.Sprintf("BM[21]%s", val), "Batch/Shift")
	}

	return nil
}

// PrintBatchIndexed осуществляет наливку порции кодов S в принтер поштучно
func (d *NativeDriver) PrintBatchIndexed(fieldName string, startIndex int, codes []string) (int, error) {
	if len(codes) == 0 {
		return 0, nil
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	if err := d.ensureConnectionNoLock(); err != nil {
		return 0, err
	}

	targetField := "20"
	if fieldName != "" {
		targetField = fieldName
	}

	sentInBatch := 0
	for _, rawCode := range codes {
		cleanCode := d.formatDM(rawCode)

		// 1. Данные кода маркировки
		if err := d.sendSingleCmdNoLock(fmt.Sprintf("BM[%s]%s", targetField, cleanCode), "Set DM"); err != nil {
			return sentInBatch, err
		}

		// 2. Тираж 1 оттиск
		if err := d.sendSingleCmdNoLock("FBBA--r00001---", "Set Batch 1"); err != nil {
			return sentInBatch, err
		}

		// 3. Старт/взвод в очередь диспенсера
		if err := d.sendSingleCmdNoLock("FBC---r--------", "Arm Print FBC"); err != nil {
			return sentInBatch, err
		}

		d.totalSent++
		sentInBatch++
	}

	slog.Info("VALENTIN-NATIVE: Порция кодов успешно залита в буфер",
		"sent_batch", sentInBatch,
		"total_sent", d.totalSent,
		"in_flight", d.totalSent-d.totalPrinted,
	)
	return sentInBatch, nil
}

// PollBufferStatus опрашивает одометр FBBC, вычисляет In-Flight остаток и сигнализирует о необходимости доливки
func (d *NativeDriver) PollBufferStatus() (inFlight int, needRefill bool, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if err := d.ensureConnectionNoLock(); err != nil {
		return 0, false, err
	}

	// Читаем физический регистр одометра FBBC--w
	rawFBBC, err := d.readRawFBBCNoLock()
	if err != nil {
		return d.totalSent - d.totalPrinted, false, err
	}

	// Обработка дельты одометра с защитой от сброса при переподаче
	if rawFBBC > d.lastRawFBBC {
		delta := rawFBBC - d.lastRawFBBC
		d.totalPrinted += delta
		d.lastRawFBBC = rawFBBC
	} else if rawFBBC < d.lastRawFBBC {
		// Одометр занулился контроллером при новом кадре: инкрементируем по факту схода
		if rawFBBC > 0 {
			d.totalPrinted += rawFBBC
		}
		d.lastRawFBBC = rawFBBC
	}

	inFlight = d.totalSent - d.totalPrinted
	if inFlight < 0 {
		inFlight = 0
	}

	// Проверяем условие нижнего порога: если в тракте осталось <= N, пора заливать порцию S
	needRefill = inFlight <= d.ThresholdN

	return inFlight, needRefill, nil
}

// TriggerManualFeed программно эмулирует датчик схода этикетки (FCMDF 1)
func (d *NativeDriver) TriggerManualFeed() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.sendSingleCmdNoLock("FCMDF-r1-------", "Software Trigger FCMDF")
}

// readRawFBBCNoLock вычитывает одометр с фильтрацией склеенных кадров TD"
func (d *NativeDriver) readRawFBBCNoLock() (int, error) {
	// Сброс старых байт
	_ = d.conn.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
	drainBuf := make([]byte, 512)
	for {
		if n, _ := d.conn.Read(drainBuf); n == 0 {
			break
		}
	}

	_ = d.conn.SetDeadline(time.Now().Add(d.Timeout))
	query := fmt.Sprintf("%cFBBC--w        %c", SOH, ETB)
	if _, err := d.conn.Write([]byte(query)); err != nil {
		d.closeConnNoLock()
		return 0, err
	}

	buf := make([]byte, 256)
	start := time.Now()
	for time.Since(start) < 200*time.Millisecond {
		n, err := d.conn.Read(buf)
		if err != nil {
			break
		}
		resp := string(buf[:n])
		if strings.Contains(resp, "TD\"") && !strings.Contains(resp, "A") {
			continue
		}
		if match := fbbcRegex.FindStringSubmatch(resp); len(match) > 1 {
			val, _ := strconv.Atoi(match[1])
			return val, nil
		}
	}

	return d.lastRawFBBC, nil
}

func (d *NativeDriver) sendSingleCmdNoLock(payload, desc string) error {
	frame := []byte(fmt.Sprintf("%c%s%c", SOH, payload, ETB))
	_ = d.conn.SetWriteDeadline(time.Now().Add(d.Timeout))
	if _, err := d.conn.Write(frame); err != nil {
		d.closeConnNoLock()
		return fmt.Errorf("ошибка отправки [%s]: %w", desc, err)
	}
	time.Sleep(d.cmdDelay)
	return nil
}

func (d *NativeDriver) formatDM(raw string) string {
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

func (d *NativeDriver) ensureConnectionNoLock() error {
	if d.conn != nil {
		return nil
	}
	addr := net.JoinHostPort(d.Address, strconv.Itoa(d.Port))
	conn, err := net.DialTimeout("tcp", addr, d.Timeout)
	if err != nil {
		return fmt.Errorf("подключение к %s не удалось: %w", addr, err)
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

// ClearQueue аппаратный сброс буфера и возврат принтера в стоп
func (d *NativeDriver) ClearQueue() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_ = d.sendSingleCmdNoLock("FD----r0-------", "Stop")
	_ = d.sendSingleCmdNoLock("FGA---r--------", "Clear Buffer FGA")
	d.totalSent = 0
	d.totalPrinted = 0
	d.lastRawFBBC = 0
	return nil
}

// Контракт core.Printer
func (d *NativeDriver) GetStatus() (string, error)                        { return "ГОТОВ", nil }
func (d *NativeDriver) GetBufferFreeSpace() (int, error)                  { return d.BatchSizeS, nil }
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
func (d *NativeDriver) GetCurrentPrintCount() (string, error) {
	return strconv.Itoa(d.totalPrinted), nil
}

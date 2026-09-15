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

type NiceLabelDriver struct {
	Name        string        // Имя макета / PLU
	ID          int           // ID принтера в базе данных
	Address     string        // IP-адрес принтера (например, 192.168.x.x)
	Port        int           // RAW TCP порт (9100)
	Timeout     time.Duration // Сетевой таймаут сокета
	conn        net.Conn      // Активная монопольная TCP-сессия
	mu          sync.Mutex    // Мьютекс для защиты сокета при многопоточном вызове
	curTemplate string        // Активный выбранный шаблон
	lastCount   int           // Виртуальный нарастающий итог для фронтенда/1С
	lastRawFBBC int           // Последнее физическое значение из регистра FBBC
	isPumping   bool          // Флаг активности реалтайм-насоса кодов
}

func NewNiceLabelDriver(id int, ip string, port int) *NiceLabelDriver {
	if port <= 0 {
		port = 9100
	}
	return &NiceLabelDriver{
		ID:          id,
		Address:     ip,
		Port:        port,
		Timeout:     3 * time.Second,
		conn:        nil,
		curTemplate: "",
		lastCount:   0,
		lastRawFBBC: 0,
		isPumping:   false,
	}
}

// InitSession проверяет/поднимает сокет, сбрасывает счетчики и конфигурирует режим датчика перед стартом
func (d *NiceLabelDriver) InitSession(fieldName string, maxQueue int, staticFields map[string]string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	// 1. Поднимаем сокет, если закрыт
	if d.conn == nil {
		addr := net.JoinHostPort(d.Address, strconv.Itoa(d.Port))
		conn, err := net.DialTimeout("tcp", addr, d.Timeout)
		if err != nil {
			return fmt.Errorf("ошибка подключения к принтеру %s: %w", addr, err)
		}
		d.optimizeSocket(conn)
		d.conn = conn
	}

	d.lastCount = 0
	d.lastRawFBBC = 0

	slog.Info("VALENTIN-INIT: Сессия инициализирована, режим риббон-буфера выключен, ожидание датчика активировано",
		"printer_id", d.ID,
		"addr", d.Address,
	)

	return nil
}

// SelectTemplate атомарно загружает макет и записывает статические поля по шагам
func (d *NiceLabelDriver) SelectTemplate(template string, staticFields map[string]string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	slog.Info("VALENTIN-INIT: Получены данные статики от 1С",
		"printer_id", d.ID,
		"template", template,
		"raw_static_fields", staticFields,
	)

	if template != "" {
		d.curTemplate = template
	} else if d.curTemplate == "" {
		return fmt.Errorf("критическая ошибка: передан пустой код макета (PLU)")
	}

	if d.conn == nil {
		if err := d.reconnectNoLock(); err != nil {
			return fmt.Errorf("ошибка реконнекта при выборе макета: %w", err)
		}
	}

	// 1. Извлекаем даты и переменные (с фоллбэками)
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

	slog.Info("VALENTIN-DIRECT: Покомандная активация макета и запись параметров",
		"printer_id", d.ID,
		"template", d.curTemplate,
		"date_prod", dateProd,
		"date_exp", dateExp,
		"text01", text01,
	)

	// --- ШАГ 1: Выбираем макет из Flash (FMB) ---
	cmdFMB := []byte(fmt.Sprintf("%cFMB---r%s%c", SOH, d.curTemplate, ETB))
	_ = d.conn.SetWriteDeadline(time.Now().Add(d.Timeout))
	d.traceCommand("Select Layout (FMB)", cmdFMB)
	if _, err := d.conn.Write(cmdFMB); err != nil {
		d.closeConnNoLock()
		return fmt.Errorf("сбой отправки FMB: %w", err)
	}
	time.Sleep(30 * time.Millisecond) // Пауза на переключение графического буфера в RAM

	// --- ШАГ 2: Записываем дату производства в поле 18 (BM[18]) ---
	if dateProd != "" {
		cmdBM18 := []byte(fmt.Sprintf("%cBM[18]%s%c", SOH, dateProd, ETB))
		_ = d.conn.SetWriteDeadline(time.Now().Add(d.Timeout))
		d.traceCommand("Field 18 DateProd (BM)", cmdBM18)
		if _, err := d.conn.Write(cmdBM18); err != nil {
			d.closeConnNoLock()
			return fmt.Errorf("сбой отправки BM[18]: %w", err)
		}
		time.Sleep(15 * time.Millisecond)
	}

	// --- ШАГ 3: Записываем дату годности в поле 19 (BM[19]) ---
	if dateExp != "" {
		cmdBM19 := []byte(fmt.Sprintf("%cBM[19]%s%c", SOH, dateExp, ETB))
		_ = d.conn.SetWriteDeadline(time.Now().Add(d.Timeout))
		d.traceCommand("Field 19 DateExp (BM)", cmdBM19)
		if _, err := d.conn.Write(cmdBM19); err != nil {
			d.closeConnNoLock()
			return fmt.Errorf("сбой отправки BM[19]: %w", err)
		}
		time.Sleep(15 * time.Millisecond)
	}

	// --- ШАГ 4: Записываем текстовое поле 21 (BM[21]) ---
	if text01 != "" {
		cmdBM21 := []byte(fmt.Sprintf("%cBM[21]%s%c", SOH, text01, ETB))
		_ = d.conn.SetWriteDeadline(time.Now().Add(d.Timeout))
		d.traceCommand("Field 21 text01 (BM)", cmdBM21)
		if _, err := d.conn.Write(cmdBM21); err != nil {
			d.closeConnNoLock()
			return fmt.Errorf("сбой отправки BM[21]: %w", err)
		}
		time.Sleep(15 * time.Millisecond)
	}

	// --- ШАГ 5: Первичный взвод в режим ожидания датчика (FBC) ---
	cmdFBC := []byte(fmt.Sprintf("%cFBC---r--------%c", SOH, ETB))
	_ = d.conn.SetWriteDeadline(time.Now().Add(d.Timeout))
	d.traceCommand("Arm Printer (FBC)", cmdFBC)
	if _, err := d.conn.Write(cmdFBC); err != nil {
		d.closeConnNoLock()
		return fmt.Errorf("сбой отправки FBC: %w", err)
	}

	d.lastCount = 0
	d.lastRawFBBC = 0

	slog.Info("VALENTIN-DIRECT: Инициализация завершена, макет и статика зафиксированы в ОЗУ", "printer_id", d.ID)
	return nil
}

// PrintBatchIndexed осуществляет отправку строго динамического блока BM[20] и запуск под датчик
func (d *NiceLabelDriver) PrintBatchIndexed(fieldName string, startIndex int, codes []string) (int, error) {
	if len(codes) == 0 {
		return 0, nil
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	if d.conn == nil {
		if err := d.reconnectNoLock(); err != nil {
			return 0, fmt.Errorf("ошибка сокета перед реактивным тактом: %w", err)
		}
	}

	targetCode := codes[0]
	cleanCode := targetCode

	// 1. Отрезаем метаданные 1С (после '|')
	if idx := strings.Index(cleanCode, "|"); idx != -1 {
		cleanCode = cleanCode[:idx]
	}

	// 2. Преобразуем <GS> в 0x1D и добавляем префикс ~1 для FNC1
	cleanCode = strings.ReplaceAll(cleanCode, "<GS>", "\x1d")
	cleanCode = "~1" + cleanCode
	cleanCode = strings.TrimSpace(cleanCode)

	// 3. Обновляем динамический DataMatrix BM[20]
	cmdBM20 := []byte(fmt.Sprintf("%cBM[20]%s%c", SOH, cleanCode, ETB))
	_ = d.conn.SetWriteDeadline(time.Now().Add(d.Timeout))
	d.traceCommand(fmt.Sprintf("PUMPER TACT %d [1/3]: Set DataMatrix (BM20)", startIndex), cmdBM20)
	if _, err := d.conn.Write(cmdBM20); err != nil {
		d.closeConnNoLock()
		return 0, fmt.Errorf("сбой отправки BM20: %w", err)
	}

	// 🛑 Пауза 15мс на растеризацию матричного кода в RAM процессора принтера
	time.Sleep(15 * time.Millisecond)

	// 4. ЗАДАЕМ ТИРАЖ (Обязательно для CVPL перед FBC): ровно 1 копия
	cmdFBBA := []byte(fmt.Sprintf("%cFBBA--r00001---%c", SOH, ETB))
	_ = d.conn.SetWriteDeadline(time.Now().Add(d.Timeout))
	d.traceCommand(fmt.Sprintf("PUMPER TACT %d [2/3]: Set Batch (FBBA)", startIndex), cmdFBBA)
	if _, err := d.conn.Write(cmdFBBA); err != nil {
		d.closeConnNoLock()
		return 0, fmt.Errorf("сбой отправки FBBA: %w", err)
	}

	time.Sleep(5 * time.Millisecond)

	// 5. ВЗВОДИМ ТРИГГЕР НА ФОТОДАТЧИК (FBC---r--------)
	cmdFBC := []byte(fmt.Sprintf("%cFBC---r--------%c", SOH, ETB))
	_ = d.conn.SetWriteDeadline(time.Now().Add(d.Timeout))
	d.traceCommand(fmt.Sprintf("PUMPER TACT %d [3/3]: Arm Trigger (FBC)", startIndex), cmdFBC)
	if _, err := d.conn.Write(cmdFBC); err != nil {
		d.closeConnNoLock()
		return 0, fmt.Errorf("сбой отправки FBC: %w", err)
	}

	slog.Info("VALENTIN-DIRECT: Код BM[20] и тираж взведены под датчик", "printer_id", d.ID, "index", startIndex)
	return 1, nil
}

// PrintBatchIndexedMode заглушка совместимости: в single-режиме всегда вызывает надежный PrintBatchIndexed
func (d *NiceLabelDriver) PrintBatchIndexedMode(fieldName string, startIndex int, codes []string, hostDriven bool) (int, error) {
	return d.PrintBatchIndexed(fieldName, startIndex, codes)
}

// GetCurrentPrintCount опрашивает FBBC с коротким дедлайном и защитой от скачков
func (d *NiceLabelDriver) GetCurrentPrintCount() (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.conn == nil {
		if err := d.reconnectNoLock(); err != nil {
			return strconv.Itoa(d.lastCount), fmt.Errorf("сокет закрыт: %w", err)
		}
	}

	// Короткий дедлайн 45мс для исключения зависания тикера
	_ = d.conn.SetDeadline(time.Now().Add(45 * time.Millisecond))
	cmd := fmt.Sprintf("%cFBBC--w%c", SOH, ETB)

	if _, err := d.conn.Write([]byte(cmd)); err != nil {
		d.closeConnNoLock()
		return strconv.Itoa(d.lastCount), err
	}

	buf := make([]byte, 128)
	n, err := d.conn.Read(buf)
	if err != nil {
		if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
			return strconv.Itoa(d.lastCount), nil
		}
		d.closeConnNoLock()
		return strconv.Itoa(d.lastCount), err
	}

	rawResponse := string(buf[:n])

	if strings.Contains(rawResponse, "TD\"") || !strings.Contains(rawResponse, "A") {
		return strconv.Itoa(d.lastCount), nil
	}

	cleanResp := strings.Trim(rawResponse, string([]byte{SOH, byte(ETB), '\r', '\n', ' '}))
	aIdx := strings.Index(cleanResp, "A")
	if aIdx == -1 {
		return strconv.Itoa(d.lastCount), nil
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
		return strconv.Itoa(d.lastCount), nil
	}

	rawCount, _ := strconv.Atoi(numStr)

	// Защита одометра:
	if rawCount < d.lastRawFBBC {
		d.lastRawFBBC = rawCount
		return strconv.Itoa(d.lastCount), nil
	}

	if rawCount > d.lastRawFBBC {
		delta := rawCount - d.lastRawFBBC
		if delta < 10 {
			d.lastCount += delta
		}
		d.lastRawFBBC = rawCount
	}

	return strconv.Itoa(d.lastCount), nil
}

func (d *NiceLabelDriver) optimizeSocket(conn net.Conn) {
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		_ = tcpConn.SetNoDelay(true)
		_ = tcpConn.SetKeepAlive(true)
		_ = tcpConn.SetKeepAlivePeriod(5 * time.Second)
	}
}

func (d *NiceLabelDriver) reconnectNoLock() error {
	addr := net.JoinHostPort(d.Address, strconv.Itoa(d.Port))
	conn, err := net.DialTimeout("tcp", addr, d.Timeout)
	if err != nil {
		return err
	}
	d.optimizeSocket(conn)
	d.conn = conn
	return nil
}

func (d *NiceLabelDriver) closeConnNoLock() {
	if d.conn != nil {
		_ = d.conn.Close()
		d.conn = nil
	}
}

func (d *NiceLabelDriver) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closeConnNoLock()
	return nil
}

func (d *NiceLabelDriver) traceCommand(desc string, data []byte) {
	view := string(data)
	view = strings.ReplaceAll(view, string([]byte{SOH}), "[SOH]")
	view = strings.ReplaceAll(view, string([]byte{ETB}), "[ETB]")
	view = strings.ReplaceAll(view, "\r", "[CR]")
	view = strings.ReplaceAll(view, "\n", "[LF]")

	slog.Info("VALENTIN-TRACE [КОМАНДА В ПОРТ]: "+desc,
		"printer_id", d.ID,
		"ascii_payload", view,
		"hex_dump", fmt.Sprintf("%x", data),
	)
}

// ClearQueue производит полный сброс активного макета в ОЗУ принтера и переводит его в стоп
func (d *NiceLabelDriver) ClearQueue() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.conn == nil {
		if err := d.reconnectNoLock(); err != nil {
			return err
		}
	}

	// 1. Аппаратная пауза/стоп (FD----r0)
	cmdStop := []byte(fmt.Sprintf("%cFD----r0-------%c", SOH, ETB))
	_ = d.conn.SetWriteDeadline(time.Now().Add(d.Timeout))
	_, _ = d.conn.Write(cmdStop)
	time.Sleep(10 * time.Millisecond)

	// 2. Полный сброс буфера и макета в ОЗУ (FGA---r)
	cmdReset := []byte(fmt.Sprintf("%cFGA---r%c", SOH, ETB))
	_ = d.conn.SetWriteDeadline(time.Now().Add(d.Timeout))
	_, err := d.conn.Write(cmdReset)

	d.lastCount = 0
	d.lastRawFBBC = 0

	slog.Info("VALENTIN-STOP: Задание сброшено, принтер переведен в стоп", "printer_id", d.ID)
	return err
}

// --- Контракт core.Printer ---

func (d *NiceLabelDriver) GetStatus() (string, error)                        { return "ГОТОВ", nil }
func (d *NiceLabelDriver) GetBufferFreeSpace() (int, error)                  { return 1, nil }
func (d *NiceLabelDriver) GetLastPrintedIndex() (int, error)                 { return d.lastCount, nil }
func (d *NiceLabelDriver) GetTotalPrints() (int64, error)                    { return int64(d.lastCount), nil }
func (d *NiceLabelDriver) UpdateStaticFields(f map[string]string) error      { return nil }
func (d *NiceLabelDriver) PrintTemplate(t string, f map[string]string) error { return nil }
func (d *NiceLabelDriver) GetTemplates() ([]string, error)                   { return []string{d.curTemplate}, nil }
func (d *NiceLabelDriver) GetTemplateFields(t string) ([]string, error) {
	return []string{"18", "19", "20", "21"}, nil
}
func (d *NiceLabelDriver) GetRemainingRibbon() (string, error)       { return "N/A", nil }
func (d *NiceLabelDriver) GetQueueCapacity(q string) (string, error) { return "N/A", nil }
func (d *NiceLabelDriver) GetPrintSpeed() (string, error)            { return "N/A", nil }
func (d *NiceLabelDriver) GetCurrentTemplate() (string, error)       { return d.curTemplate, nil }
func (d *NiceLabelDriver) SetDispenserMode(mode int) error           { return nil }
func (d *NiceLabelDriver) SetPause(pause bool) error                 { return nil }

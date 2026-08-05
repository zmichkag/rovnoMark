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

// InitSession проверяет/поднимает монопольный сокет и запускает первичную подготовку макета
func (d *NiceLabelDriver) InitSession(fieldName string, maxQueue int, staticFields map[string]string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	// Только поднимаем сокет, если закрыт. Повторно SelectTemplate НЕ ВЫЗЫВАЕМ!
	if d.conn == nil {
		addr := net.JoinHostPort(d.Address, strconv.Itoa(d.Port))
		conn, err := net.DialTimeout("tcp", addr, d.Timeout)
		if err != nil {
			return fmt.Errorf("ошибка подключения к принтеру %s: %w", addr, err)
		}
		d.optimizeSocket(conn)
		d.conn = conn
	}
	return nil
}

// SelectTemplate атомарно (покомандно) загружает макет из Flash в ОЗУ и записывает поля 18 и 19
// SelectTemplate атомарно загружает макет и записывает статические поля ровно в том виде, как их отдала 1С
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

	// 1. ИЗВЛЕКАЕМ ДАТЫ СТРОГО ИЗ 1С
	dateProd, ok1 := staticFields["date01"]
	dateExp, ok2 := staticFields["date02"]
	text01, ok3 := staticFields["text01"]

	// ВАЛИДАЦИЯ: Если 1С не передала обязательные поля — жестко бракуем запуск!
	if !ok1 || strings.TrimSpace(dateProd) == "" {
		return fmt.Errorf("ошибка валидации 1С: поле дата производства 'data01' не заполнено")
	}
	if !ok2 || strings.TrimSpace(dateExp) == "" {
		return fmt.Errorf("ошибка валидации 1С: поле дата годности 'data02' не заполнено")
	}
	if !ok3 || strings.TrimSpace(text01) == "" {
		return fmt.Errorf("ошибка валидации 1С: строка смены не заполнена")
	}

	slog.Info("VALENTIN-DIRECT: Покомандная активация макета и запись точных дат из 1С",
		"printer_id", d.ID,
		"template", d.curTemplate,
		"date_prod", dateProd,
		"date_exp", dateExp,
		"text01", text01,
	)

	// --- ШАГ 1: Выбираем макет из Flash (FMB) ---
	cmdFMB := []byte(fmt.Sprintf("%cFMB---r%s%c", SOH, d.curTemplate, ETB))
	d.conn.SetWriteDeadline(time.Now().Add(d.Timeout))
	d.traceCommand("Select Layout (FMB)", cmdFMB)
	if _, err := d.conn.Write(cmdFMB); err != nil {
		d.closeConnNoLock()
		return fmt.Errorf("сбой отправки FMB: %w", err)
	}
	time.Sleep(20 * time.Millisecond) // Пауза на переключение графического буфера в RAM

	// --- ШАГ 2: Записываем точную дату производства в поле 18 (BM[18]) ---
	cmdBM18 := []byte(fmt.Sprintf("%cBM[18]%s%c", SOH, dateProd, ETB))
	d.conn.SetWriteDeadline(time.Now().Add(d.Timeout))
	d.traceCommand("Field 18 DateProd (BM)", cmdBM18)
	if _, err := d.conn.Write(cmdBM18); err != nil {
		d.closeConnNoLock()
		return fmt.Errorf("сбой отправки BM[18]: %w", err)
	}
	time.Sleep(10 * time.Millisecond)

	// --- ШАГ 3: Записываем точную дату годности в поле 19 (BM[19]) ---
	cmdBM19 := []byte(fmt.Sprintf("%cBM[19]%s%c", SOH, dateExp, ETB))
	d.conn.SetWriteDeadline(time.Now().Add(d.Timeout))
	d.traceCommand(" Field 19 DateExp (BM)", cmdBM19)
	if _, err := d.conn.Write(cmdBM19); err != nil {
		d.closeConnNoLock()
		return fmt.Errorf("сбой отправки BM[19]: %w", err)
	}
	time.Sleep(10 * time.Millisecond)

	cmdBM21 := []byte(fmt.Sprintf("%cBM[21]%s%c", SOH, text01, ETB))
	d.conn.SetWriteDeadline(time.Now().Add(d.Timeout))
	d.traceCommand("Field 21 text01 (BM)", cmdBM21)
	if _, err := d.conn.Write(cmdBM21); err != nil {
		d.closeConnNoLock()
		return fmt.Errorf(
			"сбой отправки BM[21]: %w", err)
	}
	time.Sleep(10 * time.Millisecond)

	// --- ШАГ 4: Первичный взвод в режим ожидания датчика (FBC) ---
	cmdFBC := []byte(fmt.Sprintf("%cFBC---r--------%c", SOH, ETB))
	d.conn.SetWriteDeadline(time.Now().Add(d.Timeout))
	d.traceCommand("Arm Printer (FBC)", cmdFBC)
	if _, err := d.conn.Write(cmdFBC); err != nil {
		d.closeConnNoLock()
		return fmt.Errorf("сбой отправки FBC: %w", err)
	}

	d.lastRawFBBC = 0

	slog.Info("VALENTIN-DIRECT: Инициализация завершена, оригинальные даты 1С зафиксированы в ОЗУ", "printer_id", d.ID)
	return nil
}

// PrintBatchIndexed осуществляет отправку строго динамического блока BM[20] (Честный Знак)
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

	// 1. Отрезаем хвост с техническими метаданными 1С (если есть '|')
	if idx := strings.Index(cleanCode, "|"); idx != -1 {
		cleanCode = cleanCode[:idx]
	}

	// 2. Заменяем текстовую заглушку "<GS>" бинарный байт 0x1D (ASCII 29)
	cleanCode = strings.ReplaceAll(cleanCode, "<GS>", "\x1d")

	cleanCode = strings.TrimSpace(cleanCode)

	//// Ставим паузу
	//cmdFDPause := []byte(fmt.Sprintf("%cFD----r0%c", SOH, ETB))
	//d.conn.SetWriteDeadline(time.Now().Add(d.Timeout))
	//d.traceCommand(fmt.Sprintf("PUMPER TACT %d [0/3]: Set Stop (FD)", startIndex), cmdFDPause)
	//if _, err := d.conn.Write(cmdFDPause); err != nil {
	//	d.closeConnNoLock()
	//	return 0, fmt.Errorf("сбой отправки FD: %w", err)
	//}

	// Обновляем динамический DataMatrix BM[20] ---
	cmdBM20 := []byte(fmt.Sprintf("%cBM[20]%s%c", SOH, cleanCode, ETB))
	d.conn.SetWriteDeadline(time.Now().Add(d.Timeout))
	d.traceCommand(fmt.Sprintf("PUMPER TACT %d [1/3]: Set DataMatrix (BM20)", startIndex), cmdBM20)
	if _, err := d.conn.Write(cmdBM20); err != nil {
		d.closeConnNoLock()
		return 0, fmt.Errorf("сбой отправки BM20: %w", err)
	}

	// 🛑 ВАЖНО: Физическая пауза 15мс для перерисовки графического блока в RAM!
	time.Sleep(2 * time.Millisecond)

	////Снимаем паузу
	//cmdFD := []byte(fmt.Sprintf("%cFD----r1%c", SOH, ETB))
	//d.conn.SetWriteDeadline(time.Now().Add(d.Timeout))
	//d.traceCommand(fmt.Sprintf("PUMPER TACT %d [2/3]: Set Wait (FD)", startIndex), cmdFD)
	//if _, err := d.conn.Write(cmdFD); err != nil {
	//	d.closeConnNoLock()
	//	return 0, fmt.Errorf("сбой отправки FD: %w", err)
	//}

	// 🛑 ВАЖНО: Пауза 10мс перед взводом
	//time.Sleep(10 * time.Millisecond)

	// --- ШАГ 3: Взвод триггера на фотодатчик (FBC) ---
	cmdFBC := []byte(fmt.Sprintf("%cFBC---r--------%c", SOH, ETB))
	d.conn.SetWriteDeadline(time.Now().Add(d.Timeout))
	d.traceCommand(fmt.Sprintf("PUMPER TACT %d [3/3]: Arm Trigger (FBC)", startIndex), cmdFBC)
	if _, err := d.conn.Write(cmdFBC); err != nil {
		d.closeConnNoLock()
		return 0, fmt.Errorf("сбой отправки FBC: %w", err)
	}

	slog.Info("VALENTIN-DIRECT: Код BM[20] успешно взведен на датчик", "printer_id", d.ID, "index", startIndex)
	return 1, nil
}

// GetCurrentPrintCount опрашивает FBBC с жестким фильтром мусора от тачскрина
func (d *NiceLabelDriver) GetCurrentPrintCount() (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.conn == nil {
		if err := d.reconnectNoLock(); err != nil {
			return strconv.Itoa(d.lastCount), fmt.Errorf("сокет закрыт: %w", err)
		}
	}

	d.conn.SetDeadline(time.Now().Add(d.Timeout))
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

	// 🛑 ЖЕСТКИЙ ФИЛЬТР: Игнорируем пакеты дисплея (TD) и всё, где нет маркера ответа 'A'
	if strings.Contains(rawResponse, "TD\"") || !strings.Contains(rawResponse, "A") {
		return strconv.Itoa(d.lastCount), nil
	}

	// Парсим только цифры после маркера 'A'
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

	// 🛑 ЛОГИКА ЗАЩИТЫ СЧЕТЧИКА (Блокировка полетов в космос):
	if rawCount < d.lastRawFBBC {
		// Принтер сбросил счетчик (например, из-за команды FBC).
		// Фиксируем новый ноль, виртуальный счетчик НЕ трогаем.
		d.lastRawFBBC = rawCount
		return strconv.Itoa(d.lastCount), nil
	}

	if rawCount > d.lastRawFBBC {
		delta := rawCount - d.lastRawFBBC
		// Защита от аномальных скачков (больше 10 за один такт опроса быть не может)
		if delta < 10 {
			d.lastCount += delta
		}
		d.lastRawFBBC = rawCount
	}

	return strconv.Itoa(d.lastCount), nil
}

// optimizeSocket отключает задержки алгоритма Nagle и включает сетевой KeepAlive
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
		d.conn.Close()
		d.conn = nil
	}
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

// --- ЗАГЛУШКИ СОВМЕСТИМОСТИ ИНТЕРФЕЙСА ---

func (d *NiceLabelDriver) ClearQueue() error                                 { return nil }
func (d *NiceLabelDriver) GetStatus() (string, error)                        { return "ГОТОВ", nil }
func (d *NiceLabelDriver) GetBufferFreeSpace() (int, error)                  { return 1, nil }
func (d *NiceLabelDriver) GetLastPrintedIndex() (int, error)                 { return d.lastCount, nil }
func (d *NiceLabelDriver) UpdateStaticFields(f map[string]string) error      { return nil }
func (d *NiceLabelDriver) PrintTemplate(t string, f map[string]string) error { return nil }
func (d *NiceLabelDriver) GetTemplates() ([]string, error)                   { return []string{d.curTemplate}, nil }
func (d *NiceLabelDriver) GetTemplateFields(t string) ([]string, error) {
	return []string{"18", "19", "20"}, nil
}
func (d *NiceLabelDriver) GetRemainingRibbon() (string, error) { return "N/A", nil }
func (d *NiceLabelDriver) GetQueueCapacity(q string) (string, error) {
	return "N/A", nil
}
func (d *NiceLabelDriver) GetPrintSpeed() (string, error)      { return "N/A", nil }
func (d *NiceLabelDriver) GetCurrentTemplate() (string, error) { return d.curTemplate, nil }

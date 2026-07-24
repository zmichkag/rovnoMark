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

	// 1. Проверяем или поднимаем монопольное TCP-соединение
	if d.conn == nil {
		addr := net.JoinHostPort(d.Address, strconv.Itoa(d.Port))
		conn, err := net.DialTimeout("tcp", addr, d.Timeout)
		if err != nil {
			d.mu.Unlock()
			return fmt.Errorf("ошибка подключения к принтеру %s: %w", addr, err)
		}
		d.optimizeSocket(conn)
		d.conn = conn
		slog.Info("VALENTIN-DIRECT: Монопольный TCP-сокет успешно открыт", "printer_id", d.ID, "addr", addr)
	}

	d.mu.Unlock()

	// 2. Сразу переводим принтер на указанный макет и записываем статические даты
	return d.SelectTemplate(d.Name, staticFields)
}

// SelectTemplate атомарно (покомандно) загружает макет из Flash в ОЗУ и записывает поля 18 и 19
func (d *NiceLabelDriver) SelectTemplate(template string, staticFields map[string]string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

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

	// Извлекаем даты из staticFields или формируем дефолты текущего дня
	dateProd, ok := staticFields["data01"]
	if !ok || dateProd == "" {
		dateProd = time.Now().Format("02.01.2006")
	}

	dateExp, ok := staticFields["data02"]
	if !ok || dateExp == "" {
		dateExp = time.Now().AddDate(0, 1, 0).Format("02.01.2006") // +1 месяц по умолчанию
	}

	slog.Info("VALENTIN-DIRECT: Покомандная активация макета и запись дат",
		"printer_id", d.ID,
		"template", d.curTemplate,
		"date_prod", dateProd,
		"date_exp", dateExp,
	)

	// --- ШАГ 1: Выбираем макет из Flash (FMB) ---
	cmdFMB := []byte(fmt.Sprintf("%cFMB---r%s%c", SOH, d.curTemplate, ETB))
	d.conn.SetWriteDeadline(time.Now().Add(d.Timeout))
	d.traceCommand("STEP 1: Select Layout (FMB)", cmdFMB)
	if _, err := d.conn.Write(cmdFMB); err != nil {
		d.closeConnNoLock()
		return fmt.Errorf("сбой отправки FMB: %w", err)
	}
	time.Sleep(20 * time.Millisecond) // Пауза на переключение графического буфера в RAM

	// --- ШАГ 2: Фиксируем дату производства в поле 18 (BM[18]) ---
	cmdBM18 := []byte(fmt.Sprintf("%cBM[18]%s%c", SOH, dateProd, ETB))
	d.conn.SetWriteDeadline(time.Now().Add(d.Timeout))
	d.traceCommand("STEP 2: Field 18 DateProd (BM)", cmdBM18)
	if _, err := d.conn.Write(cmdBM18); err != nil {
		d.closeConnNoLock()
		return fmt.Errorf("сбой отправки BM[18]: %w", err)
	}
	time.Sleep(10 * time.Millisecond)

	// --- ШАГ 3: Фиксируем дату годности в поле 19 (BM[19]) ---
	cmdBM19 := []byte(fmt.Sprintf("%cBM[19]%s%c", SOH, dateExp, ETB))
	d.conn.SetWriteDeadline(time.Now().Add(d.Timeout))
	d.traceCommand("STEP 3: Field 19 DateExp (BM)", cmdBM19)
	if _, err := d.conn.Write(cmdBM19); err != nil {
		d.closeConnNoLock()
		return fmt.Errorf("сбой отправки BM[19]: %w", err)
	}
	time.Sleep(10 * time.Millisecond)

	// --- ШАГ 4: Первичный взвод в режим ожидания датчика (FBC) ---
	cmdFBC := []byte(fmt.Sprintf("%cFBC---r--------%c", SOH, ETB))
	d.conn.SetWriteDeadline(time.Now().Add(d.Timeout))
	d.traceCommand("STEP 4: Arm Printer (FBC)", cmdFBC)
	if _, err := d.conn.Write(cmdFBC); err != nil {
		d.closeConnNoLock()
		return fmt.Errorf("сбой отправки FBC: %w", err)
	}

	// Сбрасываем физическую засечку, так как FMB обнуляет внутренний FBBC
	d.lastRawFBBC = 0

	slog.Info("VALENTIN-DIRECT: Инициализация завершена, макет и даты зафиксированы в ОЗУ", "printer_id", d.ID)
	return nil
}

// PrintBatchIndexed отправляет строго минимальный реактивный кадр с кодом маркировки (BM[20])
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

	// Берем строго 1 код из входящей пачки (режим 1 в 1)
	targetCode := codes[0]

	// Фильтрация чистейшего криптохвоста от лишних метаданных
	cleanCode := targetCode
	if idx := strings.Index(cleanCode, "|"); idx != -1 {
		cleanCode = cleanCode[:idx]
	}
	cleanCode = strings.TrimSpace(cleanCode)

	var batchPayload bytes.Buffer

	// 1. Обновляем ТОЛЬКО динамический блок 20 (Честный Знак / DataMatrix)
	batchPayload.WriteString(fmt.Sprintf("%cBM[20]%s%c", SOH, cleanCode, ETB))

	// 2. Выставляем тираж строго 1 шт.
	batchPayload.WriteString(fmt.Sprintf("%cFD----r1%c", SOH, ETB))

	// 3. Взводим триггер ожидания фотодатчика на конвейере
	batchPayload.WriteString(fmt.Sprintf("%cFBC---r--------%c", SOH, ETB))

	d.conn.SetWriteDeadline(time.Now().Add(d.Timeout))
	d.traceCommand(fmt.Sprintf("PUMPER MINIMAL TACT (Индекс: %d)", startIndex), batchPayload.Bytes())

	if _, err := d.conn.Write(batchPayload.Bytes()); err != nil {
		slog.Error("VALENTIN-DIRECT: Сбой отправки минимального кадра", "printer_id", d.ID, "err", err)
		d.closeConnNoLock()
		return 0, err
	}

	slog.Info("VALENTIN-DIRECT: Код BM[20] взведен на датчик", "printer_id", d.ID, "index", startIndex)
	return 1, nil
}

// GetCurrentPrintCount опрашивает FBBC и высчитывает виртуальный нарастающий итог для фронтенда
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

	buf := make([]byte, 64)
	n, err := d.conn.Read(buf)
	if err != nil {
		// При таймауте возвращаем последнее известное виртуальное значение без разрыва сокета
		if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
			return strconv.Itoa(d.lastCount), nil
		}
		d.closeConnNoLock()
		return strconv.Itoa(d.lastCount), err
	}

	cleanResp := strings.Trim(string(buf[:n]), string([]byte{SOH, byte(ETB), '\r', '\n', ' '}))

	rawCount := 0
	if strings.HasPrefix(cleanResp, "A") {
		parts := strings.Split(cleanResp, "A")
		if len(parts) > 1 {
			rawCount, _ = strconv.Atoi(strings.TrimSpace(parts[1]))
		}
	} else {
		rawCount, _ = strconv.Atoi(strings.TrimSpace(cleanResp))
	}

	// ЛОГИКА ВИРТУАЛЬНОГО ИНКРЕМЕНТА:
	// Если физический счетчик Valentin вырос по сравнению с последним опросом
	if rawCount > d.lastRawFBBC {
		delta := rawCount - d.lastRawFBBC
		d.lastCount += delta     // Увеличиваем виртуальный счетчик смены
		d.lastRawFBBC = rawCount // Запоминаем текущую физическую засечку
	} else if rawCount < d.lastRawFBBC {
		// Если принтер сбросил FBBC в 0 (например, произошел перезапуск макета FMB)
		d.lastCount += rawCount
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

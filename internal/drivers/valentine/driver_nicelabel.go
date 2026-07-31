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

	// ВАЛИДАЦИЯ: Если 1С не передала обязательные поля — жестко бракуем запуск!
	if !ok1 || strings.TrimSpace(dateProd) == "" {
		return fmt.Errorf("ошибка валидации 1С: поле дата производства 'data01' не заполнено")
	}
	if !ok2 || strings.TrimSpace(dateExp) == "" {
		return fmt.Errorf("ошибка валидации 1С: поле дата годности 'data02' не заполнено")
	}

	slog.Info("VALENTIN-DIRECT: Покомандная активация макета и запись точных дат из 1С",
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

	// --- ШАГ 2: Записываем точную дату производства в поле 18 (BM[18]) ---
	cmdBM18 := []byte(fmt.Sprintf("%cBM[18]%s%c", SOH, dateProd, ETB))
	d.conn.SetWriteDeadline(time.Now().Add(d.Timeout))
	d.traceCommand("STEP 2: Field 18 DateProd (BM)", cmdBM18)
	if _, err := d.conn.Write(cmdBM18); err != nil {
		d.closeConnNoLock()
		return fmt.Errorf("сбой отправки BM[18]: %w", err)
	}
	time.Sleep(10 * time.Millisecond)

	// --- ШАГ 3: Записываем точную дату годности в поле 19 (BM[19]) ---
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

	d.lastRawFBBC = 0

	slog.Info("VALENTIN-DIRECT: Инициализация завершена, оригинальные даты 1С зафиксированы в ОЗУ", "printer_id", d.ID)
	return nil
}

// PrintBatchIndexed обновляет DataMatrix в ОЗУ принтера и взводит триггер печати
func (d *NiceLabelDriver) PrintBatchIndexed(fieldName string, startIndex int, codes []string) (int, error) {
	if len(codes) == 0 {
		return 0, nil
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	if d.conn == nil {
		if err := d.reconnectNoLock(); err != nil {
			return 0, fmt.Errorf("ошибка сокета: %w", err)
		}
	}

	cleanCode := strings.TrimSpace(codes[0])
	if idx := strings.Index(cleanCode, "|"); idx != -1 {
		cleanCode = cleanCode[:idx]
	}
	cleanCode = strings.ReplaceAll(cleanCode, "<GS>", "\x1d") // Восстановление байта GS1

	// 1. Пауза перед записью (снимаем готовность триггера)
	cmdFBDPause := []byte(fmt.Sprintf("%cFBD---r0-------%c", SOH, ETB))
	d.conn.SetWriteDeadline(time.Now().Add(d.Timeout))
	if _, err := d.conn.Write(cmdFBDPause); err != nil {
		d.closeConnNoLock()
		return 0, fmt.Errorf("сбой FBD r0: %w", err)
	}

	time.Sleep(15 * time.Millisecond)

	// 2. Отправляем DataMatrix в BM[20]
	cmdBM20 := []byte(fmt.Sprintf("%cBM[20]%s%c", SOH, cleanCode, ETB))
	d.conn.SetWriteDeadline(time.Now().Add(d.Timeout))
	if _, err := d.conn.Write(cmdBM20); err != nil {
		d.closeConnNoLock()
		return 0, fmt.Errorf("сбой BM20: %w", err)
	}

	time.Sleep(15 * time.Millisecond)

	// 3. Взвод триггера (готовность к печати по фотодатчику)
	cmdFBDRun := []byte(fmt.Sprintf("%cFBD---r1-------%c", SOH, ETB))
	d.conn.SetWriteDeadline(time.Now().Add(d.Timeout))
	if _, err := d.conn.Write(cmdFBDRun); err != nil {
		d.closeConnNoLock()
		return 0, fmt.Errorf("сбой FBD r1: %w", err)
	}

	return 1, nil
}

// GetCurrentPrintCount просто отдает виртуальный итог для UI
func (d *NiceLabelDriver) GetCurrentPrintCount() (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
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

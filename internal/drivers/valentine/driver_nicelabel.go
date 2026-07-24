package valentine

import (
	"bytes"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

type NiceLabelDriver struct {
	Name         string        // Сетевое/системное имя принтера для NiceLabel
	ID           int           // ID принтера из базы данных
	Address      string        // IP-адрес принтера
	Port         int           // Порт принтера (9100)
	Timeout      time.Duration // Сетевой таймаут сокета
	NiceLabelURL string        // Адрес HTTP-триггера NiceLabel Automation
	conn         net.Conn      // Активная монопольная TCP-сессия
	mu           sync.Mutex    // Мьютекс для защиты сокета
	curTemplate  string        // Текущий активный макет (код PLU)
	lastCount    int           // Последнее валидное значение счетчика FBBC
	isPumping    bool          // Флаг активности реалтайм-насоса кодов
	stopPumping  chan struct{} // Канал останова
}

func NewNiceLabelDriver(id int, ip string, port int) *NiceLabelDriver {
	return &NiceLabelDriver{
		ID:           id,
		Address:      ip,
		Port:         port,
		Timeout:      3 * time.Second,
		NiceLabelURL: "http://srv205:10000/",
		conn:         nil,
		curTemplate:  "",
		lastCount:    -1,
		isPumping:    false,
		stopPumping:  make(chan struct{}),
	}
}

// InitSession проверяет готовность линии и удерживает активный сокет
func (d *NiceLabelDriver) InitSession(fieldName string, maxQueue int, staticFields map[string]string) error {
	d.mu.Lock()
	if d.conn != nil {
		slog.Info("VALENTIN-MANAGED: Сессия уже активна, сокет удерживается.", "printer_id", d.ID)
		d.mu.Unlock()
		return nil
	}

	addr := net.JoinHostPort(d.Address, strconv.Itoa(d.Port))
	conn, err := net.DialTimeout("tcp", addr, d.Timeout)
	if err != nil {
		d.mu.Unlock()
		return fmt.Errorf("ошибка первичного подключения к порту: %w", err)
	}

	d.optimizeSocket(conn)
	d.conn = conn

	// Запрашиваем аппаратный статус диска А
	d.conn.SetDeadline(time.Now().Add(d.Timeout))
	if _, err = d.conn.Write([]byte{SOH, 'F', 'M', 'S', '-', '-', '-', 'w', 'A', ETB}); err != nil {
		d.closeConnNoLock()
		d.mu.Unlock()
		return fmt.Errorf("ошибка отправки команды FMS: %w", err)
	}

	buf := make([]byte, 32)
	n, err := d.conn.Read(buf)
	if err != nil {
		d.closeConnNoLock()
		d.mu.Unlock()
		return fmt.Errorf("ошибка чтения статуса диска FMS: %w", err)
	}

	statusResp := strings.Trim(string(buf[:n]), string([]byte{SOH, byte(ETB), '\r', '\n', ' '}))
	if !strings.HasPrefix(statusResp, "AA2") {
		d.closeConnNoLock()
		d.mu.Unlock()
		return fmt.Errorf("критический статус накопителя принтера: %s", statusResp)
	}

	d.mu.Unlock()
	return d.SelectTemplate(d.Name, staticFields)
}

// SelectTemplate выполняет мягкий цикл смены шаблона и взаимодействия с NiceLabel
func (d *NiceLabelDriver) SelectTemplate(template string, staticFields map[string]string) error {
	addr := net.JoinHostPort(d.Address, strconv.Itoa(d.Port))

	if template != "" {
		d.curTemplate = template
	} else if d.curTemplate == "" {
		return fmt.Errorf("критическая ошибка: передан пустой код макета (PLU)")
	}

	d.mu.Lock()
	d.isPumping = true
	d.closeConnNoLock() // Плавно закрываем старый сокет перед передачей управления NiceLabel

	// --- ШАГ 1: ПОДКЛЮЧЕНИЕ И ФОРМАТИРОВАНИЕ ---
	conn, err := net.DialTimeout("tcp", addr, d.Timeout)
	if err != nil {
		d.isPumping = false
		d.mu.Unlock()
		return fmt.Errorf("ошибка подключения для форматирования: %w", err)
	}
	d.optimizeSocket(conn)
	d.conn = conn

	slog.Info("VALENTIN-MANAGED: Форматирование накопителя А...", "printer_id", d.ID)
	cmdFormat := fmt.Sprintf("%cFMD---rA%c", SOH, ETB)
	d.traceCommand("FMD (Форматирование диска)", []byte(cmdFormat))

	d.conn.SetWriteDeadline(time.Now().Add(d.Timeout))
	if _, err = d.conn.Write([]byte(cmdFormat)); err != nil {
		d.closeConnNoLock()
		d.isPumping = false
		d.mu.Unlock()
		return fmt.Errorf("ошибка отправки кадра FMD: %w", err)
	}

	// Даем контроллеру 2 секунды на очистку FAT вместо жесткой 5-секундной блокировки
	time.Sleep(2000 * time.Millisecond)
	d.closeConnNoLock() // Освобождаем порт для NiceLabel Automation
	d.mu.Unlock()

	// --- ШАГ 2: ВЫЗОВ HTTP-ТРИГГЕРА NICELABEL AUTOMATION ---
	staticDate, ok := staticFields["data01"]
	if !ok {
		staticDate = time.Now().Format("02.01.2006")
	}
	printerIDStr := strconv.Itoa(d.ID)
	xmlPayload := fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?><LABEL><action><PRINT>TRUE</PRINT><PRINTERNAME>%s</PRINTERNAME></action><data><plu>%s</plu><date>%s</date></data></LABEL>`, printerIDStr, d.curTemplate, staticDate)

	slog.Info("VALENTIN-MANAGED: Отправка XML-триггера в NiceLabel Automation", "printer_id", d.ID)
	resp, err := http.Post(d.NiceLabelURL, "application/xml", bytes.NewBufferString(xmlPayload))
	if err != nil {
		d.mu.Lock()
		d.isPumping = false
		d.mu.Unlock()
		return fmt.Errorf("ошибка отправки XML в NiceLabel: %w", err)
	}
	resp.Body.Close()

	// --- ШАГ 3: МЯГКИЙ RETRY-ЦИКЛ ПЕРЕХВАТА ПОРТА ---
	// Пауза 2 секунды, чтобы NiceLabel успел отстрелять и закрыть свой сокет
	time.Sleep(2000 * time.Millisecond)

	var newConn net.Conn
	maxRetries := 10
	retryInterval := 500 * time.Millisecond

	for i := 1; i <= maxRetries; i++ {
		newConn, err = net.DialTimeout("tcp", addr, 2*time.Second)
		if err == nil {
			break
		}
		time.Sleep(retryInterval)
	}

	if err != nil {
		d.mu.Lock()
		d.isPumping = false
		d.mu.Unlock()
		return fmt.Errorf("NiceLabel Automation не освободил порт 9100: %w", err)
	}

	d.mu.Lock()
	d.optimizeSocket(newConn)
	d.conn = newConn

	// --- ШАГ 4: ВЗВОД МАКЕТА В ОЗУ ---
	d.conn.SetWriteDeadline(time.Now().Add(d.Timeout))

	cmdSelectLayout := fmt.Sprintf("%cFMB---r5580%c", SOH, ETB)
	cmdActivateLayout := fmt.Sprintf("%cFBC---r--------%c", SOH, ETB)

	d.traceCommand("FMB (Выбор макета)", []byte(cmdSelectLayout))
	d.traceCommand("FBC (Взвод макета)", []byte(cmdActivateLayout))

	var layoutPayload bytes.Buffer
	layoutPayload.WriteString(cmdSelectLayout)
	layoutPayload.WriteString(cmdActivateLayout)

	if _, err = d.conn.Write(layoutPayload.Bytes()); err != nil {
		d.closeConnNoLock()
		d.isPumping = false
		d.mu.Unlock()
		return fmt.Errorf("ошибка отправки команд FMB/FBC: %w", err)
	}

	slog.Info("VALENTIN-MANAGED: Подготовка завершена. Монопольный сокет зафиксирован.", "layout", d.curTemplate)
	d.isPumping = false
	d.mu.Unlock()
	return nil
}

// PrintBatchIndexed осуществляет мягкую загрузку кодов в буфер принтера без лишних разрывов
// PrintBatchIndexed осуществляет синхронную отправку строго 1 кода по сигналу сдвига счетчика
func (d *NiceLabelDriver) PrintBatchIndexed(fieldName string, startIndex int, codes []string) (int, error) {
	if len(codes) == 0 {
		return 0, nil
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	// Страховка сокета
	if d.conn == nil {
		if err := d.reconnectNoLock(); err != nil {
			return 0, fmt.Errorf("ошибка пампера: не удалось восстановить сокет: %w", err)
		}
	}

	// ЖЕСТКИЙ СИНХРОННЫЙ ТАКТ: Берем СТРОГО 1 код из пачки для исключения непрерывной печати
	targetCode := codes[0]

	// Очистка криптохвоста от возможных метаданных
	cleanCode := targetCode
	if idx := strings.Index(cleanCode, "|"); idx != -1 {
		cleanCode = cleanCode[:idx]
	}
	cleanCode = strings.TrimSpace(cleanCode)

	var batchPayload bytes.Buffer

	// 1. Загружаем чистый Datamatrix в блок 19
	batchPayload.WriteString(fmt.Sprintf("%cBM[19]%s%c", SOH, cleanCode, ETB))

	// 2. Выставляем тираж строго 1 штука
	batchPayload.WriteString(fmt.Sprintf("%cFD----r1%c", SOH, ETB))

	// 3. Взводим ожидание физического датчика конвейера
	batchPayload.WriteString(fmt.Sprintf("%cFBC---r--------%c", SOH, ETB))

	d.conn.SetWriteDeadline(time.Now().Add(d.Timeout))

	d.traceCommand(fmt.Sprintf("PUMPER SYNC TACT (Код КМ: %d)", startIndex), batchPayload.Bytes()[:protoMin(batchPayload.Len(), 128)])

	if _, err := d.conn.Write(batchPayload.Bytes()); err != nil {
		slog.Error("VALENTIN-PUMPER: Критический сбой отправки синхронного кадра", "printer_id", d.ID, "err", err)
		d.closeConnNoLock()
		return 0, err
	}

	slog.Info("VALENTIN-PUMPER: Код взведен на датчик, ожидание прохода продукта", "printer_id", d.ID, "index", startIndex)

	// Возвращаем 1 — мы обработали строго один код
	return 1, nil
}

// GetCurrentPrintCount — безопасный опрос счетчика без лишних разрывов сокета
func (d *NiceLabelDriver) GetCurrentPrintCount() (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.conn == nil {
		if err := d.reconnectNoLock(); err != nil {
			return "0", fmt.Errorf("сокет закрыт: %w", err)
		}
	}

	d.conn.SetDeadline(time.Now().Add(d.Timeout))
	cmd := fmt.Sprintf("%cFBBC--w%c", SOH, ETB)

	if _, err := d.conn.Write([]byte(cmd)); err != nil {
		slog.Warn("VALENTIN-NICE: Ошибка записи FBBC, инициируем переподключение", "printer_id", d.ID)
		d.closeConnNoLock()
		return "0", err
	}

	buf := make([]byte, 64)
	n, err := d.conn.Read(buf)
	if err != nil {
		// При таймауте не рвем сокет сразу, даем контроллеру завершить такт
		if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
			return "0", nil
		}
		d.closeConnNoLock()
		return "0", err
	}

	cleanResp := strings.Trim(string(buf[:n]), string([]byte{SOH, byte(ETB), '\r', '\n', ' '}))
	if strings.HasPrefix(cleanResp, "A") {
		parts := strings.Split(cleanResp, "A")
		if len(parts) > 1 {
			return strings.TrimSpace(parts[1]), nil
		}
	}

	return "0", nil
}

// Вспомогательный метод тонкой настройки сетевого стека Go
func (d *NiceLabelDriver) optimizeSocket(conn net.Conn) {
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		_ = tcpConn.SetNoDelay(true)   // Отключаем алгоритм Nagle для мгновенной отправки пакетов
		_ = tcpConn.SetKeepAlive(true) // Включаем Keep-Alive
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

func protoMin(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Заглушки интерфейса
func (d *NiceLabelDriver) ClearQueue() error                                 { return nil }
func (d *NiceLabelDriver) GetStatus() (string, error)                        { return "ГОТОВ", nil }
func (d *NiceLabelDriver) GetBufferFreeSpace() (int, error)                  { return 1, nil }
func (d *NiceLabelDriver) GetLastPrintedIndex() (int, error)                 { return d.lastCount, nil }
func (d *NiceLabelDriver) UpdateStaticFields(f map[string]string) error      { return nil }
func (d *NiceLabelDriver) PrintTemplate(t string, f map[string]string) error { return nil }
func (d *NiceLabelDriver) GetTemplates() ([]string, error)                   { return []string{d.curTemplate}, nil }
func (d *NiceLabelDriver) GetTemplateFields(t string) ([]string, error)      { return []string{"19"}, nil }
func (d *NiceLabelDriver) GetRemainingRibbon() (string, error)               { return "N/A", nil }
func (d *NiceLabelDriver) GetQueueCapacity(q string) (string, error)         { return "N/A", nil }
func (d *NiceLabelDriver) GetPrintSpeed() (string, error)                    { return "N/A", nil }
func (d *NiceLabelDriver) GetCurrentTemplate() (string, error)               { return d.curTemplate, nil }

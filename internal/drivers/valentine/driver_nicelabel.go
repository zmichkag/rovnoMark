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
	Name         string        // Сетевое/системное имя принтера для NiceLabel (CFS)
	ID           int           // ID принтера из базы данных
	Address      string        // IP-адрес принтера Валентин/GEA
	Port         int           // Порт принтера (9100)
	Timeout      time.Duration // Сетевой таймаут сокета
	NiceLabelURL string        // Адрес HTTP-триггера NiceLabel Automation
	conn         net.Conn      // Активная монопольная TCP-сессия
	mu           sync.Mutex    // Мьютекс для защиты сокета при многопоточном обращении
	curTemplate  string        // Текущий активный макет (он же код PLU)
	lastCount    int           // Последнее валидное значение счетчика FBBC
	isPumping    bool          // Флаг активности реалтайм-насоса кодов
	stopPumping  chan struct{} // Канал для graceful-останова горутины накачки
}

func NewNiceLabelDriver(id int, ip string, port int) *NiceLabelDriver {
	return &NiceLabelDriver{
		ID:           id,
		Address:      ip,
		Port:         port,
		Timeout:      3 * time.Second,
		NiceLabelURL: "http://srv205:10000/",
		conn:         nil,
		mu:           sync.Mutex{},
		curTemplate:  "",
		lastCount:    -1,
		isPumping:    false,
		stopPumping:  make(chan struct{}),
	}
}

// InitSession проверяет готовность. Если сокет уже удерживается — просто выходим, задача уже решена!
func (d *NiceLabelDriver) InitSession(fieldName string, maxQueue int, staticFields map[string]string) error {
	addr := net.JoinHostPort(d.Address, strconv.Itoa(d.Port))

	d.mu.Lock()
	// ИСПРАВЛЕНО: Если сокет живой, значит макет уже залит и взведен.
	// Просто возвращаем nil наружу, прерывая дублирующий контур!
	if d.conn != nil {
		slog.Info("VALENTIN-MANAGED: Сессия уже активна, сокет удерживается. Повторная подготовка не требуется.", "printer_id", d.ID)
		d.mu.Unlock()
		return nil
	}

	var err error
	d.conn, err = net.DialTimeout("tcp", addr, d.Timeout)
	if err != nil {
		d.mu.Unlock()
		return fmt.Errorf("ошибка первичного подключения к порту: %w", err)
	}

	// Запрашиваем аппаратный статус диска А
	d.conn.SetWriteDeadline(time.Now().Add(d.Timeout))
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

	// Вызываем подготовку шаблона ТОЛЬКО при первичном холодном запуске соединения!
	return d.SelectTemplate(d.Name, staticFields)
}

// SelectTemplate выполняет форматирование, интеграцию с NiceLabel и детально логирует отправку команд
func (d *NiceLabelDriver) SelectTemplate(template string, staticFields map[string]string) error {
	addr := net.JoinHostPort(d.Address, strconv.Itoa(d.Port))

	// Защита от пустого PLУ из 1С при повторном чихе
	if template != "" {
		d.curTemplate = template
	} else if d.curTemplate == "" {
		return fmt.Errorf("критическая ошибка: передан пустой код макета (PLU), и нет сохраненного значения в сессии")
	}

	// Жестко блокируем поллер счетчика
	d.mu.Lock()
	d.isPumping = true

	if d.conn != nil {
		d.conn.Close()
		d.conn = nil
	}

	// --- ШАГ 1: ПОДКЛЮЧЕНИЕ И ФОРМАТИРОВАНИЕ ---
	var err error
	d.conn, err = net.DialTimeout("tcp", addr, d.Timeout)
	if err != nil {
		d.isPumping = false
		d.mu.Unlock()
		return fmt.Errorf("ошибка подключения для форматирования: %w", err)
	}

	slog.Info("VALENTIN-MANAGED: Выполнение принудительного форматирования накопителя А...", "printer_id", d.ID)

	cmdFormat := fmt.Sprintf("%cFMD---rA%c", SOH, ETB)

	// ЛОГ ТРАССИРОВКИ КОМАНДЫ FMD
	d.traceCommand("FMD (Форматирование диска)", []byte(cmdFormat))

	d.conn.SetWriteDeadline(time.Now().Add(d.Timeout))
	if _, err = d.conn.Write([]byte(cmdFormat)); err != nil {
		d.closeConnNoLock()
		d.isPumping = false
		d.mu.Unlock()
		return fmt.Errorf("ошибка отправки кадра FMD: %w", err)
	}

	time.Sleep(5000 * time.Millisecond) // Технологическая пауза Flash-памяти
	d.closeConnNoLock()
	d.mu.Unlock() // Освобождаем порт для NiceLabel Automation

	// --- ШАГ 2: ВЫЗОВ HTTP-ТРИГГЕРА NICELABEL AUTOMATION ---
	staticDate, ok := staticFields["data01"]
	if !ok {
		staticDate = time.Now().Format("02.01.2006")
	}
	printerIDStr := strconv.Itoa(d.ID)
	xmlPayload := fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?><LABEL><action><PRINT>TRUE</PRINT><PRINTERNAME>%s</PRINTERNAME></action><data><plu>%s</plu><date>%s</date></data></LABEL>`, printerIDStr, d.curTemplate, staticDate)

	slog.Info("VALENTIN-MANAGED: Отправка XML-триггера в NiceLabel Automation", "printer_id", d.ID, "target_url", d.NiceLabelURL)
	resp, err := http.Post(d.NiceLabelURL, "application/xml", bytes.NewBufferString(xmlPayload))
	if err != nil {
		d.mu.Lock()
		d.isPumping = false
		d.mu.Unlock()
		return fmt.Errorf("ошибка отправки XML в NiceLabel: %w", err)
	}
	resp.Body.Close()

	slog.Info("VALENTIN-MANAGED: Шаблон отправлен в Найс. Ожидание освобождения порта...")

	time.Sleep(5000 * time.Millisecond)

	// --- ШАГ 3: RETRY-ЦИКЛ ПЕРЕХВАТА ПОРТА 9100 ---
	var conn net.Conn
	maxRetries := 6
	retryInterval := 1000 * time.Millisecond

	for i := 1; i <= maxRetries; i++ {
		conn, err = net.DialTimeout("tcp", addr, 1*time.Second)
		if err == nil {
			break
		}
		slog.Warn("VALENTIN-MANAGED: Порт занят Найсом, ожидание освобождения линии...", "try", i, "err", err.Error())
		time.Sleep(retryInterval)
	}

	if err != nil {
		d.mu.Lock()
		d.isPumping = false
		d.mu.Unlock()
		return fmt.Errorf("NiceLabel Automation не освободил порт 9100 за отведенное время: %w", err)
	}

	d.mu.Lock()
	d.conn = conn
	if tcpConn, ok := d.conn.(*net.TCPConn); ok {
		_ = tcpConn.SetKeepAlive(true)
		_ = tcpConn.SetKeepAlivePeriod(10 * time.Second)
	}

	// --- ШАГ 4: ОТПРАВКА КОМАНД АКТИВАЦИИ МАКЕТА В ОЗУ ПРИНТЕРА ---
	d.conn.SetWriteDeadline(time.Now().Add(d.Timeout))

	// Собираем команды воедино
	cmdSelectLayout := fmt.Sprintf("%cFMB---r5580%c", SOH, ETB)
	cmdActivateLayout := fmt.Sprintf("%cFBC---r--------%c", SOH, ETB)

	// ЛОГ ТРАССИРОВКИ КОМАНД АКТИВАЦИИ
	d.traceCommand("FMB (Выбор макета)", []byte(cmdSelectLayout))
	d.traceCommand("FBC (Взвод макета на печать)", []byte(cmdActivateLayout))

	var layoutPayload bytes.Buffer
	layoutPayload.WriteString(cmdSelectLayout)
	layoutPayload.WriteString(cmdActivateLayout)

	if _, err = d.conn.Write(layoutPayload.Bytes()); err != nil {
		d.closeConnNoLock()
		d.isPumping = false
		d.mu.Unlock()
		return fmt.Errorf("ошибка отправки команд взвода макета FMB/FBC: %w", err)
	}

	slog.Info("VALENTIN-MANAGED: Подготовка завершена. Сокет зафиксирован.", "layout", d.curTemplate)

	d.isPumping = false
	d.mu.Unlock()
	return nil
}

// traceCommand форматирует управляющие байты CVPL в человекочитаемый вид для логов
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

// Вспомогательные методы сетевого обмена и парсинга
func (d *NiceLabelDriver) writeCVPL(cmd string) error {
	d.conn.SetWriteDeadline(time.Now().Add(d.Timeout))
	_, err := d.conn.Write([]byte(cmd))
	return err
}

func (d *NiceLabelDriver) readRawResponse() (string, error) {
	d.conn.SetReadDeadline(time.Now().Add(d.Timeout))
	buf := make([]byte, 8192)
	n, err := d.conn.Read(buf)
	if err != nil {
		return "", err
	}
	// Зачищаем управляющие маркеры протокола для чистого строкового анализа
	return strings.Trim(string(buf[:n]), string([]byte{SOH, byte(ETB), '\r', '\n', ' '})), nil
}

func (d *NiceLabelDriver) isDriveDirty(listing string) bool {
	lines := strings.Split(listing, string([]byte{ETB}))
	for _, line := range lines {
		clean := strings.Trim(line, string([]byte{SOH, '\r', '\n', ' '}))
		if clean == "" {
			continue
		}
		// Если находим старые .prn макеты или графику — диск требует очистки
		if strings.Contains(clean, ".prn") || strings.Contains(clean, ".png") || strings.Contains(clean, "_graphics") {
			return true
		}
	}
	return false
}

func (d *NiceLabelDriver) extractTemplateName(listing string) string {
	lines := strings.Split(listing, string([]byte{ETB}))
	for _, line := range lines {
		clean := strings.Trim(line, string([]byte{SOH, '\r', '\n', ' '}))
		if clean == "" {
			continue
		}

		// Жесткие исключения: пропускаем директории и системный мусор
		if strings.Contains(clean, "<DIR>") ||
			strings.Contains(clean, "System Volume Information") ||
			strings.Contains(clean, "WPSettings") ||
			strings.Contains(clean, "IndexerVolumeGuid") ||
			strings.Contains(clean, "$RECYCLE.BIN") ||
			strings.Contains(clean, "desktop.ini") ||
			strings.Contains(clean, "_graphics") { // Отсекаем файлы графики макета
			continue
		}

		// Ищем строку, которая содержит информацию о размере файла в байтах,
		// например: "5580(2942Byte) -----A"
		if strings.Contains(clean, "Byte)") && strings.Contains(clean, "-----A") {
			parts := strings.Split(clean, "(")
			if len(parts) > 0 {
				// Вытаскиваем имя до скобки и зачищаем пробелы. Получим строго "5580"
				detected := strings.TrimSpace(parts[0])
				if detected != "" {
					return detected
				}
			}
		}
	}
	return ""
}

func (d *NiceLabelDriver) closeConnNoLock() {
	if d.conn != nil {
		d.conn.Close()
		d.conn = nil
	}
}

// PrintBatchIndexed осуществляет поштучную накачку уникальных кодов по схеме BM с жестким хардкодом поля 19
func (d *NiceLabelDriver) PrintBatchIndexed(fieldName string, startIndex int, codes []string) (int, error) {
	if len(codes) == 0 {
		return 0, nil
	}

	d.mu.Lock()
	if d.isPumping {
		d.mu.Unlock()
		return 0, fmt.Errorf("ошибка пампера: драйвер уже занят накачкой кодов")
	}
	d.isPumping = true

	// Проверка и авто-восстановление сокета
	if d.conn == nil {
		slog.Warn("VALENTIN-PUMPER: Восстановление монопольного линка перед заливкой кодов...", "printer_id", d.ID)
		if err := d.reconnectNoLock(); err != nil {
			d.isPumping = false
			d.mu.Unlock()
			return 0, fmt.Errorf("ошибка пампера: авто-реконнект провалился: %w", err)
		}
	}
	d.mu.Unlock()

	var batchPayload bytes.Buffer

	slog.Info("VALENTIN-PUMPER: Запуск накачки кодов в Block Mode", "printer_id", d.ID, "count", len(codes))

	for _, code := range codes {
		// КРИТИЧЕСКИЙ ФИКС: Жестко зашиваем только индекс поля [19],
		// полностью игнорируя мусорные строки, прилетающие из внешней конфигурации.
		batchPayload.WriteString(fmt.Sprintf("%cBM[19]%s%c", SOH, code, ETB))

		// Установка тиража на 1 штуку для текущего ШК
		batchPayload.WriteString(fmt.Sprintf("%cFD----r1%c", SOH, ETB))

		// Взвод триггера ожидания датчика продукта на конвейере
		batchPayload.WriteString(fmt.Sprintf("%cFBC---r--------%c", SOH, ETB))
	}

	// Монопольный захват сокета для записи в физический порт 9100
	d.mu.Lock()
	defer func() {
		d.isPumping = false
		d.mu.Unlock()
	}()

	if d.conn == nil {
		return 0, fmt.Errorf("ошибка пампера: сокет упал непосредственно перед отправкой пакета")
	}

	d.conn.SetWriteDeadline(time.Now().Add(d.Timeout))

	// Выводим точную трассировку того, что реально уходит в принтер
	d.traceCommand(fmt.Sprintf("PUMPER BM BATCH (Проливка кодов, count: %d)", len(codes)), batchPayload.Bytes()[:min(batchPayload.Len(), 128)])

	if _, err := d.conn.Write(batchPayload.Bytes()); err != nil {
		slog.Error("VALENTIN-PUMPER: Критический сбой отправки пакета BM в сокет", "printer_id", d.ID, "err", err)
		d.closeConnNoLock()
		return 0, err
	}

	slog.Info("VALENTIN-PUMPER: Насос успешно пролил чистые коды в Valentin.", "printer_id", d.ID, "count", len(codes))
	return len(codes), nil
}

// GetCurrentPrintCount — жесткий шлюз от спама поллера ядра
func (d *NiceLabelDriver) GetCurrentPrintCount() (string, error) {
	d.mu.Lock()

	// КРИТИЧЕСКИЙ ФИКС: Если драйвер занят накачкой или подготовкой макета,
	// поллер ядра мгновенно отваливается без сетевой активности!
	if d.isPumping || d.curTemplate == "" {
		d.mu.Unlock()
		return "0", nil // Возвращаем дефолт, не трогая сокет
	}

	if d.conn == nil {
		slog.Warn("VALENTIN-NICE: Сокет закрыт при поллинге счетчика, инициируем реконнект...", "printer_id", d.ID)
		if err := d.reconnectNoLock(); err != nil {
			d.mu.Unlock()
			return "", fmt.Errorf("сокет принтера закрыт и реконнект провалился: %w", err)
		}
	}

	d.conn.SetDeadline(time.Now().Add(d.Timeout))
	cmd := fmt.Sprintf("%cFBBC--w%c", SOH, ETB)

	if _, err := d.conn.Write([]byte(cmd)); err != nil {
		slog.Error("VALENTIN-NICE: Сбой отправки команды FBBC", "printer_id", d.ID, "err", err)
		d.closeConnNoLock()
		d.mu.Unlock()
		return "", err
	}

	d.mu.Unlock() // Отпускаем сеть

	buf := make([]byte, 64)
	n, err := d.conn.Read(buf)
	if err != nil {
		d.mu.Lock()
		slog.Error("VALENTIN-NICE: Ошибка физического чтения ответа FBBC", "printer_id", d.ID, "err", err)

		// НЕ рвем сокет, если это обычный таймаут занятого принтера
		if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
			d.mu.Unlock()
			return "0", nil
		}
		d.closeConnNoLock()
		d.mu.Unlock()
		return "", err
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

func (d *NiceLabelDriver) reconnectNoLock() error {
	addr := net.JoinHostPort(d.Address, strconv.Itoa(d.Port))

	// Попытка установки TCP-соединения с жестким ограничением по времени
	conn, err := net.DialTimeout("tcp", addr, d.Timeout)
	if err != nil {
		slog.Error("VALENTIN-NICE: Не удалось поднять TCP-линк с принтером при реконнекте",
			"target_addr", addr,
			"err", err,
		)
		return err
	}

	d.conn = conn

	// Активируем Keep-Alive механизм, чтобы сокет не дропался при простоях линии
	if tcpConn, ok := d.conn.(*net.TCPConn); ok {
		_ = tcpConn.SetKeepAlive(true)
		_ = tcpConn.SetKeepAlivePeriod(10 * time.Second)
	}

	slog.Info("VALENTIN-NICE: Монопольный RAW TCP сокет успешно восстановлен поллером",
		"printer_id", d.ID,
		"target_addr", addr,
	)
	return nil
}

// Вспомогательная функция для безопасного лога среза байт
func protoMin(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Заглушки совместимости интерфейса
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

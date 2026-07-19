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

// InitSession осуществляет первичную жесткую валидацию аппаратного статуса диска
func (d *NiceLabelDriver) InitSession(fieldName string, maxQueue int, staticFields map[string]string) error {
	addr := net.JoinHostPort(d.Address, strconv.Itoa(d.Port))
	slog.Info("VALENTIN-MANAGED: Проверка аппаратного статуса линии", "printer_id", d.ID)

	d.mu.Lock()
	// Сброс старого линка, если он существовал
	if d.conn != nil {
		d.conn.Close()
		d.conn = nil
	}

	var err error
	d.conn, err = net.DialTimeout("tcp", addr, d.Timeout)
	if err != nil {
		d.mu.Unlock()
		return fmt.Errorf("ошибка первичного подключения к порту 9100: %w", err)
	}

	// Шлем запрос состояния накопителя А
	if err = d.writeCVPL(fmt.Sprintf("%cFMS---wA%c", SOH, ETB)); err != nil {
		d.closeConnNoLock()
		d.mu.Unlock()
		return fmt.Errorf("ошибка отправки команды FMS: %w", err)
	}

	// Читаем статус диска
	statusResp, err := d.readRawResponse()
	if err != nil {
		d.closeConnNoLock()
		d.mu.Unlock()
		return fmt.Errorf("ошибка чтения статуса диска FMS: %w", err)
	}

	// Жесткая валидация ответа: нам нужен строго статус готовности "AA2"
	if statusResp != "AA2" {
		d.closeConnNoLock()
		d.mu.Unlock()
		slog.Error("VALENTIN-MANAGED: Накопитель принтера не готов к работе", "expected", "AA2", "got", statusResp)
		return fmt.Errorf("критический статус накопителя принтера: %s. Работа невозможна", statusResp)
	}

	slog.Info("VALENTIN-MANAGED: Накопитель находится в состоянии готовности (AA2). Переходим к подготовке шаблона.")

	// Передаем управление в метод SelectTemplate. Мьютекс остается ЗАЖАТЫМ, так как SelectTemplate — внутренняя логика инициализации.
	if err = d.SelectTemplate(d.Name, staticFields); err != nil {
		// Мьютекс будет отпущен внутри SelectTemplate при ошибке или успехе
		return err
	}

	return nil
}

// SelectTemplate управляет содержимым диска, интеграцией с NiceLabel и активацией макета
// ВНИМАНИЕ: Вызывается из InitSession с уже заблокированным мьютексом d.mu!
func (d *NiceLabelDriver) SelectTemplate(template string, staticFields map[string]string) error {
	addr := net.JoinHostPort(d.Address, strconv.Itoa(d.Port))

	// Считываем текущее оглавление диска А
	if err := d.writeCVPL(fmt.Sprintf("%cFMG---wA%c", SOH, ETB)); err != nil {
		d.closeConnNoLock()
		d.mu.Unlock()
		return fmt.Errorf("ошибка запроса FMG в SelectTemplate: %w", err)
	}

	rawListing, err := d.readRawResponse()
	if err != nil {
		d.closeConnNoLock()
		d.mu.Unlock()
		return fmt.Errorf("ошибка чтения оглавления диска: %w", err)
	}

	// Анализируем содержимое диска
	if d.isDriveDirty(rawListing) {
		slog.Info("VALENTIN-MANAGED: Обнаружены посторонние файлы. Запуск форматирования накопителя...")
		if err = d.writeCVPL(fmt.Sprintf("%cFMD---rA%c", SOH, ETB)); err != nil {
			d.closeConnNoLock()
			d.mu.Unlock()
			return fmt.Errorf("ошибка форматирования диска FMD: %w", err)
		}
		// Ждем физического пересоздания структуры FAT контроллером
		time.Sleep(1500 * time.Millisecond)
	} else {
		slog.Info("VALENTIN-MANAGED: Накопитель чист от макетов, форматирование пропущено.")
	}

	// Освобождаем RAW-порт 9100, чтобы NiceLabel Automation мог залить файлы по сети
	d.closeConnNoLock()
	d.mu.Unlock()

	// ВЫЗОВ NICELABEL AUTOMATION (Вне лока сокета)
	staticDate, ok := staticFields["data01"]
	if !ok {
		staticDate = time.Now().Format("02.01.2006")
	}
	printerIDStr := strconv.Itoa(d.ID)
	xmlPayload := fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?><LABEL><action><PRINT>TRUE</PRINT><PRINTERNAME>%s</PRINTERNAME></action><data><plu>%s</plu><date>%s</date></data></LABEL>`, printerIDStr, template, staticDate)

	resp, err := http.Post(d.NiceLabelURL, "application/xml", bytes.NewBufferString(xmlPayload))
	if err != nil {
		return fmt.Errorf("ошибка HTTP-запроса к NiceLabel Automation: %w", err)
	}
	resp.Body.Close()

	slog.Info("VALENTIN-MANAGED: Задача успешно передана в NiceLabel. Ожидание трансфера файлов на принтер...")
	time.Sleep(2500 * time.Millisecond) // Технологическая пауза для сетевой заливки файлов Найсом

	// ПОВТОРНЫЙ МЕНЕДЖЕРСКИЙ ПЕРЕХВАТ СОКЕТА ДЛЯ ВАЛИДАЦИИ И ЗАПУСКА
	d.mu.Lock()
	d.conn, err = net.DialTimeout("tcp", addr, d.Timeout)
	if err != nil {
		d.mu.Unlock()
		return fmt.Errorf("ошибка повторного перехвата порта 9100: %w", err)
	}

	// Запрашиваем обновленное оглавление диска
	if err = d.writeCVPL(fmt.Sprintf("%cFMG---wA%c", SOH, ETB)); err != nil {
		d.closeConnNoLock()
		d.mu.Unlock()
		return fmt.Errorf("ошибка повторного запроса FMG: %w", err)
	}

	freshListing, err := d.readRawResponse()
	if err != nil {
		d.closeConnNoLock()
		d.mu.Unlock()
		return fmt.Errorf("ошибка чтения свежего оглавления диска: %w", err)
	}

	// Ищем скомпилированный макет по расширению .prn
	detectedTemplate := d.extractTemplateName(freshListing)
	if detectedTemplate == "" {
		d.closeConnNoLock()
		d.mu.Unlock()
		return fmt.Errorf("критический сбой интеграции: файл макета .prn не обнаружен на диске после NiceLabel")
	}
	d.curTemplate = detectedTemplate
	slog.Info("VALENTIN-MANAGED: Валидация успешна. Шаблон обнаружен в ПЗУ", "target_layout", d.curTemplate)

	// АКТИВАЦИЯ ШАБЛОНА В ОЗУ ПРИНТЕРА И ВЫВОД НА ЭКРАН
	d.conn.SetWriteDeadline(time.Now().Add(d.Timeout))
	var layoutPayload bytes.Buffer
	layoutPayload.WriteString(fmt.Sprintf("%cFMB---r%s%c", SOH, d.curTemplate, ETB))
	layoutPayload.WriteString(fmt.Sprintf("%cFBC---r%c", SOH, ETB))

	if _, err = d.conn.Write(layoutPayload.Bytes()); err != nil {
		d.closeConnNoLock()
		d.mu.Unlock()
		return fmt.Errorf("ошибка отправки команд взвода макета FMB/FBC: %w", err)
	}

	slog.Info("VALENTIN-MANAGED: Макет успешно активирован, линия переведена в боевой режим печати", "template", d.curTemplate)
	d.mu.Unlock()
	return nil
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

// PrintBatchIndexed осуществляет поштучную накачку уникальных кодов по немецкой схеме
func (d *NiceLabelDriver) PrintBatchIndexed(fieldName string, startIndex int, codes []string) (int, error) {
	if len(codes) == 0 {
		return 0, nil
	}
	d.mu.Lock()
	if d.isPumping {
		d.mu.Unlock()
		return 0, nil
	}
	d.isPumping = true
	if d.conn == nil {
		d.isPumping = false
		d.mu.Unlock()
		return 0, fmt.Errorf("ошибка пампера: монопольный сокет закрыт")
	}

	d.conn.SetWriteDeadline(time.Now().Add(d.Timeout))
	var batchPayload bytes.Buffer
	const hardcodedFieldName = "19"

	for _, code := range codes {
		batchPayload.WriteString(fmt.Sprintf("%cBV[%s]%s%c", SOH, hardcodedFieldName, code, ETB))
		batchPayload.WriteString(fmt.Sprintf("%cFBAA---r5%c", SOH, ETB))
		batchPayload.WriteString(fmt.Sprintf("%cFBBA--r00001%c", SOH, ETB))
		batchPayload.WriteString(fmt.Sprintf("%cFBC---r%c", SOH, ETB))
	}

	if _, err := d.conn.Write(batchPayload.Bytes()); err != nil {
		d.closeConnNoLock()
		d.isPumping = false
		d.mu.Unlock()
		return 0, err
	}
	d.isPumping = false
	d.mu.Unlock()
	return len(codes), nil
}

func (d *NiceLabelDriver) GetCurrentPrintCount() (string, error) {
	d.mu.Lock()

	// Автопереподключение: если сокет упал или был закрыт после InitSession
	if d.conn == nil {
		slog.Warn("VALENTIN-NICE: Сокет закрыт при поллинге счетчика, инициируем реконнект...", "printer_id", d.ID)
		if err := d.reconnectNoLock(); err != nil {
			d.mu.Unlock()
			return "", fmt.Errorf("сокет принтера закрыт и реконнект провалился: %w", err)
		}
	}

	// Взводим дедлайн на сетевую операцию I/O
	d.conn.SetDeadline(time.Now().Add(d.Timeout))

	// Формируем CVPL фрейм запроса счетчика
	cmd := fmt.Sprintf("%cFBBC--w%c", SOH, ETB)

	if _, err := d.conn.Write([]byte(cmd)); err != nil {
		slog.Error("VALENTIN-NICE: Сбой отправки команды FBBC", "printer_id", d.ID, "err", err)
		d.closeConnNoLock()
		d.mu.Unlock()
		return "", err
	}

	d.mu.Unlock() // Отпускаем лок перед блокирующим сетевым чтением

	// Читаем сырой ответ из сокета фиксированным буфером
	buf := make([]byte, 64)
	n, err := d.conn.Read(buf)
	if err != nil {
		d.mu.Lock()
		slog.Error("VALENTIN-NICE: Ошибка физического чтения ответа FBBC", "printer_id", d.ID, "err", err)
		d.closeConnNoLock()
		d.mu.Unlock()
		return "", err
	}

	// Зачищаем служебные маркеры протокола вокруг полезного груза
	cleanResp := strings.Trim(string(buf[:n]), string([]byte{SOH, byte(ETB), '\r', '\n', ' '}))

	// Валидируем префикс Valentin (формат ответа: "A00150")
	if strings.HasPrefix(cleanResp, "A") {
		parts := strings.Split(cleanResp, "A")
		if len(parts) > 1 {
			return strings.TrimSpace(parts[1]), nil
		}
	}

	return "", fmt.Errorf("получен некорректный формат ответа счетчика CVPL: %s", cleanResp)
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

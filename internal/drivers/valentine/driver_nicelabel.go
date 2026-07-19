package valentine

import (
	"bufio"
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

// ----------------------------------------------------------------───────
// КОНТУР А: ПОДГОТОВКА И ЗАПУСК СЕССИИ (INIT & HANDSHAKE)
// ----------------------------------------------------------------───────

// InitSession реализует сквозной оркестрованный контур подготовки и запуска линии
func (d *NiceLabelDriver) InitSession(fieldName string, maxQueue int, staticFields map[string]string) error {
	addr := net.JoinHostPort(d.Address, strconv.Itoa(d.Port))

	// Входим под мьютекс один раз на старте. Весь процесс переключения линии атомарен!
	d.mu.Lock()

	// Шаг 1: Если сокет был открыт, жестко рвем его
	if d.conn != nil {
		d.conn.Close()
		d.conn = nil
	}

	// Шаг 2-3: Вызываем подготовку шаблона и интеграцию с Найсом из текущего контекста.
	// Передаем имя шаблона, сохраненное в d.curTemplate (или передавай через параметры, если нужно)
	if err := d.SelectTemplate(d.curTemplate, staticFields); err != nil {
		d.mu.Unlock()
		return fmt.Errorf("отказ на этапе подготовки шаблона в SelectTemplate: %w", err)
	}

	// Шаг 4: Монопольный перехват RAW-сокета 9100.
	// Найс гарантированно отработал и закрыл свое соединение 2 секунды назад.
	var conn net.Conn
	var err error
	conn, err = net.DialTimeout("tcp", addr, d.Timeout)
	if err != nil {
		d.mu.Unlock()
		return fmt.Errorf("ошибка монопольного перехвата порта 9100 в InitSession: %w", err)
	}
	d.conn = conn

	if tcpConn, ok := d.conn.(*net.TCPConn); ok {
		_ = tcpConn.SetKeepAlive(true)
		_ = tcpConn.SetKeepAlivePeriod(10 * time.Second)
	}

	// Шаг 5: АКТИВАЦИЯ ШАБЛОНА В ОЗУ ПРИНТЕРА И ОБНОВЛЕНИЕ ЭКРАНА
	d.conn.SetWriteDeadline(time.Now().Add(d.Timeout))

	var layoutPayload bytes.Buffer
	cmdSelectLayout := fmt.Sprintf("%cFMB---r%s%c", SOH, d.curTemplate, ETB)
	layoutPayload.WriteString(cmdSelectLayout)

	cmdActivateLayout := fmt.Sprintf("%cFBC---r--------%c", SOH, ETB)
	layoutPayload.WriteString(cmdActivateLayout)

	var nWritten int
	var nErr error
	nWritten, nErr = d.conn.Write(layoutPayload.Bytes())
	if nErr != nil {
		d.closeConnNoLock()
		d.mu.Unlock()
		return fmt.Errorf("ошибка записи команд активации макета (FMB/FBC): %w", nErr)
	}

	slog.Info("VALENTIN-NICE: Макет успешно взведен в ОЗУ принтера", "bytes_sent", nWritten)

	// Выходим из критической секции. Сокет d.conn удерживается монопольно.
	d.mu.Unlock()

	// Шаг 6: ЧТЕНИЕ СТАРТОВОЙ ТОЧКИ АППАРАТНОГО СЧЕТЧИКА
	// Метод GetCurrentPrintCount внутри себя сам захватит и отпустит d.mu.Lock()
	initCountStr, err := d.GetCurrentPrintCount()
	if err == nil {
		if val, errConv := strconv.Atoi(initCountStr); errConv == nil {
			d.mu.Lock()
			d.lastCount = val
			d.mu.Unlock()
		}
	}

	slog.Info("VALENTIN-NICE: Контур инициализации успешно зафиксирован. Линия готова к накачке кодов.")
	return nil
}

// SelectTemplate выполняет предварительную подготовку железа, зачистку флеш-памяти
// и инициирует рендеринг макета на сервере NiceLabel Automation
func (d *NiceLabelDriver) SelectTemplate(template string, staticFields map[string]string) error {
	// ВНИМАНИЕ: Мьютекс d.mu НЕ захватывается здесь, так как этот метод
	// вызывается изнутри InitSession, который уже находится под локом!
	d.curTemplate = template
	slog.Info("VALENTIN-NICE [Внутренний вызов]: Подготовка шаблона (PLU)", "template", template)

	// --- 1. НИЗКОУРОВНЕВОЕ ФОРМАТИРОВАНИЕ НАКОПИТЕЛЯ ПРИНТЕРА ---
	addr := net.JoinHostPort(d.Address, strconv.Itoa(d.Port))
	cleanupConn, err := net.DialTimeout("tcp", addr, d.Timeout)
	if err != nil {
		return fmt.Errorf("ошибка подключения для форматирования памяти: %w", err)
	}

	cleanupConn.SetWriteDeadline(time.Now().Add(3 * time.Second))

	var cleanupPayload bytes.Buffer
	cmdFormatDrive := fmt.Sprintf("%cFMD---rA%c", SOH, ETB)
	cleanupPayload.WriteString(cmdFormatDrive)

	var nWritten int
	var nErr error
	nWritten, nErr = cleanupConn.Write(cleanupPayload.Bytes())
	cleanupConn.Close()
	if nErr != nil {
		return fmt.Errorf("ошибка отправки кадра клинапа FMD: %w", nErr)
	}

	slog.Info("VALENTIN-NICE: Кадр форматирования диска A: успешно передан", "bytes", nWritten)
	time.Sleep(500 * time.Millisecond) // Пауза на пересоздание таблицы FAT контроллером

	// --- 2. ИНТЕГРАЦИЯ С NICELABEL AUTOMATION ---
	staticDate, ok := staticFields["data01"]
	if !ok {
		staticDate = time.Now().Format("02.01.2006")
	}

	printerIDStr := strconv.Itoa(d.ID)
	xmlPayload := fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
	<LABEL>
	 <action>
	   <PRINT>TRUE</PRINT>
	   <PRINTERNAME>%s</PRINTERNAME>
	 </action>
	 <data>
	   <plu>%s</plu>
	   <date>%s</date>
	 </data>
	</LABEL>`, printerIDStr, d.curTemplate, staticDate)

	resp, err := http.Post(d.NiceLabelURL, "application/xml", bytes.NewBufferString(xmlPayload))
	if err != nil {
		return fmt.Errorf("ошибка отправки XML в триггер NiceLabel: %w", err)
	}
	resp.Body.Close()

	// Технологическое окно: Найс успевает залить файлы по сети, сокет принтера освобождается
	slog.Info("VALENTIN-NICE: Шаблон отправлен в Найс, ожидание завершения трансфера файлов...")
	time.Sleep(2 * time.Second)

	return nil
}

// ИСПРАВЛЕНО: Метод closeConnNoLock восстановлен в коде структуры для обеспечения сброса сокетов
func (d *NiceLabelDriver) closeConnNoLock() {
	if d.conn != nil {
		d.conn.Close()
		d.conn = nil
	}
}

// ----------------------------------------------------------------───────
// КОНТУР Б: РЕАЛТАЙМ-ОБМЕН И ПОЛЛИНГ СЧЕТЧИКА
// ----------------------------------------------------------------───────

// PrintBatchIndexed осуществляет поштучную накачку и мгновенную активацию кодов по немецкой схеме
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

	// Проверяем монопольный сокет 9100
	if d.conn == nil {
		d.isPumping = false
		d.mu.Unlock()
		return 0, fmt.Errorf("ошибка пампера: монопольное TCP-соединение 9100 не установлено")
	}

	d.conn.SetWriteDeadline(time.Now().Add(d.Timeout))

	var batchPayload bytes.Buffer

	// ЖЕСТКИЙ ХАРДКОД: Имя переменной поля в макете согласно ТЗ
	const hardcodedFieldName = "19"

	// Итерируемся по КМ и на каждый код формируем триаду команд управления страницей
	for _, code := range codes {
		// 1. Инъекция кода Честного Знака в захардкоженное поле
		cmdVariable := fmt.Sprintf("%cBV[%s]%s%c", SOH, hardcodedFieldName, code, ETB)
		batchPayload.WriteString(cmdVariable)

		// 2. Установка количества элементов/линий в макете (FBA).
		// Немец прислал "5", фиксируем эту конфигурацию для буфера ОЗУ.[cite: 1]
		cmdElements := fmt.Sprintf("%cFBAA—r5%c", SOH, ETB)
		batchPayload.WriteString(cmdElements)

		// 3. Установка тиража для текущего кадра (FBB) — 1 копия[cite: 1]
		cmdCopies := fmt.Sprintf("%cFBBA--r00001%c", SOH, ETB)
		batchPayload.WriteString(cmdCopies)

		// 4. Команда мгновенной генерации страницы и старта печати (FBC с суффиксом r)[cite: 1]
		cmdRenderStart := fmt.Sprintf("%cFBC---r%c", SOH, ETB)
		batchPayload.WriteString(cmdRenderStart)
	}

	slog.Info("VALENTIN-PUMPER: Пуш пачки поштучных страниц (Немецкая схема)",
		"printer_id", d.ID,
		"codes_count", len(codes),
		"field_used", hardcodedFieldName,
		"payload_bytes", batchPayload.Len(),
	)

	// Выплевываем весь сформированный массив страниц в RAW-порт одним пакетом
	var nWritten int
	var nErr error
	nWritten, nErr = d.conn.Write(batchPayload.Bytes())
	if nErr != nil {
		d.closeConnNoLock()
		d.isPumping = false
		d.mu.Unlock()
		slog.Error("VALENTIN-PUMPER: Сбой физической передачи пачки страниц", "printer_id", d.ID, "err", nErr)
		return 0, fmt.Errorf("ошибка записи пачки страниц в сокет: %w", nErr)
	}

	slog.Info("VALENTIN-PUMPER: Все страницы успешно улетели в сетевой стек принтера",
		"printer_id", d.ID,
		"bytes_sent", nWritten,
	)

	d.isPumping = false
	d.mu.Unlock()

	return len(codes), nil
}

func (d *NiceLabelDriver) GetCurrentPrintCount() (string, error) {
	d.mu.Lock()

	if d.conn == nil {
		slog.Warn("VALENTIN-NICE: Сокет закрыт, инициируем автопереподключение...", "printer_id", d.ID)
		if err := d.reconnectNoLock(); err != nil {
			d.mu.Unlock()
			return "", fmt.Errorf("сокет принтера закрыт и реконнект провалился: %w", err)
		}
	}

	d.conn.SetDeadline(time.Now().Add(d.Timeout))

	cmd := fmt.Sprintf("%cFBBC--w%c", SOH, ETB)

	if _, err := d.conn.Write([]byte(cmd)); err != nil {
		slog.Error("VALENTIN-NICE: Ошибка отправки команды FBBC в сокет", "printer_id", d.ID, "err", err)
		d.closeConnNoLock()
		d.mu.Unlock()
		return "", err
	}

	reader := bufio.NewReader(d.conn)
	d.mu.Unlock() // Отпускаем мьютекс на период сетевого ожидания read

	respBytes, err := reader.ReadBytes(byte(ETB))
	if err != nil {
		d.mu.Lock()
		slog.Error("VALENTIN-NICE: Ошибка чтения ответа FBBC из сокета", "printer_id", d.ID, "err", err)
		d.closeConnNoLock()
		d.mu.Unlock()
		return "", err
	}

	cleanResp := strings.Trim(string(respBytes), string([]byte{SOH, byte(ETB), '\r', '\n'}))

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

	conn, err := net.DialTimeout("tcp", addr, d.Timeout)
	if err != nil {
		slog.Error("VALENTIN-NICE: Не удалось поднять TCP-линк с принтером", "target_addr", addr, "err", err)
		return err
	}

	d.conn = conn

	if tcpConn, ok := d.conn.(*net.TCPConn); ok {
		_ = tcpConn.SetKeepAlive(true)
		_ = tcpConn.SetKeepAlivePeriod(10 * time.Second)
	}

	slog.Info("VALENTIN-NICE: Монопольный RAW TCP сокет успешно восстановлен", "printer_id", d.ID, "target_addr", addr)
	return nil
}

func (d *NiceLabelDriver) ClearQueue() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.conn == nil {
		return nil
	}
	packet := []byte{SOH, ESC, 'C', ETB}
	_, err := d.conn.Write(packet)
	return err
}

func (d *NiceLabelDriver) GetStatus() (string, error) {
	return "ГОТОВ", nil
}

func (d *NiceLabelDriver) GetBufferFreeSpace() (int, error) {
	return 1, nil
}

func (d *NiceLabelDriver) GetLastPrintedIndex() (int, error) {
	return d.lastCount, nil
}

func (d *NiceLabelDriver) UpdateStaticFields(fields map[string]string) error { return nil }
func (d *NiceLabelDriver) PrintTemplate(t string, f map[string]string) error { return nil }
func (d *NiceLabelDriver) GetTemplates() ([]string, error)                   { return []string{d.curTemplate}, nil }
func (d *NiceLabelDriver) GetTemplateFields(t string) ([]string, error) {
	return []string{"Barcode1"}, nil
}
func (d *NiceLabelDriver) GetRemainingRibbon() (string, error)       { return "N/A", nil }
func (d *NiceLabelDriver) GetQueueCapacity(q string) (string, error) { return "N/A", nil }
func (d *NiceLabelDriver) GetPrintSpeed() (string, error)            { return "N/A", nil }
func (d *NiceLabelDriver) GetCurrentTemplate() (string, error)       { return d.curTemplate, nil }

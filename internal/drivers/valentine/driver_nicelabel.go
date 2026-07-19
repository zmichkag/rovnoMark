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

// InitSession реализует блок подготовки железа и вызова NiceLabel триггера
func (d *NiceLabelDriver) InitSession(fieldName string, maxQueue int, staticFields map[string]string) error {
	// --- 1. ТРАНСПОРТНЫЙ КЛИНАП СЕССИИ ---
	d.mu.Lock()
	slog.Info("VALENTIN-NICE: Инициализация сессии", "plu_template", d.curTemplate)
	if d.conn != nil {
		d.conn.Close()
		d.conn = nil
	}
	d.mu.Unlock() // Освобождаем мьютекс на время внешних сетевых операций

	// --- 2. НИЗКОУРОВНЕВОЕ ФОРМАТИРОВАНИЕ НАКОПИТЕЛЯ ПРИНТЕРА ---
	addr := net.JoinHostPort(d.Address, strconv.Itoa(d.Port))
	cleanupConn, err := net.DialTimeout("tcp", addr, d.Timeout)
	if err != nil {
		return fmt.Errorf("ошибка подключения для форматирования памяти: %w", err)
	}

	slog.Info("VALENTIN-NICE: Сервисный сокет клинапа успешно открыт",
		"printer_id", d.ID,
		"target_addr", addr,
	)

	cleanupConn.SetWriteDeadline(time.Now().Add(3 * time.Second))

	var cleanupPayload bytes.Buffer
	// ИСПРАВЛЕНО: Добавлено двоеточие после литеры диска A (A:) согласно спецификации CVPL
	cmdFormatDrive := fmt.Sprintf("%cFMD---rA:%c", SOH, ETB)
	cleanupPayload.WriteString(cmdFormatDrive)

	payloadBytes := cleanupPayload.Bytes()

	visiblePayload := strings.NewReplacer(
		string([]byte{SOH}), "<SOH>",
		string([]byte{ETB}), "<ETB>",
	).Replace(cleanupPayload.String())

	slog.Info("VALENTIN-NICE: Подготовка к отправке кадра форматирования",
		"printer_id", d.ID,
		"buffer_size_bytes", len(payloadBytes),
		"raw_payload_ascii", visiblePayload,
	)

	bytesWritten, writeErr := cleanupConn.Write(payloadBytes)
	if writeErr != nil {
		cleanupConn.Close()
		slog.Error("VALENTIN-NICE: Критическая ошибка при физической передаче кадра клинапа",
			"printer_id", d.ID,
			"err", writeErr,
		)
		return fmt.Errorf("ошибка записи команды форматирования в сокет: %w", writeErr)
	}

	slog.Info("VALENTIN-NICE: Кадр форматирования успешно передан в сетевой стек",
		"printer_id", d.ID,
		"bytes_sent_successfully", bytesWritten,
	)

	cleanupConn.Close()
	slog.Debug("VALENTIN-NICE: Сервисный сокет клинапа закрыт со стороны Go-сервиса", "printer_id", d.ID)

	// Технологическая пауза для физического пересоздания FAT на контроллере
	time.Sleep(500 * time.Millisecond)

	// --- 3. ИНТЕГРАЦИЯ С NICELABEL AUTOMATION ---
	staticDate, ok := staticFields["data01"]
	if !ok {
		staticDate = time.Now().Format("02.01.2006")
		slog.Warn("VALENTIN-NICE: Поле 'date01' не найдено, взвод текущей даты", "task", d.curTemplate)
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

	// Технологическое окно ожидания для рендеринга и трансфера файлов со стороны HTTP-триггера
	time.Sleep(2 * time.Second)

	// --- 4. МОНОПОЛЬНЫЙ ПЕРЕХВАТ RAW-СОКЕТА ДРАЙВЕРОМ ---
	d.mu.Lock() // Лочим критическую секцию один раз на весь контур до конца Шага 5

	// Явно объявляем локальную переменную conn типа net.Conn через var,
	// чтобы не использовать оператор := и не ломать область видимости err
	var conn net.Conn
	conn, err = net.DialTimeout("tcp", addr, d.Timeout)
	if err != nil {
		d.mu.Unlock()
		return fmt.Errorf("ошибка монопольного перехвата порта 9100: %w", err)
	}
	d.conn = conn

	if tcpConn, ok := d.conn.(*net.TCPConn); ok {
		_ = tcpConn.SetKeepAlive(true)
		_ = tcpConn.SetKeepAlivePeriod(10 * time.Second)
	}

	// --- Шаг 5: АКТИВАЦИЯ ШАБЛОНА В ОЗУ ПРИНТЕРА И ОБНОВЛЕНИЕ ЭКРАНА ---
	// ВНИМАНИЕ: Дублирующий d.mu.Lock() отсюда УБРАН, так как мы уже находимся под локом

	if d.conn == nil {
		d.mu.Unlock()
		return fmt.Errorf("ошибка активации макета: монопольное TCP-соединение не установлено")
	}

	d.conn.SetWriteDeadline(time.Now().Add(d.Timeout))

	var layoutPayload bytes.Buffer
	var visibleLayoutCmd string

	// А) Динамическая команда выбора макета
	cmdSelectLayout := fmt.Sprintf("%cFMB---r5080", SOH, d.curTemplate, ETB)
	layoutPayload.WriteString(cmdSelectLayout)

	// Б) Команда активации и обновления экрана принтера (FBC)
	cmdActivateLayout := fmt.Sprintf("%cFBC---r--------%c", SOH, ETB)
	layoutPayload.WriteString(cmdActivateLayout)

	visibleLayoutCmd = strings.NewReplacer(
		string([]byte{SOH}), "<SOH>",
		string([]byte{ETB}), "<ETB>",
	).Replace(layoutPayload.String())

	slog.Info("VALENTIN-NICE: Отправка команд активации макета в ОЗУ",
		"printer_id", d.ID,
		"plu_template", d.curTemplate,
		"raw_payload_ascii", visibleLayoutCmd,
	)

	// ЯВНО ИЗОЛИРУЕМ переменные для этого шага через var, чтобы исключить
	// переиспользование и наложение контекста ошибок из Шага 2
	var nWritten int
	var nErr error

	nWritten, nErr = d.conn.Write(layoutPayload.Bytes())
	if nErr != nil {
		d.closeConnNoLock()
		d.mu.Unlock()
		slog.Error("VALENTIN-NICE: Критический сбой при отправке кадров FMB/FBC", "printer_id", d.ID, "err", nErr)
		return fmt.Errorf("ошибка записи команд активации макета в сокет: %w", nErr)
	}

	slog.Info("VALENTIN-NICE: Макет успешно активирован, экран принтера обновлен",
		"printer_id", d.ID,
		"bytes_sent", nWritten,
	)

	d.mu.Unlock() // ОСВОБОЖДАЕМ мьютекс строго перед переходом к открытым операциям.

	// --- 6. ЧТЕНИЕ СТАРТОВОЙ ТОЧКИ АППАРАТНОГО СЧЕТЧИКА ---
	initCountStr, err := d.GetCurrentPrintCount()
	if err == nil {
		if val, errConv := strconv.Atoi(initCountStr); errConv == nil {
			d.mu.Lock()
			d.lastCount = val
			d.mu.Unlock()
		}
	}

	slog.Info("VALENTIN-NICE: Контур инициализации успешно зафиксирован. Принтер готов к накачке", "ip", d.Address)
	return nil
}

func (d *NiceLabelDriver) SelectTemplate(template string, fields map[string]string) error {
	d.curTemplate = template
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

func (d *NiceLabelDriver) PrintBatchIndexed(fieldName string, startIndex int, codes []string) (int, error) {
	d.mu.Lock()
	if d.isPumping {
		d.mu.Unlock()
		return 0, nil
	}
	d.isPumping = true
	d.mu.Unlock()

	slog.Info("VALENTIN-NICE [DEBUG-STUB]: Пампер вызвал метод, имитируем успешную накачку",
		"count", len(codes),
		"start_index", startIndex,
	)

	time.Sleep(100 * time.Millisecond)

	d.mu.Lock()
	d.isPumping = false
	d.lastCount += len(codes)
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

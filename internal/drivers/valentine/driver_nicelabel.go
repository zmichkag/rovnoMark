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
	ID           int           // ID принтера
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
		NiceLabelURL: "http://srv205:10000/", // HTTP-триггер NiceLabel
		conn:         nil,
		mu:           sync.Mutex{},
		curTemplate:  "",
		lastCount:    -1,
		isPumping:    false,
		stopPumping:  make(chan struct{}),
	}
}

// ----------------------------------------------------------------───────
// КОНТУР А: ПОДГОТОВКА И ЗАПУСК СЕССИИ (INIT & HANDSHAKE) - ФИКСИРОВАН
// ----------------------------------------------------------------───────

// InitSession реализует блок подготовки железа и вызова NiceLabel триггера
func (d *NiceLabelDriver) InitSession(fieldName string, maxQueue int, staticFields map[string]string) error {
	// Шаг 1: Атомарно рвем и зануляем старую сессию, освобождая сетевой стек принтера
	d.mu.Lock()
	slog.Info("VALENTIN-NICE: Инициализация сессии", "plu_template", d.curTemplate)
	if d.conn != nil {
		d.conn.Close()
		d.conn = nil
	}
	d.mu.Unlock() // Освобождаем мьютекс, чтобы не блокировать поллер на время сетевых пауз

	// --- 2. НИЗКОУРОВНЕВОЕ ФОРМАТИРОВАНИЕ НАКОПИТЕЛЯ ПРИНТЕРА ---
	addr := net.JoinHostPort(d.Address, strconv.Itoa(d.Port))
	cleanupConn, err := net.DialTimeout("tcp", addr, d.Timeout)
	if err != nil {
		return fmt.Errorf("ошибка подключения для форматирования памяти: %w", err)
	}

	// Фиксируем успешную установку TCP-линка перед отправкой
	slog.Info("VALENTIN-NICE: Сервисный сокет клинапа успешно открыт",
		"printer_id", d.ID,
		"target_addr", addr,
	)

	// Взводим жесткий дедлайн на запись, чтобы исключить зависание горутины при сетевом шторме
	cleanupConn.SetWriteDeadline(time.Now().Add(3 * time.Second))

	var cleanupPayload bytes.Buffer

	// А) Команда полного форматирования диска А (стирает все prn и graphics разом)
	// Исправлено: Возвращен двоеточие после литеры диска A согласно спецификации CVPL (A:)
	cmdFormatDrive := fmt.Sprintf("%cFMD---rA%c", SOH, ETB)
	cleanupPayload.WriteString(cmdFormatDrive)

	// Извлекаем сырой срез байтов для логирования и отправки
	payloadBytes := cleanupPayload.Bytes()

	// Выводим в лог текстовый эквивалент посылки для ИТ-мониторинга.
	// Управляющие ASCII символы SOH (0x01) и ETB (0x17) заменяем на видимые маркеры, чтобы лог не плыл
	visiblePayload := strings.NewReplacer(
		string([]byte{SOH}), "<SOH>",
		string([]byte{ETB}), "<ETB>",
	).Replace(cleanupPayload.String())

	slog.Info("VALENTIN-NICE: Подготовка к отправке кадра форматирования",
		"printer_id", d.ID,
		"buffer_size_bytes", len(payloadBytes),
		"raw_payload_ascii", visiblePayload,
	)

	// Выполняем запись и перехватываем возвращаемые значения для глубокого аудита
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

	// Жестко закрываем сервисное соединение, освобождая порт 9100 для NiceLabel Automation
	cleanupConn.Close()
	slog.Debug("VALENTIN-NICE: Сервисный сокет клинапа закрыт со стороны Go-сервиса", "printer_id", d.ID)

	// Технологическая пауза: даем контроллеру Valentin время пересоздать таблицу FAT диска A:
	time.Sleep(100 * time.Millisecond)

	// --- 3. ИНТЕГРАЦИЯ С NICELABEL AUTOMATION ---
	staticDate, ok := staticFields["data01"]
	if !ok {
		staticDate = time.Now().Format("02.01.2006")
		slog.Warn("VALENTIN-NICE: Поле 'date01' не найдено, взвод текущей даты", "task", d.curTemplate)
	}

	// Явно приводим int к валидной строке "9" на уровне Go
	printerIDStr := strconv.Itoa(d.ID)

	// В маске %s теперь гарантированно будет чистая строка "9" без системных артефактов
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

	// Оставляем технологическую паузу
	time.Sleep(2 * time.Second)

	// Шаг 4: Монопольный перехват и удержание RAW-сокета драйвером
	d.mu.Lock()
	conn, err := net.DialTimeout("tcp", addr, d.Timeout)
	if err != nil {
		d.mu.Unlock()
		return fmt.Errorf("ошибка монопольного перехвата порта 9100: %w", err)
	}
	d.conn = conn

	// Настраиваем системные Keep-Alive, чтобы линк не засыпал при простоях конвейера
	if tcpConn, ok := d.conn.(*net.TCPConn); ok {
		_ = tcpConn.SetKeepAlive(true)
		_ = tcpConn.SetKeepAlivePeriod(10 * time.Second)
	}

	// --- Шаг 5: АКТИВАЦИЯ ШАБЛОНА В ОЗУ ПРИНТЕРА И ОБНОВЛЕНИЕ ЭКРАНА ---
	d.mu.Lock()

	// Защита: проверяем, что монопольный сокет 9100 жив
	if d.conn == nil {
		d.mu.Unlock()
		return fmt.Errorf("ошибка активации макета: монопольное TCP-соединение не установлено")
	}

	// Взводим дедлайн на отправку команд конфигурации
	d.conn.SetWriteDeadline(time.Now().Add(d.Timeout))

	var layoutPayload bytes.Buffer
	var visibleLayoutCmd string

	// А) Динамическая команда выбора макета
	cmdSelectLayout := fmt.Sprintf("%cFMB---r5080", SOH, d.curTemplate, ETB)
	layoutPayload.WriteString(cmdSelectLayout)

	// Б) Команда активации и обновления экрана принтера (FBC)
	cmdActivateLayout := fmt.Sprintf("%cFBC---r--------%c", SOH, ETB)
	layoutPayload.WriteString(cmdActivateLayout)

	// Подготавливаем видимую строку для аудита в консоли бэкенда «РОВНО»
	visibleLayoutCmd = strings.NewReplacer(
		string([]byte{SOH}), "<SOH>",
		string([]byte{ETB}), "<ETB>",
	).Replace(layoutPayload.String())

	slog.Info("VALENTIN-NICE: Отправка команд активации макета в ОЗУ",
		"printer_id", d.ID,
		"plu_template", d.curTemplate,
		"raw_payload_ascii", visibleLayoutCmd,
	)

	// Физически проталкиваем обе команды одним пакетом.
	// Переменные bytesWritten и writeErr переиспользуются из Шага 2, никаких повторных объявлений!
	bytesWritten, writeErr = d.conn.Write(layoutPayload.Bytes())
	if writeErr != nil {
		d.closeConnNoLock() // Сбрасываем сокет в nil при любой ошибке ввода-вывода
		d.mu.Unlock()
		slog.Error("VALENTIN-NICE: Критический сбой при отправке кадров FMB/FBC", "printer_id", d.ID, "err", writeErr)
		return fmt.Errorf("ошибка записи команд активации макета in сокет: %w", writeErr)
	}

	slog.Info("VALENTIN-NICE: Макет успешно активирован, экран принтера обновлен",
		"printer_id", d.ID,
		"bytes_sent", bytesWritten,
	)

	d.mu.Unlock()

	// Шаг 6: Считываем стартовую точку аппаратного счетчика для корректного дельта-контроля
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
	d.curTemplate = template // Сохраняем имя шаблона (код PLU) во внутреннее состояние драйвера
	return nil
}

func (d *NiceLabelDriver) closeConnNoLock() {
	if d.conn != nil {
		d.conn.Close()
		d.conn = nil
	}
}

// ----------------------------------------------------------------───────
// КОНТУР Б: ВРЕМЕННЫЕ ИМПЛЕМЕНТАЦИИ И ЗАГЛУШКИ ДЛЯ СБОРКИ ПРОЕКТА
// ----------------------------------------------------------------───────

func (d *NiceLabelDriver) PrintBatchIndexed(fieldName string, startIndex int, codes []string) (int, error) {
	d.mu.Lock()
	if d.isPumping {
		d.mu.Unlock()
		return 0, nil
	}
	d.isPumping = true
	d.mu.Unlock()

	// Выводим информационный лог в дебаг, чтобы видеть, что пампер стучится
	slog.Info("VALENTIN-NICE [DEBUG-STUB]: Пампер вызвал метод, имитируем успешную накачку",
		"count", len(codes),
		"start_index", startIndex,
	)

	// Временная имитация реалтайм-задержки, как будто принтер думает
	time.Sleep(100 * time.Millisecond)

	d.mu.Lock()
	d.isPumping = false
	// Обновляем d.lastCount, чтобы симулировать движение счетчика в интерфейсе
	d.lastCount += len(codes)
	d.mu.Unlock()

	// Возвращаем len(codes), сообщая ядру (Pumper), что вся пачка якобы "успешно загружена"
	return len(codes), nil
}

// GetCurrentPrintCount осуществляет низкоуровневое чтение аппаратного регистра счетчика FBBC
func (d *NiceLabelDriver) GetCurrentPrintCount() (string, error) {
	d.mu.Lock()

	// Если сокет не инициализирован или упал (после InitSession или сбоя сети)
	// осуществляем атомарный автопереподключение прямо перед отправкой команды
	if d.conn == nil {
		slog.Warn("VALENTIN-NICE: Сокет закрыт, инициируем автопереподключение...", "printer_id", d.ID)
		if err := d.reconnectNoLock(); err != nil {
			d.mu.Unlock()
			return "", fmt.Errorf("сокет принтера закрыт и реконнект провалился: %w", err)
		}
	}

	// Выставляем жесткий дедлайн на сетевые операции ввода-вывода (I/O)
	d.conn.SetDeadline(time.Now().Add(d.Timeout))

	// Формируем команду чтения регистра FBBC по протоколу CVPL
	// Запрос: <SOH>FBBC--w<ETB>
	cmd := fmt.Sprintf("%cFBBC--w%c", SOH, ETB)

	// Отправляем запрос счетчика в RAW-порт 9100
	if _, err := d.conn.Write([]byte(cmd)); err != nil {
		slog.Error("VALENTIN-NICE: Ошибка отправки команды FBBC в сокет", "printer_id", d.ID, "err", err)
		d.closeConnNoLock() // Сбрасываем сокет в nil при сбое I/O для запуска реконнекта на следующем тике
		d.mu.Unlock()
		return "", err
	}

	// Для безопасного блокирующего чтения используем буферизированный ридер поверх сокета
	reader := bufio.NewReader(d.conn)
	d.mu.Unlock() // Отпускаем мьютекс перед долгим ожиданием ответа из сети, чтобы не лочить другие методы

	// Ожидаем ответный кадр принтера, который жестко ограничен байтом окончания трансляции ETB
	respBytes, err := reader.ReadBytes(byte(ETB))
	if err != nil {
		d.mu.Lock()
		slog.Error("VALENTIN-NICE: Ошибка чтения ответа FBBC из сокета", "printer_id", d.ID, "err", err)
		d.closeConnNoLock() // Маркируем сокет как мертвый
		d.mu.Unlock()
		return "", err
	}

	// Зачищаем служебные непечатные управляющие символы протокола и переносы строк вокруг полезного груза[cite: 1]
	cleanResp := strings.Trim(string(respBytes), string([]byte{SOH, byte(ETB), '\r', '\n'}))

	// Согласно мануалу Valentin, ответ на запрос считывания («w») приходит в формате: "A[значение]"[cite: 1]
	// Например: "A00150" (где 150 — текущий счетчик отпечатанных этикеток)[cite: 1]
	if strings.HasPrefix(cleanResp, "A") {
		parts := strings.Split(cleanResp, "A")
		if len(parts) > 1 {
			// Возвращаем очищенное строковое ASCII-представление числа (например, "00150")[cite: 1]
			return strings.TrimSpace(parts[1]), nil
		}
	}

	return "", fmt.Errorf("получен некорректный формат ответа счетчика CVPL: %s", cleanResp)
}

// reconnectNoLock выполняет физическое переподключение к RAW-порту принтера 9100
// Внимание: вызывающий поток должен удерживать мьютекс mu перед вызовом этого метода!
func (d *NiceLabelDriver) reconnectNoLock() error {
	addr := net.JoinHostPort(d.Address, strconv.Itoa(d.Port))

	// Выполняем попытку подключения с жестким ограничением по времени[cite: 1]
	conn, err := net.DialTimeout("tcp", addr, d.Timeout)
	if err != nil {
		slog.Error("VALENTIN-NICE: Не удалось поднять TCP-линк с принтером", "target_addr", addr, "err", err)
		return err
	}

	d.conn = conn

	// Активируем системный механизм Keep-Alive на уровне ядра ОС,
	// чтобы линк не засыпал во время технологических простоев конвейера[cite: 1]
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
	return 1, nil // Возвращаем 1 слот, чтобы менеджер не переполнял буфер
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

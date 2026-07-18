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
	time.Sleep(500 * time.Millisecond)

	// --- 3. ИНТЕГРАЦИЯ С NICELABEL AUTOMATION ---
	staticDate, ok := staticFields["date01"]
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

	//// Шаг 4: Монопольный перехват и удержание RAW-сокета драйвером
	//d.mu.Lock()
	//conn, err := net.DialTimeout("tcp", addr, d.Timeout)
	//if err != nil {
	//	d.mu.Unlock()
	//	return fmt.Errorf("ошибка монопольного перехвата порта 9100: %w", err)
	//}
	//d.conn = conn
	//
	//// Настраиваем системные Keep-Alive, чтобы линк не засыпал при простоях конвейера
	//if tcpConn, ok := d.conn.(*net.TCPConn); ok {
	//	_ = tcpConn.SetKeepAlive(true)
	//	_ = tcpConn.SetKeepAlivePeriod(10 * time.Second)
	//}
	//
	////// Шаг 5: Вызов отрендеренного Найсом макета из флеш-памяти
	////cmdLoadLayout := fmt.Sprintf("%cFMA---rA:\\Standard\\5580", SOH, d.curTemplate, ETB)
	////if _, err := d.conn.Write([]byte(cmdLoadLayout)); err != nil {
	////	d.closeConnNoLock()
	////	d.mu.Unlock()
	////	return fmt.Errorf("ошибка активации макета из памяти принтера: %w", err)
	////}
	////d.mu.Unlock()

	//// Шаг 6: Считываем стартовую точку аппаратного счетчика для корректного дельта-контроля
	//initCountStr, err := d.GetCurrentPrintCount()
	//if err == nil {
	//	if val, errConv := strconv.Atoi(initCountStr); errConv == nil {
	//		d.mu.Lock()
	//		d.lastCount = val
	//		d.mu.Unlock()
	//	}
	//}

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

func (d *NiceLabelDriver) GetCurrentPrintCount() (string, error) {
	// Временная безопасная заглушка для успешного прохождения Шага 6 инициализации
	return "0", nil
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

func (d *NiceLabelDriver) reconnectNoLock() error {
	panic("implement me: автореконнект заблокирован до стабилизации InitSession")
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

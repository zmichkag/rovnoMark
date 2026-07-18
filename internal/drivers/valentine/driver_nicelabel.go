package valentine

import (
	"bytes"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

type NiceLabelDriver struct {
	Name         string        // Сетевое/системное имя принтера для NiceLabel (CFS)
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

func NewNiceLabelDriver(name string, ip string, port int) *NiceLabelDriver {
	return &NiceLabelDriver{
		Name:         name,
		Address:      ip,
		Port:         port,
		Timeout:      3 * time.Second,
		NiceLabelURL: "http://srv205:10000/", // Твой рабочий HTTP-триггер
		lastCount:    -1,
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

	// Взводим жесткий дедлайн на запись
	cleanupConn.SetWriteDeadline(time.Now().Add(3 * time.Second))

	var cleanupPayload bytes.Buffer

	// А) Команда полного форматирования диска А (стирает все prn и graphics разом)
	cmdFormatDrive := fmt.Sprintf("%cFMD---rA:%c", SOH, ETB)
	cleanupPayload.WriteString(cmdFormatDrive)

	// Отправляем пакет и закрываем сессию
	_, _ = cleanupConn.Write(cleanupPayload.Bytes())
	cleanupConn.Close()

	// ждем форматирование
	time.Sleep(500 * time.Millisecond)

	// --- 3. ИНТЕГРАЦИЯ С NICELABEL AUTOMATION ---
	staticDate, ok := staticFields["date01"]
	if !ok {
		staticDate = time.Now().Format("02.01.2006")
		slog.Warn("VALENTIN-NICE: Поле 'date01' не найдено, взвод текущей даты", "task", d.curTemplate)
	}

	// Передаем d.Name вместо IP-адреса. NiceLabel Automation подхватит именно системное имя принтера
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
</LABEL>`, d.Name, d.curTemplate, staticDate)

	resp, err := http.Post(d.NiceLabelURL, "application/xml", bytes.NewBufferString(xmlPayload))
	if err != nil {
		return fmt.Errorf("ошибка отправки XML в триггер NiceLabel: %w", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("NiceLabel Automation вернул некорректный статус: %d", resp.StatusCode)
	}

	// Оставляем технологическую паузу, чтобы файлы гарантированно записались на флешку принтера
	time.Sleep(2 * time.Second)

	//// Шаг 4: Монопольный перехват и удержание RAW-сокета драйвером «РОВНО»
	//d.mu.Lock()
	//conn, err := net.DialTimeout("tcp", addr, d.Timeout)
	//if err != nil {
	//	d.mu.Unlock()
	//	return fmt.Errorf("ошибка монопольного перехвата порта 9100: %w", err)
	//}
	//d.conn = conn
	//
	//// Настраиваем системные Keep-Alive, чтобы линк не засыпал при простоях конвейера GEA
	//if tcpConn, ok := d.conn.(*net.TCPConn); ok {
	//	_ = tcpConn.SetKeepAlive(true)
	//	_ = tcpConn.SetKeepAlivePeriod(10 * time.Second)
	//}
	//
	//// Шаг 5: Вызов отрендеренного Найсом макета из флеш-памяти в текущее ОЗУ термоголовки
	//cmdLoadLayout := fmt.Sprintf("%cFMA---rA:\\Standard\\%s%c", SOH, d.curTemplate, ETB)
	//if _, err := d.conn.Write([]byte(cmdLoadLayout)); err != nil {
	//	d.closeConnNoLock()
	//	d.mu.Unlock()
	//	return fmt.Errorf("ошибка активации макета из памяти принтера: %w", err)
	//}
	//d.mu.Unlock()
	//
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

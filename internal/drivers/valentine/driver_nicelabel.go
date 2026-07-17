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

func NewNiceLabelDriver(ip string, port int) *NiceLabelDriver {
	return &NiceLabelDriver{
		Address:      ip,
		Port:         port,
		Timeout:      3 * time.Second,
		NiceLabelURL: "http://srv205:10000/", // Временный хардкод HTTP-триггера
		lastCount:    -1,
		stopPumping:  make(chan struct{}),
	}
}

// --------------------------------───────────────────────────────────────
// КОНТУР А: ПОДГОТОВКА И ЗАПУСК СЕССИИ (INIT & HANDSHAKE)
// --------------------------------───────────────────────────────────────

// InitSession реализует блок подготовки железа и вызова NiceLabel триггера
func (d *NiceLabelDriver) InitSession(fieldName string, maxQueue int, staticFields map[string]string) error {
	d.mu.Lock()
	slog.Info("VALENTIN-NICE: Инициализация сессии", "plu_template", d.curTemplate)

	// 1. Гарантированно рвем старую монопольную сессию
	if d.conn != nil {
		d.conn.Close()
		d.conn = nil
	}
	d.mu.Unlock() // Освобождаем мьютекс только после закрытия

	// 2. Короткий коннект для зачистки SD-карты
	addr := net.JoinHostPort(d.Address, strconv.Itoa(d.Port))
	cleanupConn, err := net.DialTimeout("tcp", addr, d.Timeout)
	if err != nil {
		return fmt.Errorf("ошибка подключения для зачистки SD-карты: %w", err)
	}

	// Взводим жесткий дедлайн на запись команд зачистки, чтобы не зависнуть тут навсегда
	cleanupConn.SetWriteDeadline(time.Now().Add(2 * time.Second))

	cmdDelPrn := fmt.Sprintf("%cFMD---rA:\\Standard\\%s.prn%c", SOH, d.curTemplate, ETB)
	cmdDelPcx := fmt.Sprintf("%cFMD---rA:\\Standard\\%s.pcx%c", SOH, d.curTemplate, ETB)
	cmdClearQ := fmt.Sprintf("%c%c%c%c", SOH, ESC, 'C', ETB)

	_, _ = cleanupConn.Write([]byte(cmdDelPrn + cmdDelPcx + cmdClearQ))
	cleanupConn.Close() // И СРАЗУ ЗАКРЫВАЕМ ФИЗИЧЕСКИ

	// Увеличиваем паузу до 300мс, чтобы сетевой стек принтера успел сбросить буферы
	time.Sleep(300 * time.Millisecond)

	// 3. Отправка XML-задания в HTTP-триггер NiceLabel Automation
	// Используем имя шаблона (d.curTemplate) как код PLU номенклатуры
	// Вытаскиваем дату из статических полей, присланных из 1С (ключ "date01")

	staticDate, ok := staticFields["date01"]
	if !ok {
		// Фоллбэк на случай, если 1С забыла прислать поле "date01"
		staticDate = time.Now().Format("02.01.2006")
		slog.Warn("VALENTIN-NICE: Поле 'date01' не найдено в статических полях, использована текущая дата", "task", d.curTemplate)
	}

	// Формируем XML-пакет. d.curTemplate здесь — это код PLU (например, "3225")
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
</LABEL>`, d.Address, d.curTemplate, staticDate)

	resp, err := http.Post(d.NiceLabelURL, "application/xml", bytes.NewBufferString(xmlPayload))
	if err != nil {
		return fmt.Errorf("ошибка отправки XML в триггер NiceLabel: %w", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("NiceLabel Automation вернул некорректный статус: %d", resp.StatusCode)
	}

	// Даем время NiceLabel отрендерить макет, залить файлы по сети и закрыть сокет
	time.Sleep(500 * time.Millisecond)

	// 4. Монопольный перехват RAW-сокета на порту 9100 драйвером «РОВНО»
	d.mu.Lock()
	conn, err := net.DialTimeout("tcp", addr, d.Timeout)
	if err != nil {
		d.mu.Unlock()
		return fmt.Errorf("ошибка монопольного перехвата порта 9100: %w", err)
	}
	d.conn = conn

	// 5. Вызов и активация созданного шаблона из памяти принтера в текущее ОЗУ
	cmdLoadLayout := fmt.Sprintf("%cFMA---rA:\\Standard\\%s%c", SOH, d.curTemplate, ETB)
	if _, err := d.conn.Write([]byte(cmdLoadLayout)); err != nil {
		d.closeConnNoLock()
		d.mu.Unlock()
		return fmt.Errorf("ошибка активации макета из памяти принтера: %w", err)
	}
	d.mu.Unlock()

	// Инициализируем стартовое значение счетчика перед запуском накачки
	initCountStr, err := d.GetCurrentPrintCount()
	if err == nil {
		if val, errConv := strconv.Atoi(initCountStr); errConv == nil {
			d.lastCount = val
		}
	}

	slog.Info("VALENTIN-NICE: Принтер успешно захвачен и готов к накачке", "ip", d.Address)
	return nil
}

// ----------------------------------------------------------------───────
// КОНТУР Б: РЕАЛТАЙМ-НАКАЧКА КОДОВ (PUMPER & MONITORING LOOP)
// --------------------------------───────────────────────────────────────

// PrintBatchIndexed реализует логику реалтайм-инъекции по аппаратному датчику
func (d *NiceLabelDriver) PrintBatchIndexed(fieldName string, startIndex int, codes []string) (int, error) {
	d.mu.Lock()
	if d.isPumping {
		d.mu.Unlock()
		return 0, nil // Насос уже работает в фоновой горутине, исключаем наложение
	}
	d.isPumping = true
	d.mu.Unlock()

	successCount := 0
	totalCodes := len(codes)

	slog.Info("VALENTIN-NICE: Запуск цикла поштучной инъекции по датчику", "count", totalCodes)

	// Переходим в микроцикл опроса регистра с шагом в 10мс
	for successCount < totalCodes {
		select {
		case <-d.stopPumping:
			slog.Info("VALENTIN-NICE: Сигнал останова получен, завершаем накачку")
			d.mu.Lock()
			d.isPumping = false
			d.mu.Unlock()
			return successCount, nil
		default:
			// ПРОВЕРКА И РЕКОННЕКТ ПЕРЕД ОПРОСОМ
			d.mu.Lock()
			if d.conn == nil {
				err := d.reconnectNoLock()
				if err != nil {
					d.mu.Unlock()
					slog.Error("VALENTIN-NICE: Сбой автореконнекта, принтер недоступен", "err", err)
					time.Sleep(1 * time.Second) // Даем сети отдохнуть
					continue
				}
			}
			d.mu.Unlock()

			// Читаем текущее значение счетчика печатных циклов FBBC
			countStr, err := d.GetCurrentPrintCount()
			if err != nil {
				slog.Error("VALENTIN-NICE IO Error: Сбой опроса счетчика, сброс сокета...", "err", err)
				// Важно: GetCurrentPrintCount сам обнулит d.conn при ошибке I/O,
				// поэтому на следующем круге сработает наш reconnectNoLock
				time.Sleep(1 * time.Second)
				continue
			}

			currentCount, errConv := strconv.Atoi(countStr)
			if errConv != nil {
				time.Sleep(10 * time.Millisecond)
				continue // Мусор в сокете, пропускаем итерацию
			}

			// Если это самый первый запуск — фиксируем точку отсчета
			if d.lastCount == -1 {
				d.lastCount = currentCount
			}

			// Проверка изменения счетчика (факт физической печати по датчику конвейера)
			// Учитываем аппаратное переполнение регистра Valentin (переход с 99999 на 00000)
			if currentCount != d.lastCount {
				slog.Info("[ДАТЧИК GEA] Этикетка напечатана", "old_idx", d.lastCount, "new_idx", currentCount)

				// Извлекаем следующий код Честного Знака для нанесения
				currentCode := codes[successCount]

				// Экранируем управляющий символ GS1 для протокола Valentin
				cleanCode := strings.ReplaceAll(currentCode, "\x1d", "~1")

				// Сборка атомарного пакета инъекции в один TCP-фрейм
				var packet bytes.Buffer

				// Поле №19 (ШК) захардкожено согласно спецификации макета NiceLabel
				packet.WriteByte(SOH)
				packet.WriteString("FD----r0") // Блокировка кадра для атомарного обновления RAM
				packet.WriteByte(ETB)

				packet.WriteByte(SOH)
				packet.WriteString(fmt.Sprintf("BM[19]%s", cleanCode)) // Инъекция данных Честного Знака
				packet.WriteByte(ETB)

				packet.WriteByte(SOH)
				packet.WriteString("FD----r1") // Разблокировка кадра
				packet.WriteByte(ETB)

				packet.WriteByte(SOH)
				packet.WriteString("FBC---r--------") // Взвод триггера готовности следующего цикла печати
				packet.WriteByte(ETB)

				d.mu.Lock()
				if d.conn != nil {
					_, err = d.conn.Write(packet.Bytes())
				} else {
					err = fmt.Errorf("соединение потеряно в момент инъекции")
				}
				d.mu.Unlock()

				if err != nil {
					slog.Error("VALENTIN-NICE: Критическая ошибка инъекции кода в сокет", "err", err)
					time.Sleep(1 * time.Second)
					continue
				}

				// Фиксируем новое состояние счетчика и инкрементируем индекс успешно переданных в буфер кодов
				d.lastCount = currentCount
				successCount++
			}

			// Твоя проверенная задержка микроцикла поллера
			time.Sleep(10 * time.Millisecond)
		}
	}

	d.mu.Lock()
	d.isPumping = false
	d.mu.Unlock()

	return successCount, nil
}

// GetCurrentPrintCount осуществляет низкоуровневое чтение аппаратного регистра счетчика FBBC
func (d *NiceLabelDriver) GetCurrentPrintCount() (string, error) {
	d.mu.Lock()
	if d.conn == nil {
		d.mu.Unlock()
		return "", fmt.Errorf("сокет принтера закрыт")
	}

	d.conn.SetReadDeadline(time.Now().Add(d.Timeout))
	cmd := fmt.Sprintf("%cFBBC--w%c", SOH, ETB)

	if _, err := d.conn.Write([]byte(cmd)); err != nil {
		d.closeConnNoLock()
		d.mu.Unlock()
		return "", err
	}

	reader := bufio.NewReader(d.conn)
	d.mu.Unlock()

	// Ожидаем ответ в формате фрейма, ограниченного байтом ETB
	respBytes, err := reader.ReadBytes(ETB)
	if err != nil {
		d.mu.Lock()
		d.closeConnNoLock()
		d.mu.Unlock()
		return "", err
	}

	cleanResp := strings.Trim(string(respBytes), string([]byte{SOH, ETB, CR, LF}))

	// Извлекаем числовую часть из ответа вида "A00150"
	if strings.HasPrefix(cleanResp, "A") {
		parts := strings.Split(cleanResp, "A")
		if len(parts) > 1 {
			return strings.TrimSpace(parts[1]), nil
		}
	}

	return "", fmt.Errorf("получен некорректный ответ счетчика: %s", cleanResp)
}

// ----------------------------------------------------------------───────
// ВСПОМОГАТЕЛЬНЫЕ МЕТОДЫ И ИНТЕРФЕЙСНЫЕ ФУНКЦИИ
// --------------------------------───────────────────────────────────────

func (d *NiceLabelDriver) SelectTemplate(template string, fields map[string]string) error {
	d.curTemplate = template // Сохраняем имя шаблона (код PLU) во внутреннее состояние драйвера
	return nil
}

func (d *NiceLabelDriver) ClearQueue() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.conn == nil {
		return nil
	}
	packet := []byte{SOH, ESC, 'C', ETB} // Реалтайм Escape-команда мгновенной очистки буфера
	_, err := d.conn.Write(packet)
	return err
}

func (d *NiceLabelDriver) closeConnNoLock() {
	if d.conn != nil {
		d.conn.Close()
		d.conn = nil
	}
}

func (d *NiceLabelDriver) GetBufferFreeSpace() (int, error) {
	// Для логики реалтайм-накачки возвращаем фиксированный размер порции
	return 1, nil
}

func (d *NiceLabelDriver) GetLastPrintedIndex() (int, error) {
	return d.lastCount, nil
}

func (d *NiceLabelDriver) reconnectNoLock() error {
	if d.conn != nil {
		return nil // Соединение живое
	}

	addr := net.JoinHostPort(d.Address, strconv.Itoa(d.Port))
	conn, err := net.DialTimeout("tcp", addr, d.Timeout)
	if err != nil {
		return err
	}

	d.conn = conn

	// Взводим TCP KeepAlive на уровне ОС, чтобы сеть не дропала молчащие сессии
	if tcpConn, ok := d.conn.(*net.TCPConn); ok {
		_ = tcpConn.SetKeepAlive(true)
		_ = tcpConn.SetKeepAlivePeriod(10 * time.Second)
	}

	slog.Info("VALENTIN-NICE: Монопольный сокет 9100 успешно восстановлен в цикле накачки", "ip", d.Address)
	return nil
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
func (d *NiceLabelDriver) GetStatus() (string, error) {
	return "ГОТОВ", nil
}

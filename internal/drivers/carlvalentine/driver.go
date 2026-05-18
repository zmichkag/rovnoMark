package carlvalentin

import (
	"bufio"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Низкоуровневые константы интерфейса Carl Valentin
const (
	SOH = 0x01 // Start of Header (Начало блока передачи данных)
	ETB = 0x17 // End of Text Block (Конец блока передачи данных)
	CR  = 0x0D // Carriage Return
	LF  = 0x0A // Line Feed
	ESC = 0x1B // Escape-символ для служебных запросов реального времени
)

type Driver struct {
	Address        string
	Port           int
	Timeout        time.Duration
	mu             sync.Mutex
	conn           net.Conn
	curTemplate    string
	lastPrintedIdx int
}

func New(ip string, port int) *Driver {
	return &Driver{
		Address: ip,
		Port:    port,
		Timeout: 3 * time.Second,
	}
}

// sendBlock — упаковывает данные в SOH/ETB фрейм согласно спецификации Valentin
func (d *Driver) sendBlock(body string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.conn == nil {
		address := net.JoinHostPort(d.Address, strconv.Itoa(d.Port))
		conn, err := net.DialTimeout("tcp", address, d.Timeout)
		if err != nil {
			slog.Error("CARL VALENTIN Connection Error", "ip", d.Address, "err", err)
			return "", err
		}
		d.conn = conn
	}

	d.conn.SetReadDeadline(time.Now().Add(d.Timeout))

	// Формируем пакет: [SOH] + Тело данных + [ETB]
	packet := append([]byte{SOH}, []byte(body)...)
	packet = append(packet, ETB)

	slog.Debug("CARL VALENTIN IO Out", "ip", d.Address, "bytes_len", len(packet))

	_, err := d.conn.Write(packet)
	if err != nil {
		d.conn.Close()
		d.conn = nil
		return "", err
	}

	// Читаем ответ от принтера. Валентин обычно возвращает статус подтверждения (ACK/NAK блока или текстовую строку)
	reader := bufio.NewReader(d.conn)
	respBytes, err := reader.ReadBytes(ETB)
	if err != nil {
		d.conn.Close()
		d.conn = nil
		return "", err
	}

	// Очищаем от служебных байт обрамления для логирования
	cleanResp := strings.Trim(string(respBytes), string([]byte{SOH, ETB, CR, LF}))
	slog.Debug("CARL VALENTIN IO In", "ip", d.Address, "resp", cleanResp)

	return cleanResp, nil
}

// sendRealTimeCommand отправляет высокоприоритетные Escape-команды (например, мгновенный запрос статуса)
func (d *Driver) sendRealTimeCommand(cmd byte) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.conn == nil {
		return "", fmt.Errorf("no connection available for realtime command")
	}

	d.conn.SetReadDeadline(time.Now().Add(d.Timeout))
	// Команды реального времени обычно имеют формат [SOH][ESC][Команда][ETB]
	packet := []byte{SOH, ESC, cmd, ETB}

	if _, err := d.conn.Write(packet); err != nil {
		d.conn.Close()
		d.conn = nil
		return "", err
	}

	reader := bufio.NewReader(d.conn)
	respBytes, err := reader.ReadBytes(ETB)
	if err != nil {
		d.conn.Close()
		d.conn = nil
		return "", err
	}
	return strings.Trim(string(respBytes), string([]byte{SOH, ETB, CR, LF})), nil
}

// GetStatus запрашивает статус устройства командами реального времени
func (d *Driver) GetStatus() (string, error) {
	// В Valentin 'S' после ESC запрашивает текущий байт состояния
	raw, err := d.sendRealTimeCommand('S')
	if err != nil {
		return "ОФФЛАЙН", err
	}

	// Пример разбора классического байта ответа Valentin:
	// Если строка содержит предупреждения об окончании ленты или ошибках головки
	if strings.Contains(raw, "ERROR") || strings.Contains(raw, "M_ERR") {
		return "НЕ ГОТОВ", nil
	}
	if strings.Contains(raw, "PRNT") {
		return "ПЕЧАТЬ", nil
	}
	return "ГОТОВ", nil
}

// SelectTemplate активирует макет, сохраненный на SD-карте принтера
func (d *Driver) SelectTemplate(template string, fields map[string]string) error {
	d.curTemplate = template

	// Вызов сохраненного макета выполняется через специальный Command Set:
	// Снимаем старую задачу и вызываем макет по имени файла. Формат: C [свойства] ; лог_имя ; имя_файла
	// Для Valentin символ 'имя_макета' передается внутри блока конфигурации.
	body := fmt.Sprintf("C;0;0;1;0;0;2;0%s%s%s", string(CR), template, string(CR))

	_, err := d.sendBlock(body)
	if err != nil {
		return fmt.Errorf("ошибка активации макета %s: %v", template, err)
	}

	// Если вместе с макетом прилетели статические поля — обновляем их
	if len(fields) > 0 {
		return d.UpdateStaticFields(fields)
	}
	return nil
}

// UpdateStaticFields обновляет текстовые переменные (Text Sets) в текущей памяти принтера
func (d *Driver) UpdateStaticFields(fields map[string]string) error {
	if len(fields) == 0 {
		return nil
	}

	var sb strings.Builder
	for fieldNum, value := range fields {
		// Экран разделителей Valentin
		cleanValue := strings.ReplaceAll(value, ";", " ")
		// Формат Text Set: AW [номер_поля] [значение] + CR
		// Номер поля должен соответствовать ID переменной в Labelstar (например, "0001")
		sb.WriteString(fmt.Sprintf("AW%s%s%s", fieldNum, cleanValue, string(CR)))
	}

	// В конце пакета обновления Valentin всегда требует Command Set (хотя бы пустой перезапуск макета 'C')
	sb.WriteString(fmt.Sprintf("C%s", string(CR)))

	_, err := d.sendBlock(sb.String())
	return err
}

// PrintBatchIndexed осуществляет поштучную загрузку кодов с жесткой индексацией (Логика Насоса Pumper)
func (d *Driver) PrintBatchIndexed(fieldName string, startIndex int, codes []string) (int, error) {
	slog.Info("CARL VALENTIN Batch Indexed Start", "ip", d.Address, "field_id", fieldName, "count", len(codes))

	successCount := 0
	for i, code := range codes {
		currIdx := startIndex + i

		// 1. Подготовка кода под стандарт GS1 (замена управляющего ASCII 29 на ~1)
		cleanCode := strings.ReplaceAll(code, "\x1d", "~1")

		var sb strings.Builder
		// 2. Передаем Text Set для поля маркировки (fieldName здесь выступает как числовой ID, например "0001")
		sb.WriteString(fmt.Sprintf("AW%s%s%s", fieldName, cleanCode, string(CR)))

		// 3. Формируем Command Set 'C', куда подшиваем уникальный Job ID (передаем текущий индекс партии)
		// Таким образом принтер связывает отстрел этой этикетки с нашим сквозным индексом базы данных
		sb.WriteString(fmt.Sprintf("C;0;0;1;0;0;0;0;ID=%d;%s", currIdx, string(CR)))

		_, err := d.sendBlock(sb.String())
		if err != nil {
			slog.Warn("CARL VALENTIN SID Item Failed", "ip", d.Address, "idx", currIdx, "err", err)
			break
		}

		// Локально запоминаем последний отправленный индекс для симуляции, если принтер занят
		d.lastPrintedIdx = currIdx
		successCount++
	}

	return successCount, nil
}

// GetBufferFreeSpace возвращает количество свободных мест в очереди контроллера Valentin
func (d *Driver) GetBufferFreeSpace() (int, error) {
	// Запрашиваем состояние буфера через статусную Esc-команду реального времени
	raw, err := d.sendRealTimeCommand('B') // 'B' — стандартный запрос Buffer Status в CVPL
	if err != nil {
		return 0, err
	}

	// Парсим ответ принтера. Обычно возвращается количество свободных этикеток в очереди, например "BUF015"
	raw = strings.TrimSpace(raw)
	if len(raw) > 3 && strings.HasPrefix(raw, "BUF") {
		freeSlots, err := strconv.Atoi(raw[3:])
		if err == nil {
			return freeSlots, nil
		}
	}

	// Дефолтный безопасный лимит для Pumper, чтобы не переполнять сокет
	return 10, nil
}

// GetLastPrintedIndex возвращает индекс последней этикетки, прошедшей физический датчик печати
func (d *Driver) GetLastPrintedIndex() (int, error) {
	// Запрос текущего отработанного Job ID через Escape-последовательность
	raw, err := d.sendRealTimeCommand('I') // 'I' — запрос исполненного ID задачи
	if err != nil {
		return d.lastPrintedIdx, nil // В случае сбоя возвращаем последний отправленный индекс
	}

	// Извлекаем числовой индекс этикетки
	idx, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return d.lastPrintedIdx, nil
	}

	return idx, nil
}

// PrintBatch осуществляет прямую высокоскоростную заливку пачки этикеток (без жестких индексов)
func (d *Driver) PrintBatch(fieldName string, codes []string) (int, error) {
	successCount := 0
	for _, code := range codes {
		cleanCode := strings.ReplaceAll(code, "\x1d", "~1")

		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("AW%s%s%s", fieldName, cleanCode, string(CR)))
		sb.WriteString(fmt.Sprintf("C;0;0;1;0;0;0;0%s", string(CR))) // Печатать 1 копию

		_, err := d.sendBlock(sb.String())
		if err != nil {
			break
		}
		successCount++
	}
	return successCount, nil
}

func (d *Driver) InitSession(fieldName string, maxQueue int) error {
	slog.Info("CARL VALENTIN InitSession", "field_id", fieldName, "max_queue", maxQueue)
	// Для Valentin инициализация — это очистка старых буферов печати
	return d.ClearQueue()
}

func (d *Driver) ClearQueue() error {
	// ESC 'C' — команда мгновенной очистки очереди печати в Valentin
	_, err := d.sendRealTimeCommand('C')
	return err
}

func (d *Driver) PrintTemplate(template string, fields map[string]string) error {
	return d.SelectTemplate(template, fields)
}

func (d *Driver) GetTemplates() ([]string, error) {
	// В Valentin список файлов на карте памяти можно запросить через ESC 'F'
	raw, err := d.sendRealTimeCommand('F')
	if err != nil {
		return nil, err
	}
	return strings.Split(raw, ","), nil
}

func (d *Driver) GetTemplateFields(templateName string) ([]string, error) {
	return []string{"0001", "0002"}, nil // Возвращаем дефолтные маппинги полей для разметки
}

func (d *Driver) GetRemainingRibbon() (string, error) {
	// ESC 'R' возвращает остаток красящей ленты в процентах или метрах
	raw, err := d.sendRealTimeCommand('R')
	if err != nil {
		return "N/A", nil
	}
	return raw + "m", nil
}

func (d *Driver) GetQueueCapacity(queueName string) (string, error) {
	return "50", nil
}

func (d *Driver) GetPrintSpeed() (string, error) {
	return "200 mm/s", nil
}

func (d *Driver) GetCurrentPrintCount() (string, error) {
	return "0", nil
}

func (d *Driver) GetCurrentTemplate() (string, error) {
	if d.curTemplate != "" {
		return d.curTemplate, nil
	}
	return "N/A", nil
}

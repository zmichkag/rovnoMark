package savema

import (
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Driver struct {
	Address string
	Port    int
	Timeout time.Duration
	mu      sync.Mutex
}

func (d *Driver) SelectTemplate(template string, fields map[string]string) error {
	// Пока здесь остается заглушка для будущего протокола Savema,
	// но сигнатура теперь соответствует интерфейсу Printer
	slog.Debug("SAVEMA: Выбор шаблона (заглушка)", "template", template, "fields_count", len(fields))
	return nil
}

// InitSession подготавливает принтер Savema к серийной печати
func (d *Driver) InitSession(fieldName string, maxQueue int) error {
	slog.Info("SAVEMA: Инициализация сессии маркировки", "field", fieldName, "max_queue", maxQueue)
	// Для Савемы обычно перед началом новой партии очищают буфер
	return d.ClearQueue()
}

func (d *Driver) UpdateStaticFields(fields map[string]string) error {
	//TODO implement me
	panic("implement me")
}

// ClearQueue очищает текущую очередь печати в памяти принтера
func (d *Driver) ClearQueue() error {
	// Команда SPLCQD очищает буфер динамических данных очереди в SPPL
	_, err := d.sendRaw("SPLCQD")
	return err
}

// GetBufferFreeSpace возвращает количество доступных слотов буфера
func (d *Driver) GetBufferFreeSpace() (int, error) {
	// Запрашиваем статус буфера очереди (команда SPLGQS - Get Queue Status)
	raw, err := d.sendRaw("SPLGQS")
	if err != nil {
		return 0, err
	}

	clean := CleanResponse(raw) // Вытаскиваем ответ из обертки ~...^

	// Савема на SPLGQS обычно возвращает строку вида "BUSY:5,FREE:15" или просто число свободных слотов.
	// Если протокол Rev.12 возвращает просто число доступных слотов:
	if freeSlots, errParse := strconv.Atoi(clean); errParse == nil {
		return freeSlots, nil
	}

	// Защитная логика: если принтер в сети, но протокол отдал нечитаемый кастом,
	// возвращаем дефолтные 10 слотов, чтобы насос Pumper не блокировал линию.
	return 10, nil
}

func (d *Driver) GetTemplateFields(templateName string) ([]string, error) {
	return []string{"DataMatrix", "Text01"}, nil
}

func (d *Driver) GetTemplates() ([]string, error) {
	return []string{"CZDM.rox"}, nil
}

// PrintBatchIndexed отправляет пачку кодов с привязкой к стартовому индексу
func (d *Driver) PrintBatchIndexed(fieldName string, startIndex int, codes []string) (int, error) {
	slog.Info("SAVEMA: Загрузка индексированной пачки кодов", "field", fieldName, "start_idx", startIndex, "count", len(codes))

	// Собираем пачку кодов. В Савеме разделителем в SPLAQD обычно выступает перевод строки \n
	allCodes := strings.Join(codes, "\n")

	// Передаем команду добавления кодов в очередь вместе со стартовым индексом
	// Формат команды: SPLAQD{имя_поля~gt~индекс~gt~коды}
	cmd := fmt.Sprintf("SPLAQD{%s~gt~%d~gt~%s}", fieldName, startIndex, allCodes)

	resp, err := d.sendRaw(cmd)
	if err != nil {
		return 0, err
	}

	if strings.Contains(resp, "OK") {
		return len(codes), nil
	}

	return 0, fmt.Errorf("принтер Savema отклонил пачку: %s", resp)
}

// GetLastPrintedIndex возвращает индекс последнего отпечатанного кода
func (d *Driver) GetLastPrintedIndex() (int, error) {
	raw, err := d.sendRaw("SPLGPL") // Get Printed Index/Label
	if err != nil {
		return 0, err
	}
	clean := CleanResponse(raw)
	idx, err := strconv.Atoi(clean)
	if err != nil {
		return 0, nil // Еще ничего не напечатано
	}
	return idx, nil
}

func New(ip string, port int) *Driver {
	return &Driver{
		Address: ip,
		Port:    port,
		Timeout: 3 * time.Second,
	}
}

// sendRaw — базовый метод обмена данными[cite: 1]
func (d *Driver) sendRaw(cmd string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	address := net.JoinHostPort(d.Address, strconv.Itoa(d.Port))
	conn, err := net.DialTimeout("tcp", address, d.Timeout)
	if err != nil {
		return "", err
	}
	defer conn.Close()

	// Оборачиваем команду в спецсимволы Savema[cite: 1]
	fmt.Fprintf(conn, "~%s^", cmd)

	//	log.Printf("[SAVEMA %s] ", d.Address)

	conn.SetReadDeadline(time.Now().Add(d.Timeout))
	reader := make([]byte, 1024)
	reply, err := conn.Read(reader)
	if err != nil {
		return "", err
	}
	slog.Debug("SAVEMA IO",
		"ip", d.Address,
		"send", cmd,
		"reply", string(reader[:reply]),
	)
	return string(reader[:reply]), nil
}

// PrintBatch — загрузка очереди Честного Знака[cite: 1]
func (d *Driver) PrintBatch(fieldName string, codes []string) (int, error) {
	allCodes := strings.Join(codes, "\n")
	// Команда SPLAQD добавляет данные в очередь[cite: 1]
	cmd := fmt.Sprintf("SPLAQD{%s~gt~%s}", fieldName, allCodes)
	resp, err := d.sendRaw(cmd)
	if err != nil {
		return 0, err
	}
	if strings.Contains(resp, "OK") {
		return len(codes), nil
	}
	return 0, fmt.Errorf("принтер не принял пачку: %s", resp)
}

func (d *Driver) PrintTemplate(template string, fields map[string]string) error {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("SPLLTF{%s}", template))
	// Используем разделитель | для отправки нескольких команд за раз[cite: 1]
	for k, v := range fields {
		sb.WriteString(fmt.Sprintf("|SPMCTV{%s~gt~%s}", k, v))
	}
	_, err := d.sendRaw(sb.String())
	return err
}

func (d *Driver) GetStatus() (string, error) {
	raw, err := d.sendRaw("SPPSTA")
	if err != nil {
		return "", err
	}
	clean := CleanResponse(raw)
	// Маппинг статусов согласно документации Rev.12[cite: 1]
	switch strings.ToUpper(clean) {
	case "WAITING":
		return "ГОТОВ", nil
	case "RUNNING":
		return "ПЕЧАТЬ", nil
	case "INIT":
		return "ЗАПУСК", nil
	case "ERROR":
		return "ОШИБКА", nil
	default:
		return clean, nil
	}
}

func CleanResponse(raw string) string {
	clean := strings.TrimSpace(raw)
	start := strings.Index(clean, ":")
	end := strings.LastIndex(clean, "}")
	if start != -1 && end != -1 && end > start {
		return strings.TrimSpace(clean[start+1 : end])
	}
	return clean
}

// Заглушки для интерфейса Printer[cite: 4]
func (d *Driver) GetRemainingRibbon() (string, error) {
	r, e := d.sendRaw("SPGGRR")
	return CleanResponse(r), e
}
func (d *Driver) GetQueueCapacity(q string) (string, error) {
	r, e := d.sendRaw(fmt.Sprintf("SPLGQC{%s}", q))
	return CleanResponse(r), e
}
func (d *Driver) GetPrintSpeed() (string, error) {
	r, e := d.sendRaw("SPCGPS")
	return CleanResponse(r), e
}
func (d *Driver) GetCurrentPrintCount() (string, error) {
	r, e := d.sendRaw("SPGGCP")
	return CleanResponse(r), e
}
func (d *Driver) GetCurrentTemplate() (string, error) {
	r, e := d.sendRaw("SPLGAT")
	return CleanResponse(r), e
}

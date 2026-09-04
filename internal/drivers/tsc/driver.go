package tsc

import (
	"bytes"
	"fmt"
	"log/slog"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"
)

type Driver struct {
	Address     string
	Port        int
	Timeout     time.Duration
	mu          sync.Mutex
	currstate   string
	CurTemplate string
	conn        net.Conn

	// Хранилище загруженного в память драйвера сырого текста шаблона
	currentTemplateBody string
}

func New(ip string, port int) *Driver {
	return &Driver{
		Address: ip,
		Port:    port,
		Timeout: 3 * time.Second,
	}
}

// sendRaw — низкоуровневый обмен данными с принтером
func (d *Driver) sendRaw(cmd string, waitReply bool) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.conn == nil {
		address := net.JoinHostPort(d.Address, strconv.Itoa(d.Port))
		conn, err := net.DialTimeout("tcp", address, d.Timeout)
		if err != nil {
			slog.Error("TSC Connect Error", "ip", d.Address, "err", err)
			return "", err
		}
		d.conn = conn
	}

	d.conn.SetReadDeadline(time.Now().Add(d.Timeout))
	slog.Debug("TSC IO Out", "ip", d.Address, "cmd", cmd)

	_, err := d.conn.Write([]byte(cmd + "\r\n"))
	if err != nil {
		d.closeConn()
		return "", err
	}

	if !waitReply {
		return "", nil
	}

	buf := make([]byte, 10)
	n, err := d.conn.Read(buf)
	if err != nil {
		d.closeConn()
		return "", err
	}

	reply := string(buf[:n])
	slog.Debug("TSC IO In", "ip", d.Address, "resp", reply)
	return reply, nil
}

func (d *Driver) closeConn() {
	if d.conn != nil {
		d.conn.Close()
		d.conn = nil
	}
}

func (d *Driver) sendRawBytes(payload []byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.conn == nil {
		address := net.JoinHostPort(d.Address, strconv.Itoa(d.Port))
		conn, err := net.DialTimeout("tcp", address, d.Timeout)
		if err != nil {
			slog.Error("TSC Connect Error", "ip", d.Address, "err", err)
			return err
		}
		d.conn = conn
	}

	d.conn.SetReadDeadline(time.Now().Add(d.Timeout))
	_, err := d.conn.Write(payload)
	if err != nil {
		slog.Error("TSC Socket Write Error", "ip", d.Address, "err", err)
		d.closeConn()
		return err
	}
	return nil
}

func (d *Driver) GetStatus() (string, error) {
	resp, err := d.sendRaw("\x1b!?", true)
	if err != nil {
		return "ОШИБКА СВЯЗИ", err
	}
	if len(resp) == 0 {
		return "НЕИЗВЕСТНО", nil
	}

	statusByte := resp[0]
	d.currstate = fmt.Sprintf("%d", statusByte)

	if statusByte == 0 {
		return "ГОТОВ", nil
	}
	if (statusByte & 0x40) != 0 {
		return "ПЕЧАТЬ", nil
	}
	if (statusByte & 0x20) != 0 {
		return "ПАУЗА", nil
	}
	if (statusByte&0x08) != 0 || (statusByte&0x10) != 0 {
		return "НЕ ГОТОВ (НЕТ МАТЕРИАЛА)", nil
	}
	if (statusByte & 0x02) != 0 {
		return "НЕ ГОТОВ (ОТКРЫТА КРЫШКА)", nil
	}

	return "ГОТОВ", nil
}

// SelectTemplate принимает имя макета. Физически загружает его тело из СИСТЕМЫ (или БД через менеджер)
// Для TSC мы сохраняем тело шаблона в памяти драйвера, чтобы потом наполнять его данными
func (d *Driver) SelectTemplate(name string, fields map[string]string) error {
	d.CurTemplate = name

	// Если в метод сразу переданы статические поля, выполняем рендеринг и печать
	if len(fields) > 0 && d.currentTemplateBody != "" {
		return d.PrintTemplate(d.currentTemplateBody, fields)
	}
	return nil
}

// SetTemplateBody — кастомный метод для TSC/Savema, позволяющий менеджеру передать сырой текст TSPL-шаблона из БД в драйвер
func (d *Driver) SetTemplateBody(body string) {
	d.currentTemplateBody = body
}

// PrintTemplate выполняет подстановку переменных (Go template) в сырой TSPL макет и отправляет в принтер
func (d *Driver) PrintTemplate(templateBlob string, fields map[string]string) error {
	t, err := template.New("tsc_label").Parse(templateBlob)
	if err != nil {
		slog.Error("TSC TSPL Parsing Error", "ip", d.Address, "err", err)
		return fmt.Errorf("ошибка структуры TSPL-макета: %w", err)
	}

	var buf bytes.Buffer
	if err := t.Execute(&buf, fields); err != nil {
		slog.Error("TSC TSPL Execution Error", "ip", d.Address, "err", err)
		return fmt.Errorf("ошибка подстановки данных: %w", err)
	}

	finalPayload := buf.Bytes()
	if !bytes.HasSuffix(finalPayload, []byte("\r\n")) {
		finalPayload = append(finalPayload, []byte("\r\n")...)
	}

	return d.sendRawBytes(finalPayload)
}

// GetTemplateFields парсит текст TSPL шаблона регулярным выражением, находя все теги {{.Имя_Поля}}
func (d *Driver) GetTemplateFields(templateName string) ([]string, error) {
	if d.currentTemplateBody == "" {
		return []string{"DataMatrix", "Text"}, nil // Дефолтный фоллбэк
	}

	// Ищем паттерны вида {{.FieldName}}
	re := regexp.MustCompile(`\{\{\s*\.([a-zA-Z0-9_\-]+)\s*\}\}`)
	matches := re.FindAllStringSubmatch(d.currentTemplateBody, -1)

	var fields []string
	seen := make(map[string]bool)

	for _, match := range matches {
		if len(match) > 1 {
			fieldName := match[1]
			if !seen[fieldName] {
				seen[fieldName] = true
				fields = append(fields, fieldName)
			}
		}
	}

	return fields, nil
}

// PrintBatchIndexed интегрирует работу Насоса (Pumper) с TSPL шаблонами
func (d *Driver) PrintBatchIndexed(fieldName string, startIndex int, codes []string) (int, error) {
	if d.currentTemplateBody == "" {
		// Если макет из БД не загружен, используем жестко зашитый аварийный макет
		return d.PrintBatch(fieldName, codes)
	}

	successCount := 0
	for _, code := range codes {
		// Подготовка кодов для маркировки "Честный Знак" внутри TSPL
		cleanCode := strings.ReplaceAll(code, "\x1d", "{FNC1}")

		// Собираем мапу переменных для конкретной этикетки в пачке
		fields := map[string]string{
			fieldName:   cleanCode,                               // Динамическое поле (например, {{.DataMatrix}})
			"INDEX":     strconv.Itoa(startIndex + successCount), // Сквозной индекс для контроля геометрии партии
			"TIMESTAMP": time.Now().Format("15:04:05"),
		}

		// Рендерим и отправляем текущую этикетку
		err := d.PrintTemplate(d.currentTemplateBody, fields)
		if err != nil {
			slog.Error("TSC PrintBatchIndexed Item Failed", "ip", d.Address, "err", err)
			break
		}
		successCount++
	}

	return successCount, nil
}

func (d *Driver) PrintBatch(fieldName string, codes []string) (int, error) {
	successCount := 0
	for _, code := range codes {
		var sb strings.Builder
		sb.WriteString("CLS\r\n")
		cleanCode := strings.ReplaceAll(code, "\x1d", "{FNC1}")
		sb.WriteString(fmt.Sprintf("DMATRIX 50,50,400,400,\"%s\"\r\n", cleanCode))
		sb.WriteString(fmt.Sprintf("TEXT 50,460,\"3\",0,1,1,\"%s\"\r\n", fieldName))
		sb.WriteString("PRINT 1,1\r\n")

		_, err := d.sendRaw(sb.String(), false)
		if err != nil {
			slog.Error("TSC PrintBatch Failed", "ip", d.Address, "err", err)
			break
		}
		successCount++
	}
	return successCount, nil
}

// Фиктивный метод — TSC не возвращает список файлов по сети прозрачно, отдаем виртуальное имя из системы
func (d *Driver) GetTemplates() ([]string, error) {
	if d.CurTemplate != "" {
		return []string{d.CurTemplate}, nil
	}
	return []string{"TSPL_БД_МАКЕТ"}, nil
}

func (d *Driver) ClearQueue() error {
	_, err := d.sendRaw("CLS", false)
	return err
}

func (d *Driver) GetBufferFreeSpace() (int, error)                  { return 10, nil }
func (d *Driver) UpdateStaticFields(fields map[string]string) error { return nil }
func (d *Driver) InitSession(fieldName string, maxQueue int) error  { return d.ClearQueue() }
func (d *Driver) GetRemainingRibbon() (string, error)               { return "N/A", nil }
func (d *Driver) GetPrintSpeed() (string, error)                    { return "N/A", nil }
func (d *Driver) GetCurrentPrintCount() (string, error)             { return "N/A", nil }
func (d *Driver) GetLastPrintedIndex() (int, error)                 { return 0, nil }
func (d *Driver) GetQueueCapacity(queueName string) (string, error) { return "N/A", nil }

func (d *Driver) GetCurrentTemplate() (string, error) {
	if d.CurTemplate == "" {
		return "N/A", nil
	}
	return d.CurTemplate, nil
}

func (d *Driver) GetTotalPrints() (int64, error) {
	return 0, nil
}

package savema

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultTimeout = 3 * time.Second
	dataCsvName    = "mark_code.csv"
)

// Driver реализует промышленный драйвер Savema SPPL Rev.12 для ядра RovnoMark.
type Driver struct {
	Address string
	Port    int
	Timeout time.Duration

	mu   sync.Mutex // Защищает сетевые операции и сокет
	conn net.Conn   // Постоянная монопольная TCP-сессия

	stateMu      sync.RWMutex
	curTemplate  string
	batchSize    int
	lastCount    int
	staticFields map[string]string
}

func New(ip string, port int) *Driver {
	if port <= 0 {
		port = 9100
	}
	return &Driver{
		Address:      ip,
		Port:         port,
		Timeout:      defaultTimeout,
		staticFields: make(map[string]string),
	}
}

// ============================================================================
// Низкоуровневый сетевой слой (Persistent TCP Connection)
// ============================================================================

func (d *Driver) ensureConnectionLocked() error {
	if d.conn != nil {
		return nil
	}

	address := net.JoinHostPort(d.Address, strconv.Itoa(d.Port))
	conn, err := net.DialTimeout("tcp", address, d.Timeout)
	if err != nil {
		return fmt.Errorf("SAVEMA: ошибка подключения к %s: %w", address, err)
	}

	if tcpConn, ok := conn.(*net.TCPConn); ok {
		_ = tcpConn.SetNoDelay(true)
		_ = tcpConn.SetKeepAlive(true)
		_ = tcpConn.SetKeepAlivePeriod(10 * time.Second)
	}

	d.conn = conn
	return nil
}

func (d *Driver) closeConnectionLocked() {
	if d.conn != nil {
		_ = d.conn.Close()
		d.conn = nil
	}
}

func (d *Driver) sendRaw(cmd string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	cmd = strings.TrimSpace(cmd)
	if !strings.HasPrefix(cmd, "~") {
		cmd = "~" + cmd
	}
	if !strings.HasSuffix(cmd, "^") {
		cmd = cmd + "^"
	}

	for attempt := 1; attempt <= 2; attempt++ {
		if err := d.ensureConnectionLocked(); err != nil {
			if attempt == 2 {
				return "", err
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}

		_ = d.conn.SetDeadline(time.Now().Add(d.Timeout))

		if _, err := io.WriteString(d.conn, cmd); err != nil {
			d.closeConnectionLocked()
			if attempt == 2 {
				return "", fmt.Errorf("SAVEMA: сбой записи в сокет: %w", err)
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}

		var result bytes.Buffer
		buf := make([]byte, 4096)

		for {
			n, err := d.conn.Read(buf)
			if n > 0 {
				result.Write(buf[:n])
				if bytes.Contains(result.Bytes(), []byte("^")) {
					break
				}
			}
			if err != nil {
				d.closeConnectionLocked()
				break
			}
		}

		if result.Len() > 0 {
			raw := strings.TrimSpace(result.String())
			slog.Debug("SAVEMA IO", "ip", d.Address, "cmd", cmd, "reply", raw)
			return raw, nil
		}
	}

	return "", fmt.Errorf("SAVEMA: нет ответа от принтера на команду %s", cmd)
}

func CleanResponse(raw string) string {
	clean := strings.TrimSpace(raw)
	start := strings.Index(clean, ":")
	end := strings.LastIndex(clean, "}")
	if start != -1 && end != -1 && end > start {
		return strings.TrimSpace(clean[start+1 : end])
	}
	return strings.Trim(clean, "~^")
}

// ============================================================================
// Реализация контракта core.Printer
// ============================================================================

// InitSession подготавливает устройство к новой партии:
// мягко останавливает печать, очищает буфер БД контроллера и сносит старый файл CSV.
func (d *Driver) InitSession(fieldName string, maxQueue int, staticFields map[string]string) error {
	slog.Info("SAVEMA: Инициализация сессии печати", "ip", d.Address, "field", fieldName)

	// 1. Остановка печати предыдущей смены
	_, _ = d.sendRaw("SPPSTP")

	// 2. Сброс лимита печати в 0
	_, _ = d.sendRaw("SPPSLQ{0}")

	// 3. Очистка буфера БД контроллера в ОЗУ
	_, _ = d.sendRaw("SPLCDB")

	// 4. Удаление старого файла партии
	_, _ = d.sendRaw(fmt.Sprintf("SPLDDF{%s}", dataCsvName))

	// 5. Запись статических параметров (если переданы)
	if len(staticFields) > 0 {
		_ = d.UpdateStaticFields(staticFields)
	}

	d.stateMu.Lock()
	d.lastCount = 0
	d.batchSize = 0
	d.stateMu.Unlock()

	return nil
}

// SelectTemplate выбирает шаблон в контроллере и обновляет статические поля.
func (d *Driver) SelectTemplate(template string, fields map[string]string) error {
	template = strings.TrimSpace(template)
	if template == "" {
		return fmt.Errorf("SAVEMA: передан пустой шаблон")
	}

	// Загрузка шаблона допустима только в режиме STOP (WAITING)
	_, _ = d.sendRaw("SPPSTP")

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("SPLLTF{%s}", template))

	for k, v := range fields {
		cleanVal := strings.ReplaceAll(v, "|", "")
		cleanVal = strings.ReplaceAll(cleanVal, "~gt~", "")
		sb.WriteString(fmt.Sprintf("|SPMCTV{%s~gt~%s}", k, cleanVal))
	}

	resp, err := d.sendRaw(sb.String())
	if err != nil {
		return fmt.Errorf("SAVEMA: ошибка отправки команды выбора шаблона %s: %w", template, err)
	}
	if strings.Contains(resp, "FAIL") {
		return fmt.Errorf("SAVEMA: контроллер отклонил шаблон %s (ответ: %s)", template, resp)
	}

	d.stateMu.Lock()
	d.curTemplate = template
	for k, v := range fields {
		d.staticFields[k] = v
	}
	d.stateMu.Unlock()

	slog.Info("SAVEMA: Шаблон успешно активирован", "ip", d.Address, "template", template)
	return nil
}

// PrintBatchIndexed реализует загрузку кодов Честного Знака через CSV с защитой от циклов:
// 1. Формирование строк CSV с GS1-разделителем.
// 2. Удаление старого файла и сброс кэша базы (SPLDDF + SPLCDB).
// 3. Загрузка CSV через SPLCDF.
// 4. Фиксация лимита печати РОВНО на N строк через SPPSLQ{N} и запуск автомата SPPSAP.
func (d *Driver) PrintBatchIndexed(fieldName string, startIndex int, codes []string) (int, error) {
	if len(codes) == 0 {
		return 0, nil
	}

	// 1. Нормализация кодов (замена текстового <GS> на байт ASCII 29 для DataMatrix)
	var rows []string
	for _, c := range codes {
		clean := strings.TrimSpace(c)
		clean = strings.ReplaceAll(clean, "<GS>", "\x1d")
		clean = strings.ReplaceAll(clean, "~", "") // Защита от разрушения пакета SPPL
		clean = strings.ReplaceAll(clean, "^", "")
		rows = append(rows, clean)
	}
	csvPayload := strings.Join(rows, "\r\n")

	// 2. Очистка старых следов базы перед заливкой
	_, _ = d.sendRaw(fmt.Sprintf("SPLDDF{%s}", dataCsvName))
	_, _ = d.sendRaw("SPLCDB")

	// 3. Загрузка нового файла в память принтера
	uploadCmd := fmt.Sprintf("SPLCDF{%s~gt~%s}", dataCsvName, csvPayload)
	respUpload, err := d.sendRaw(uploadCmd)
	if err != nil || strings.Contains(respUpload, "FAIL") {
		return 0, fmt.Errorf("SAVEMA: сбой заливки CSV-файла: %v (ответ: %s)", err, respUpload)
	}

	totalItems := len(codes)

	// 4. ЗАЩИТА ОТ ЦИКЛИЧЕСКОЙ ПЕЧАТИ:
	// Лимит тиража устанавливается строго равным количеству кодов в файле.
	// Контроллер отпечатает ровно totalItems раз и перейдет в режим WAITING (стоп по датчику).
	guardCmd := fmt.Sprintf("SPPSLQ{%d}|SPPSAP", totalItems)
	respGuard, err := d.sendRaw(guardCmd)
	if err != nil || strings.Contains(respGuard, "FAIL") {
		return 0, fmt.Errorf("SAVEMA: сбой взвода лимита партии SPPSLQ: %v (ответ: %s)", err, respGuard)
	}

	d.stateMu.Lock()
	d.batchSize = totalItems
	d.lastCount = 0
	d.stateMu.Unlock()

	slog.Info("SAVEMA: Партия кодов загружена в CSV, лимит зафиксирован",
		"ip", d.Address,
		"records", totalItems,
		"start_index", startIndex,
	)

	return totalItems, nil
}

// GetLastPrintedIndex возвращает порядковый номер последнего отпечатанного кода
// на основе аппаратного счетчика текущего макета/тиража (SPGGCP).
func (d *Driver) GetLastPrintedIndex() (int, error) {
	raw, err := d.sendRaw("SPGGCP")
	if err != nil {
		d.stateMu.RLock()
		defer d.stateMu.RUnlock()
		return d.lastCount, err
	}

	clean := CleanResponse(raw)
	cnt, err := strconv.Atoi(clean)
	if err != nil {
		d.stateMu.RLock()
		defer d.stateMu.RUnlock()
		return d.lastCount, nil
	}

	d.stateMu.Lock()
	d.lastCount = cnt
	d.stateMu.Unlock()

	return cnt, nil
}

// GetBufferFreeSpace возвращает остаток неотпечатанного тиража по счетчику SPPGLQ.
func (d *Driver) GetBufferFreeSpace() (int, error) {
	raw, err := d.sendRaw("SPPGLQ")
	if err != nil {
		return 0, err
	}
	clean := CleanResponse(raw)
	remaining, err := strconv.Atoi(clean)
	if err != nil {
		return 0, nil
	}
	return remaining, nil
}

// ClearQueue аппаратно останавливает принтер, сбрасывает счетчик лимита и чистит CSV.
func (d *Driver) ClearQueue() error {
	slog.Info("SAVEMA: Полный сброс очереди и файла данных", "ip", d.Address)
	_, _ = d.sendRaw("SPPSTP")
	_, _ = d.sendRaw("SPPSLQ{0}")
	_, _ = d.sendRaw("SPLCDB")
	_, err := d.sendRaw(fmt.Sprintf("SPLDDF{%s}", dataCsvName))

	d.stateMu.Lock()
	d.batchSize = 0
	d.lastCount = 0
	d.stateMu.Unlock()

	return err
}

func (d *Driver) UpdateStaticFields(fields map[string]string) error {
	if len(fields) == 0 {
		return nil
	}

	var sb strings.Builder
	first := true
	for k, v := range fields {
		if !first {
			sb.WriteString("|")
		}
		cleanVal := strings.ReplaceAll(v, "|", "")
		cleanVal = strings.ReplaceAll(cleanVal, "~gt~", "")
		sb.WriteString(fmt.Sprintf("SPMCTV{%s~gt~%s}", k, cleanVal))
		first = false
	}

	resp, err := d.sendRaw(sb.String())
	if err != nil {
		return err
	}
	if strings.Contains(resp, "FAIL") {
		return fmt.Errorf("SAVEMA: ошибка обновления статики: %s", resp)
	}

	d.stateMu.Lock()
	for k, v := range fields {
		d.staticFields[k] = v
	}
	d.stateMu.Unlock()

	return nil
}

func (d *Driver) GetStatus() (string, error) {
	raw, err := d.sendRaw("SPPSTA")
	if err != nil {
		return "ОФФЛАЙН", err
	}

	clean := CleanResponse(raw)
	cleanUpper := strings.ToUpper(clean)

	if strings.Contains(cleanUpper, "WAITING") {
		return "ГОТОВ", nil
	}
	if strings.Contains(cleanUpper, "RUNNING") {
		return "ПЕЧАТЬ", nil
	}
	if strings.Contains(cleanUpper, "INIT") {
		return "ЗАПУСК", nil
	}
	if strings.Contains(cleanUpper, "ERROR") {
		return "АВАРИЯ: " + clean, nil
	}
	return clean, nil
}

func (d *Driver) PrintTemplate(template string, fields map[string]string) error {
	return d.SelectTemplate(template, fields)
}

func (d *Driver) GetTemplates() ([]string, error) {
	raw, err := d.sendRaw("SPLGST")
	if err != nil {
		return nil, err
	}
	clean := CleanResponse(raw)
	if clean == "" || strings.Contains(clean, "FAIL") {
		return nil, fmt.Errorf("SAVEMA: список шаблонов пуст или недоступен: %s", raw)
	}

	parts := strings.Split(clean, "<")
	var tpls []string
	for _, p := range parts {
		item := strings.TrimSpace(p)
		if item != "" {
			tpls = append(tpls, item)
		}
	}
	return tpls, nil
}

func (d *Driver) GetTemplateFields(templateName string) ([]string, error) {
	raw, err := d.sendRaw(fmt.Sprintf("SPLGFN{%s}", templateName))
	if err != nil {
		return nil, err
	}
	clean := CleanResponse(raw)
	parts := strings.Split(clean, "<")
	if len(parts) <= 1 {
		return []string{"DataMatrix", "Text01"}, nil
	}
	return parts[1:], nil
}

func (d *Driver) GetRemainingRibbon() (string, error) {
	raw, err := d.sendRaw("SPGGRR")
	if err != nil {
		return "N/A", err
	}
	clean := CleanResponse(raw)
	if clean == "" || strings.Contains(clean, "FAIL") {
		return "N/A", nil
	}
	return clean + "%", nil
}

func (d *Driver) GetQueueCapacity(q string) (string, error) {
	raw, err := d.sendRaw("SPPGLQ")
	if err != nil {
		return "0", err
	}
	return CleanResponse(raw), nil
}

func (d *Driver) GetPrintSpeed() (string, error) {
	raw, err := d.sendRaw("SPCGPS")
	if err != nil {
		return "N/A", err
	}
	return CleanResponse(raw), nil
}

func (d *Driver) GetCurrentPrintCount() (string, error) {
	raw, err := d.sendRaw("SPGGCP")
	if err != nil {
		return "0", err
	}
	return CleanResponse(raw), nil
}

func (d *Driver) GetCurrentTemplate() (string, error) {
	raw, err := d.sendRaw("SPLGAT")
	if err != nil {
		d.stateMu.RLock()
		defer d.stateMu.RUnlock()
		return d.curTemplate, err
	}
	clean := CleanResponse(raw)
	if clean != "" && !strings.Contains(clean, "FAIL") {
		d.stateMu.Lock()
		d.curTemplate = clean
		d.stateMu.Unlock()
	}
	return clean, nil
}

func (d *Driver) GetTotalPrints() (int64, error) {
	raw, err := d.sendRaw("SPGGTP")
	if err != nil {
		return 0, err
	}
	clean := CleanResponse(raw)
	val, _ := strconv.ParseInt(clean, 10, 64)
	return val, nil
}

func (d *Driver) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closeConnectionLocked()
	return nil
}

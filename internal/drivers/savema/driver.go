package savema

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultTimeout      = 5 * time.Second
	defaultQueueTimeout = 15 * time.Second
	defaultQueueLimit   = 30
	maxQueueLimit       = 200
	maxResponseSize     = 1024 * 1024
	continuousPrints    = 1000
)

// Driver implements the SAVEMA SPPL Rev.12 protocol. A session uses one
// dynamic field as a FIFO queue; field names come from the task created by 1C.
type Driver struct {
	Address string
	Port    int
	Timeout time.Duration
	// QueueTimeout allows SPLAQD more time to parse a batch than short status commands.
	QueueTimeout time.Duration
	// LocalCSVDir overrides the directory used for diagnostic copies of CSV
	// files. When empty, copies are stored beside the running executable.
	LocalCSVDir string

	ioMu    sync.Mutex
	stateMu sync.Mutex
	fileMu  sync.Mutex
	csvMu   sync.Mutex

	template         string
	queueField       string
	queueLimit       int
	sessionReady     bool
	firstIndex       int
	totalSent        int
	lastPrinted      int
	baseTotalPrints  int64
	localCSVPath     string
	remoteCSVName    string
	remoteCSVReady   bool
	staticFields     map[string]string
	lastRawStatus    string
	lastStatusDetail string
}

type spplResponse struct {
	Command string
	Payload string
}

func New(ip string, port int) *Driver {
	return &Driver{Address: ip, Port: port, Timeout: defaultTimeout}
}

// PrepareDynamicDataFile prepares diagnostics and checks whether the CSV
// already exists. SPPL rejects an empty File Content, so a missing file is
// created later from the first real production code.
func (d *Driver) PrepareDynamicDataFile(fieldName string) (string, string, bool, []string, error) {
	fieldName = strings.TrimSpace(fieldName)
	if err := validateSPPLName("имя динамического поля", fieldName); err != nil {
		return "", "", false, nil, err
	}
	fileName := fieldName + ".csv"
	localPath, err := d.writeLocalCSV(fileName, "")
	if err != nil {
		return fileName, "", false, nil, fmt.Errorf("SAVEMA: не удалось сохранить локальную копию CSV %q: %w", fileName, err)
	}
	d.stateMu.Lock()
	d.localCSVPath = localPath
	d.stateMu.Unlock()
	slog.Info("SAVEMA CSV: подготовлена локальная копия", "ip", d.Address, "file", fileName, "path", localPath)

	if err := d.stopPrinting(); err != nil {
		return fileName, localPath, false, nil, fmt.Errorf("SAVEMA: не удалось остановить печать перед подготовкой CSV %q: %w", fileName, err)
	}

	stored, err := d.execute("SPLGSD")
	if err != nil {
		return fileName, localPath, false, nil, fmt.Errorf("SAVEMA: не удалось получить список CSV-файлов перед подготовкой %q: %w", fileName, err)
	}
	storedFiles := splitNames(stored)
	ready := containsName(stored, fileName)
	d.stateMu.Lock()
	d.remoteCSVName = fileName
	d.remoteCSVReady = ready
	d.stateMu.Unlock()
	if ready {
		slog.Info("SAVEMA CSV: файл уже существует в памяти", "ip", d.Address, "file", fileName, "files", storedFiles)
	} else {
		slog.Info("SAVEMA CSV: файл будет создан из первой пачки реальных кодов", "ip", d.Address, "file", fileName, "files", storedFiles)
	}
	return fileName, localPath, ready, storedFiles, nil
}

func (d *Driver) ensureRemoteCSV(firstCode string) error {
	d.csvMu.Lock()
	defer d.csvMu.Unlock()

	d.stateMu.Lock()
	fileName, ready := d.remoteCSVName, d.remoteCSVReady
	template, queueField := d.template, d.queueField
	staticFields := cloneStringMap(d.staticFields)
	d.stateMu.Unlock()
	if ready {
		return nil
	}
	if fileName == "" {
		return errors.New("SAVEMA: имя CSV-файла не инициализировано")
	}
	if _, err := d.execute("SPLCDF{" + fileName + "~gt~" + firstCode + "}"); err != nil {
		return fmt.Errorf("SAVEMA: не удалось создать CSV-файл %q из первого рабочего кода: %w", fileName, err)
	}
	stored, err := d.execute("SPLGSD")
	if err != nil {
		return fmt.Errorf("SAVEMA: CSV-файл %q создан, но не удалось проверить память: %w", fileName, err)
	}
	if !containsName(stored, fileName) {
		return fmt.Errorf("SAVEMA: контроллер ответил OK на создание %q, но файл отсутствует в SPLGSD", fileName)
	}
	// SAVEMA creates CSV queues while loading the template. Since the file did
	// not exist during the initial load, clear the database buffer and reload
	// the template now that SPLCDF has created it.
	if _, err := d.execute("SPLCDB"); err != nil {
		return fmt.Errorf("SAVEMA: CSV-файл %q создан, но не удалось очистить буфер данных: %w", fileName, err)
	}
	if err := d.stopPrinting(); err != nil {
		return fmt.Errorf("SAVEMA: CSV-файл %q создан, но не удалось остановить печать перед перезагрузкой макета: %w", fileName, err)
	}
	if _, err := d.execute("SPLLTF{" + template + "}"); err != nil {
		return fmt.Errorf("SAVEMA: не удалось повторно загрузить макет %q после создания CSV: %w", template, err)
	}
	active, err := d.execute("SPLGAT")
	if err != nil || !containsName(active, template) {
		return fmt.Errorf("SAVEMA: не удалось подтвердить макет %q после создания CSV: ответ=%q, ошибка=%v", template, active, err)
	}
	if len(staticFields) > 0 {
		if err := d.updateStaticFields(staticFields); err != nil {
			return fmt.Errorf("SAVEMA: не удалось восстановить статические поля после перезагрузки макета: %w", err)
		}
	}
	// Loading the template puts the bootstrap row into the new queue. Remove it;
	// the same real code is appended below with the complete production batch.
	if _, err := d.execute("SPLCQD{" + queueField + "}"); err != nil {
		return fmt.Errorf("SAVEMA: очередь %q создана, но не удалось удалить загрузочную строку: %w", queueField, err)
	}
	d.stateMu.Lock()
	d.remoteCSVReady = true
	d.stateMu.Unlock()
	slog.Info("SAVEMA CSV: файл создан из первого рабочего кода", "ip", d.Address, "file", fileName)
	return nil
}

func (d *Driver) writeLocalCSV(fileName, content string) (string, error) {
	d.fileMu.Lock()
	defer d.fileMu.Unlock()

	if filepath.Base(fileName) != fileName || strings.Contains(fileName, ":") {
		return "", fmt.Errorf("небезопасное имя файла %q", fileName)
	}
	dir := strings.TrimSpace(d.LocalCSVDir)
	if dir == "" {
		executable, err := os.Executable()
		if err != nil {
			return "", fmt.Errorf("не удалось определить путь программы: %w", err)
		}
		dir = filepath.Dir(executable)
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("не удалось подготовить каталог %q: %w", dir, err)
	}
	path := filepath.Join(dir, fileName)
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return "", err
	}
	return path, nil
}

func (d *Driver) appendLocalCSV(values []string) error {
	if len(values) == 0 {
		return nil
	}
	d.fileMu.Lock()
	defer d.fileMu.Unlock()

	d.stateMu.Lock()
	path := d.localCSVPath
	d.stateMu.Unlock()
	if path == "" {
		return errors.New("путь локальной копии CSV не инициализирован")
	}

	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	prefix := ""
	if info.Size() > 0 {
		prefix = "\n"
	}
	_, err = io.WriteString(file, prefix+strings.Join(values, "\n"))
	return err
}

func (d *Driver) SelectTemplate(template string, fields map[string]string) error {
	template = strings.TrimSpace(template)
	if err := validateSPPLName("имя шаблона", template); err != nil {
		return err
	}
	// Loading a template is only supported in the stop position. A previous
	// task may have left the controller in RUNNING even though no task is active.
	if err := d.stopPrinting(); err != nil {
		return fmt.Errorf("SAVEMA: не удалось остановить печать перед загрузкой шаблона: %w", err)
	}
	if _, err := d.execute("SPLLTF{" + template + "}"); err != nil {
		return fmt.Errorf("SAVEMA: не удалось загрузить шаблон %q: %w", template, err)
	}
	active, err := d.execute("SPLGAT")
	if err != nil {
		return fmt.Errorf("SAVEMA: не удалось проверить активный шаблон: %w", err)
	}
	if !containsName(active, template) {
		return fmt.Errorf("SAVEMA: активный шаблон %q не совпадает с ожидаемым %q", active, template)
	}

	d.stateMu.Lock()
	d.template = template
	d.sessionReady = false
	d.stateMu.Unlock()

	resolvedFields, _, err := d.resolveTemplateFields(template, fields, "")
	if err != nil {
		return err
	}
	if err := d.updateStaticFields(resolvedFields); err != nil {
		return err
	}
	d.stateMu.Lock()
	d.staticFields = cloneStringMap(resolvedFields)
	d.stateMu.Unlock()
	slog.Info("SAVEMA: шаблон выбран", "ip", d.Address, "template", template)
	return nil
}

func (d *Driver) InitSession(fieldName string, maxQueue int, staticFields map[string]string) error {
	// Older generic callers can pass a composite list. SAVEMA has one queue
	// field in the current task API; static fields arrive separately.
	if before, _, ok := strings.Cut(fieldName, ";"); ok {
		fieldName = before
	}
	fieldName = strings.TrimSpace(fieldName)
	if err := validateSPPLName("динамическое поле", fieldName); err != nil {
		return err
	}

	d.stateMu.Lock()
	template := d.template
	d.stateMu.Unlock()
	if template == "" {
		return errors.New("SAVEMA: перед инициализацией сессии не выбран шаблон")
	}
	_, resolvedField, err := d.resolveTemplateFields(template, staticFields, fieldName)
	if err != nil {
		return err
	}

	d.stateMu.Lock()
	d.queueField = resolvedField
	d.queueLimit = normalizedQueueLimit(maxQueue)
	d.sessionReady = false
	d.firstIndex = 0
	d.totalSent = 0
	d.lastPrinted = 0
	d.stateMu.Unlock()
	if err := d.ClearQueue(); err != nil {
		return fmt.Errorf("SAVEMA: не удалось подготовить очередь %q: %w", resolvedField, err)
	}
	base, err := d.readTotalPrints()
	if err != nil {
		return fmt.Errorf("SAVEMA: не удалось прочитать общий счётчик перед запуском: %w", err)
	}

	d.stateMu.Lock()
	d.baseTotalPrints = base
	d.sessionReady = true
	d.stateMu.Unlock()

	slog.Info("SAVEMA: сессия готова", "ip", d.Address, "template", template,
		"queue_field", resolvedField, "queue_limit", normalizedQueueLimit(maxQueue),
		"base_total_prints", base)
	return nil
}

func (d *Driver) UpdateStaticFields(fields map[string]string) error {
	if len(fields) == 0 {
		return nil
	}
	d.stateMu.Lock()
	template := d.template
	d.stateMu.Unlock()
	if template == "" {
		return errors.New("SAVEMA: перед обновлением статических полей не выбран шаблон")
	}
	resolvedFields, _, err := d.resolveTemplateFields(template, fields, "")
	if err != nil {
		return err
	}
	if err := d.updateStaticFields(resolvedFields); err != nil {
		return err
	}
	d.stateMu.Lock()
	d.staticFields = cloneStringMap(resolvedFields)
	d.stateMu.Unlock()
	return nil
}

func cloneStringMap(source map[string]string) map[string]string {
	if len(source) == 0 {
		return nil
	}
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func (d *Driver) updateStaticFields(fields map[string]string) error {
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)

	var params strings.Builder
	for _, name := range names {
		value := fields[name]
		if err := validateSPPLName("имя статического поля", name); err != nil {
			return err
		}
		if err := validateSPPLValue(name, value); err != nil {
			return err
		}
		if params.Len() > 0 {
			params.WriteString("~gt~")
		}
		params.WriteString(name)
		params.WriteString("~gt~")
		params.WriteString(value)
	}
	if _, err := d.execute("SPMCSV{" + params.String() + "}"); err != nil {
		return fmt.Errorf("SAVEMA: не удалось установить статические поля: %w; проверьте, что объекты шаблона имеют источник External", err)
	}
	return nil
}

func (d *Driver) ClearQueue() error {
	if err := d.stopPrinting(); err != nil {
		return err
	}

	d.stateMu.Lock()
	field := d.queueField
	remoteCSVName, remoteCSVReady := d.remoteCSVName, d.remoteCSVReady
	d.stateMu.Unlock()
	useFieldQueue := field != "" && (remoteCSVName == "" || remoteCSVReady)
	if useFieldQueue {
		if _, err := d.execute("SPLCQD{" + field + "}"); err != nil {
			slog.Warn("SAVEMA: SPLCQD отклонена, используется общая очистка SPLCDB", "ip", d.Address, "field", field, "err", err)
			if _, fallbackErr := d.execute("SPLCDB"); fallbackErr != nil {
				return fmt.Errorf("не удалось очистить очередь %q через SPLCQD (%v) и SPLCDB (%w)", field, err, fallbackErr)
			}
		}
	} else if _, err := d.execute("SPLCDB"); err != nil {
		return fmt.Errorf("не удалось очистить буфер данных: %w", err)
	}

	base, err := d.readTotalPrints()
	if err != nil {
		return err
	}
	d.stateMu.Lock()
	d.baseTotalPrints = base
	d.firstIndex = 0
	d.totalSent = 0
	d.lastPrinted = 0
	d.stateMu.Unlock()
	return nil
}

func (d *Driver) GetBufferFreeSpace() (int, error) {
	d.stateMu.Lock()
	ready, field, limit := d.sessionReady, d.queueField, d.queueLimit
	remoteCSVName, remoteCSVReady := d.remoteCSVName, d.remoteCSVReady
	d.stateMu.Unlock()
	if !ready || field == "" {
		return 0, errors.New("SAVEMA: очередь не инициализирована")
	}
	// A missing CSV is created by PrintBatchIndexed from the first real code.
	// Querying its queue before that returns FAIL and would prevent the pumper
	// from ever reaching PrintBatchIndexed.
	if remoteCSVName != "" && !remoteCSVReady {
		if _, err := d.refreshPrintedCount(); err != nil {
			return 0, err
		}
		return limit, nil
	}
	queued, err := d.queueCount(field)
	if err != nil {
		return 0, err
	}
	if _, err := d.refreshPrintedCount(); err != nil {
		return 0, err
	}
	d.stateMu.Lock()
	outstanding := d.totalSent - d.lastPrinted
	d.stateMu.Unlock()
	if outstanding < 0 {
		outstanding = 0
	}
	if queued > outstanding {
		outstanding = queued
	}
	free := limit - outstanding
	if free < 0 {
		free = 0
	}
	return free, nil
}

func (d *Driver) GetTemplateFields(templateName string) ([]string, error) {
	templateName = strings.TrimSpace(templateName)
	if err := validateSPPLName("имя шаблона", templateName); err != nil {
		return nil, err
	}
	payload, err := d.execute("SPLGFN{" + templateName + "}")
	if err != nil {
		return nil, err
	}
	parts := splitNames(payload)
	if len(parts) > 0 && strings.EqualFold(parts[0], templateName) {
		parts = parts[1:]
	}
	return parts, nil
}

func (d *Driver) GetTemplates() ([]string, error) {
	payload, err := d.execute("SPLGST")
	if err != nil {
		return nil, err
	}
	return splitNames(payload), nil
}

func (d *Driver) PrintBatchIndexed(fieldName string, startIndex int, codes []string) (int, error) {
	if len(codes) == 0 {
		return 0, nil
	}
	fieldName = strings.TrimSpace(fieldName)
	d.stateMu.Lock()
	ready, configuredField := d.sessionReady, d.queueField
	expectedIndex := d.firstIndex + d.totalSent
	d.stateMu.Unlock()
	if !ready {
		return 0, errors.New("SAVEMA: сессия не инициализирована")
	}
	if !strings.EqualFold(fieldName, configuredField) {
		return 0, fmt.Errorf("SAVEMA: поле пакета %q не совпадает с полем сессии %q", fieldName, configuredField)
	}
	if startIndex <= 0 {
		return 0, fmt.Errorf("SAVEMA: некорректный начальный индекс %d", startIndex)
	}
	if expectedIndex > 0 && startIndex != expectedIndex {
		return 0, fmt.Errorf("SAVEMA: нарушена последовательность индексов: ожидался %d, получен %d", expectedIndex, startIndex)
	}

	prepared := make([]string, len(codes))
	for i, code := range codes {
		code = strings.ReplaceAll(code, "<GS>", "\x1d")
		if err := validateQueueValue(code); err != nil {
			return 0, fmt.Errorf("SAVEMA: код %d: %w", i, err)
		}
		prepared[i] = code
	}
	if err := d.ensureRemoteCSV(prepared[0]); err != nil {
		return 0, err
	}
	if err := d.rebaseBeforeFirstBatch(); err != nil {
		return 0, fmt.Errorf("SAVEMA: не удалось зафиксировать счётчик перед первой пачкой: %w", err)
	}
	queueBefore, queueBeforeErr := d.queueCount(configuredField)
	totalBefore, totalBeforeErr := d.readTotalPrints()
	command := "SPLAQD{" + configuredField + "~gt~" + strings.Join(prepared, "\n") + "}"
	slog.Info("SAVEMA: отправка пачки в очередь", "ip", d.Address, "field", configuredField,
		"start_index", startIndex, "count", len(prepared), "bytes", len(command)+2)
	queueTimeout := d.QueueTimeout
	if queueTimeout <= 0 {
		queueTimeout = defaultQueueTimeout
	}
	_, sendErr := d.executeWithTimeout(command, queueTimeout)
	ambiguous := false
	if sendErr != nil {
		if !isTimeoutError(sendErr) {
			return 0, fmt.Errorf("SAVEMA: принтер не принял пакет: %w", sendErr)
		}

		queueAfter, queueAfterErr := d.queueCount(configuredField)
		totalAfter, totalAfterErr := d.readTotalPrints()
		if queueBeforeErr == nil && totalBeforeErr == nil && queueAfterErr == nil && totalAfterErr == nil {
			observed := queueAfter - queueBefore + int(totalAfter-totalBefore)
			switch {
			case observed > 0:
				// SPLAQD inserts the supplied rows as one operation. The printer may
				// dequeue one or more rows for printing before SPLGQC is read, so a
				// positive observed change confirms the complete command.
				slog.Warn("SAVEMA: ответ SPLAQD потерян, но приём пачки подтверждён движением очереди", "ip", d.Address,
					"count", len(prepared), "queue_before", queueBefore, "queue_after", queueAfter,
					"prints_before", totalBefore, "prints_after", totalAfter, "observed_change", observed)
			case observed == 0:
				return 0, fmt.Errorf("SAVEMA: таймаут SPLAQD; очередь и счётчик не изменились: %w", sendErr)
			default:
				ambiguous = true
			}
		} else {
			ambiguous = true
		}
		if ambiguous {
			slog.Error("SAVEMA: результат SPLAQD неоднозначен; коды оставлены in_buffer во избежание дублей", "ip", d.Address,
				"field", configuredField, "count", len(prepared), "err", sendErr,
				"queue_before_err", queueBeforeErr, "queue_after_err", queueAfterErr,
				"total_before_err", totalBeforeErr, "total_after_err", totalAfterErr)
		}
	}
	if err := d.appendLocalCSV(prepared); err != nil {
		slog.Error("SAVEMA CSV: пакет принят принтером, но не записан в локальную копию", "ip", d.Address,
			"field", configuredField, "count", len(prepared), "err", err)
	}

	d.stateMu.Lock()
	if d.firstIndex == 0 {
		d.firstIndex = startIndex
	}
	d.totalSent += len(codes)
	d.stateMu.Unlock()

	if err := d.ensurePrinting(); err != nil {
		return len(codes), fmt.Errorf("SAVEMA: пакет принят, но печать не запущена: %w", err)
	}
	queueAfter, queueAfterErr := d.queueCount(configuredField)
	slog.Info("SAVEMA: пачка учтена как загруженная", "ip", d.Address, "field", configuredField,
		"start_index", startIndex, "count", len(codes), "ambiguous", ambiguous,
		"queue_items", queueAfter, "queue_check_error", queueAfterErr)
	if ambiguous {
		return len(codes), fmt.Errorf("SAVEMA: ответ SPLAQD не получен и состояние очереди не удалось подтвердить; повторная отправка заблокирована")
	}
	return len(codes), nil
}

func (d *Driver) rebaseBeforeFirstBatch() error {
	d.stateMu.Lock()
	firstBatch := d.totalSent == 0
	d.stateMu.Unlock()
	if !firstBatch {
		return nil
	}
	total, err := d.readTotalPrints()
	if err != nil {
		return err
	}
	d.stateMu.Lock()
	if d.totalSent == 0 {
		d.baseTotalPrints = total
		d.lastPrinted = 0
	}
	d.stateMu.Unlock()
	return nil
}

func (d *Driver) GetLastPrintedIndex() (int, error) {
	if _, err := d.refreshPrintedCount(); err != nil {
		return 0, err
	}
	d.stateMu.Lock()
	defer d.stateMu.Unlock()
	if d.firstIndex == 0 || d.lastPrinted == 0 {
		return 0, nil
	}
	return d.firstIndex + d.lastPrinted - 1, nil
}

func (d *Driver) PrintBatch(fieldName string, codes []string) (int, error) {
	d.stateMu.Lock()
	start := d.firstIndex + d.totalSent
	if start == 0 {
		start = 1
	}
	d.stateMu.Unlock()
	return d.PrintBatchIndexed(fieldName, start, codes)
}

func (d *Driver) PrintTemplate(template string, fields map[string]string) error {
	return d.SelectTemplate(template, fields)
}

func (d *Driver) GetStatus() (string, error) {
	status, detail, err := d.printerStatus()
	if err != nil {
		return "", err
	}
	if strings.Contains(strings.ToUpper(detail), "BLOCKED") {
		return "БЛОКИРОВАН: " + status, nil
	}
	switch status {
	case "WAITING":
		return "ГОТОВ", nil
	case "RUNNING":
		return "ПЕЧАТЬ", nil
	case "INIT":
		return "ЗАПУСК", nil
	case "ERROR":
		if detail != "" {
			return "ОШИБКА: " + detail, nil
		}
		return "ОШИБКА", nil
	default:
		return status, nil
	}
}

func (d *Driver) GetRemainingRibbon() (string, error) { return d.execute("SPGGRR") }

func (d *Driver) GetQueueCapacity(q string) (string, error) {
	count, err := d.queueCount(q)
	if err != nil {
		return "", err
	}
	return strconv.Itoa(count), nil
}

func (d *Driver) GetPrintSpeed() (string, error) { return d.execute("SPCGPS") }

func (d *Driver) GetCurrentPrintCount() (string, error) {
	payload, err := d.execute("SPGGCP")
	if err != nil {
		return "", err
	}
	if _, err := strconv.ParseInt(strings.TrimSpace(payload), 10, 64); err != nil {
		return "", fmt.Errorf("SAVEMA: некорректный текущий счётчик %q", payload)
	}
	return strings.TrimSpace(payload), nil
}

func (d *Driver) GetCurrentTemplate() (string, error) { return d.execute("SPLGAT") }
func (d *Driver) GetTotalPrints() (int64, error)      { return d.readTotalPrints() }

func (d *Driver) resolveTemplateFields(template string, staticFields map[string]string, dynamicField string) (map[string]string, string, error) {
	fields, err := d.GetTemplateFields(template)
	if err != nil {
		return nil, "", fmt.Errorf("SAVEMA: не удалось получить поля шаблона %q: %w", template, err)
	}
	available := make(map[string]string, len(fields))
	for _, field := range fields {
		field = strings.TrimSpace(field)
		available[strings.ToLower(field)] = field
	}
	resolve := func(name, kind string) (string, error) {
		resolved, ok := available[strings.ToLower(strings.TrimSpace(name))]
		if !ok {
			return "", fmt.Errorf("SAVEMA: %s %q отсутствует в шаблоне %q; доступно: %s",
				kind, name, template, strings.Join(fields, ", "))
		}
		return resolved, nil
	}
	resolvedDynamic := ""
	if dynamicField != "" {
		resolvedDynamic, err = resolve(dynamicField, "динамическое поле")
		if err != nil {
			return nil, "", err
		}
	}
	resolvedStatic := make(map[string]string, len(staticFields))
	for name := range staticFields {
		if strings.EqualFold(name, dynamicField) {
			return nil, "", fmt.Errorf("SAVEMA: поле %q одновременно указано как динамическое и статическое", name)
		}
		resolved, err := resolve(name, "статическое поле")
		if err != nil {
			return nil, "", err
		}
		resolvedStatic[resolved] = staticFields[name]
	}
	return resolvedStatic, resolvedDynamic, nil
}

func (d *Driver) refreshPrintedCount() (int, error) {
	total, err := d.readTotalPrints()
	if err != nil {
		return 0, err
	}
	d.stateMu.Lock()
	defer d.stateMu.Unlock()
	if !d.sessionReady {
		return 0, errors.New("SAVEMA: сессия не инициализирована")
	}
	if d.totalSent == 0 {
		// The printer exposes a lifetime counter. Prints completed between task
		// creation and its first data batch belong to an earlier/external run.
		d.baseTotalPrints = total
		d.lastPrinted = 0
		return 0, nil
	}
	delta := total - d.baseTotalPrints
	if delta < 0 {
		return 0, fmt.Errorf("SAVEMA: общий счётчик уменьшился с %d до %d", d.baseTotalPrints, total)
	}
	if delta > int64(d.totalSent) {
		slog.Warn("SAVEMA: счётчик вырос больше числа отправленных кодов", "ip", d.Address,
			"delta", delta, "sent", d.totalSent)
		delta = int64(d.totalSent)
	}
	d.lastPrinted = int(delta)
	return d.lastPrinted, nil
}

func (d *Driver) readTotalPrints() (int64, error) {
	payload, err := d.execute("SPGGTP")
	if err != nil {
		return 0, err
	}
	value, err := strconv.ParseInt(strings.TrimSpace(payload), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("SAVEMA: некорректный общий счётчик %q", payload)
	}
	return value, nil
}

func (d *Driver) queueCount(field string) (int, error) {
	if err := validateSPPLName("поле очереди", field); err != nil {
		return 0, err
	}
	payload, err := d.execute("SPLGQC{" + field + "}")
	if err != nil {
		return 0, err
	}
	value, err := strconv.Atoi(strings.TrimSpace(payload))
	if err != nil {
		return 0, fmt.Errorf("SAVEMA: некорректный размер очереди %q", payload)
	}
	return value, nil
}

func (d *Driver) printerStatus() (string, string, error) {
	payload, err := d.execute("SPPSTA")
	if err != nil {
		return "", "", err
	}
	parts := strings.SplitN(payload, "<", 2)
	status := strings.ToUpper(strings.TrimSpace(parts[0]))
	detail := ""
	if len(parts) == 2 {
		detail = strings.TrimSpace(parts[1])
	}
	d.stateMu.Lock()
	changed := status != d.lastRawStatus || detail != d.lastStatusDetail
	previousStatus, previousDetail := d.lastRawStatus, d.lastStatusDetail
	d.lastRawStatus, d.lastStatusDetail = status, detail
	d.stateMu.Unlock()
	if changed {
		slog.Info("SAVEMA: изменилось состояние контроллера", "ip", d.Address,
			"from_status", previousStatus, "from_detail", previousDetail,
			"to_status", status, "to_detail", detail)
		d.stateMu.Lock()
		field, sessionReady := d.queueField, d.sessionReady
		d.stateMu.Unlock()
		if sessionReady && field != "" {
			queueItems, queueErr := d.queueCount(field)
			slog.Info("SAVEMA: состояние печати при переходе", "ip", d.Address, "status", status,
				"field", field, "queue_items", queueItems, "queue_check_error", queueErr)
		}
	}
	return status, detail, nil
}

func (d *Driver) ensurePrinting() error {
	status, detail, err := d.printerStatus()
	if err != nil {
		return err
	}
	if strings.Contains(strings.ToUpper(detail), "BLOCKED") {
		return fmt.Errorf("принтер заблокирован оператором: %s %s", status, detail)
	}
	switch status {
	case "RUNNING":
		return nil
	case "ERROR", "INIT":
		return fmt.Errorf("принтер не готов: %s %s", status, detail)
	}
	if err := d.startLimitedPrinting(continuousPrints); err != nil {
		return err
	}
	slog.Info("SAVEMA: печать запущена", "ip", d.Address, "previous_status", status,
		"limited_quantity", continuousPrints)
	return nil
}

func (d *Driver) startLimitedPrinting(quantity int) error {
	command := "SPPSLQ{" + strconv.Itoa(quantity) + "}|SPPSAP"
	raw, err := d.sendRawExpect(command, 0, 2)
	if err != nil {
		return fmt.Errorf("не удалось запустить ограниченную печать: %w", err)
	}
	responses := parseResponses(raw)
	for _, expected := range []string{"SPPSLQ", "SPPSAP"} {
		found := false
		for _, response := range responses {
			if !responseMatches(expected, response.Command) {
				continue
			}
			found = true
			upper := strings.ToUpper(response.Payload)
			if strings.Contains(upper, "FAIL") || strings.Contains(upper, "NOT FOUND") {
				return fmt.Errorf("%s: %s", response.Command, response.Payload)
			}
			break
		}
		if !found {
			return fmt.Errorf("SAVEMA: в ответе составного запуска отсутствует %s: %q", expected, raw)
		}
	}
	return nil
}

func (d *Driver) stopPrinting() error {
	status, detail, err := d.printerStatus()
	if err != nil {
		return err
	}
	if strings.Contains(strings.ToUpper(detail), "BLOCKED") {
		return fmt.Errorf("принтер заблокирован оператором: %s %s", status, detail)
	}
	if status != "RUNNING" {
		return nil
	}
	if _, err := d.execute("SPPSTP"); err != nil {
		return fmt.Errorf("не удалось остановить печать: %w", err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(250 * time.Millisecond)
		status, _, err = d.printerStatus()
		if err != nil {
			return err
		}
		if status != "RUNNING" {
			return nil
		}
	}
	return errors.New("принтер не остановился за 20 секунд")
}

func (d *Driver) execute(command string) (string, error) {
	return d.executeWithTimeout(command, 0)
}

func (d *Driver) executeWithTimeout(command string, timeout time.Duration) (string, error) {
	raw, err := d.sendRawWithTimeout(command, timeout)
	if err != nil {
		return "", err
	}
	expected := commandName(command)
	for _, response := range parseResponses(raw) {
		if !responseMatches(expected, response.Command) {
			continue
		}
		upper := strings.ToUpper(response.Payload)
		if strings.Contains(upper, "FAIL") || strings.Contains(upper, "NOT FOUND") {
			return "", fmt.Errorf("%s: %s", response.Command, response.Payload)
		}
		return strings.TrimSpace(response.Payload), nil
	}
	return "", fmt.Errorf("SAVEMA: ответ на %s не найден: %q", expected, raw)
}

// sendRaw uses a dedicated connection and reads a complete '^'-terminated
// response. This also prevents a stale response from a previous request.
func (d *Driver) sendRaw(command string) (string, error) {
	return d.sendRawWithTimeout(command, 0)
}

func (d *Driver) sendRawWithTimeout(command string, timeout time.Duration) (string, error) {
	return d.sendRawExpect(command, timeout, 1)
}

func (d *Driver) sendRawExpect(command string, timeout time.Duration, expectedResponses int) (string, error) {
	d.ioMu.Lock()
	defer d.ioMu.Unlock()
	command = strings.TrimSpace(command)
	if !strings.HasPrefix(command, "~") {
		command = "~" + command
	}
	if !strings.HasSuffix(command, "^") {
		command += "^"
	}
	if timeout <= 0 {
		timeout = d.Timeout
	}
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	address := net.JoinHostPort(d.Address, strconv.Itoa(d.Port))
	conn, err := net.DialTimeout("tcp", address, timeout)
	if err != nil {
		return "", fmt.Errorf("SAVEMA %s: соединение: %w", address, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	if _, err := io.WriteString(conn, command); err != nil {
		return "", fmt.Errorf("SAVEMA %s: отправка: %w", address, err)
	}

	var result bytes.Buffer
	buffer := make([]byte, 4096)
	for result.Len() < maxResponseSize {
		n, readErr := conn.Read(buffer)
		if n > 0 {
			_, _ = result.Write(buffer[:n])
			if len(parseResponses(result.String())) >= expectedResponses {
				break
			}
		}
		if readErr != nil {
			if result.Len() > 0 && errors.Is(readErr, io.EOF) {
				break
			}
			return "", fmt.Errorf("SAVEMA %s: чтение ответа: %w", address, readErr)
		}
	}
	if result.Len() == 0 {
		return "", errors.New("SAVEMA: получен пустой ответ")
	}
	if result.Len() >= maxResponseSize {
		return "", errors.New("SAVEMA: ответ превышает допустимый размер")
	}
	raw := strings.TrimSpace(result.String())
	slog.Debug("SAVEMA IO", "ip", d.Address, "command", commandName(command),
		"sent_bytes", len(command), "reply", raw)
	return raw, nil
}

func isTimeoutError(err error) bool {
	var networkError net.Error
	return errors.As(err, &networkError) && networkError.Timeout()
}

func parseResponses(raw string) []spplResponse {
	var result []spplResponse
	remaining := raw
	for {
		start := strings.Index(remaining, "SPGRES{")
		if start < 0 {
			break
		}
		bodyStart := start + len("SPGRES{")
		end := strings.Index(remaining[bodyStart:], "}")
		if end < 0 {
			break
		}
		end += bodyStart
		terminator := 1
		if end+1 < len(remaining) && remaining[end+1] == '^' {
			terminator = 2
		}
		body := remaining[bodyStart:end]
		command, payload, ok := strings.Cut(body, ":")
		if ok {
			result = append(result, spplResponse{Command: strings.ToUpper(strings.TrimSpace(command)), Payload: strings.TrimSpace(payload)})
		}
		remaining = remaining[end+terminator:]
	}
	return result
}

func CleanResponse(raw string) string {
	responses := parseResponses(raw)
	if len(responses) > 0 {
		return responses[0].Payload
	}
	return strings.TrimSpace(raw)
}

func commandName(command string) string {
	command = strings.TrimLeft(strings.TrimSpace(command), "~")
	for i, r := range command {
		if r == '{' || r == '|' || r == '^' {
			return strings.ToUpper(strings.TrimSpace(command[:i]))
		}
	}
	return strings.ToUpper(strings.TrimSpace(command))
}

func responseMatches(expected, actual string) bool {
	if strings.EqualFold(expected, actual) {
		return true
	}
	switch strings.ToUpper(expected) {
	case "SPGGCP":
		return strings.EqualFold(actual, "SPGGTP")
	case "SPPGLQ", "SPCGLQ":
		return strings.EqualFold(actual, "SPPGLQ") || strings.EqualFold(actual, "SPCGLQ")
	case "SPLGMQ":
		return strings.EqualFold(actual, "SPLGQC")
	default:
		return false
	}
}

func splitNames(payload string) []string {
	parts := strings.Split(payload, "<")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			result = append(result, part)
		}
	}
	return result
}

func containsName(payload, expected string) bool {
	for _, name := range splitNames(payload) {
		if strings.EqualFold(name, expected) {
			return true
		}
	}
	return false
}

func validateSPPLName(kind, value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("SAVEMA: не заполнено %s", kind)
	}
	if strings.ContainsAny(value, "{}|^\r\n") || strings.Contains(value, "~gt~") {
		return fmt.Errorf("SAVEMA: %s %q содержит служебные символы SPPL", kind, value)
	}
	return nil
}

func validateSPPLValue(name, value string) error {
	if strings.ContainsAny(value, "{}|^\r\n") || strings.Contains(value, "~gt~") {
		return fmt.Errorf("SAVEMA: значение поля %q содержит служебные символы SPPL", name)
	}
	return nil
}

func validateQueueValue(value string) error {
	if value == "" {
		return errors.New("пустое значение")
	}
	if strings.ContainsAny(value, "{}|^\r\n") || strings.Contains(value, "~gt~") {
		return errors.New("значение содержит служебные символы SPPL")
	}
	return nil
}

func normalizedQueueLimit(value int) int {
	if value <= 0 {
		return defaultQueueLimit
	}
	if value > maxQueueLimit {
		return maxQueueLimit
	}
	return value
}

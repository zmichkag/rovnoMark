package extserver

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"text/template"
	"time"
)

// Driver реализует виртуальный драйвер внешнего сервера печати (NiceLabel, MES, ERP)
type Driver struct {
	endpointURL string
	timeout     time.Duration
	httpClient  *http.Client

	mu               sync.RWMutex
	curTemplate      string
	templateBody     string // Сырой шаблон из БД (raw_body), содержащий Go-теги {{.PLU}}, {{.Codes}} и т.д.
	lastPrintedIndex int
}

// New создает новый экземпляр драйвера
func New(ip string, port int) *Driver {
	endpoint := fmt.Sprintf("http://%s:%d/", ip, port)

	transport := &http.Transport{
		DisableKeepAlives:   false,
		MaxIdleConnsPerHost: 10,
		MaxIdleConns:        100,
		IdleConnTimeout:     90 * time.Second,
	}

	return &Driver{
		endpointURL: endpoint,
		timeout:     5 * time.Second,
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   5 * time.Second,
		},
	}
}

// SetTemplateBody загружает виртуальный шаблон (XML/JSON/Raw) из SQLite
func (d *Driver) SetTemplateBody(body string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.templateBody = body
}

// ============================================================================
// 1. Управление печатью (Боевые методы)
// ============================================================================

func (d *Driver) SelectTemplate(templateName string, fields map[string]string) error {
	d.mu.Lock()
	d.curTemplate = templateName
	rawTpl := d.templateBody
	d.mu.Unlock()

	// Готовим контекст подстановки для шаблонизатора
	data := make(map[string]interface{})
	for k, v := range fields {
		data[k] = v
	}
	data["TemplateName"] = templateName
	data["EventType"] = "PrintSummary"

	payload, err := d.renderTemplate(rawTpl, data)
	if err != nil {
		return fmt.Errorf("ошибка рендеринга виртуального шаблона: %w", err)
	}

	return d.sendPayload(payload)
}

func (d *Driver) PrintBatchIndexed(fieldName string, startIndex int, codes []string) (int, error) {
	d.mu.Lock()
	templateName := d.curTemplate
	rawTpl := d.templateBody
	d.mu.Unlock()

	data := map[string]interface{}{
		"TemplateName": templateName,
		"EventType":    "PrintBatch",
		"FieldName":    fieldName,
		"StartIndex":   startIndex,
		"Codes":        codes,
	}

	payload, err := d.renderTemplate(rawTpl, data)
	if err != nil {
		return 0, fmt.Errorf("ошибка рендеринга пачки: %w", err)
	}

	err = d.sendPayload(payload)
	if err != nil {
		return 0, err
	}

	d.mu.Lock()
	d.lastPrintedIndex = startIndex + len(codes) - 1
	d.mu.Unlock()

	return len(codes), nil
}

func (d *Driver) InitSession(fieldName string, maxQueue int, staticFields map[string]string) error {
	return nil
}

func (d *Driver) PrintTemplate(templateName string, fields map[string]string) error {
	return d.SelectTemplate(templateName, fields)
}

func (d *Driver) UpdateStaticFields(fields map[string]string) error {
	return nil
}

func (d *Driver) ClearQueue() error {
	return nil
}

// ============================================================================
// 2. Телеметрия и мониторинг (Виртуализация под core.Printer)
// ============================================================================

func (d *Driver) GetStatus() (string, error) {
	d.mu.RLock()
	url := d.endpointURL
	d.mu.RUnlock()

	ctx, cancel := context.WithTimeout(context.Background(), d.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodOptions, url, nil)
	if err != nil {
		return "ОФФЛАЙН", err
	}

	resp, err := d.httpClient.Do(req)
	if err != nil {
		return "ОФФЛАЙН", err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return "ГОТОВ", nil
	}

	return fmt.Sprintf("ОФФЛАЙН: HTTP %d", resp.StatusCode), nil
}

func (d *Driver) GetBufferFreeSpace() (int, error) {
	return 999, nil // Аппаратного буфера нет, отдаем свободный канал
}

func (d *Driver) GetLastPrintedIndex() (int, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.lastPrintedIndex, nil
}

func (d *Driver) GetCurrentPrintCount() (string, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return strconv.Itoa(d.lastPrintedIndex), nil
}

func (d *Driver) GetCurrentTemplate() (string, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.curTemplate != "" {
		return d.curTemplate, nil
	}
	return "N/A", nil
}

func (d *Driver) GetTemplates() ([]string, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.curTemplate != "" {
		return []string{d.curTemplate}, nil
	}
	return []string{"VIRTUAL_XML_TEMPLATE"}, nil
}

func (d *Driver) GetTemplateFields(templateName string) ([]string, error) {
	return []string{"PLU", "Weight", "DATAMATRIX"}, nil
}

func (d *Driver) GetRemainingRibbon() (string, error)       { return "N/A", nil }
func (d *Driver) GetPrintSpeed() (string, error)            { return "N/A", nil }
func (d *Driver) GetQueueCapacity(q string) (string, error) { return "N/A", nil }

// ============================================================================
// Вспомогательная логика интерполяции и сети
// ============================================================================

func (d *Driver) renderTemplate(rawBody string, data map[string]interface{}) ([]byte, error) {
	if rawBody == "" {
		// Дефолтный фоллбэк, если шаблон из БД не был передан
		rawBody = `<PrintRequest><Template>{{.TemplateName}}</Template><EventType>{{.EventType}}</EventType></PrintRequest>`
	}

	tmpl, err := template.New("ext_tpl").Parse(rawBody)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

func (d *Driver) sendPayload(payload []byte) error {
	d.mu.RLock()
	url := d.endpointURL
	d.mu.RUnlock()

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewBuffer(payload))
	if err != nil {
		return fmt.Errorf("ошибка создания HTTP-запроса: %w", err)
	}

	req.Header.Set("Content-Type", "application/xml")

	resp, err := d.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("ошибка отправки HTTP-запроса: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("ошибка внешнего сервера (HTTP %d): %s", resp.StatusCode, string(body))
	}

	return nil
}

func (d *Driver) GetTotalPrints() (int64, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return int64(d.lastPrintedIndex), nil
}

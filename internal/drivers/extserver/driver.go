package extserver

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// PrintPayload — структура для XML-обертки запросов печати
type PrintPayload struct {
	XMLName    xml.Name          `xml:"PrintRequest"`
	Template   string            `xml:"Template,omitempty"`
	EventType  string            `xml:"EventType"`
	Fields     map[string]string `xml:"Fields,omitempty"`
	Codes      []string          `xml:"Codes>Code,omitempty"`
	StartIndex int               `xml:"StartIndex,omitempty"`
}

// ServerResponse — структура для парсинга ответов от внешнего Print Engine
type ServerResponse struct {
	XMLName xml.Name `xml:"Response"`
	Status  string   `xml:"Status"`
	Error   string   `xml:"Error,omitempty"`
}

// Driver реализует контракт core.Printer для внешних серверов печати (NiceLabel, MES и др.)
type Driver struct {
	endpointURL string
	timeout     time.Duration
	httpClient  *http.Client

	mu               sync.RWMutex
	curTemplate      string
	customURLSuffix  string
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

// ============================================================================
// 1. Управление печатью (Боевые методы)
// ============================================================================

func (d *Driver) SelectTemplate(template string, fields map[string]string) error {
	d.mu.Lock()
	d.curTemplate = template
	url := d.endpointURL + d.customURLSuffix
	d.mu.Unlock()

	payload := PrintPayload{
		Template:  template,
		EventType: "PrintSummary",
		Fields:    fields,
	}

	return d.sendXMLRequest(url, payload)
}

func (d *Driver) InitSession(fieldName string, maxQueue int, staticFields map[string]string) error {
	return nil
}

func (d *Driver) PrintBatchIndexed(fieldName string, startIndex int, codes []string) (int, error) {
	d.mu.Lock()
	url := d.endpointURL + d.customURLSuffix
	template := d.curTemplate
	d.mu.Unlock()

	payload := PrintPayload{
		Template:   template,
		EventType:  "PrintBatch",
		StartIndex: startIndex,
		Codes:      codes,
		Fields:     map[string]string{"fieldName": fieldName},
	}

	err := d.sendXMLRequest(url, payload)
	if err != nil {
		return 0, err
	}

	d.mu.Lock()
	d.lastPrintedIndex = startIndex + len(codes) - 1
	d.mu.Unlock()

	return len(codes), nil
}

func (d *Driver) PrintTemplate(template string, fields map[string]string) error {
	return d.SelectTemplate(template, fields)
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
	url := d.endpointURL + d.customURLSuffix
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

	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNoContent {
		return "ГОТОВ", nil
	}

	return fmt.Sprintf("ОФФЛАЙН: HTTP %d", resp.StatusCode), nil
}

func (d *Driver) GetBufferFreeSpace() (int, error) {
	return 999, nil
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
	return []string{"EXT_SERVER_TEMPLATE"}, nil
}

func (d *Driver) GetTemplateFields(templateName string) ([]string, error) {
	return []string{"PLU", "Weight", "DATAMATRIX"}, nil
}

func (d *Driver) GetRemainingRibbon() (string, error) {
	return "N/A", nil
}

func (d *Driver) GetPrintSpeed() (string, error) {
	return "N/A", nil
}

func (d *Driver) GetQueueCapacity(queueName string) (string, error) {
	return "N/A", nil
}

func (d *Driver) SetTemplateBody(body string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.customURLSuffix = body
}

// ============================================================================
// Вспомогательные методы
// ============================================================================

func (d *Driver) sendXMLRequest(url string, payload interface{}) error {
	xmlData, err := xml.Marshal(payload)
	if err != nil {
		return fmt.Errorf("ошибка маршалинга XML: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewBuffer(xmlData))
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
		return fmt.Errorf("внешний сервер вернул статус HTTP %d", resp.StatusCode)
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err == nil && len(bodyBytes) > 0 {
		var sResp ServerResponse
		if errUnmarshal := xml.Unmarshal(bodyBytes, &sResp); errUnmarshal == nil {
			if sResp.Status == "error" || sResp.Error != "" {
				return fmt.Errorf("ошибка Print Engine: %s", sResp.Error)
			}
		}
	}

	return nil
}

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
	Name         string
	ID           int
	Address      string
	Port         int
	Timeout      time.Duration
	NiceLabelURL string
	conn         net.Conn
	mu           sync.Mutex
	curTemplate  string
	lastCount    int
	isPumping    bool
	stopPumping  chan struct{}
}

func NewNiceLabelDriver(id int, ip string, port int) *NiceLabelDriver {
	return &NiceLabelDriver{
		ID:           id,
		Address:      ip,
		Port:         port,
		Timeout:      3 * time.Second,
		NiceLabelURL: "http://srv205:10000/",
		conn:         nil,
		mu:           sync.Mutex{},
		curTemplate:  "",
		lastCount:    -1,
		isPumping:    false,
		stopPumping:  make(chan struct{}),
	}
}

func (d *NiceLabelDriver) InitSession(fieldName string, maxQueue int, staticFields map[string]string) error {
	addr := net.JoinHostPort(d.Address, strconv.Itoa(d.Port))

	slog.Info("VALENTIN-MANAGED: Начало детерминированной инициализации сессии", "printer_id", d.ID)

	// ==========================================
	// ЭТАП 0 & 1 & 2: МОНОПОЛЬНЫЙ АНАЛИЗ ДИСКА
	// ==========================================
	d.mu.Lock()
	if d.conn != nil {
		d.conn.Close()
		d.conn = nil
	}

	var err error
	d.conn, err = net.DialTimeout("tcp", addr, d.Timeout)
	if err != nil {
		d.mu.Unlock()
		return fmt.Errorf("ошибка первичного подключения к порту 9100: %w", err)
	}

	// Читаем состояние диска
	if err = d.writeCVPL(fmt.Sprintf("%cFMS---wA%c", SOH, ETB)); err != nil {
		d.closeConnNoLock()
		d.mu.Unlock()
		return fmt.Errorf("ошибка запроса FMS: %w", err)
	}
	_, _ = d.readRawResponse() // Пропускаем ответ состояния (например, <SOH>AA2<ETB>)

	// Считываем оглавление
	if err = d.writeCVPL(fmt.Sprintf("%cFMG---wA%c", SOH, ETB)); err != nil {
		d.closeConnNoLock()
		d.mu.Unlock()
		return fmt.Errorf("ошибка запроса FMG: %w", err)
	}

	rawListing, err := d.readRawResponse()
	if err != nil {
		d.closeConnNoLock()
		d.mu.Unlock()
		return fmt.Errorf("ошибка чтения оглавления диска: %w", err)
	}

	// Проверяем, пуст ли диск от пользовательских макетов
	if d.isDriveDirty(rawListing) {
		slog.Info("VALENTIN-MANAGED: На диске обнаружены старые макеты/графика. Запуск форматирования...")
		if err = d.writeCVPL(fmt.Sprintf("%cFMD---rA:%c", SOH, ETB)); err != nil {
			d.closeConnNoLock()
			d.mu.Unlock()
			return fmt.Errorf("ошибка выполнения форматирования FMD: %w", err)
		}
		time.Sleep(1500 * time.Millisecond) // Технологическое окно на пересоздание FAT
	} else {
		slog.Info("VALENTIN-MANAGED: Диск чист, форматирование пропущено")
	}

	// Освобождаем порт 9100 для NiceLabel Automation
	d.closeConnNoLock()
	d.mu.Unlock()

	// ==========================================
	// ЭТАП 3: ИНТЕГРАЦИЯ С NICELABEL AUTOMATION
	// ==========================================
	staticDate, ok := staticFields["data01"]
	if !ok {
		staticDate = time.Now().Format("02.01.2006")
	}
	printerIDStr := strconv.Itoa(d.ID)
	xmlPayload := fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?><LABEL><action><PRINT>TRUE</PRINT><PRINTERNAME>%s</PRINTERNAME></action><data><plu>%s</plu><date>%s</date></data></LABEL>`, printerIDStr, d.Name, staticDate)

	resp, err := http.Post(d.NiceLabelURL, "application/xml", bytes.NewBufferString(xmlPayload))
	if err != nil {
		return fmt.Errorf("ошибка отправки XML в триггер NiceLabel: %w", err)
	}
	resp.Body.Close()

	slog.Info("VALENTIN-MANAGED: Макет отправлен в Найс, ожидание рендеринга и заливки...")
	time.Sleep(2500 * time.Millisecond) // Окно трансфера файлов Найсом на принтер

	// ==========================================
	// ЭТАП 4: ПОВТОРНЫЙ ПЕРЕХВАТ И ВАЛИДАЦИЯ ЗАЛИВКИ
	// ==========================================
	d.mu.Lock()
	d.conn, err = net.DialTimeout("tcp", addr, d.Timeout)
	if err != nil {
		d.mu.Unlock()
		return fmt.Errorf("ошибка повторного перехвата порта 9100: %w", err)
	}

	if err = d.writeCVPL(fmt.Sprintf("%cFMG---wA%c", SOH, ETB)); err != nil {
		d.closeConnNoLock()
		d.mu.Unlock()
		return fmt.Errorf("ошибка повторного запроса FMG: %w", err)
	}

	freshListing, err := d.readRawResponse()
	if err != nil {
		d.closeConnNoLock()
		d.mu.Unlock()
		return fmt.Errorf("ошибка чтения оглавления после Найса: %w", err)
	}

	// Ищем имя загруженного шаблона на базе расширения .prn или числового имени
	detectedTemplate := d.extractTemplateName(freshListing)
	if detectedTemplate == "" {
		d.closeConnNoLock()
		d.mu.Unlock()
		return fmt.Errorf("критическая ошибка: Найс не залил макет, файл .prn не найден в оглавлении")
	}
	d.curTemplate = detectedTemplate
	slog.Info("VALENTIN-MANAGED: Обнаружен рабочий макет", "template_file", d.curTemplate)

	// ==========================================
	// ЭТАП 5: АКТИВАЦИЯ НАЙДЕННОГО ШАБЛОНА В ОЗУ
	// ==========================================
	d.conn.SetWriteDeadline(time.Now().Add(d.Timeout))
	var layoutPayload bytes.Buffer
	layoutPayload.WriteString(fmt.Sprintf("%cFMB---r%s%c", SOH, d.curTemplate, ETB))
	layoutPayload.WriteString(fmt.Sprintf("%cFBC---r--------%c", SOH, ETB))

	if _, err = d.conn.Write(layoutPayload.Bytes()); err != nil {
		d.closeConnNoLock()
		d.mu.Unlock()
		return fmt.Errorf("ошибка отправки команд активации FMB/FBC: %w", err)
	}

	slog.Info("VALENTIN-MANAGED: Шаблон успешно активирован в ОЗУ термоголовки", "template", d.curTemplate)
	d.mu.Unlock()

	return nil
}

// Вспомогательные методы парсинга и I/O
func (d *NiceLabelDriver) writeCVPL(cmd string) error {
	d.conn.SetWriteDeadline(time.Now().Add(d.Timeout))
	_, err := d.conn.Write([]byte(cmd))
	return err
}

func (d *NiceLabelDriver) readRawResponse() (string, error) {
	d.conn.SetReadDeadline(time.Now().Add(d.Timeout))
	buf := make([]byte, 8192)
	n, err := d.conn.Read(buf)
	if err != nil {
		return "", err
	}
	return string(buf[:n]), nil
}

func (d *NiceLabelDriver) isDriveDirty(listing string) bool {
	lines := strings.Split(listing, string([]byte{ETB}))
	for _, line := range lines {
		clean := strings.Trim(line, string([]byte{SOH, '\r', '\n', ' '}))
		if clean == "" {
			continue
		}
		// Если находим файлы .prn или .png или куски _graphics — диск грязный
		if strings.Contains(clean, ".prn") || strings.Contains(clean, ".png") || strings.Contains(clean, "_graphics") {
			return true
		}
	}
	return false
}

func (d *NiceLabelDriver) extractTemplateName(listing string) string {
	lines := strings.Split(listing, string([]byte{ETB}))
	for _, line := range lines {
		clean := strings.Trim(line, string([]byte{SOH, '\r', '\n', ' '}))
		if clean == "" || strings.Contains(clean, "STANDARD") || strings.Contains(clean, "HOTSTART") {
			continue
		}
		// Ищем строку с целевым макетом, отсекая расширение
		if strings.Contains(clean, ".prn") {
			parts := strings.Fields(clean)
			if len(parts) > 0 {
				fileName := parts[0] // Например "5580.prn"
				return strings.TrimSuffix(fileName, ".prn")
			}
		}
	}
	// Фолбэк на случай, если макет записан без расширения
	return ""
}

func (d *NiceLabelDriver) closeConnNoLock() {
	if d.conn != nil {
		d.conn.Close()
		d.conn = nil
	}
}

// Метод поштучной накачки кодов по немецкой схеме
func (d *NiceLabelDriver) PrintBatchIndexed(fieldName string, startIndex int, codes []string) (int, error) {
	if len(codes) == 0 {
		return 0, nil
	}
	d.mu.Lock()
	if d.isPumping {
		d.mu.Unlock()
		return 0, nil
	}
	d.isPumping = true
	if d.conn == nil {
		d.isPumping = false
		d.mu.Unlock()
		return 0, fmt.Errorf("ошибка пампера: сокет закрыт")
	}

	d.conn.SetWriteDeadline(time.Now().Add(d.Timeout))
	var batchPayload bytes.Buffer
	const hardcodedFieldName = "19"

	for _, code := range codes {
		batchPayload.WriteString(fmt.Sprintf("%cBV[%s]%s%c", SOH, hardcodedFieldName, code, ETB))
		batchPayload.WriteString(fmt.Sprintf("%cFBAA---r5%c", SOH, ETB))
		batchPayload.WriteString(fmt.Sprintf("%cFBBA--r00001%c", SOH, ETB))
		batchPayload.WriteString(fmt.Sprintf("%cFBC---r%c", SOH, ETB))
	}

	if _, err := d.conn.Write(batchPayload.Bytes()); err != nil {
		d.closeConnNoLock()
		d.isPumping = false
		d.mu.Unlock()
		return 0, err
	}
	d.isPumping = false
	d.mu.Unlock()
	return len(codes), nil
}

// Заглушки для обеспечения совместимости с интерфейсом ядра
func (d *NiceLabelDriver) ClearQueue() error                                 { return nil }
func (d *NiceLabelDriver) GetStatus() (string, error)                        { return "ГОТОВ", nil }
func (d *NiceLabelDriver) GetBufferFreeSpace() (int, error)                  { return 1, nil }
func (d *NiceLabelDriver) GetLastPrintedIndex() (int, error)                 { return d.lastCount, nil }
func (d *NiceLabelDriver) UpdateStaticFields(f map[string]string) error      { return nil }
func (d *NiceLabelDriver) PrintTemplate(t string, f map[string]string) error { return nil }
func (d *NiceLabelDriver) GetTemplates() ([]string, error)                   { return []string{d.curTemplate}, nil }
func (d *NiceLabelDriver) GetTemplateFields(t string) ([]string, error)      { return []string{"19"}, nil }
func (d *NiceLabelDriver) GetRemainingRibbon() (string, error)               { return "N/A", nil }
func (d *NiceLabelDriver) GetQueueCapacity(q string) (string, error)         { return "N/A", nil }
func (d *NiceLabelDriver) GetPrintSpeed() (string, error)                    { return "N/A", nil }
func (d *NiceLabelDriver) GetCurrentTemplate() (string, error)               { return d.curTemplate, nil }

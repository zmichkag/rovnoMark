package valentine

import (
	"bytes"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

type NiceLabelDriver struct {
	Name        string        // Имя макета / PLU
	ID          int           // ID принтера в БД
	Address     string        // IP-адрес
	Port        int           // Порт (9100)
	Timeout     time.Duration // Таймаут
	conn        net.Conn      // Монопольная TCP-сессия
	mu          sync.Mutex    // Мьютекс защиты сокета
	curTemplate string        // Активный шаблон
	lastCount   int           // Счетчик FBBC
	isPumping   bool          // Флаг накачки
}

func NewNiceLabelDriver(id int, ip string, port int) *NiceLabelDriver {
	return &NiceLabelDriver{
		ID:          id,
		Address:     ip,
		Port:        port,
		Timeout:     3 * time.Second,
		conn:        nil,
		curTemplate: "",
		lastCount:   -1,
		isPumping:   false,
	}
}

// InitSession проверяет и держит монопольный сокет
func (d *NiceLabelDriver) InitSession(fieldName string, maxQueue int, staticFields map[string]string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.conn != nil {
		slog.Info("VALENTIN-DIRECT: Сессия активна, сокет удерживается", "printer_id", d.ID)
		return nil
	}

	addr := net.JoinHostPort(d.Address, strconv.Itoa(d.Port))
	conn, err := net.DialTimeout("tcp", addr, d.Timeout)
	if err != nil {
		return fmt.Errorf("ошибка подключения к принтеру %s: %w", addr, err)
	}

	d.optimizeSocket(conn)
	d.conn = conn

	slog.Info("VALENTIN-DIRECT: Монопольный TCP-сокет успешно открыт", "printer_id", d.ID, "addr", addr)
	return nil
}

// SelectTemplate вызывается при старте задачи — выбирает шаблон с Flash и взводит его в ОЗУ
func (d *NiceLabelDriver) SelectTemplate(template string, staticFields map[string]string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if template != "" {
		d.curTemplate = template
	} else if d.curTemplate == "" {
		return fmt.Errorf("ошибка: передан пустой код макета (PLU)")
	}

	if d.conn == nil {
		if err := d.reconnectNoLock(); err != nil {
			return fmt.Errorf("ошибка реконнекта при выборе макета: %w", err)
		}
	}

	slog.Info("VALENTIN-DIRECT: Активация шаблона из Flash-памяти", "printer_id", d.ID, "template", d.curTemplate)

	// Формируем команды прямым вызовом по имени шаблона
	cmdSelectLayout := fmt.Sprintf("%cFMB---r%s%c", SOH, d.curTemplate, ETB)
	cmdActivateLayout := fmt.Sprintf("%cFBC---r--------%c", SOH, ETB)

	var layoutPayload bytes.Buffer
	layoutPayload.WriteString(cmdSelectLayout)
	layoutPayload.WriteString(cmdActivateLayout)

	d.conn.SetWriteDeadline(time.Now().Add(d.Timeout))
	d.traceCommand("FMB+FBC (Активация шаблона)", layoutPayload.Bytes())

	if _, err := d.conn.Write(layoutPayload.Bytes()); err != nil {
		d.closeConnNoLock()
		return fmt.Errorf("сбой отправки команд активации макета: %w", err)
	}

	slog.Info("VALENTIN-DIRECT: Шаблон успешно загружен в ОЗУ и взведен", "printer_id", d.ID, "template", d.curTemplate)
	return nil
}

// PrintBatchIndexed отправляет strictly минимальный пакет с кодом маркировки
func (d *NiceLabelDriver) PrintBatchIndexed(fieldName string, startIndex int, codes []string) (int, error) {
	if len(codes) == 0 {
		return 0, nil
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	if d.conn == nil {
		if err := d.reconnectNoLock(); err != nil {
			return 0, fmt.Errorf("ошибка сокета перед реактивным тактом: %w", err)
		}
	}

	// Берем строго 1 код
	targetCode := codes[0]

	// Фильтрация чистейшего криптохвоста
	cleanCode := targetCode
	if idx := strings.Index(cleanCode, "|"); idx != -1 {
		cleanCode = cleanCode[:idx]
	}
	cleanCode = strings.TrimSpace(cleanCode)

	var batchPayload bytes.Buffer

	// 1. Обновляем ТОЛЬКО поле 20 (Честный Знак)
	batchPayload.WriteString(fmt.Sprintf("%cBM[20]%s%c", SOH, cleanCode, ETB))

	// 2. Тираж 1 шт.
	batchPayload.WriteString(fmt.Sprintf("%cFD----r1%c", SOH, ETB))

	// 3. Взвод на датчик
	batchPayload.WriteString(fmt.Sprintf("%cFBC---r--------%c", SOH, ETB))

	d.conn.SetWriteDeadline(time.Now().Add(d.Timeout))
	d.traceCommand(fmt.Sprintf("PUMPER MINIMAL TACT (Индекс: %d)", startIndex), batchPayload.Bytes())

	if _, err := d.conn.Write(batchPayload.Bytes()); err != nil {
		slog.Error("VALENTIN-DIRECT: Сбой отправки минимального кадра", "printer_id", d.ID, "err", err)
		d.closeConnNoLock()
		return 0, err
	}

	slog.Info("VALENTIN-DIRECT: Код BM[20] взведен на датчик", "printer_id", d.ID, "index", startIndex)
	return 1, nil
}

// GetCurrentPrintCount опрашивает счетчик отпечатанных этикеток FBBC
func (d *NiceLabelDriver) GetCurrentPrintCount() (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.conn == nil {
		if err := d.reconnectNoLock(); err != nil {
			return "0", fmt.Errorf("сокет закрыт: %w", err)
		}
	}

	d.conn.SetDeadline(time.Now().Add(d.Timeout))
	cmd := fmt.Sprintf("%cFBBC--w%c", SOH, ETB)

	if _, err := d.conn.Write([]byte(cmd)); err != nil {
		d.closeConnNoLock()
		return "0", err
	}

	buf := make([]byte, 64)
	n, err := d.conn.Read(buf)
	if err != nil {
		if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
			return "0", nil
		}
		d.closeConnNoLock()
		return "0", err
	}

	cleanResp := strings.Trim(string(buf[:n]), string([]byte{SOH, byte(ETB), '\r', '\n', ' '}))
	if strings.HasPrefix(cleanResp, "A") {
		parts := strings.Split(cleanResp, "A")
		if len(parts) > 1 {
			return strings.TrimSpace(parts[1]), nil
		}
	}

	return "0", nil
}

func (d *NiceLabelDriver) optimizeSocket(conn net.Conn) {
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		_ = tcpConn.SetNoDelay(true)
		_ = tcpConn.SetKeepAlive(true)
		_ = tcpConn.SetKeepAlivePeriod(5 * time.Second)
	}
}

func (d *NiceLabelDriver) reconnectNoLock() error {
	addr := net.JoinHostPort(d.Address, strconv.Itoa(d.Port))
	conn, err := net.DialTimeout("tcp", addr, d.Timeout)
	if err != nil {
		return err
	}
	d.optimizeSocket(conn)
	d.conn = conn
	return nil
}

func (d *NiceLabelDriver) closeConnNoLock() {
	if d.conn != nil {
		d.conn.Close()
		d.conn = nil
	}
}

func (d *NiceLabelDriver) traceCommand(desc string, data []byte) {
	view := string(data)
	view = strings.ReplaceAll(view, string([]byte{SOH}), "[SOH]")
	view = strings.ReplaceAll(view, string([]byte{ETB}), "[ETB]")
	view = strings.ReplaceAll(view, "\r", "[CR]")
	view = strings.ReplaceAll(view, "\n", "[LF]")

	slog.Info("VALENTIN-TRACE [КОМАНДА В ПОРТ]: "+desc,
		"printer_id", d.ID,
		"ascii_payload", view,
		"hex_dump", fmt.Sprintf("%x", data),
	)
}

// Заглушки совместимости интерфейса
func (d *NiceLabelDriver) ClearQueue() error                                 { return nil }
func (d *NiceLabelDriver) GetStatus() (string, error)                        { return "ГОТОВ", nil }
func (d *NiceLabelDriver) GetBufferFreeSpace() (int, error)                  { return 1, nil }
func (d *NiceLabelDriver) GetLastPrintedIndex() (int, error)                 { return d.lastCount, nil }
func (d *NiceLabelDriver) UpdateStaticFields(f map[string]string) error      { return nil }
func (d *NiceLabelDriver) PrintTemplate(t string, f map[string]string) error { return nil }
func (d *NiceLabelDriver) GetTemplates() ([]string, error)                   { return []string{d.curTemplate}, nil }
func (d *NiceLabelDriver) GetTemplateFields(t string) ([]string, error) {
	return []string{"18", "19", "20"}, nil
}
func (d *NiceLabelDriver) GetRemainingRibbon() (string, error)       { return "N/A", nil }
func (d *NiceLabelDriver) GetQueueCapacity(q string) (string, error) { return "N/A", nil }
func (d *NiceLabelDriver) GetPrintSpeed() (string, error)            { return "N/A", nil }
func (d *NiceLabelDriver) GetCurrentTemplate() (string, error)       { return d.curTemplate, nil }

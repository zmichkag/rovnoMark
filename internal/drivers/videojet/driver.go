package videojet

import (
	"bufio"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Driver struct {
	Address     string
	Port        int
	Timeout     time.Duration
	mu          sync.Mutex
	currstate   string
	CurTemplate string
}

func New(ip string, port int) *Driver {
	return &Driver{
		Address: ip,
		Port:    port,
		Timeout: 3 * time.Second,
	}
}

// sendRaw
func (d *Driver) sendRaw(cmd string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	// Склеиваем IP и Port. %d — формат для целого числа.
	address := net.JoinHostPort(d.Address, strconv.Itoa(d.Port))

	conn, err := net.DialTimeout("tcp", address, d.Timeout)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	//log.Printf("[VIDEOJET %s] Посылаю: %s", d.Address, cmd)

	// Videojet требует терминатор \r
	_, err = conn.Write([]byte(cmd + "\r"))
	if err != nil {
		return "", err
		log.Printf("[VIDEOJET %s] поломалось %s", d.Address, cmd)
	}

	conn.SetReadDeadline(time.Now().Add(d.Timeout))
	//log.Printf("[VIDEOJET %s] Жду ответа...", d.Address)
	// Читаем до символа \r (терминатор ответа)
	reader := bufio.NewReader(conn)
	reply, err := reader.ReadString('\r')
	if err != nil {
		return "", err
	}
	//log.Printf("[VIDEOJET %s] SEND: %q, REPLY: %s", d.Address, cmd, reply)
	//log.Printf("[VIDEOJET %s] SEND: %q, REPLY: %s", d.Address, cmd, reply)
	return strings.TrimSpace(reply), nil
}

// GetStatus запрашивает GST и разбирает состояние [cite: 426]
func (d *Driver) GetStatus() (string, error) {
	// Ответ выглядит так: STS |overall|error|job|batch|total|
	raw, err := d.sendRaw("GST")
	if err != nil {
		return "", err
	}

	d.currstate = raw

	parts := strings.Split(raw, "|")
	if len(parts) < 2 {
		return "ОШИБКА ПРОТОКОЛА", nil
	}

	// overallstate: 0-Shutdown, 3-Running, 4-Offline [cite: 1256-1260]
	// errorstate: 0-No errors, 1-Warnings, 2-Faults [cite: 1300-1302]
	stateCode := parts[1]
	errorCode := ""
	if len(parts) > 2 {
		errorCode = parts[2]
	}

	switch stateCode {
	case "0":
		return "ВЫКЛЮЧЕН", nil
	case "1":
		return "НЕ ГОТОВ", nil
	case "2":
		return "ГОТОВ", nil
	case "3":
		if errorCode == "2" {
			return "ПЕЧАТЬ (ОШИБКА)", nil
		}
		return "ПЕЧАТЬ", nil
	case "4":
		return "ГОТОВ", nil
	default:
		return "НЕИЗВЕСТНО", nil
	}
}

// GetRemainingRibbon использует команду GCL (Consumable Levels) [cite: 1086]
func (d *Driver) GetRemainingRibbon() (string, error) {
	raw, err := d.sendRaw("GST")
	if err != nil {
		return "", err
	}
	// Формат ответа: GCL <уровень> [cite: 1102]
	//log.Printf("[VIDEOJET %s] RAW: %s", d.Address, raw)
	return strings.TrimPrefix(raw, "GST "), nil
}

// GetQueueCapacity запрашивает QSZ (Queue Size) [cite: 673]
func (d *Driver) GetQueueCapacity(queueName string) (string, error) {
	raw, err := d.sendRaw("QSZ")
	if err != nil {
		return "", err
	}
	//log.Printf("[VIDEOJET %s] RAW: %s", d.Address, raw)
	// Ответ: QSZ | <nn> | <s> | [cite: 678]
	parts := strings.Split(raw, "|")
	if len(parts) >= 2 {
		return strings.TrimSpace(parts[1]), nil
	}
	return "0", nil
}

// PrintTemplate выполняет выбор задания (SLA) и команду печати (PRN) [cite: 123, 347]
func (d *Driver) PrintTemplate(template string, fields map[string]string) error {
	// 1. Формируем команду выбора задания с полями [cite: 123]
	// SLA |имя|поле1=значение|поле2=значение|
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("SLA|%s", template))
	for k, v := range fields {
		sb.WriteString(fmt.Sprintf("|%s=%s", k, v))
	}
	sb.WriteString("|")

	res, err := d.sendRaw(sb.String())
	if err != nil || res == "ERR" {
		return fmt.Errorf("ошибка выбора задания: %v", err)
	}

	// 2. Команда на физическую печать [cite: 347]
	_, err = d.sendRaw("PRN")
	return err
}

func (d *Driver) GetPrintSpeed() (string, error) {
	return "N/A", nil
}

func (d *Driver) GetCurrentPrintCount() (string, error) {
	if d.currstate == "" {
		return "0", nil
	}

	parts := strings.Split(d.currstate, "|")
	if len(parts) < 5 {
		return "0", nil
	}

	return strings.TrimSpace(parts[4]), nil
}

func (d *Driver) GetCurrentTemplate() (string, error) {
	if d.currstate == "" {
		return "0", nil
	}

	parts := strings.Split(d.currstate, "|")
	if len(parts) < 5 {
		return "0", nil
	}

	return strings.TrimSpace(parts[3]), nil
}

func (d *Driver) PrintBatch(fieldName string, codes []string) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	address := net.JoinHostPort(d.Address, strconv.Itoa(d.Port))
	log.Printf("[VIDEOJET %s] Старт полной прогрузки пачки (%d шт.)", d.Address, len(codes))

	conn, err := net.DialTimeout("tcp", address, 5*time.Second)
	if err != nil {
		return 0, fmt.Errorf("ошибка связи: %v", err)
	}
	defer conn.Close()

	buf := make([]byte, 1024)
	conn.SetDeadline(time.Now().Add(3 * time.Second))

	// 1. Очистка буфера (SCB)
	conn.Write([]byte("\rSCB\r"))
	n, _ := conn.Read(buf)
	log.Printf("[VIDEOJET %s] 1. SCB: %q", d.Address, string(buf[:n]))

	time.Sleep(100 * time.Millisecond)

	// 2. Выбор шаблона (SLA)[cite: 5]
	templateName := "CHZ"
	conn.Write([]byte(fmt.Sprintf("SLA|%s|\r", templateName)))
	n, _ = conn.Read(buf)
	log.Printf("[VIDEOJET %s] 2. SLA (%s): %q", d.Address, templateName, string(buf[:n]))

	// 3. Настройка поля (SHO)[cite: 5]
	conn.Write([]byte(fmt.Sprintf("SHO|%s|\r", fieldName)))
	n, _ = conn.Read(buf)
	log.Printf("[VIDEOJET %s] 3. SHO (%s): %q", d.Address, fieldName, string(buf[:n]))

	// 4. Лимит записей (SMR)[cite: 5]
	limit := len(codes) + 50
	conn.Write([]byte(fmt.Sprintf("SMR|%d|\r", limit)))
	n, _ = conn.Read(buf)
	log.Printf("[VIDEOJET %s] 4. SMR (%d): %q", d.Address, limit, string(buf[:n]))

	// 5. Финальный цикл загрузки (SDO)
	successCount := 0
	log.Printf("[VIDEOJET %s] 5. Начало потоковой отправки...", d.Address)

	for i, code := range codes {
		// --- ПАРСИНГ И ЭКРАНИРОВАНИЕ ---
		// 1. Дублируем тильды, чтобы они напечатались как текст
		cleanCode := strings.ReplaceAll(code, "~", "~~")
		// 2. Экранируем вертикальную черту (разделитель полей Zipher)
		cleanCode = strings.ReplaceAll(cleanCode, "|", "~|")
		// 3. Заменяем спецсимвол GS (0x1d) на управляющий символ FNC1 (~1)[cite: 3]
		cleanCode = strings.ReplaceAll(cleanCode, "\x1d", "~1")

		// Команда SDO отправляет данные в поля, заданные через SHO[cite: 5]
		cmd := fmt.Sprintf("SDO|%s|\r", cleanCode)

		conn.SetDeadline(time.Now().Add(2 * time.Second))
		_, err := conn.Write([]byte(cmd))
		if err != nil {
			log.Printf("[VIDEOJET %s] КРИТИЧНО: Обрыв связи на коде #%d: %v", d.Address, i+1, err)
			break
		}

		// Читаем ответ принтера[cite: 3, 5]
		n, err = conn.Read(buf)
		response := string(buf[:n])

		log.Printf("[VIDEOJET %s] > %q", d.Address, strings.TrimSpace(response))

		if err != nil || strings.Contains(response, "ERR") {
			log.Printf("[VIDEOJET %s] ОШИБКА: Принтер отклонил код #%d [%s]. Ответ: %q", d.Address, i+1, cleanCode, strings.TrimSpace(response))
			break
		}

		log.Printf("[VIDEOJET %s] Код #%d (%q) OK. Ответ: %q", d.Address, i+1, cleanCode, strings.TrimSpace(response))

		successCount++

		// Небольшая пауза 10мс, чтобы контроллер Videojet успевал записывать данные в Flash[cite: 3]
		time.Sleep(10 * time.Millisecond)
	}

	log.Printf("[VIDEOJET %s] Цикл завершен. Успешно: %d/%d", d.Address, successCount, len(codes))
	return successCount, nil
}

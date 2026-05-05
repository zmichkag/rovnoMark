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
	log.Printf("[VIDEOJET %s] Посылаю: %s", d.Address, cmd)

	// Videojet требует терминатор \r
	_, err = conn.Write([]byte(cmd + "\r"))
	if err != nil {
		return "", err
		log.Printf("[VIDEOJET %s] поломалось %s", d.Address, cmd)
	}

	conn.SetReadDeadline(time.Now().Add(d.Timeout))
	log.Printf("[VIDEOJET %s] Жду ответа...", d.Address)
	// Читаем до символа \r (терминатор ответа)
	reader := bufio.NewReader(conn)
	reply, err := reader.ReadString('\r')
	if err != nil {
		return "", err
	}
	log.Printf("[VIDEOJET %s] SEND: %q, REPLY: %s", d.Address, cmd, reply)
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
	log.Printf("[VIDEOJET %s] RAW: %s", d.Address, raw)
	return strings.TrimPrefix(raw, "GST "), nil
}

// GetQueueCapacity запрашивает QSZ (Queue Size) [cite: 673]
func (d *Driver) GetQueueCapacity(queueName string) (string, error) {
	raw, err := d.sendRaw("QSZ")
	if err != nil {
		return "", err
	}
	log.Printf("[VIDEOJET %s] RAW: %s", d.Address, raw)
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
	conn, err := net.DialTimeout("tcp", address, 10*time.Second)
	if err != nil {
		return 0, err
	}
	defer conn.Close()

	reader := bufio.NewReader(conn)
	conn.Write([]byte("\r")) // Сброс парсера

	// 1. Очищаем старый буфер сериализации (команда SCB)
	fmt.Fprint(conn, "SCB\r")
	reader.ReadString('\r')

	// 2. Устанавливаем лимит записей (например, до 2000), чтобы не упереться в память
	fmt.Fprintf(conn, "SMR|%d|\r", 2000)
	reader.ReadString('\r')

	// 3. Объявляем, для какого поля мы будем слать данные (команда SHO)
	// Синтаксис: SHO | <имя_поля> |
	fmt.Fprintf(conn, "SHO|%s|\r", fieldName)
	resp, _ := reader.ReadString('\r')
	if strings.Contains(resp, "ERR") {
		return 0, fmt.Errorf("поле %s не поддерживает сериализацию", fieldName)
	}

	successCount := 0
	for _, code := range codes {
		// Подготовка кода для GS1 (замена \x1d на ~1)
		cleanCode := strings.ReplaceAll(code, "\x1d", "~1")

		// 4. Заливаем данные в буфер (команда SDO)
		// Синтаксис: SDO | <данные> |
		fmt.Fprintf(conn, "SDO|%s|\r", cleanCode)

		// При успехе SDO возвращает количество свободного места (SFS)
		resp, err := reader.ReadString('\r')
		if err != nil || strings.Contains(resp, "ERR") {
			log.Printf("[SERIAL %s] Ошибка загрузки кода %d", d.Address, successCount)
			break
		}
		successCount++
	}

	return successCount, nil
}

//func (d *Driver) PrintBatch(fieldName string, codes []string) (int, error) {
//	d.mu.Lock()
//	defer d.mu.Unlock()
//
//	address := net.JoinHostPort(d.Address, strconv.Itoa(d.Port))
//
//	// 1. Увеличиваем общий таймаут на подключение до 10 секунд
//	conn, err := net.DialTimeout("tcp", address, 10*time.Second)
//	if err != nil {
//		return 0, fmt.Errorf("ошибка подключения: %v", err)
//	}
//	defer conn.Close()
//
//	successCount := 0
//	reader := bufio.NewReader(conn)
//
//	// Сброс буфера принтера (рекомендация Videojet)
//	conn.Write([]byte("\r"))
//
//	for _, code := range codes {
//		// Заменяем спецсимвол GS на ~1, как в вашем рабочем шаблоне
//		cleanCode := strings.ReplaceAll(code, "\x1d", "~1")
//
//		// Формируем команду JDI
//		cmd := fmt.Sprintf("JDI|1|%s=%s|\r", fieldName, cleanCode)
//
//		// Устанавливаем дедлайн на КАЖДУЮ команду (1 секунда)
//		conn.SetDeadline(time.Now().Add(1 * time.Second))
//
//		_, err := conn.Write([]byte(cmd))
//		if err != nil {
//			log.Printf("[VIDEOJET %s] Оборвалась связь на коде %d: %v", d.Address, successCount, err)
//			break
//		}
//
//		// Ждем подтверждение (ID задания)
//		resp, err := reader.ReadString('\r')
//		if err != nil {
//			log.Printf("[VIDEOJET %s] Принтер не ответил на код %d: %v", d.Address, successCount, err)
//			break
//		}
//
//		// Если в ответе есть число — код принят[cite: 2]
//		if resp != "ERR\r" && resp != "NACK\r" && resp != "" {
//			successCount++
//		}
//
//		// ВАЖНО: Пауза 20мс. Даем принтеру время записать код в очередь.
//		// Без этой паузы на больших пачках Videojet может "зависнуть".
//		time.Sleep(20 * time.Millisecond)
//	}
//
//	return successCount, nil
//}

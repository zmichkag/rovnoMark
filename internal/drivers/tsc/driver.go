package tsc

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
	Address     string
	Port        int
	Timeout     time.Duration
	mu          sync.Mutex
	currstate   string
	CurTemplate string
	conn        net.Conn
}

// New создает новый экземпляр драйвера TSC.
// Стандартный порт для JetDirect/TCP-печати обычно 9100.
func New(ip string, port int) *Driver {
	return &Driver{
		Address: ip,
		Port:    port,
		Timeout: 3 * time.Second,
	}
}

// sendRaw — низкоуровневый обмен данными с принтером по протоколу TSPL
func (d *Driver) sendRaw(cmd string, waitReply bool) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	// 1. Инициализация/восстановление соединения
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

	// 2. Отправка команды. В TSPL команды разделяются переводом строки (\r\n)
	_, err := d.conn.Write([]byte(cmd + "\r\n"))
	if err != nil {
		d.closeConn()
		return "", err
	}

	if !waitReply {
		return "", nil
	}

	// 3. Чтение ответа (актуально для команд статуса вроде <ESC>!? или ~!@)
	// Принтеры TSC на статус-команды обычно отвечают 1 байтом состояния.
	buf := make([]byte, 10)
	n, err := d.conn.Read(buf)
	if err != nil {
		d.closeConn() // При таймауте сбрасываем сокет
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

// GetStatus запрашивает состояние принтера с помощью стандартной команды TSPL <ESC>!?
// Возвращает байт статуса, который мы транслируем в понятные системе RovnoMark состояния.
func (d *Driver) GetStatus() (string, error) {
	// \x1b!? — это статус-запрос в TSPL. Возвращает 1 байт flags.
	resp, err := d.sendRaw("\x1b!?", true)
	if err != nil {
		return "ОШИБКА СВЯЗИ", err
	}

	if len(resp) == 0 {
		return "НЕИЗВЕСТНО", nil
	}

	statusByte := resp[0]
	d.currstate = fmt.Sprintf("%d", statusByte)

	// Разбор битовой маски статуса TSC:
	// Bit 0: Normal (Ready)
	// Bit 1: Head opened
	// Bit 2: Paper jam
	// Bit 3: Out of paper
	// Bit 4: Out of ribbon
	// Bit 5: Pause
	// Bit 6: Printing
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

// PrintTemplate выполняет отправку готового шаблона (дизайна) с замененными переменными
func (d *Driver) PrintTemplate(templateName string, fields map[string]string) error {
	var sb strings.Builder

	// Очистка буфера принтера перед формированием этикетки
	sb.WriteString("CLS\r\n")

	// В TSPL динамический текст выводится через команду TEXT x,y,"font",rotation,x-multi,y-multi,"content"
	// Для демонстрации генерируем простую структуру. В продакшене здесь может быть загрузка пред-скомпилированного .BAS макета
	for name, value := range fields {
		// Пример: вывод полей в виде строк. Координаты и параметры подставляются условно
		sb.WriteString(fmt.Sprintf("TEXT 50,50,\"ROMAN.TTF\",0,1,1,\"%s: %s\"\r\n", name, value))
	}

	// Команда печати: PRINT m[,n] -> PRINT 1,1 (Напечатать 1 копию)
	sb.WriteString("PRINT 1,1\r\n")

	_, err := d.sendRaw(sb.String(), false)
	return err
}

// SelectTemplate запоминает имя текущего макета в памяти драйвера
func (d *Driver) SelectTemplate(name string, fields map[string]string) error {
	d.CurTemplate = name
	if len(fields) > 0 {
		return d.PrintTemplate(name, fields)
	}
	return nil
}

// PrintBatch реализует пакетную отправку кодов (например, DataMatrix маркировки Честный Знак)
func (d *Driver) PrintBatch(fieldName string, codes []string) (int, error) {
	successCount := 0

	for _, code := range codes {
		var sb strings.Builder
		sb.WriteString("CLS\r\n")

		// Очищаем код от лишних символов, подготавливаем GS1 разделители если нужно
		cleanCode := strings.ReplaceAll(code, "\x1d", "{FNC1}")

		// Формируем команду печати 2D-кода DataMatrix: DMATRIX x,y,width,height,[content]
		// Параметры подбираются под физическое разрешение принтера (например, 203 или 300 dpi)
		sb.WriteString(fmt.Sprintf("DMATRIX 50,50,400,400,\"%s\"\r\n", cleanCode))

		// Дополнительно печатаем текстовое представление под кодом
		sb.WriteString(fmt.Sprintf("TEXT 50,460,\"3\",0,1,1,\"%s\"\r\n", fieldName))

		sb.WriteString("PRINT 1,1\r\n")

		_, err := d.sendRaw(sb.String(), false)
		if err != nil {
			slog.Error("TSC PrintBatch Item Failed", "ip", d.Address, "err", err)
			break
		}
		successCount++
	}

	return successCount, nil
}

// Заглушки под методы интерфейса Printer для обеспечения совместимости с аппаратной абстракцией

func (d *Driver) GetRemainingRibbon() (string, error) {
	// TSPL стандартно не возвращает точный % остатка ленты в цифровом виде
	return "N/A", nil
}

func (d *Driver) GetPrintSpeed() (string, error) {
	return "N/A", nil
}

func (d *Driver) GetCurrentPrintCount() (string, error) {
	return "N/A", nil
}

func (d *Driver) GetCurrentTemplate() (string, error) {
	if d.CurTemplate == "" {
		return "N/A", nil
	}
	return d.CurTemplate, nil
}

func (d *Driver) ClearQueue() error {
	// У TSC нет очереди в понимании Videojet, просто очищаем текущий буфер команд
	_, err := d.sendRaw("CLS", false)
	return err
}

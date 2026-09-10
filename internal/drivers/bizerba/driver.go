package bizerba

import (
	"fmt"
	"log/slog"
	"rovnoMark/internal/core/marking"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultRequestTimeout = 3 * time.Second
	receivePollTimeout    = 200 * time.Millisecond
	spontaneousQueue      = "DUSTBIN"
)

type queuedMark struct {
	index  int
	code   string
	serial string
	crypto string
}

type sessionRequest struct {
	prepared *preparedSession
	kind     string
	marks    []queuedMark
	call     func(bcsConnection) error
	resp     chan sessionResponse
}

type sessionResponse struct {
	value int
	err   error
}

type markSession struct {
	requests chan sessionRequest
	done     chan struct{}
	start    chan error
	errMu    sync.RWMutex
	err      error
}

type preparedSession struct {
	capacity     int
	staticFields map[string]string
}

// Driver управляет Bizerba через COM Automation API службы BCS ConnectService.
// Параметры host и port сохранены в New только для совместимости с общей конфигурацией принтеров.
type Driver struct {
	device    string
	timeout   time.Duration
	newConn   connectionFactory
	equipment bizerbaEquipment

	transientMu   sync.Mutex
	startMu       sync.Mutex
	mu            sync.RWMutex
	session       *markSession
	prepared      *preparedSession
	sessionErr    error
	lastPrinted   int
	currentPLU    string
	fields        map[string]string
	lastPLURead   time.Time
	recordGXNET   bool
	capturePaused bool
	closed        bool
	captureStop   chan struct{}
	captureDone   chan struct{}
}

// New создаёт драйвер для устройства с указанным именем подключения в BCS.
// Значения host и port для COM-драйвера не используются.
func New(_ string, _ int, device string) *Driver {
	return newDriver(device, newBCSConnection)
}

// NewWithProfile создаёт драйвер с Bizerba-специфичным профилем оборудования.
func NewWithProfile(_ string, _ int, device string, profile Profile) *Driver {
	return newDriverWithProfile(device, newBCSConnection, profile)
}

// newDriver создаёт драйвер с заданной фабрикой COM-соединений.
// Отдельная фабрика позволяет проверять логику драйвера без настоящего оборудования.
func newDriver(device string, factory connectionFactory) *Driver {
	return newDriverWithProfile(device, factory, Profile{})
}

// newDriverWithProfile создаёт тестируемый драйвер с заданным профилем оборудования.
func newDriverWithProfile(device string, factory connectionFactory, profile Profile) *Driver {
	d := &Driver{device: strings.TrimSpace(device), timeout: defaultRequestTimeout,
		recordGXNET:   profile.RecordGXNET && profile.RecordResponse != nil,
		capturePaused: true,
		equipment:     configuredBizerbaEquipment(profile), lastPrinted: -1, fields: make(map[string]string)}
	if profile.RecordGXNET && profile.RecordResponse != nil {
		baseFactory := factory
		factory = func() (bcsConnection, error) {
			conn, err := baseFactory()
			if err != nil {
				return nil, err
			}
			return &recordingConnection{bcsConnection: conn, record: func(r Response) error {
				d.mu.RLock()
				paused := d.capturePaused || d.closed
				d.mu.RUnlock()
				if paused {
					return nil
				}
				return profile.RecordResponse(r)
			}, requests: make(map[string]gxnetRequest)}, nil
		}
	}
	d.newConn = factory
	return d
}

// withConnection выполняет одну операцию в коротком COM-сеансе без спонтанных сообщений.
func (d *Driver) withConnection(fn func(bcsConnection) error) error {
	if d.device == "" {
		return fmt.Errorf("не задано имя устройства Bizerba в BCS")
	}
	d.transientMu.Lock()
	defer d.transientMu.Unlock()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	conn, err := d.newConn()
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := conn.Open(d.device, d.device, false); err != nil {
		return err
	}
	return fn(conn)
}

// sendWrite отправляет команду записи и проверяет статус, возвращённый BCS.
func sendWrite(conn bcsConnection, header, data string, timeout time.Duration) error {
	_, status, err := conn.Send(header, data, timeout)
	if err != nil {
		return err
	}
	if status != bcsStatusOK {
		return fmt.Errorf("BCS Send(%q): получен статус %d", header, status)
	}
	return nil
}

// executeQuery отправляет команду чтения и получает ответ по выданному BCS дескриптору.
func executeQuery(conn bcsConnection, command, parameter string, timeout time.Duration) (string, error) {
	handle, status, err := conn.Send(command, parameter, timeout)
	if err != nil {
		return "", err
	}
	if (status != bcsStatusOK && status != bcsStatusMore) || strings.TrimSpace(handle) == "" {
		return "", fmt.Errorf("BCS Send(%q): получен статус %d и дескриптор %q", command, status, handle)
	}
	payload, status, err := conn.ReceiveOne(handle, timeout)
	if err != nil {
		return "", err
	}
	if status != bcsStatusOK && status != bcsStatusMore {
		return "", fmt.Errorf("BCS ReceiveOne(%q): получен статус %d", handle, status)
	}
	return payload, nil
}

// runCommand выполняет команду в активной сессии марок либо открывает короткий COM-сеанс.
func (d *Driver) runCommand(fn func(bcsConnection) error) error {
	if d.recordGXNET {
		d.startMu.Lock()
		err := d.ensureCaptureLocked()
		d.startMu.Unlock()
		if err != nil {
			return err
		}
	}
	d.mu.RLock()
	s := d.session
	closed := d.closed
	d.mu.RUnlock()
	if closed {
		return fmt.Errorf("драйвер Bizerba закрыт")
	}
	if s != nil {
		return s.call(fn)
	}
	return d.withConnection(fn)
}

// call передаёт произвольную операцию в поток активной COM-сессии.
func (s *markSession) call(fn func(bcsConnection) error) error {
	_, err := s.request(sessionRequest{kind: "call", call: fn})
	return err
}

// request передаёт управляющий запрос в сессию и ожидает результат или её завершение.
func (s *markSession) request(req sessionRequest) (int, error) {
	req.resp = make(chan sessionResponse, 1)
	select {
	case s.requests <- req:
	case <-s.done:
		return 0, s.closedError()
	}
	select {
	case result := <-req.resp:
		return result.value, result.err
	case <-s.done:
		select {
		case result := <-req.resp:
			return result.value, result.err
		default:
			return 0, s.closedError()
		}
	}
}

// setError сохраняет последнюю ошибку сессии.
func (s *markSession) setError(err error) {
	if err == nil {
		return
	}
	s.errMu.Lock()
	s.err = err
	s.errMu.Unlock()
}

// failure возвращает сохранённую ошибку сессии.
func (s *markSession) failure() error {
	s.errMu.RLock()
	defer s.errMu.RUnlock()
	return s.err
}

// closedError возвращает причину аварийного завершения либо сообщение о закрытой сессии.
func (s *markSession) closedError() error {
	if err := s.failure(); err != nil {
		return err
	}
	return fmt.Errorf("сессия передачи марок Bizerba закрыта")
}

// SelectTemplate устанавливает на устройстве выбранный PLU и статические поля.
func (d *Driver) SelectTemplate(plu string, fields map[string]string) error {
	plu = strings.TrimSpace(plu)
	if plu == "" {
		return fmt.Errorf("не задан код PLU Bizerba")
	}
	err := d.runCommand(func(conn bcsConnection) error {
		if loadErr := sendWrite(conn, "A!XV00|GL19|LX02", plu, d.timeout); loadErr != nil {
			// Some GLPMax versions apply GL19 but report "Datensatz nicht vorhanden"
			// for the LX02 screen refresh. Accept the operation only after reading
			// the selected PLU back from the device.
			response, verifyErr := executeQuery(conn, "A?GL19", "0", d.timeout)
			loadedPLU, parseErr := ParseFieldResponse(response, "GL19")
			if verifyErr != nil || parseErr != nil || loadedPLU != plu {
				return fmt.Errorf("ошибка установки PLU Bizerba %q: %w", plu, loadErr)
			}
			slog.Warn("Bizerba загрузила PLU, но не выполнила обновление экрана", "device", d.device, "plu", plu, "err", loadErr)
		}
		return d.updateStaticFieldsOn(conn, fields)
	})
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.currentPLU, d.fields, d.lastPLURead = plu, cloneFields(fields), time.Now()
	d.mu.Unlock()
	return nil
}

// PrintTemplate выбирает PLU и заполняет его статические поля.
func (d *Driver) PrintTemplate(plu string, fields map[string]string) error {
	return d.SelectTemplate(plu, fields)
}

// UpdateStaticFields обновляет поддерживаемые статические поля текущего PLU.
func (d *Driver) UpdateStaticFields(fields map[string]string) error {
	if err := d.runCommand(func(conn bcsConnection) error { return d.updateStaticFieldsOn(conn, fields) }); err != nil {
		return err
	}
	d.mu.Lock()
	for key, value := range fields {
		d.fields[key] = value
	}
	d.mu.Unlock()
	return nil
}

// updateStaticFieldsOn записывает дату в GL06 и бригаду в GL15 через готовое соединение.
func (d *Driver) updateStaticFieldsOn(conn bcsConnection, fields map[string]string) error {
	if date := firstField(fields, "date", "production_date", "GL06", "дата"); date != "" {
		formatted, err := normalizeBizerbaDate(date)
		if err != nil {
			return err
		}
		if err := sendWrite(conn, "A!GL06", formatted, d.timeout); err != nil {
			return fmt.Errorf("ошибка установки даты Bizerba %q: %w", formatted, err)
		}
	}
	if brigade := firstField(fields, "brigade", "team", "GL15", "бригада"); brigade != "" {
		if err := sendWrite(conn, "A!GL15", brigade, d.timeout); err != nil {
			return fmt.Errorf("ошибка установки бригады Bizerba %q: %w", brigade, err)
		}
	}
	return nil
}

// GetCurrentTemplate возвращает текущий PLU, используя кратковременный кэш чтения.
func (d *Driver) GetCurrentTemplate() (string, error) {
	d.mu.RLock()
	cached, readAt := d.currentPLU, d.lastPLURead
	d.mu.RUnlock()
	if cached != "" && time.Since(readAt) < 3*time.Second {
		return cached, nil
	}
	var response string
	err := d.runCommand(func(conn bcsConnection) error {
		var err error
		response, err = executeQuery(conn, "A?GL19", "0", d.timeout)
		return err
	})
	if err != nil {
		return "", fmt.Errorf("ошибка чтения PLU Bizerba: %w", err)
	}
	plu, err := ParseFieldResponse(response, "GL19")
	if err != nil {
		return "", err
	}
	d.mu.Lock()
	d.currentPLU, d.lastPLURead = plu, time.Now()
	d.mu.Unlock()
	return plu, nil
}

// ParseFieldResponse извлекает значение ожидаемого поля из ответа протокола Bizerba.
func ParseFieldResponse(response, expectedField string) (string, error) {
	parts := strings.SplitN(strings.TrimSpace(response), "|", 2)
	if len(parts) != 2 || strings.TrimSpace(parts[0]) != "A!"+expectedField {
		return "", fmt.Errorf("неожиданный ответ Bizerba %q для поля %s", response, expectedField)
	}
	value := strings.TrimSpace(parts[1])
	if value == "" {
		return "", fmt.Errorf("поле Bizerba %s пустое", expectedField)
	}
	return value, nil
}

// GetStatus проверяет доступность устройства чтением общего состояния PL03.
// Проверка не зависит от того, выбран ли на машине PLU и существует ли его текст.
func (d *Driver) GetStatus() (string, error) {
	var response string
	err := d.runCommand(func(conn bcsConnection) error {
		var err error
		response, err = executeQuery(conn, "A?PL03", "0", d.timeout)
		return err
	})
	if err != nil {
		return "OFFLINE", err
	}
	if _, err := ParseFieldResponse(response, "PL03"); err != nil {
		return "ОШИБКА", err
	}
	return "ГОТОВ", nil
}

// InitSession сохраняет параметры будущей сессии марок, не открывая COM и канал E.
func (d *Driver) InitSession(_ string, maxQueue int, staticFields map[string]string) error {
	if maxQueue <= 0 {
		return fmt.Errorf("размер очереди Bizerba должен быть больше нуля")
	}
	if d.device == "" {
		return fmt.Errorf("не задано имя устройства Bizerba в BCS")
	}
	d.startMu.Lock()
	defer d.startMu.Unlock()
	d.mu.RLock()
	closed := d.closed
	d.mu.RUnlock()
	if closed {
		return fmt.Errorf("драйвер Bizerba закрыт")
	}
	if err := d.stopSession(); err != nil {
		return err
	}
	d.mu.Lock()
	d.lastPrinted = -1
	d.sessionErr = nil
	d.capturePaused = false
	d.prepared = &preparedSession{capacity: maxQueue, staticFields: cloneFields(staticFields)}
	d.mu.Unlock()
	if d.recordGXNET {
		return d.ensureCaptureLocked()
	}
	return nil
}

// startPreparedSession запускается очередью Bizerba после поступления списка марок.
// Благодаря отложенному запуску /task/create не захватывает спонтанный COM-клиент
// и не открывает канал E до первого успешного /task/append.
func (d *Driver) startPreparedSession() error {
	d.startMu.Lock()
	defer d.startMu.Unlock()
	d.mu.RLock()
	closed := d.closed
	d.mu.RUnlock()
	if closed {
		return fmt.Errorf("драйвер Bizerba закрыт")
	}
	if d.recordGXNET {
		if err := d.ensureCaptureLocked(); err != nil {
			return err
		}
		d.mu.RLock()
		prepared, s, previousErr := d.prepared, d.session, d.sessionErr
		d.mu.RUnlock()
		if prepared == nil {
			return previousErr
		}
		_, err := s.request(sessionRequest{kind: "prepare", prepared: prepared})
		if err == nil {
			d.mu.Lock()
			d.prepared = nil
			d.mu.Unlock()
		}
		return err
	}

	d.mu.RLock()
	active, prepared, previousErr := d.session, d.prepared, d.sessionErr
	d.mu.RUnlock()
	if active != nil {
		return nil
	}
	if prepared == nil {
		return previousErr
	}

	s := &markSession{requests: make(chan sessionRequest), done: make(chan struct{}), start: make(chan error, 1)}
	d.mu.Lock()
	d.session = s
	d.prepared = nil
	d.mu.Unlock()
	go d.runSession(s, prepared.capacity, prepared.staticFields)
	if err := <-s.start; err != nil {
		d.mu.Lock()
		if d.session == s {
			d.session = nil
		}
		d.mu.Unlock()
		return err
	}
	return nil
}

// runSession обслуживает очередь марок в одном закреплённом COM-потоке.
// Сессия открывает канал E, принимает PV01/PW05 и устанавливает следующую марку.
func (d *Driver) runSession(s *markSession, capacity int, staticFields map[string]string) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer func() {
		d.mu.Lock()
		if d.session == s {
			d.session = nil
		}
		d.sessionErr = s.failure()
		d.mu.Unlock()
		close(s.done)
	}()
	conn, err := d.newConn()
	if err == nil {
		err = conn.Open(d.device, d.device, true)
	}
	if err == nil {
		err = sendWrite(conn, "A!GWC3", "1", d.timeout)
	}
	if err == nil {
		err = d.updateStaticFieldsOn(conn, staticFields)
	}
	s.setError(err)
	s.start <- err
	if err != nil {
		if conn != nil {
			_ = conn.Close()
		}
		return
	}
	defer conn.Close()

	var pending []queuedMark
	var active *queuedMark
	// clearMarkFields удаляет данные последней марки, чтобы при пустой очереди
	// устройство не могло повторно напечатать уже использованный DataMatrix.
	clearMarkFields := func() error {
		if err := sendWrite(conn, "A!GT03", "", d.timeout); err != nil {
			return fmt.Errorf("ошибка очистки поля Bizerba GT03: %w", err)
		}
		if err := sendWrite(conn, "A!GT04", "", d.timeout); err != nil {
			return fmt.Errorf("ошибка очистки поля Bizerba GT04: %w", err)
		}
		return nil
	}
	installNext := func() error {
		if active != nil || len(pending) == 0 {
			return nil
		}
		next := pending[0]
		pending = pending[1:]
		// В выбранном PLU-шаблоне текст DataMatrix собирается из полей GT03 и GT04:
		// GT03 содержит шестисимвольный блок AI 21 (код страны и серийный номер),
		// а GT04 — разделитель GS в виде @1D, идентификатор AI 93 и криптохвост.
		if err := sendWrite(conn, "A!GT03", next.serial, d.timeout); err != nil {
			return fmt.Errorf("ошибка установки поля Bizerba GT03: %w", err)
		}
		if err := sendWrite(conn, "A!GT04", next.crypto, d.timeout); err != nil {
			return fmt.Errorf("ошибка установки поля Bizerba GT04: %w", err)
		}
		active = &next
		return nil
	}

	for {
		select {
		case req := <-s.requests:
			switch req.kind {
			case "enqueue":
				if capacity <= 0 {
					req.resp <- sessionResponse{err: fmt.Errorf("сессия передачи марок Bizerba не инициализирована")}
					continue
				}
				used := len(pending)
				if active != nil {
					used++
				}
				free := capacity - used
				if free < len(req.marks) {
					req.resp <- sessionResponse{err: fmt.Errorf("в очереди Bizerba свободно место для %d из %d марок", free, len(req.marks))}
					continue
				}
				pending = append(pending, req.marks...)
				err := installNext()
				req.resp <- sessionResponse{value: len(req.marks), err: err}
				if err != nil {
					s.setError(err)
					_ = sendWrite(conn, "A!GWC3", "0", d.timeout)
					return
				}
			case "free":
				used := len(pending)
				if active != nil {
					used++
				}
				req.resp <- sessionResponse{value: capacity - used}
			case "call":
				req.resp <- sessionResponse{err: req.call(conn)}
			case "prepare":
				err := d.updateStaticFieldsOn(conn, req.prepared.staticFields)
				if err == nil {
					capacity = req.prepared.capacity
				}
				req.resp <- sessionResponse{err: err}
			case "stop", "close":
				err := sendWrite(conn, "A!GWC3", "0", d.timeout)
				req.resp <- sessionResponse{err: err}
				return
			}
		default:
			payload, status, receiveErr := conn.ReceiveOne(spontaneousQueue, receivePollTimeout)
			if receiveErr != nil {
				s.setError(receiveErr)
				_ = sendWrite(conn, "A!GWC3", "0", d.timeout)
				return
			}
			if status != bcsStatusOK && status != bcsStatusMore && status != bcsStatusTimeout {
				s.setError(fmt.Errorf("BCS ReceiveOne(%q): получен статус %d", spontaneousQueue, status))
				_ = sendWrite(conn, "A!GWC3", "0", d.timeout)
				return
			}
			isWeightPackage := status != bcsStatusTimeout && hasField(payload, "PV01")
			isPassingPackage := status != bcsStatusTimeout && hasField(payload, "PW05")
			if active != nil && (isWeightPackage || isPassingPackage) {
				weight := ""
				var weightErr error
				switch {
				case isWeightPackage && d.equipment.captureWeight:
					weight, weightErr = bizerbaWeight(payload)
				case isPassingPackage:
					// PW05 reports a passing package without weighing data. The mark was
					// applied, so persist it with zero weight and advance the queue.
					weight = "0"
				}
				if weight != "" || weightErr != nil {
					switch {
					case weightErr != nil:
						slog.Error("Не удалось извлечь вес из пакета Bizerba", "mark", active.code, "err", weightErr, "packet", payload)
					case d.equipment.recordWeight == nil:
						slog.Error("Для Bizerba не настроено хранилище веса напечатанной марки", "mark", active.code, "weight", weight)
					default:
						if recordErr := d.equipment.recordWeight(active.index, active.code, weight); recordErr != nil {
							slog.Error("Не удалось сохранить вес напечатанной марки", "mark", active.code, "weight", weight, "err", recordErr)
						}
					}
				}
				d.mu.Lock()
				d.lastPrinted = active.index
				d.mu.Unlock()
				active = nil
				if err := installNext(); err != nil {
					s.setError(err)
					_ = sendWrite(conn, "A!GWC3", "0", d.timeout)
					return
				}
				if active == nil && len(pending) == 0 {
					if err := clearMarkFields(); err != nil {
						s.setError(err)
						_ = sendWrite(conn, "A!GWC3", "0", d.timeout)
						return
					}
				}
			}
		}
	}
}

// hasField проверяет наличие поля в пакете протокола Bizerba.
func hasField(packet, field string) bool {
	for _, part := range strings.Split(packet, "|") {
		part = strings.TrimSpace(part)
		if part == field || strings.TrimPrefix(strings.TrimPrefix(part, "A!"), "A?") == field {
			return true
		}
	}
	return false
}

// bizerbaWeight извлекает PD00 и преобразует представление «KG;степень;целое» в килограммы.
func bizerbaWeight(packet string) (string, error) {
	raw, ok := bizerbaFieldValue(packet, "PD00")
	if !ok || raw == "" {
		return "", fmt.Errorf("в пакете отсутствует поле PD00")
	}
	parts := strings.Split(raw, ";")
	if len(parts) == 1 {
		if _, err := strconv.ParseFloat(raw, 64); err != nil {
			return "", fmt.Errorf("некорректное значение PD00 %q", raw)
		}
		return raw, nil
	}
	if len(parts) != 3 || !strings.EqualFold(strings.TrimSpace(parts[0]), "KG") {
		return "", fmt.Errorf("неподдерживаемое значение PD00 %q", raw)
	}
	exponent, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return "", fmt.Errorf("некорректная степень PD00 %q: %w", parts[1], err)
	}
	weight, err := formatScaledInteger(strings.TrimSpace(parts[2]), exponent)
	if err != nil {
		return "", fmt.Errorf("некорректный вес PD00 %q: %w", parts[2], err)
	}
	return weight, nil
}

// bizerbaFieldValue возвращает значение, следующее за именем поля в пакете Bizerba.
func bizerbaFieldValue(packet, field string) (string, bool) {
	parts := strings.Split(packet, "|")
	for i, part := range parts {
		name := strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(part), "A!"), "A?")
		if name == field && i+1 < len(parts) {
			return strings.TrimSpace(parts[i+1]), true
		}
	}
	return "", false
}

// formatScaledInteger форматирует целое с десятичной степенью без потери точности.
func formatScaledInteger(value string, exponent int) (string, error) {
	negative := strings.HasPrefix(value, "-")
	digits := strings.TrimPrefix(strings.TrimPrefix(value, "-"), "+")
	if digits == "" || strings.Trim(digits, "0123456789") != "" {
		return "", fmt.Errorf("ожидалось целое число")
	}
	sign := ""
	if negative {
		sign = "-"
	}
	if exponent >= 0 {
		return sign + digits + strings.Repeat("0", exponent), nil
	}
	places := -exponent
	if len(digits) <= places {
		digits = strings.Repeat("0", places-len(digits)+1) + digits
	}
	point := len(digits) - places
	return sign + digits[:point] + "." + digits[point:], nil
}

// PrintBatchIndexed разбирает GS1-коды и добавляет их в очередь активной сессии.
func (d *Driver) PrintBatchIndexed(_ string, startIndex int, codes []string) (int, error) {
	marks := make([]queuedMark, 0, len(codes))
	for i, code := range codes {
		parsed, err := marking.ParseAndValidateShortGS1(strings.TrimSpace(code))
		if err != nil {
			return 0, fmt.Errorf("некорректная марка с индексом %d в пакете: %w", i, err)
		}
		marks = append(marks, queuedMark{index: startIndex + i, code: parsed.ToDBFormat(), serial: parsed.CountryCode + parsed.Serial,
			crypto: "@1D93" + parsed.CryptoTail})
	}
	if err := d.startPreparedSession(); err != nil {
		return 0, err
	}
	d.mu.RLock()
	s := d.session
	d.mu.RUnlock()
	if s == nil {
		d.mu.RLock()
		err := d.sessionErr
		d.mu.RUnlock()
		if err != nil {
			return 0, err
		}
		return 0, fmt.Errorf("сессия передачи марок Bizerba не инициализирована")
	}
	return s.request(sessionRequest{kind: "enqueue", marks: marks})
}

// stopSession закрывает канал E и завершает постоянный COM-сеанс.
func (d *Driver) stopSession() error {
	d.mu.RLock()
	s := d.session
	d.mu.RUnlock()
	if s == nil {
		d.mu.Lock()
		err := d.sessionErr
		d.sessionErr = nil
		d.mu.Unlock()
		return err
	}
	_, err := s.request(sessionRequest{kind: "stop"})
	<-s.done
	d.mu.Lock()
	if d.session == s {
		d.session = nil
	}
	d.mu.Unlock()
	return err
}

// Close освобождает ресурсы активной или подготовленной сессии.
func (d *Driver) Close() error {
	d.startMu.Lock()
	defer d.startMu.Unlock()
	d.mu.Lock()
	d.closed = true
	d.prepared = nil
	s := d.session
	d.mu.Unlock()
	if d.captureStop != nil {
		close(d.captureStop)
		d.captureStop = nil
	}
	if s == nil {
		return nil
	}
	_, err := s.request(sessionRequest{kind: "close"})
	<-s.done
	return err
}

// ClearQueue отменяет подготовленную очередь либо закрывает активную сессию марок.
func (d *Driver) ClearQueue() error { return d.clearSession() }

// clearSession удаляет отложенную конфигурацию и останавливает активную сессию.
func (d *Driver) clearSession() error {
	d.startMu.Lock()
	defer d.startMu.Unlock()
	d.mu.Lock()
	d.prepared = nil
	d.capturePaused = true
	d.mu.Unlock()
	return d.stopSession()
}

// GetBufferFreeSpace запускает подготовленную сессию и возвращает свободное место очереди.
func (d *Driver) GetBufferFreeSpace() (int, error) {
	if err := d.startPreparedSession(); err != nil {
		return 0, err
	}
	d.mu.RLock()
	s := d.session
	d.mu.RUnlock()
	if s == nil {
		d.mu.RLock()
		err := d.sessionErr
		d.mu.RUnlock()
		return 0, err
	}
	return s.request(sessionRequest{kind: "free"})
}

// GetLastPrintedIndex возвращает индекс последней марки, подтверждённой пакетом PV01 или PW05.
func (d *Driver) GetLastPrintedIndex() (int, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.lastPrinted, nil
}

// GetTemplates возвращает текущий PLU как доступный шаблон устройства.
func (d *Driver) GetTemplates() ([]string, error) {
	plu, err := d.GetCurrentTemplate()
	if err != nil {
		return nil, err
	}
	return []string{plu}, nil
}

// GetTemplateFields возвращает поля, поддерживаемые драйвером Bizerba.
func (d *Driver) GetTemplateFields(string) ([]string, error) {
	return []string{"date", "brigade", "DATAMATRIX"}, nil
}

// GetRemainingRibbon возвращает заглушку: BCS-драйвер пока не читает остаток ленты.
func (d *Driver) GetRemainingRibbon() (string, error) { return "N/A", nil }

// GetQueueCapacity возвращает заглушку: физическая ёмкость очереди устройством не сообщается.
func (d *Driver) GetQueueCapacity(string) (string, error) { return "0", nil }

// GetPrintSpeed возвращает заглушку: скорость печати через BCS пока не читается.
func (d *Driver) GetPrintSpeed() (string, error) { return "N/A", nil }

// GetCurrentPrintCount возвращает заглушку: количество определяется по пакетам PV01/PW05.
func (d *Driver) GetCurrentPrintCount() (string, error) { return "N/A", nil }

// firstField ищет первое непустое значение по одному из допустимых имён поля.
func firstField(fields map[string]string, names ...string) string {
	for key, value := range fields {
		for _, name := range names {
			if strings.EqualFold(strings.TrimSpace(key), name) {
				return strings.TrimSpace(value)
			}
		}
	}
	return ""
}

// normalizeBizerbaDate преобразует поддерживаемые представления даты в формат ddMMyy.
func normalizeBizerbaDate(value string) (string, error) {
	value = strings.TrimSpace(value)
	if len(value) == 6 {
		if _, err := time.Parse("020106", value); err == nil {
			return value, nil
		}
	}
	for _, layout := range []string{"02.01.2006", "2006-01-02", time.RFC3339} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.Format("020106"), nil
		}
	}
	return "", fmt.Errorf("неподдерживаемый формат даты Bizerba %q; ожидается ddMMyy, dd.MM.yyyy или yyyy-MM-dd", value)
}

// cloneFields создаёт независимую копию набора статических полей.
func cloneFields(fields map[string]string) map[string]string {
	result := make(map[string]string, len(fields))
	for key, value := range fields {
		result[key] = value
	}
	return result
}

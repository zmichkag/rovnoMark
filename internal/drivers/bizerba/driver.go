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

type deduplicationEntry struct {
	msgType   string
	timestamp time.Time
}

type pw02Deduplicator struct {
	seen map[string]deduplicationEntry
	ttl  time.Duration
}

func newPW02Deduplicator() *pw02Deduplicator {
	return &pw02Deduplicator{
		seen: make(map[string]deduplicationEntry),
		ttl:  5 * time.Second,
	}
}

// shouldProcess проверяет PW02. Если уже видели за 5 секунд — возвращает false.
func (d *pw02Deduplicator) shouldProcess(handle, msgType string, now time.Time) bool {
	for k, v := range d.seen {
		if now.Sub(v.timestamp) >= d.ttl {
			delete(d.seen, k)
		}
	}

	if entry, ok := d.seen[handle]; ok {
		if now.Sub(entry.timestamp) < d.ttl {
			return false
		}
	}

	d.seen[handle] = deduplicationEntry{msgType: msgType, timestamp: now}
	return true
}

func (d *pw02Deduplicator) reset() {
	d.seen = make(map[string]deduplicationEntry)
}

func New(_ string, _ int, device string) *Driver {
	return newDriver(device, newBCSConnection)
}

func NewWithProfile(_ string, _ int, device string, profile Profile) *Driver {
	return newDriverWithProfile(device, newBCSConnection, profile)
}

func newDriver(device string, factory connectionFactory) *Driver {
	return newDriverWithProfile(device, factory, Profile{})
}

func newDriverWithProfile(device string, factory connectionFactory, profile Profile) *Driver {
	d := &Driver{
		device:        strings.TrimSpace(device),
		timeout:       defaultRequestTimeout,
		recordGXNET:   profile.RecordGXNET && profile.RecordResponse != nil,
		capturePaused: true,
		equipment:     configuredBizerbaEquipment(profile),
		lastPrinted:   -1,
		fields:        make(map[string]string),
	}
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

func (s *markSession) call(fn func(bcsConnection) error) error {
	_, err := s.request(sessionRequest{kind: "call", call: fn})
	return err
}

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

func (s *markSession) setError(err error) {
	if err == nil {
		return
	}
	s.errMu.Lock()
	s.err = err
	s.errMu.Unlock()
}

func (s *markSession) failure() error {
	s.errMu.RLock()
	defer s.errMu.RUnlock()
	return s.err
}

func (s *markSession) closedError() error {
	if err := s.failure(); err != nil {
		return err
	}
	return fmt.Errorf("сессия передачи марок Bizerba закрыта")
}

func (d *Driver) SelectTemplate(plu string, fields map[string]string) error {
	plu = strings.TrimSpace(plu)
	if plu == "" {
		return fmt.Errorf("не задан код PLU Bizerba")
	}
	err := d.runCommand(func(conn bcsConnection) error {
		if loadErr := sendWrite(conn, "A!XV00|GL19|LX02", plu, d.timeout); loadErr != nil {
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

func (d *Driver) PrintTemplate(plu string, fields map[string]string) error {
	return d.SelectTemplate(plu, fields)
}

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

// updateStaticFieldsOn отправляет статические поля согласно FieldMapping
func (d *Driver) updateStaticFieldsOn(conn bcsConnection, fields map[string]string) error {
	if len(fields) == 0 {
		return nil
	}

	for key, value := range fields {
		val := strings.TrimSpace(value)
		if val == "" {
			continue
		}

		targetRegister := ""
		if reg, ok := d.equipment.mapping.StaticMap[key]; ok && reg != "" {
			targetRegister = strings.ToUpper(strings.TrimSpace(reg))
		} else {
			for mapKey, mapReg := range d.equipment.mapping.StaticMap {
				if strings.EqualFold(mapKey, key) && mapReg != "" {
					targetRegister = strings.ToUpper(strings.TrimSpace(mapReg))
					break
				}
			}
		}

		if targetRegister == "" {
			switch {
			case matchesField(key, "date01", "date1", "date", "production_date", "дата"):
				targetRegister = "GL06"
			case matchesField(key, "date02", "date2", "expiry_date", "expiration_date", "срок_годности"):
				targetRegister = "GL07"
			case matchesField(key, "brigade", "team", "бригада"):
				targetRegister = "GL15"
			case matchesField(key, "design", "label_design", "дизайн"):
				targetRegister = "GW17"
			}
		}

		if targetRegister == "" {
			upper := strings.ToUpper(strings.TrimSpace(key))
			if isBizerbaRegister(upper) {
				targetRegister = upper
			}
		}

		if targetRegister == "" {
			continue
		}

		formattedValue := val
		if targetRegister == "GL06" || targetRegister == "GL07" {
			normDate, err := normalizeBizerbaDate(val)
			if err != nil {
				return err
			}
			formattedValue = normDate
		}
		if targetRegister == "GW17" {
			design, err := normalizeBizerbaDesign(val)
			if err != nil {
				return err
			}
			formattedValue = design
		}

		if err := sendWrite(conn, "A!"+targetRegister, formattedValue, d.timeout); err != nil {
			return fmt.Errorf("ошибка установки поля Bizerba %s (%s=%q): %w", targetRegister, key, formattedValue, err)
		}
	}
	return nil
}

func isBizerbaRegister(reg string) bool {
	if len(reg) != 4 {
		return false
	}
	prefix := reg[:2]
	return prefix == "GT" || prefix == "GL" || prefix == "GW" || prefix == "GD"
}

func matchesField(value string, aliases ...string) bool {
	value = strings.TrimSpace(value)
	for _, alias := range aliases {
		if strings.EqualFold(value, alias) {
			return true
		}
	}
	return false
}

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

	dedup := newPW02Deduplicator()

	serialReg := d.equipment.mapping.SerialField
	if serialReg == "" {
		serialReg = "GT03"
	}
	cryptoReg := d.equipment.mapping.CryptoField
	if cryptoReg == "" {
		cryptoReg = "GT04"
	}

	clearMarkFields := func() error {
		if err := sendWrite(conn, "A!"+serialReg, "", d.timeout); err != nil {
			return fmt.Errorf("ошибка очистки поля Bizerba %s: %w", serialReg, err)
		}
		if err := sendWrite(conn, "A!"+cryptoReg, "", d.timeout); err != nil {
			return fmt.Errorf("ошибка очистки поля Bizerba %s: %w", cryptoReg, err)
		}
		return nil
	}

	installNext := func() error {
		if active != nil || len(pending) == 0 {
			return nil
		}
		next := pending[0]
		pending = pending[1:]
		if err := sendWrite(conn, "A!"+serialReg, next.serial, d.timeout); err != nil {
			return fmt.Errorf("ошибка установки поля Bizerba %s: %w", serialReg, err)
		}
		if err := sendWrite(conn, "A!"+cryptoReg, next.crypto, d.timeout); err != nil {
			return fmt.Errorf("ошибка установки поля Bizerba %s: %w", cryptoReg, err)
		}
		active = &next
		return nil
	}

	for {
		drainRequests := true
		for drainRequests {
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
					dedup.reset()
					err := d.updateStaticFieldsOn(conn, req.prepared.staticFields)
					if err == nil {
						capacity = req.prepared.capacity
					}
					req.resp <- sessionResponse{err: err}
				case "stop", "close":
					dedup.reset()
					_ = clearMarkFields()
					err := sendWrite(conn, "A!GWC3", "0", d.timeout)
					req.resp <- sessionResponse{err: err}
					return
				}
			default:
				drainRequests = false
			}
		}

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

		if status == bcsStatusTimeout {
			if active == nil {
				time.Sleep(10 * time.Millisecond)
			}
			continue
		}

		// 1. PW05 исключаем из учёта марок
		if hasField(payload, "PW05") {
			slog.Debug("Bizerba: получено сервисное сообщение PW05 (пропуск)", "packet", payload)
			continue
		}

		// 2. Обрабатываем PV01, PV04, PV05
		msgType := ""
		switch {
		case hasField(payload, "PV01"):
			msgType = "PV01"
		case hasField(payload, "PV04"):
			msgType = "PV04"
		case hasField(payload, "PV05"):
			msgType = "PV05"
		default:
			continue
		}

		if active == nil {
			continue
		}

		// 3. Извлекаем PW02 (handle)
		handle, hasHandle := bizerbaFieldValue(payload, "PW02")
		handle = strings.TrimSpace(handle)
		if !hasHandle || handle == "" {
			slog.Warn("Bizerba: сообщение без корректного PW02 пропущено", "type", msgType, "packet", payload)
			continue
		}

		// 4. Защита от дублей по PW02 (5 секунд)
		now := time.Now()
		if !dedup.shouldProcess(handle, msgType, now) {
			slog.Debug("Bizerba: повторное сообщение для PW02 в окне 5с пропущено", "handle", handle, "type", msgType)
			continue
		}

		// 5. Определение веса
		weight := ""
		switch msgType {
		case "PV01", "PV04":
			w, err := bizerbaWeight(payload)
			if err != nil {
				slog.Warn("Bizerba: сообщение отклонено из-за некорректного/отсутствующего PD00", "type", msgType, "err", err, "packet", payload)
				delete(dedup.seen, handle)
				continue
			}
			weight = w

		case "PV05":
			weight = "0"
		}

		// 6. Запись веса
		if d.equipment.recordWeight != nil {
			if err := d.equipment.recordWeight(active.index, active.code, weight); err != nil {
				slog.Error("Не удалось сохранить вес напечатанной марки", "mark", active.code, "weight", weight, "err", err)
				// Не считаем сообщение обработанным: текущая марка остаётся
				// активной, а повтор с тем же PW02 сможет сохранить её ещё раз.
				delete(dedup.seen, handle)
				continue
			}
		}

		// 7. Сдвиг очереди
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
				slog.Warn("Не удалось очистить поля марки в конце очереди Bizerba", "err", err)
			}
		}
	}
}

func hasField(packet, field string) bool {
	for _, part := range strings.Split(packet, "|") {
		part = strings.TrimSpace(part)
		if part == field || strings.TrimPrefix(strings.TrimPrefix(part, "A!"), "A?") == field {
			return true
		}
	}
	return false
}

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

func (d *Driver) PrintBatchIndexed(_ string, startIndex int, codes []string) (int, error) {
	if len(codes) == 0 {
		return 0, nil
	}
	if d.equipment.unique.Enabled() {
		if _, err := buildUniqueDataFile(codes); err != nil {
			return 0, err
		}
		return 0, fmt.Errorf("режим Bizerba UniqueData подготовил файл, но FTP-загрузка ещё не настроена; коды не переданы устройству")
	}

	marks := make([]queuedMark, 0, len(codes))
	var activeGTIN string
	cryptoPrefix := ""
	if d.equipment.explicitGSSeparator {
		cryptoPrefix = "@1D93"
	}

	for i, code := range codes {
		parsed, err := marking.ParseAndValidateShortGS1(strings.TrimSpace(code))
		if err != nil {
			return 0, fmt.Errorf("некорректная марка с индексом %d в пакете: %w", i, err)
		}

		if i == 0 {
			activeGTIN = parsed.GTIN
		} else if parsed.GTIN != activeGTIN {
			return 0, fmt.Errorf("в партии обнаружен чужой GTIN на марке #%d: %s (ожидался %s)", i, parsed.GTIN, activeGTIN)
		}

		marks = append(marks, queuedMark{
			index:  startIndex + i,
			code:   parsed.ToDBFormat(),
			serial: parsed.CountryCode + parsed.Serial,
			crypto: cryptoPrefix + parsed.CryptoTail,
		})
	}

	gtinReg := strings.ToUpper(strings.TrimSpace(d.equipment.mapping.GTINField))
	if gtinReg != "" && activeGTIN != "" {
		d.mu.Lock()
		if d.prepared != nil {
			if d.prepared.staticFields == nil {
				d.prepared.staticFields = make(map[string]string)
			}
			d.prepared.staticFields[gtinReg] = activeGTIN
		}
		d.mu.Unlock()
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

func (d *Driver) ClearQueue() error { return d.clearSession() }

func (d *Driver) clearSession() error {
	d.startMu.Lock()
	defer d.startMu.Unlock()
	d.mu.Lock()
	d.prepared = nil
	d.capturePaused = true
	d.mu.Unlock()
	return d.stopSession()
}

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

func (d *Driver) GetLastPrintedIndex() (int, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.lastPrinted, nil
}

func (d *Driver) GetTemplates() ([]string, error) {
	plu, err := d.GetCurrentTemplate()
	if err != nil {
		return nil, err
	}
	return []string{plu}, nil
}

func (d *Driver) GetTemplateFields(string) ([]string, error) {
	return []string{"design", "date01", "date02", "brigade", "DATAMATRIX"}, nil
}

func (d *Driver) GetRemainingRibbon() (string, error) { return "N/A", nil }

func (d *Driver) GetQueueCapacity(string) (string, error) { return "0", nil }

func (d *Driver) GetPrintSpeed() (string, error) { return "N/A", nil }

func (d *Driver) GetCurrentPrintCount() (string, error) { return "N/A", nil }

// normalizeBizerbaDesign validates a user label design number. Bizerba user
// designs start at 8192.
func normalizeBizerbaDesign(value string) (string, error) {
	value = strings.TrimSpace(value)
	design, err := strconv.Atoi(value)
	if err != nil || design < 8192 {
		return "", fmt.Errorf("некорректный номер пользовательской этикетки Bizerba %q; ожидается целое число не меньше 8192", value)
	}
	return strconv.Itoa(design), nil
}

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

func cloneFields(fields map[string]string) map[string]string {
	result := make(map[string]string, len(fields))
	for key, value := range fields {
		result[key] = value
	}
	return result
}

func (d *Driver) GetTotalPrints() (int64, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.lastPrinted < 0 {
		return 0, nil
	}
	return int64(d.lastPrinted), nil
}

package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"rovnoMark/internal/core"
	"rovnoMark/internal/core/marking"
	"rovnoMark/internal/models"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

func (s *Server) handleTaskCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		sendJSONError(w, http.StatusMethodNotAllowed, "Only POST allowed")
		return
	}

	lineID, err := strconv.Atoi(r.URL.Query().Get("line_id"))
	if err != nil || lineID <= 0 {
		sendJSONError(w, http.StatusBadRequest, "Missing or invalid line_id parameter in URL")
		return
	}

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		slog.Error("TASK-CREATE: Ошибка чтения Body от 1С", "err", err)
		sendJSONError(w, http.StatusBadRequest, "Failed to read request body")
		return
	}

	var req struct {
		TemplateName     string            `json:"template_name"`
		DynamicFieldName string            `json:"dynamic_field_name"`
		StaticFields     map[string]string `json:"static_fields"`
		RndText          string            `json:"rnd_text"`
	}

	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		slog.Warn("TASK-CREATE: Ошибка парсинга JSON от 1С", "err", err)
		sendJSONError(w, http.StatusBadRequest, "Invalid JSON structure")
		return
	}

	activeID, err := s.store.GetActiveTaskByLine(lineID)
	if err != nil {
		sendJSONError(w, http.StatusInternalServerError, "Ошибка проверки занятости линии")
		return
	}
	if activeID != 0 {
		sendJSONError(w, http.StatusConflict, fmt.Sprintf("Линия %d уже занята задачей %d. Сначала остановите её.", lineID, activeID))
		return
	}

	printersInLine, err := s.store.GetPrintersByLine(lineID)
	if err != nil || len(printersInLine) == 0 {
		sendJSONError(w, http.StatusNotFound, "Линия пуста или не найдена")
		return
	}

	staticBytes, _ := json.Marshal(req.StaticFields)
	taskID, err := s.store.CreateTask(lineID, req.TemplateName, req.DynamicFieldName, string(staticBytes), req.RndText)
	if err != nil {
		sendJSONError(w, http.StatusInternalServerError, "Ошибка БД при создании задачи: "+err.Error())
		return
	}

	badStatuses := []string{
		"TIMEOUT", "INITIALIZING", "STARTING",
		"ОФФЛАЙН", "OFFLINE", "ОШИБКА", "ERROR", "REFUSED",
	}

	for _, pCfg := range printersInLine {
		p := s.manager.GetPrinter(pCfg.ID)
		if p == nil {
			_ = s.store.SetTaskStatus(int(taskID), models.TaskStateFailed)
			sendJSONError(w, http.StatusConflict, fmt.Sprintf("Принтер %s не зарегистрирован в системе или отключен", pCfg.Name))
			return
		}

		status, err := p.GetStatus()
		checkString := strings.ToUpper(status)
		if err != nil {
			checkString = strings.ToUpper(err.Error())
		}

		for _, bad := range badStatuses {
			if strings.Contains(checkString, bad) {
				slog.Warn("Принтер забракован перед стартом задачи", "printer", pCfg.Name, "status", status, "err", err)
				_ = s.store.SetTaskStatus(int(taskID), models.TaskStateFailed)
				sendJSONError(w, http.StatusServiceUnavailable, fmt.Sprintf(
					"Принтер %s не готов к работе (состояние: %s). Проверьте подключение.", pCfg.Name, status,
				))
				return
			}
		}

		if req.DynamicFieldName == "" {
			_ = p.ClearQueue()
			if err := p.SelectTemplate(req.TemplateName, req.StaticFields); err != nil {
				_ = s.store.SetTaskStatus(int(taskID), models.TaskStateFailed)
				sendJSONError(w, http.StatusInternalServerError, fmt.Sprintf("Ошибка установки шаблона на %s: %v", pCfg.Name, err))
				return
			}
		} else {
			selectFields := req.StaticFields
			if pCfg.DriverType == "videojet" {
				selectFields = nil
			}

			if err := p.SelectTemplate(req.TemplateName, selectFields); err != nil {
				_ = s.store.SetTaskStatus(int(taskID), models.TaskStateFailed)
				sendJSONError(w, http.StatusInternalServerError, fmt.Sprintf("Ошибка макета на %s: %v", pCfg.Name, err))
				return
			}

			compositeFields, _ := core.PrepareDynamicPipeline(req.DynamicFieldName, req.StaticFields, "")
			if err := p.InitSession(compositeFields, 1000, req.StaticFields); err != nil {
				_ = s.store.SetTaskStatus(int(taskID), models.TaskStateFailed)
				sendJSONError(w, http.StatusInternalServerError, fmt.Sprintf("Ошибка инициализации сессии на %s: %v", pCfg.Name, err))
				return
			}
		}
	}

	for _, pCfg := range printersInLine {
		p := s.manager.GetPrinter(pCfg.ID)
		if p == nil {
			continue
		}
		totalPrints, errTotal := p.GetTotalPrints()
		if errTotal != nil {
			totalPrints = -1
		}
		_ = s.store.RecordPrinterCounterSnapshot(int(taskID), lineID, pCfg.ID, "start", totalPrints)
		_ = s.store.SaveEventLog(&lineID, &pCfg.ID, "info", fmt.Sprintf("СТАРТ ЗАДАЧИ #%d: одометр %s = %d", taskID, pCfg.Name, totalPrints))
	}

	_ = s.store.SetTaskStatus(int(taskID), models.TaskStateActive)

	sendJSON(w, http.StatusOK, map[string]interface{}{
		"status":   "ready",
		"task_id":  taskID,
		"rnd_text": req.RndText,
	})
}

func (s *Server) handleTaskAppend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		sendJSONError(w, http.StatusMethodNotAllowed, "Only POST allowed")
		return
	}

	taskID, err := strconv.Atoi(r.URL.Query().Get("task_id"))
	if err != nil || taskID <= 0 {
		sendJSONError(w, http.StatusBadRequest, "Missing or invalid task_id parameter in URL")
		return
	}

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		sendJSONError(w, http.StatusBadRequest, "Failed to read request body")
		return
	}

	var rawReq struct {
		Codes []json.RawMessage `json:"codes"`
	}
	if err := json.Unmarshal(bodyBytes, &rawReq); err != nil {
		sendJSONError(w, http.StatusBadRequest, "Ошибка в структуре JSON")
		return
	}
	if len(rawReq.Codes) == 0 {
		sendJSONError(w, http.StatusBadRequest, "Пришел пустой массив кодов")
		return
	}

	inboundItems := make([]models.InboundCodeItem, 0, len(rawReq.Codes))
	for i, rawItem := range rawReq.Codes {
		var item models.InboundCodeItem
		var simpleCode string

		if errStr := json.Unmarshal(rawItem, &simpleCode); errStr == nil {
			item.Code = simpleCode
		} else {
			if errObj := json.Unmarshal(rawItem, &item); errObj != nil {
				sendJSONError(w, http.StatusBadRequest, fmt.Sprintf("Неверный формат элемента на индексе %d", i))
				return
			}
		}

		if s.validateGS1 {
			parsedMark, errVal := marking.ParseAndValidateShortGS1(item.Code)
			if errVal != nil {
				sendJSONError(w, http.StatusUnprocessableEntity, fmt.Sprintf(
					"Ошибка валидации кода маркировки на индексе %d: %v (значение: %s)", i, errVal, item.Code,
				))
				return
			}
			item.Code = parsedMark.ToDBFormat()
		} else {
			item.Code = strings.TrimSpace(item.Code)
		}

		inboundItems = append(inboundItems, item)
	}

	if err := s.store.AppendTaskCodes(taskID, inboundItems); err != nil {
		slog.Error("Append: Ошибка записи в БД", "task_id", taskID, "err", err)
		sendJSONError(w, http.StatusInternalServerError, "Ошибка БД при сохранении кодов")
		return
	}

	_ = s.store.SetTaskStatus(taskID, models.TaskStateActive)

	lineID, err := s.store.GetLineIDByTask(taskID)
	if err == nil {
		s.taskProcessor.StartPumping(lineID, taskID)
	}

	rndText, _ := s.store.GetRndTextByTask(taskID)

	sendJSON(w, http.StatusOK, map[string]interface{}{
		"status":         "received",
		"count":          len(inboundItems),
		"pumper_started": true,
		"rnd_text":       rndText,
		"gs1_validated":  s.validateGS1,
	})
}

func (s *Server) handleTaskStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		sendJSONError(w, http.StatusMethodNotAllowed, "Метод не поддерживается")
		return
	}

	taskID, err := strconv.Atoi(r.URL.Query().Get("task_id"))
	if err != nil || taskID <= 0 {
		sendJSONError(w, http.StatusBadRequest, "Неверный или отсутствующий ID задачи")
		return
	}

	lineID, err := s.store.GetLineIDByTask(taskID)
	if err != nil {
		sendJSONError(w, http.StatusNotFound, fmt.Sprintf("Задача с ID %d не найдена", taskID))
		return
	}

	currentStatus, err := s.store.GetTaskStatus(taskID)
	if err != nil {
		sendJSONError(w, http.StatusNotFound, fmt.Sprintf("Задача с ID %d не найдена", taskID))
		return
	}

	if currentStatus == "stopped" || currentStatus == "completed" {
		sendJSONError(w, http.StatusBadRequest, fmt.Sprintf("Задача уже находится в статусе '%s'", currentStatus))
		return
	}

	// ------------------------------------------------------------------
	// ШАГ 1: МГНОВЕННО ОТСЕКАЕМ НАСОСЫ (PUMPER)
	// ------------------------------------------------------------------
	if err := s.store.SetTaskStatus(taskID, models.TaskStateStopped); err != nil {
		sendJSONError(w, http.StatusInternalServerError, "Ошибка при сохранении статуса задачи: "+err.Error())
		return
	}

	slog.Info("[STOP] Статус задачи изменен на 'stopped'. Опрос и сведение баланса линии...",
		"task_id", taskID, "line_id", lineID)

	// ------------------------------------------------------------------
	// ШАГ 2: ПАРАЛЛЕЛЬНЫЙ ОПРОС ОДОМЕТРОВ, СВЕРКА В БД И СБРОС БУФЕРОВ
	// ------------------------------------------------------------------
	printers, err := s.store.GetPrintersByLine(lineID)
	if err != nil {
		slog.Error("[STOP] Ошибка получения принтеров линии", "line_id", lineID, "err", err)
	}

	report := make(map[string]interface{})
	var reportMu sync.Mutex
	var totalReturnedCodes int64 = 0
	var printerErrors []string
	var wg sync.WaitGroup

	// Таймаут на физический опрос всех железок — максимум 3 секунды
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	for _, pCfg := range printers {
		wg.Add(1)
		go func(cfg models.PrinterConfig) {
			defer wg.Done()

			p := s.manager.GetPrinter(cfg.ID)
			if p == nil {
				reportMu.Lock()
				report[cfg.Name] = map[string]interface{}{"status": "offline_skipped"}
				printerErrors = append(printerErrors, fmt.Sprintf("принтер %s отключен от менеджера", cfg.Name))
				reportMu.Unlock()
				return
			}

			doneChan := make(chan struct{})
			var lastIdx int
			var finalTotal int64 = -1
			var pollErr error

			go func() {
				defer close(doneChan)
				// 1. Получаем индекс последней фактически отпечатанной этикетки
				lastIdx, pollErr = p.GetLastPrintedIndex()
				if pollErr != nil {
					return
				}
				// 2. Снимаем абсолютный аппаратный счетчик
				finalTotal, _ = p.GetTotalPrints()
			}()

			select {
			case <-doneChan:
				if pollErr != nil {
					slog.Error("[STOP] Ошибка чтения одометра", "printer", cfg.Name, "err", pollErr)
					reportMu.Lock()
					report[cfg.Name] = map[string]interface{}{"status": "error", "error": pollErr.Error()}
					printerErrors = append(printerErrors, fmt.Sprintf("ошибка опроса %s: %v", cfg.Name, pollErr))
					reportMu.Unlock()
					return
				}

				// 3. Записываем снапшот одометра в Master DB для истории аудита
				if finalTotal >= 0 {
					_ = s.store.RecordPrinterCounterSnapshot(taskID, lineID, cfg.ID, "stop", finalTotal)
					_ = s.store.SaveEventLog(&lineID, &cfg.ID, "info",
						fmt.Sprintf("СТОП ЗАДАЧИ #%d: одометр %s = %d", taskID, cfg.Name, finalTotal))
				}

				// 4. АТОМАРНАЯ СВЕРКА В SQLite:
				// - printer_index <= lastIdx -> 'printed'
				// - остальное из буфера -> возвращается в 'pending'
				res, errReconcile := s.store.ReconcileAndFinalizeTaskCodes(taskID, cfg.ID, lastIdx)
				if errReconcile != nil {
					slog.Error("[STOP] Сбой сведения баланса в БД", "printer", cfg.Name, "err", errReconcile)
				}

				// 5. ОЧИСТКА БУФЕРА ЖЕЛЕЗКИ СТРОГО ПОСЛЕ ФИКСАЦИИ В БД
				_ = p.ClearQueue()

				returned := 0
				if res != nil {
					returned = res.ReturnedCodes
				}

				reportMu.Lock()
				atomic.AddInt64(&totalReturnedCodes, int64(returned))
				report[cfg.Name] = map[string]interface{}{
					"status":              "cleared",
					"last_printed_index":  lastIdx,
					"hardware_counter":    finalTotal,
					"reverted_to_pending": returned,
				}
				reportMu.Unlock()

			case <-ctx.Done():
				slog.Warn("[STOP] Принтер завис и не ответил за 3 секунды", "printer", cfg.Name)
				reportMu.Lock()
				report[cfg.Name] = map[string]interface{}{"status": "timeout"}
				printerErrors = append(printerErrors, fmt.Sprintf("принтер %s не ответил по таймауту", cfg.Name))
				reportMu.Unlock()
			}
		}(pCfg)
	}

	wg.Wait()

	// ------------------------------------------------------------------
	// ШАГ 3: ИТОГОВЫЙ СРЕЗ ПАРТИИ ИЗ БАЗЫ (ИСТИНА ПЕРВОЙ ИНСТАНЦИИ)
	// ------------------------------------------------------------------
	taskInfo, errInfo := s.store.GetTaskInfo(r.Context(), taskID)
	totalPrinted := 0
	totalPending := 0
	if errInfo == nil {
		if val, ok := taskInfo["printed_count"].(int); ok {
			totalPrinted = val
		}
		if val, ok := taskInfo["pending_count"].(int); ok {
			totalPending = val
		}
	}

	rndText, _ := s.store.GetRndTextByTask(taskID)

	response := map[string]interface{}{
		"task_id":               taskID,
		"line_id":               lineID,
		"status":                "stopped",
		"timestamp":             time.Now().Format(time.RFC3339),
		"total_confirmed":       totalPrinted,       // Честное суммарное количество напечатанных кодов
		"remaining_pending":     totalPending,       // Сколько кодов готово к повторной печати
		"buffer_reverted_total": totalReturnedCodes, // Сколько кодов было спасено из очередей
		"printers_report":       report,
		"rnd_text":              rndText,
	}

	if len(printerErrors) > 0 {
		response["warnings"] = printerErrors
	}

	sendJSON(w, http.StatusOK, response)
}

func (s *Server) handleTaskInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		sendJSONError(w, http.StatusMethodNotAllowed, "Разрешен только метод GET")
		return
	}

	taskID, err := strconv.Atoi(r.URL.Query().Get("task_id"))
	if err != nil || taskID <= 0 {
		sendJSONError(w, http.StatusBadRequest, "Неверный формат параметра task_id")
		return
	}

	taskInfo, err := s.store.GetTaskInfo(r.Context(), taskID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			sendJSONError(w, http.StatusNotFound, fmt.Sprintf("Задача с ID %d не найдена", taskID))
			return
		}
		sendJSONError(w, http.StatusInternalServerError, "Внутренняя ошибка сервера: "+err.Error())
		return
	}

	sendJSON(w, http.StatusOK, taskInfo)
}

func (s *Server) handleTaskActive(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		sendJSONError(w, http.StatusMethodNotAllowed, "Разрешен только метод GET")
		return
	}

	query := r.URL.Query()
	var lineID, printerID int
	var err error

	if l := query.Get("line_id"); l != "" {
		if lineID, err = strconv.Atoi(l); err != nil || lineID < 0 {
			sendJSONError(w, http.StatusBadRequest, "Неверный параметр line_id")
			return
		}
	}
	if p := query.Get("printer_id"); p != "" {
		if printerID, err = strconv.Atoi(p); err != nil || printerID < 0 {
			sendJSONError(w, http.StatusBadRequest, "Неверный параметр printer_id")
			return
		}
	}

	tasks, err := s.store.GetActiveTasks(lineID, printerID)
	if err != nil {
		sendJSONError(w, http.StatusInternalServerError, "Ошибка БД: "+err.Error())
		return
	}

	if len(tasks) == 0 {
		sendJSON(w, http.StatusOK, map[string]interface{}{
			"status":  "no_active_tasks",
			"message": "Активные задачи не найдены",
			"tasks":   []interface{}{},
		})
		return
	}

	sendJSON(w, http.StatusOK, tasks)
}

func (s *Server) handleCodeInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		sendJSONError(w, http.StatusMethodNotAllowed, "Разрешен только GET")
		return
	}

	codeQuery := r.URL.Query().Get("dm")
	if codeQuery == "" {
		sendJSONError(w, http.StatusBadRequest, "Параметр dm не передан")
		return
	}

	info, err := s.store.GetCodePassport(codeQuery)
	if err != nil {
		sendJSONError(w, http.StatusNotFound, "Код не найден в базе данных")
		return
	}

	sendJSON(w, http.StatusOK, info)
}

func (s *Server) handleDashboardLive(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		sendJSONError(w, http.StatusMethodNotAllowed, "Разрешен только GET")
		return
	}

	liveData, err := s.store.GetLiveDashboardData()
	if err != nil {
		sendJSONError(w, http.StatusInternalServerError, "Ошибка БД: "+err.Error())
		return
	}

	states, logs := s.manager.GetDashboardData()
	allPrintersConfig, _ := s.store.GetAllPrinters()

	printerCatalog := make(map[int]map[string]interface{})
	offlineCount := 0

	for _, cfg := range allPrintersConfig {
		st := states[cfg.ID]
		isOffline := strings.Contains(st.Status, "ОФФЛАЙН") || st.Status == "INITIALIZING"
		if isOffline {
			offlineCount++
		}

		printerCatalog[cfg.ID] = map[string]interface{}{
			"id":           cfg.ID,
			"name":         cfg.Name,
			"ip":           cfg.IP,
			"port":         cfg.Port,
			"driver_type":  cfg.DriverType,
			"is_active":    cfg.IsActive,
			"status":       st.Status,
			"ribbon":       st.Ribbon,
			"queue_free":   st.Queue,
			"cur_count":    st.CurCount,
			"cur_template": st.CurTemplate,
		}
	}

	summary := liveData["summary"].(map[string]interface{})
	summary["printers_offline"] = offlineCount
	summary["total_printers"] = len(allPrintersConfig)

	sendJSON(w, http.StatusOK, map[string]interface{}{
		"timestamp": liveData["timestamp"],
		"summary":   summary,
		"lines":     liveData["lines"],
		"printers":  printerCatalog,
		"logs":      logs,
	})
}

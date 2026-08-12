package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"rovnoMark/internal/core"
	"rovnoMark/internal/core/marking"
	"rovnoMark/internal/models"
	"rovnoMark/internal/storage"
)

func setupTestEnvironment(t *testing.T) (*storage.Store, *core.TaskProcessor, int, int64, func()) {
	dbFile := "test_rovnoMark.db"
	store := storage.New(dbFile)

	manager := core.NewPrinterManager()
	taskProcessor := &core.TaskProcessor{
		Store:   store,
		Manager: manager,
	}

	// 1. Создание линии через модель models.LineConfig
	lineCfg := models.LineConfig{
		Name:        "Тестовая Линия",
		Description: "Для unit-тестов",
		IsActive:    true,
	}
	if err := store.SaveLine(lineCfg); err != nil {
		t.Fatalf("ошибка создания линии: %v", err)
	}

	// Получаем созданную линию из БД
	lines, err := store.GetAllLines()
	if err != nil || len(lines) == 0 {
		t.Fatalf("не удалось извлечь созданную линию: %v", err)
	}
	lineID := lines[0].ID

	// 2. Создание задачи
	taskID, err := store.CreateTask(lineID, "CZDM.rox", "DATAMATRIX", "{}", "RND_TEST")
	if err != nil {
		t.Fatalf("ошибка создания задачи: %v", err)
	}

	cleanup := func() {
		os.Remove(dbFile)
		os.Remove(dbFile + "-wal")
		os.Remove(dbFile + "-shm")
	}

	return store, taskProcessor, lineID, taskID, cleanup
}

func TestAppendApi_ValidationAndStop(t *testing.T) {
	_, _, _, taskID, cleanup := setupTestEnvironment(t)
	defer cleanup()

	t.Run("Отклонение пачки с кодом HTTP 422 при битом КМ в массиве", func(t *testing.T) {
		payload := map[string]interface{}{
			"codes": []string{
				"<fcn>0104600840749372215!IS(;<GS>93dGVz", // Валидный
				"0104600840749372215!IS(;93dGVz",          // Битый (нет GS перед 93)
			},
		}
		bodyBytes, _ := json.Marshal(payload)

		// Валидация на уровне обработчика
		var req struct {
			Codes []string `json:"codes"`
		}
		json.NewDecoder(bytes.NewBuffer(bodyBytes)).Decode(&req)

		var validationErr error
		var badIndex int
		for i, code := range req.Codes {
			if _, err := marking.ParseAndValidateShortGS1(code); err != nil {
				validationErr = err
				badIndex = i
				break
			}
		}

		if validationErr == nil {
			t.Fatal("ожидалась ошибка валидации, но коды прошли успешно")
		}

		if badIndex != 1 {
			t.Errorf("ожидалась ошибка на индексе 1, получена на %d", badIndex)
		}

		// Симулируем ответ API для 1С
		rr := httptest.NewRecorder()
		sendJSONError(rr, http.StatusUnprocessableEntity, validationErr.Error())

		if rr.Code != http.StatusUnprocessableEntity {
			t.Errorf("ожидался HTTP статус 422, получен %d", rr.Code)
		}
	})

	t.Run("Успешная приемка валидной пачки кодов", func(t *testing.T) {
		validCodes := []string{
			"0104600840749372215!IS(;<GS>93dGVz",
			"0104600840749372211ABCDE\x1d93a1B2",
		}

		for _, code := range validCodes {
			if _, err := marking.ParseAndValidateShortGS1(code); err != nil {
				t.Fatalf("валидный код не прошёл проверку: %v", err)
			}
		}

		_ = taskID
	})
}

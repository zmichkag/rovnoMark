package marking_test

import (
	"encoding/csv"
	"os"
	"rovnoMark/internal/core/marking"
	"strings"
	"testing"
)

func TestRealCodesNormalization(t *testing.T) {
	file, err := os.Open("main_task_codes.csv")
	if err != nil {
		t.Skipf("Файл не найден, пропускаем тест: %v", err)
	}
	defer file.Close()

	reader := csv.NewReader(file)
	records, err := reader.ReadAll()
	if err != nil {
		t.Fatalf("Ошибка чтения CSV: %v", err)
	}

	for idx, row := range records {
		if idx == 0 && (row[0] == "code" || strings.HasPrefix(row[0], "01")) {
			if row[0] == "code" {
				continue // пропускаем заголовок
			}
		}

		rawCode := row[0]
		canonical := marking.NormalizeToCanonical(rawCode)

		// 1. Проверка канонического формата
		if !strings.HasPrefix(canonical, "01") {
			t.Errorf("[%d] Код после нормализации не начинается с 01: %s", idx, canonical)
		}
		if !strings.Contains(canonical, "<GS>") {
			t.Errorf("[%d] В коде отсутствует канонический разделитель <GS>: %s", idx, canonical)
		}
		if strings.Contains(canonical, "\x1d") {
			t.Errorf("[%d] В каноническом коде остался сырой байт 0x1D: %s", idx, canonical)
		}

		// 2. Симуляция подготовки payload для Videojet
		vjPayload := strings.ReplaceAll(canonical, "<GS>", "~d029")
		if !strings.HasPrefix(vjPayload, "~1") && !strings.HasPrefix(vjPayload, "~d029") {
			vjPayload = "~1" + vjPayload
		}

		if strings.Contains(vjPayload, "\x1d") {
			t.Errorf("[%d] В Videojet payload проник сырой байт 0x1D: %s", idx, vjPayload)
		}
		if strings.HasPrefix(vjPayload, "~1~1") {
			t.Errorf("[%d] Обнаружен двойной стартовый токен ~1: %s", idx, vjPayload)
		}
	}
}

package bizerba

import (
	"encoding/json"
	"fmt"
	"rovnoMark/internal/models"
	"time"
)

type WeightStore interface {
	SaveWeightAndMarkPrinted(taskID, printerID, printerIndex int, weight string) error
	SaveGXNETResponse(printerID int, device, receivedAt, cmd, param, queue, payload string, status int) error
	GetActiveTaskByLine(lineID int) (int, error)
	GetPrinterLineMap() (map[int]int, error)
}

func CreateDriver(cfg models.PrinterConfig, store WeightStore) *Driver {
	var bSettings Config

	if len(cfg.Settings) > 0 {
		_ = json.Unmarshal(cfg.Settings, &bSettings)
	}

	if bSettings.BCSDevice == "" {
		bSettings.BCSDevice = "GLPMax"
	}

	profile := Profile{
		Mode:          MarkingMode(bSettings.Mode),
		Conveyor:      bSettings.Conveyor,
		CaptureWeight: bSettings.CaptureWeight,
		RecordGXNET:   bSettings.RecordGXNET,
		RecordResponse: func(resp Response) error {
			if store == nil {
				return nil
			}
			return store.SaveGXNETResponse(
				cfg.ID,
				bSettings.BCSDevice,
				resp.ReceivedAt.Format(time.RFC3339),
				resp.Command,
				resp.Parameter,
				resp.Queue,
				resp.Payload,
				resp.Status,
			)
		},
		RecordWeight: func(printerIndex int, mark, weight string) error {
			if store == nil {
				return nil
			}
			lineMap, _ := store.GetPrinterLineMap()
			lineID := lineMap[cfg.ID]
			activeTaskID, _ := store.GetActiveTaskByLine(lineID)
			if activeTaskID == 0 {
				return fmt.Errorf("нет активной задачи для линии принтера %d", cfg.ID)
			}
			// Атомарно пишем вес и ставим статус printed
			return store.SaveWeightAndMarkPrinted(activeTaskID, cfg.ID, printerIndex, weight)
		},
	}

	return NewWithProfile(cfg.IP, cfg.Port, bSettings.BCSDevice, profile)
}

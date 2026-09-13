package bizerba

type Config struct {
	BCSDevice     string `json:"bcs_device"`
	CaptureWeight bool   `json:"capture_weight"`
	RecordGXNET   bool   `json:"record_gxnet"`
	Mode          string `json:"mode"`     // stream / unique
	Conveyor      bool   `json:"conveyor"` // управление конвейером
}

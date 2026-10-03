package bizerba

type Config struct {
	BCSDevice           string `json:"bcs_device"`
	CaptureWeight       bool   `json:"capture_weight"`
	RecordGXNET         bool   `json:"record_gxnet"`
	Mode                string `json:"mode"` // stream / stream_gt06_gt07 / unique
	Conveyor            bool   `json:"conveyor"`
	ExplicitGSSeparator *bool  `json:"explicit_gs_separator"`
}

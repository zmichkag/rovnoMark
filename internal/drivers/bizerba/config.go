package bizerba

// FieldMapping описывает привязку полей для Bizerba (GxNet/BCS).
type FieldMapping struct {
	SerialField string            `json:"serial_field"` // Куда слать серийник КМ (по умолчанию "GT03")
	CryptoField string            `json:"crypto_field"` // Куда слать криптохвост КМ (по умолчанию "GT04")
	GTINField   string            `json:"gtin_field"`   // Куда слать GTIN, если ШК2 пустой (например, "GT22")
	StaticMap   map[string]string `json:"static_map"`   // Поля 1С -> регистры Bizerba ("composition": "GT02")
}

type Config struct {
	BCSDevice           string       `json:"bcs_device"`
	CaptureWeight       bool         `json:"capture_weight"`
	RecordGXNET         bool         `json:"record_gxnet"`
	Mode                string       `json:"mode"` // stream / stream_gt06_gt07 / unique
	Conveyor            bool         `json:"conveyor"`
	ExplicitGSSeparator *bool        `json:"explicit_gs_separator"`
	Mapping             FieldMapping `json:"mapping"` // маппинг полей
}

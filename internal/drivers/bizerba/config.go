package bizerba

// FieldMapping описывает привязку логических полей к регистрам Bizerba (GTxx, GLxx).
type FieldMapping struct {
	SerialField string            `json:"serial_field"` // Куда слать серийник КМ (по умолчанию "GT03")
	CryptoField string            `json:"crypto_field"` // Куда слать криптохвост КМ (по умолчанию "GT04")
	GTINField   string            `json:"gtin_field"`   // Куда слать GTIN марки, если ШК2 пустой (например, "GT22"). Если пусто — ШК2 не трогаем!
	StaticMap   map[string]string `json:"static_map"`   // Маппинг полей 1С: "date01" -> "GL06", "composition" -> "GT02" и т.д.
}

type Config struct {
	BCSDevice     string       `json:"bcs_device"`
	CaptureWeight bool         `json:"capture_weight"`
	RecordGXNET   bool         `json:"record_gxnet"`
	Mode          string       `json:"mode"`     // stream / unique
	Conveyor      bool         `json:"conveyor"` // управление конвейером
	Mapping       FieldMapping `json:"mapping"`  // сопоставление полей
}

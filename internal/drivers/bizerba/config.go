package bizerba

// FieldMapping задает привязку полей для конкретного принтера Bizerba
type FieldMapping struct {
	SerialField string            `json:"serial_field"` // по умолчанию "GT03"
	CryptoField string            `json:"crypto_field"` // по умолчанию "GT04"
	GTINField   string            `json:"gtin_field"`   // виртуальное поле (например "GT05" или "GT02")
	StaticMap   map[string]string `json:"static_map"`   // ключ из 1С -> регистр Bizerba (напр. "date01": "GL06")
}

type Config struct {
	BCSDevice     string       `json:"bcs_device"`
	CaptureWeight bool         `json:"capture_weight"`
	RecordGXNET   bool         `json:"record_gxnet"`
	Mode          string       `json:"mode"`     // stream / unique
	Conveyor      bool         `json:"conveyor"` // управление конвейером
	Mapping       FieldMapping `json:"mapping"`  // маппинг полей
}

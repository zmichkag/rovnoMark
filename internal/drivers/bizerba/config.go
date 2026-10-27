package bizerba

// FieldMapping описывает сопоставление полей 1С с регистрами Bizerba (GxNet).
type FieldMapping struct {
	SerialField string            `json:"serial_field"` // Регистр для серийника КМ (по умолчанию "GT03")
	CryptoField string            `json:"crypto_field"` // Регистр для криптохвоста КМ (по умолчанию "GT04")
	GTINField   string            `json:"gtin_field"`   // Виртуальный регистр для GTIN весового товара (например "GT22")
	StaticMap   map[string]string `json:"static_map"`   // 1С ключ -> регистр Bizerba ("composition": "GT02", "date01": "GL06")
}

type Config struct {
	BCSDevice     string       `json:"bcs_device"`
	CaptureWeight bool         `json:"capture_weight"`
	RecordGXNET   bool         `json:"record_gxnet"`
	Mode          string       `json:"mode"`     // stream / unique
	Conveyor      bool         `json:"conveyor"` // управление конвейером
	Mapping       FieldMapping `json:"mapping"`  // декларативный маппинг
}

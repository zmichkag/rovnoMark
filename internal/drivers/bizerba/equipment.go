package bizerba

import "strings"

type MarkingMode string

const (
	MarkingModeStream MarkingMode = "stream"
	MarkingModeUnique MarkingMode = "unique"
)

type Profile struct {
	RecordGXNET    bool
	RecordResponse func(Response) error
	Mode           MarkingMode
	Conveyor       bool
	CaptureWeight  bool
	RecordWeight   func(printerIndex int, mark, weight string) error
	Mapping        FieldMapping
}

type bizerbaEquipment struct {
	mode            MarkingMode
	conveyorEnabled bool
	conveyor        conveyorController
	unique          uniqueDataController
	captureWeight   bool
	recordWeight    func(printerIndex int, mark, weight string) error
	mapping         FieldMapping
}

func defaultBizerbaEquipment() bizerbaEquipment {
	return configuredBizerbaEquipment(Profile{})
}
func configuredBizerbaEquipment(profile Profile) bizerbaEquipment {
	profile = normalizeProfile(profile)
	unique := uniqueDataController(disabledUniqueDataController{})
	if profile.Mode == MarkingModeUnique {
		unique = stubUniqueDataController{}
	}

	mapping := profile.Mapping
	if strings.TrimSpace(mapping.SerialField) == "" {
		mapping.SerialField = "GT03"
	}
	if strings.TrimSpace(mapping.CryptoField) == "" {
		mapping.CryptoField = "GT04"
	}
	if mapping.StaticMap == nil {
		mapping.StaticMap = make(map[string]string)
	}

	return bizerbaEquipment{
		mode:            profile.Mode,
		conveyorEnabled: profile.Conveyor,
		conveyor:        noopConveyorController{},
		unique:          unique,
		captureWeight:   profile.CaptureWeight,
		recordWeight:    profile.RecordWeight,
		mapping:         mapping,
	}
}

func normalizeProfile(profile Profile) Profile {
	switch MarkingMode(strings.ToLower(strings.TrimSpace(string(profile.Mode)))) {
	case MarkingModeUnique:
		profile.Mode = MarkingModeUnique
	default:
		profile.Mode = MarkingModeStream
	}
	return profile
}

// conveyorController управляет конвейером ездовой Bizerba.
// Start должен вызываться только после успешной подготовки первой марки или
// готовности буфера Unique, Stop — при завершении задания и на любом аварийном выходе.
type conveyorController interface {
	Start(bcsConnection) error
	Stop(bcsConnection) error
}

// uniqueDataController описывает отдельный режим передачи кодов через Unique.
// Begin вызывается после установки PLU, Append принимает очередную порцию полных
// GS1-кодов, Ready проверяет готовность устройства, End завершает или отменяет сеанс.
// Реализация должна скрывать создание временного файла и вызов BCS UploadFileFTP.
type uniqueDataController interface {
	Enabled() bool
	Begin(conn bcsConnection, plu string) error
	Append(conn bcsConnection, codes []string) error
	Ready(conn bcsConnection) (bool, error)
	End(conn bcsConnection) error
}

// noopConveyorController — безопасная заглушка для стационарной Bizerba.
type noopConveyorController struct{}

// Start ничего не делает для машины без управляемого конвейера.
func (noopConveyorController) Start(bcsConnection) error { return nil }

// Stop ничего не делает для машины без управляемого конвейера.
func (noopConveyorController) Stop(bcsConnection) error { return nil }

// disabledUniqueDataController обозначает обычный потоковый режим GT03/GT04.
// Его методы ничего не отправляют устройству, поэтому существующие установки
// продолжают работать без дополнительных настроек.
type disabledUniqueDataController struct{}

// Enabled сообщает, что для устройства выбран обычный потоковый режим.
func (disabledUniqueDataController) Enabled() bool { return false }

// Begin не начинает Unique-сеанс в обычном потоковом режиме.
func (disabledUniqueDataController) Begin(bcsConnection, string) error { return nil }

// Append не загружает файл Unique в обычном потоковом режиме.
func (disabledUniqueDataController) Append(bcsConnection, []string) error { return nil }

// Ready сообщает, что буфер Unique не используется.
func (disabledUniqueDataController) Ready(bcsConnection) (bool, error) { return false, nil }

// End не завершает Unique-сеанс в обычном потоковом режиме.
func (disabledUniqueDataController) End(bcsConnection) error { return nil }

// stubUniqueDataController отмечает выбранный профиль Unique, но до реализации
// файлового обмена не отправляет команды GW7D/XX13 и не вызывает UploadFileFTP.
type stubUniqueDataController struct{}

// Enabled сообщает, что в настройках устройства выбран режим Unique.
func (stubUniqueDataController) Enabled() bool { return true }

// Begin резервирует подготовку Unique после установки PLU.
func (stubUniqueDataController) Begin(bcsConnection, string) error { return nil }

// Append резервирует загрузку очередной порции полных GS1-кодов.
func (stubUniqueDataController) Append(bcsConnection, []string) error { return nil }

// Ready пока не опрашивает SW9B и сообщает об отсутствии аппаратной реализации.
func (stubUniqueDataController) Ready(bcsConnection) (bool, error) { return false, nil }

// End резервирует безопасное завершение Unique-сеанса.
func (stubUniqueDataController) End(bcsConnection) error { return nil }

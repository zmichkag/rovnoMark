package bizerba

import "strings"

// MarkingMode определяет способ передачи кодов конкретной Bizerba.
type MarkingMode string

const (
	MarkingModeStream MarkingMode = "stream"
	MarkingModeUnique MarkingMode = "unique"
)

// Profile содержит настройки, относящиеся только к оборудованию Bizerba.
type Profile struct {
	RecordGXNET    bool
	RecordResponse func(Response) error
	Mode           MarkingMode
	Conveyor       bool
	CaptureWeight  bool
	RecordWeight   func(printerIndex int, mark, weight string) error
	Mapping        FieldMapping
}

// bizerbaEquipment объединяет возможности конкретной машины.
type bizerbaEquipment struct {
	mode            MarkingMode
	conveyorEnabled bool
	conveyor        conveyorController
	unique          uniqueDataController
	captureWeight   bool
	recordWeight    func(printerIndex int, mark, weight string) error
	mapping         FieldMapping
}

// conveyorController управляет конвейером Bizerba.
type conveyorController interface {
	Start(bcsConnection) error
	Stop(bcsConnection) error
}

// uniqueDataController описывает режим передачи кодов через Unique.
type uniqueDataController interface {
	Enabled() bool
	Begin(conn bcsConnection, plu string) error
	Append(conn bcsConnection, codes []string) error
	Ready(conn bcsConnection) (bool, error)
	End(conn bcsConnection) error
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

type noopConveyorController struct{}

func (noopConveyorController) Start(bcsConnection) error { return nil }
func (noopConveyorController) Stop(bcsConnection) error  { return nil }

type disabledUniqueDataController struct{}

func (disabledUniqueDataController) Enabled() bool                     { return false }
func (disabledUniqueDataController) Begin(bcsConnection, string) error { return nil }
func (disabledUniqueDataController) Append(bcsConnection, []string) error {
	return nil
}
func (disabledUniqueDataController) Ready(bcsConnection) (bool, error) { return false, nil }
func (disabledUniqueDataController) End(bcsConnection) error           { return nil }

type stubUniqueDataController struct{}

func (stubUniqueDataController) Enabled() bool                     { return true }
func (stubUniqueDataController) Begin(bcsConnection, string) error { return nil }
func (stubUniqueDataController) Append(bcsConnection, []string) error {
	return nil
}
func (stubUniqueDataController) Ready(bcsConnection) (bool, error) { return false, nil }
func (stubUniqueDataController) End(bcsConnection) error           { return nil }

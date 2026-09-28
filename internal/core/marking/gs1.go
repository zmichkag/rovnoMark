package marking

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

const (
	ASCII_GS   = "\x1d" // ASCII 29 (Group Separator)
	ASCII_FNC1 = "\xe8" // ASCII 232 (GS1 DataMatrix Symbol Attribute)
)

var (
	ErrEmptyCode          = errors.New("пустая строка кода маркировки")
	ErrMissingAI01        = errors.New("отсутствует или невалиден AI '01' (GTIN должен состоять из 14 цифр)")
	ErrMissingAI21        = errors.New("отсутствует или невалиден AI '21' (длина блока 6 символов)")
	ErrInvalidCountryCode = errors.New("недопустимый идентификатор государства ЕАЭС в AI '21'")
	ErrMissingGS          = errors.New("отсутствует символ-разделитель FNC1/GS (ASCII 29) после группы AI '21'")
	ErrMissingAI93        = errors.New("отсутствует или невалиден AI '93' (код проверки должен состоять из 4 символов)")
)

var validCountryCodes = map[byte]string{
	'1': "Республика Армения", 'A': "Республика Армения", 'a': "Республика Армения",
	'2': "Республика Беларусь", 'B': "Республика Беларусь", 'b': "Республика Беларусь",
	'3': "Республика Казахстан", 'C': "Республика Казахстан", 'c': "Республика Казахстан",
	'4': "Киргизская Республика", 'D': "Киргизская Республика", 'd': "Киргизская Республика",
	'5': "Российская Федерация", 'E': "Российская Федерация", 'e': "Российская Федерация",
}

var (
	serialRegex = regexp.MustCompile(`^[A-Za-z0-9!"%&'()*+,\-./:;<=>?_]{5}$`)
	cryptoRegex = regexp.MustCompile(`^[A-Za-z0-9!"%&'()*+,\-./:;<=>?_]{4}$`)
)

type ShortDataMatrix struct {
	GTIN         string
	CountryCode  string
	Serial       string
	CryptoTail   string
	HasStartFNC1 bool
}

// NormalizeToCanonical преобразует любые внешние диалекты и экранирования (1C, JSON, Videojet, TSC)
// в единый канонический формат хранения БД с маркером <GS>.
// Безопасен для длинных кодов (AI 91, 92), не ломает структуру данных при отключенной валидации.
func NormalizeToCanonical(raw string) string {
	work := strings.TrimSpace(raw)
	if len(work) == 0 {
		return ""
	}

	// 1. Срезаем стартовые FNC1 / служебные префиксы (в БД код должен начинаться строго с 01)
	prefixesToTrim := []string{
		"<fcn>", "<FCN>",
		"<GS>", "<gs>",
		ASCII_FNC1,
		ASCII_GS,
		"~1",
		"~d029",
		"{FNC1}",
	}
	for _, p := range prefixesToTrim {
		if strings.HasPrefix(work, p) {
			work = strings.TrimPrefix(work, p)
			break
		}
	}

	// 2. Унифицируем все возможные варианты разделителя групп в канонический <GS>
	// Порядок важен: сначала длинные текстовые токены, затем сырые байты
	replacer := strings.NewReplacer(
		"\\u001d", "<GS>",
		"\\u001D", "<GS>",
		"\u001d", "<GS>",
		"\u001D", "<GS>",
		ASCII_GS, "<GS>",
		"~d029", "<GS>",
		"\\029", "<GS>",
		"{FNC1}", "<GS>",
	)
	work = replacer.Replace(work)

	// 3. Если где-то внутри остался одиночный ~1 перед AI (например ~193 или ~191) — меняем на <GS>
	work = strings.ReplaceAll(work, "~191", "<GS>91")
	work = strings.ReplaceAll(work, "~192", "<GS>92")
	work = strings.ReplaceAll(work, "~193", "<GS>93")

	return work
}

func ParseAndValidateShortGS1(raw string) (*ShortDataMatrix, error) {
	// Сначала пропускаем через нормализатор: чистим стартовый мусор и приводим разделители к <GS>
	work := NormalizeToCanonical(raw)
	if len(work) == 0 {
		return nil, ErrEmptyCode
	}

	hasStartFNC1 := true // Для валидного GS1 DataMatrix наличие подразумевается стандартом

	// 1. Группа 1: AI '01' + 14 цифр GTIN
	if !strings.HasPrefix(work, "01") {
		return nil, ErrMissingAI01
	}
	work = work[2:]

	// ЗАЩИТА ОТ 1С: Если на 13-й позиции стоит "21", значит GTIN 13-значный (обрезан)
	if len(work) >= 15 && work[13:15] == "21" {
		return nil, ErrMissingAI01
	}

	// Классическая проверка на 14 символов
	if len(work) < 14 {
		return nil, ErrMissingAI01
	}

	// Валидируем, что GTIN состоит ТОЛЬКО из цифр
	gtin := work[:14]
	for _, ch := range gtin {
		if ch < '0' || ch > '9' {
			return nil, ErrMissingAI01
		}
	}

	// Отрезаем валидный 14-значный GTIN
	work = work[14:]

	// 2. Группа 2: AI '21' + 6 символов
	if !strings.HasPrefix(work, "21") {
		return nil, ErrMissingAI21
	}
	work = work[2:]

	// Проверяем длину хвоста для серийника и кода страны
	if len(work) < 6 {
		return nil, ErrMissingAI21
	}

	countryChar := work[0]
	if _, ok := validCountryCodes[countryChar]; !ok {
		return nil, fmt.Errorf("%w: '%c'", ErrInvalidCountryCode, countryChar)
	}

	serial := work[1:6]
	if !serialRegex.MatchString(serial) {
		return nil, fmt.Errorf("недопустимые символы в серийном номере AI '21': %s", serial)
	}
	work = work[6:]

	// 3. Проверка символа-разделителя GS / FNC1 (после NormalizeToCanonical здесь всегда <GS>)
	if strings.HasPrefix(work, "<GS>") {
		work = work[4:]
	} else if strings.HasPrefix(work, "29") { // Бывает в кривых выгрузках 1С как чистый текст
		work = work[2:]
	} else {
		return nil, ErrMissingGS
	}

	// 4. Группа 3: AI '93' + 4 символа
	if !strings.HasPrefix(work, "93") {
		return nil, ErrMissingAI93
	}
	work = work[2:]

	if len(work) < 4 || !cryptoRegex.MatchString(work[:4]) {
		return nil, ErrMissingAI93
	}
	cryptoTail := work[:4]

	return &ShortDataMatrix{
		GTIN:         gtin,
		CountryCode:  string(countryChar),
		Serial:       serial,
		CryptoTail:   cryptoTail,
		HasStartFNC1: hasStartFNC1,
	}, nil
}

// ToDBFormat возвращает канонический формат для записи в SQLite: 01...21...<GS>93...
func (m *ShortDataMatrix) ToDBFormat() string {
	return fmt.Sprintf("01%s21%s%s<GS>93%s", m.GTIN, m.CountryCode, m.Serial, m.CryptoTail)
}

// ToVideojetFormat возвращает готовую строку под Videojet CLARiTY (разделитель ~d029, стартовый ~1)
func (m *ShortDataMatrix) ToVideojetFormat() string {
	return fmt.Sprintf("~101%s21%s%s~d02993%s", m.GTIN, m.CountryCode, m.Serial, m.CryptoTail)
}

// ToRawGS1Format возвращает классическую бинарную строку с байтами 0x1D для Carl Valentin / принтеров с прямым сокетом
func (m *ShortDataMatrix) ToRawGS1Format(includeStartFNC1 bool) string {
	var sb strings.Builder
	if includeStartFNC1 {
		sb.WriteString(ASCII_FNC1)
	}
	sb.WriteString("01")
	sb.WriteString(m.GTIN)
	sb.WriteString("21")
	sb.WriteString(m.CountryCode)
	sb.WriteString(m.Serial)
	sb.WriteString(ASCII_GS)
	sb.WriteString("93")
	sb.WriteString(m.CryptoTail)
	return sb.String()
}

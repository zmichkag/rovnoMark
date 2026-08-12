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

func ParseAndValidateShortGS1(raw string) (*ShortDataMatrix, error) {
	work := strings.TrimSpace(raw)
	if len(work) == 0 {
		return nil, ErrEmptyCode
	}

	hasStartFNC1 := false

	// 1. Проверка и отсечение стартового FNC1 (<fcn>, \xe8, \x1d, <GS>)
	if strings.HasPrefix(strings.ToLower(work), "<fcn>") {
		hasStartFNC1 = true
		work = work[5:]
	} else if strings.HasPrefix(work, ASCII_FNC1) || (len(work) > 0 && work[0] == 232) {
		hasStartFNC1 = true
		work = work[1:]
	} else if strings.HasPrefix(work, ASCII_GS) || strings.HasPrefix(work, "<GS>") {
		hasStartFNC1 = true
		if strings.HasPrefix(work, "<GS>") {
			work = work[4:]
		} else {
			work = work[1:]
		}
	}

	// 2. Группа 1: AI '01' + 14 цифр GTIN
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

	// 3. Группа 2: AI '21' + 6 символов
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

	// 4. Символ-разделитель GS / FNC1 (\x1d, <GS>, или "29")
	if strings.HasPrefix(work, "<GS>") {
		work = work[4:]
	} else if strings.HasPrefix(work, "29") {
		work = work[2:]
	} else if len(work) > 0 && (work[0] == 29 || work[:1] == ASCII_GS) {
		work = work[1:]
	} else {
		return nil, ErrMissingGS
	}

	// 5. Группа 3: AI '93' + 4 символа
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

func (m *ShortDataMatrix) ToDBFormat() string {
	return fmt.Sprintf("01%s21%s%s<GS>93%s", m.GTIN, m.CountryCode, m.Serial, m.CryptoTail)
}

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

package marking

import (
	"errors"
	"testing"
)

func TestParseAndValidateShortGS1(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantErr     error
		checkResult func(t *testing.T, res *ShortDataMatrix)
	}{
		{
			name:    "Успех: Идеальный код с тегом <fcn> и разделителем 29",
			input:   "<fcn>0104600840749372215!IS(;2993dGVz",
			wantErr: nil,
			checkResult: func(t *testing.T, res *ShortDataMatrix) {
				if res.GTIN != "04600840749372" {
					t.Errorf("expected GTIN '04600840749372', got '%s'", res.GTIN)
				}
				if res.CountryCode != "5" {
					t.Errorf("expected CountryCode '5', got '%s'", res.CountryCode)
				}
				if res.Serial != "!IS(;" {
					t.Errorf("expected Serial '!IS(;', got '%s'", res.Serial)
				}
				if res.CryptoTail != "dGVz" {
					t.Errorf("expected CryptoTail 'dGVz', got '%s'", res.CryptoTail)
				}
				if !res.HasStartFNC1 {
					t.Error("expected HasStartFNC1 == true")
				}
			},
		},
		{
			name:    "Успех: Код с символом GS (ASCII 29) и без префикса",
			input:   "0104600840749372211ABCDE\x1d93a1B2",
			wantErr: nil,
			checkResult: func(t *testing.T, res *ShortDataMatrix) {
				if res.CountryCode != "1" {
					t.Errorf("expected CountryCode '1', got '%s'", res.CountryCode)
				}
				if res.Serial != "ABCDE" {
					t.Errorf("expected Serial 'ABCDE', got '%s'", res.Serial)
				}
				if res.CryptoTail != "a1B2" {
					t.Errorf("expected CryptoTail 'a1B2', got '%s'", res.CryptoTail)
				}
			},
		},
		{
			name:    "Успех: Код с подставным <GS> от 1С",
			input:   "0104600840749372212BCDEF<GS>931234",
			wantErr: nil,
			checkResult: func(t *testing.T, res *ShortDataMatrix) {
				if res.CountryCode != "2" {
					t.Errorf("expected CountryCode '2', got '%s'", res.CountryCode)
				}
			},
		},
		{
			name:    "Ошибка: Пустой код",
			input:   "   ",
			wantErr: ErrEmptyCode,
		},
		{
			name:    "Ошибка: Отсутствует AI 01 в начале",
			input:   "0204600840749372215!IS(;\x1d93dGVz",
			wantErr: ErrMissingAI01,
		},
		{
			name:    "Ошибка: Короткий GTIN (13 цифр вместо 14)",
			input:   "010460084074937215!IS(;\x1d93dGVz",
			wantErr: ErrMissingAI01,
		},
		{
			name:    "Ошибка: Нет AI 21",
			input:   "0104600840749372225!IS(;\x1d93dGVz",
			wantErr: ErrMissingAI21,
		},
		{
			name:    "Ошибка: Невалидный код страны ЕАЭС (символ '9')",
			input:   "0104600840749372219!IS(;\x1d93dGVz",
			wantErr: ErrInvalidCountryCode,
		},
		{
			name:    "Ошибка: Забыли разделитель GS перед AI 93",
			input:   "0104600840749372215!IS(;93dGVz",
			wantErr: ErrMissingGS,
		},
		{
			name:    "Ошибка: Неверный AI 93",
			input:   "0104600840749372215!IS(;\x1d99dGVz",
			wantErr: ErrMissingAI93,
		},
		{
			name:    "Ошибка: Длина криптохвоста AI 93 меньше 4 символов",
			input:   "0104600840749372215!IS(;\x1d93dG",
			wantErr: ErrMissingAI93,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseAndValidateShortGS1(tt.input)

			if tt.wantErr != nil {
				if err == nil {
					t.Fatalf("expected error '%v', got nil", tt.wantErr)
				}
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("expected error '%v', got '%v'", tt.wantErr, err)
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if tt.checkResult != nil {
					tt.checkResult(t, got)
				}
			}
		})
	}
}

func TestShortDataMatrix_Formats(t *testing.T) {
	raw := "0104600840749372215!IS(;\x1d93dGVz"
	parsed, err := ParseAndValidateShortGS1(raw)
	if err != nil {
		t.Fatalf("failed to parse valid code: %v", err)
	}

	dbFormat := parsed.ToDBFormat()
	expectedDB := "0104600840749372215!IS(;<GS>93dGVz"
	if dbFormat != expectedDB {
		t.Errorf("ToDBFormat() = %s; want %s", dbFormat, expectedDB)
	}

	rawFormat := parsed.ToRawGS1Format(true)
	expectedRaw := "\xe80104600840749372215!IS(;\x1d93dGVz"
	if rawFormat != expectedRaw {
		t.Errorf("ToRawGS1Format(true) = %q; want %q", rawFormat, expectedRaw)
	}
}

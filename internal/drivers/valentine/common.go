package valentine

// Константы протокола Carl Valentin (CVPL)
const (
	SOH = 0x01 // Start of Header (Начало блока передачи данных)
	ETB = 0x17 // End of Text Block (Конец блока передачи данных)
	CR  = 0x0D // Carriage Return
	LF  = 0x0A // Line Feed
	ESC = 0x1B // Escape-символ для служебных запросов реального времени
)

// Валентиновские статусы ошибок (из мануала)
const (
	BitRibbonError = 1 << 0 // Конец красящей ленты
	BitPaperError  = 1 << 1 // Конец этикеток
	BitHeadError   = 1 << 2 // Ошибка термоголовки
	BitPauseMode   = 1 << 5 // Режим паузы
	BitReadyMode   = 1 << 4 // Ожидание внешнего сигнала / Печать
)

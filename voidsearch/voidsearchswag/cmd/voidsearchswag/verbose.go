package main

import (
	"os"
)

// detectVerbose ищет признак подробного вывода в аргументах и в окружении.
//
// Вызывается до разбора субкоманды, поэтому флаг работает в любой позиции:
// и `voidsearchswag --verbose search ...`, и `voidsearchswag search ... --verbose`.
// Отдельный разбор нужен потому, что каждая субкоманда создаёт свой FlagSet и
// общий флаг в него не попадает - неизвестный флаг там ошибка.
//
// Короткий `-v` намеренно не поддерживается: на верхнем уровне он уже означает
// `version` (`voidsearchswag -v` печатает версию). Два разных смысла у одного
// ключа дали бы `voidsearchswag -v search ...` как запрос версии с игнорированием
// остального, поэтому остаются только однозначные формы.
func detectVerbose(args []string) {
	if os.Getenv("VOIDSEARCH_VERBOSE") != "" {
		verboseLogs = true
	}
	for _, a := range args {
		if a == "--verbose" || a == "-verbose" {
			verboseLogs = true
		}
	}
}

// verboseArgs убирает признак подробного вывода из списка, который дальше
// разбирает FlagSet субкоманды, чтобы он не воспринял его как неизвестный флаг.
func verboseArgs(args []string) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		if a == "--verbose" || a == "-verbose" {
			continue
		}
		out = append(out, a)
	}
	return out
}

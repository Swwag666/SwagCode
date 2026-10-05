package main

import (
	"flag"
	"strings"
)

// flagOrConfig возвращает значение флага, а при нуле - значение конфига. Ноль в
// флагах discover означает «не переопределять конфиг», а не «выключить»: оба флага
// имеют рабочий дефолт в конфиге, и нулём оператор говорит, что менять его не
// хочет.
func flagOrConfig(flagValue, cfgValue int) int {
	if flagValue > 0 {
		return flagValue
	}
	return cfgValue
}

// strFlagOrConfig возвращает значение строкового флага, а при пустой строке -
// значение конфига, а при пустом конфиге - дефолт defValue. Пустая строка у флага
// означает «не переопределять конфиг» по тому же правилу, по которому ноль делает
// это у числовых флагов (flagOrConfig).
//
// Третий слой нужен потому, что пустое значение в конфиге законно: netx подставляет
// дефолт сам, через orStr, и без него команда печатала бы пустое место вместо того
// значения, которое действительно уйдёт в запрос. Дефолт отдаётся вызывающим, а не
// зашит здесь: у разных полей он разный, и второй источник правды в CLI не нужен.
// Значение возвращается обрезанным по краям: пробелы вокруг «socks5 » попали бы
// в query-параметр запроса как есть.
func strFlagOrConfig(flagValue, cfgValue, defValue string) string {
	for _, v := range []string{flagValue, cfgValue, defValue} {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return defValue
}

// boolFlagOrConfig возвращает значение булева флага, а если флаг не назван в
// аргументах - значение конфига.
//
// Отдельная функция от strFlagOrConfig нужна из-за отсутствия у булева флага
// «пустого» значения: false - рабочий результат, а не признак «не задано». Без
// проверки по списку разобранных флагов конфиг, который просит проверку живости
// не делать, невозможно было бы перебить обратно: --skip-verify=false давал бы
// тот же false, что и отсутствие флага.
func boolFlagOrConfig(fs *flag.FlagSet, name string, flagValue, cfgValue bool) bool {
	if flagPassed(fs, name) {
		return flagValue
	}
	return cfgValue
}

// flagPassed сообщает, был ли флаг назван в разобранных аргументах. fs.Visit
// обходит только те флаги, которым значение присвоил разбор, поэтому обе формы
// --skip-verify и --skip-verify=false он видит, а нетронутый флаг - нет.
func flagPassed(fs *flag.FlagSet, name string) bool {
	passed := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			passed = true
		}
	})
	return passed
}

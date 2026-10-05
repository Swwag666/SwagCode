package main

import (
	"fmt"
	"strings"

	"voidsearchswag/internal/discover"
	"voidsearchswag/internal/netx"
)

// discoverSeedsWarning возвращает текст предупреждения о сидах, которые прогон не
// использовал, и пустую строку, когда предупреждать не о чем.
//
// opts.Seeds читает единственная ветка: internal/discover/pool.go пускает сиды в
// очередь обхода под условием opts.WithCrawl, поэтому без --crawl флаг не попадает
// ни в список источников, ни в обход. Живой замер на бинаре 0194448, копия боевой
// базы, тор выключен, транспорт direct, таймаут запроса 3s:
//
//	discover --seeds http://abcdefghijklmnop.onion/list --json
//	  rc=0, 8 штатных источников, найдено 11123, раздел crawl отсутствует,
//	  stderr 0 байт, сид не упомянут в выводе ни разу
//
//	discover --crawl --seeds http://abcdefghijklmnop.onion/list --json
//	  тот же сид попал в pages_detail: host abcdefghijklmnop.onion, pages 1,
//	  no_transport true, причина «onion-адрес недостижим напрямую»
//
// То есть флаг принимался и отбрасывался молча, а отчёт выглядел как успешная
// разведка по всем правилам. Значение разбирается тем же правилом, что и прочие
// списки флагов: пустые части отбрасываются, поэтому «--seeds ,» сидом не
// считается и предупреждения не заслуживает.
func discoverSeedsWarning(seeds string, crawl bool) string {
	if crawl || len(splitList(seeds)) == 0 {
		return ""
	}
	return "--seeds применяется только вместе с --crawl: сиды задают стартовые хосты обхода, и этот прогон их не использовал"
}

// warnUnusedSeeds печатает предупреждение о неприменённых сидах. Отдельная
// функция, а не строка внутри cmdDiscover: команда поднимает конфиг, базу и
// транспорт, а печать обязана проверяться без всего этого.
func warnUnusedSeeds(log netx.Logger, seeds string, crawl bool) {
	if w := discoverSeedsWarning(seeds, crawl); w != "" {
		log.Warnf("discover: %s", w)
	}
}

// invalidSeedsWarning возвращает текст предупреждения о сидах, которые обход не
// примет за onion-адрес, и пустую строку, когда распознаны все.
//
// Проверка та же, что в обходе: internal/discover пускает сид в очередь только
// если HostOf вернул непустой хост, а HostOf принимает base32-строку длиной 16
// или 56 символов. Всё остальное отбрасывается, и до правки отбрасывалось молча.
// Живой замер на HEAD 97194a2, копия боевой базы, тор выключен, транспорт direct:
//
//	discover --crawl --seeds http://abcdefghijklmnop.onion/list
//	                  --seeds http://qrstuvwxyz012345.onion/list --json
//	  rc=0, pages=1, no_transport=1, stderr 0 байт
//	discover --crawl --seeds http://qrstuvwxyz012345.onion/list --json
//	  rc=0, pages=0, no_transport=0, stderr 0 байт
//	discover --crawl --seeds https://example.com/list --json
//	  rc=0, pages=0, no_transport=0, stderr 0 байт
//
// Второй адрес выглядит как onion и отличается от рабочего одним символом: в
// base32 нет цифр 0 и 1. Оператор получал rc=0 и отчёт, в котором обход просто
// обошёл меньше хостов, чем задано, и не мог отличить опечатку в адресе от
// недоступности сети. Предупреждение печатается независимо от --crawl: оно
// отвечает на вопрос про значение флага, а discoverSeedsWarning - про режим, и
// путать их нельзя.
func invalidSeedsWarning(seeds []string) string {
	var bad []string
	for _, s := range seeds {
		if discover.HostOf(s) == "" {
			if s = strings.TrimSpace(s); s != "" {
				bad = append(bad, s)
			}
		}
	}
	if len(bad) == 0 {
		return ""
	}
	return fmt.Sprintf("сиды не распознаны как onion-адреса, обход их пропустит: %s (нужен base32-хост длиной 16 или 56 символов, цифры 0, 1, 8 и 9 в base32 не существуют)", strings.Join(bad, ", "))
}

// warnInvalidSeeds печатает предупреждение о нераспознанных сидах.
func warnInvalidSeeds(log netx.Logger, seeds []string) {
	if w := invalidSeedsWarning(seeds); w != "" {
		log.Warnf("discover: %s", w)
	}
}

package searchers

import (
	"strings"

	"github.com/PuerkitoBio/goquery"
)

// snippetClasses - классы и идентификаторы, которыми onion-движки размечают
// описание результата.
//
// Разметка у движков разная и ни одного общего стандарта нет: Ahmia отдаёт
// .result-body/.description, torch - .snippet, каталоги - .desc или .summary.
// Перебор по подстрокам в имени класса устойчивее точного совпадения и не
// требует знать каждый движок заранее.
var snippetClasses = []string{
	"snippet", "description", "desc", "summary", "abstract",
	"result-body", "resultbody", "result-desc", "resultdesc",
	"content", "preview", "excerpt", "text",
}

// snippetAttrs - то же для атрибутов data-*, которыми часть движков отдаёт
// описание отдельно от видимой разметки.
var snippetAttrs = []string{"data-description", "data-snippet", "data-desc", "data-summary"}

// maxSnippetRunes ограничивает длину сниппета.
//
// Описание у движков бывает полным абзацем, а CLI обрезает строку до 160
// знаков при печати. Обрезка здесь нужна не для вывода, а для того, чтобы
// длинный текст не доминировал в оценке релевантности: score начисляет за
// каждый токен запроса, найденный в сниппете, и абзац на 2000 знаков собирал бы
// совпадения там, где короткий честный сниппет их не дал бы.
const maxSnippetRunes = 320

// resultText собирает заголовок и сниппет результата из ссылки.
//
// Общая точка для onion-движков и универсального парсера: до неё обе ветки
// брали заголовок из a.Text() и не заполняли сниппет вовсе. Пустой сниппет в
// deep-режиме был не косметикой - score начисляет балл за каждый токен запроса,
// найденный в сниппете, поэтому результаты onion-движков систематически
// оценивались ниже clearnet-результатов с заполненным сниппетом при равной
// релевантности. Русскоязычные запросы страдали сильнее: у них меньше шансов
// совпасть с заголовком, и сниппет был единственным местом, где токены могли
// найтись.
//
// Заголовок-адрес заменяется последним осмысленным сегментом пути: onion-движки
// регулярно отдают ссылку, у которой видимый текст - сам адрес.
func resultText(a *goquery.Selection, rawURL string) (title, snippet string) {
	text := collapse(a.Text())
	title, isAddr := meaningfulTitle(text, rawURL)
	if isAddr {
		title = fallbackTitle(rawURL)
		// Откат на исходный текст здесь недопустим: именно его мы только что
		// признали адресом. Первая версия возвращала текст обратно, когда
		// fallbackTitle не давал ничего (URL с завершающим слэшем и без пути),
		// и заголовок-адрес благополучно переживал замену.
		//
		// Пустой заголовок честнее адреса: score штрафует его на 2 балла, но
		// адрес в заголовке бесполезен ровно так же, только ещё и занимает
		// место, где пользователь ждёт название сервиса.
		if title == "" {
			title = fallbackTitle(strings.TrimSuffix(strings.TrimSpace(rawURL), "/"))
		}
	}
	if title == "" && !isAddr {
		title = text
	}
	if title == "" {
		// Адрес-без-пути не даёт осмысленного заголовка. Пустая строка хуже
		// любой замены: CLI напечатал бы пустую строку там, где ожидается
		// название, а score штрафует пустой заголовок на 2 балла и результат
		// тонет ниже менее релевантных. Голый адрес не годится по той же
		// причине, по которой мы его только что убрали.
		//
		// Метка «(без названия)» уже принята в проекте: ровно её печатает
		// poolsearch для записей пула без названия. Переиспользование
		// соглашения важнее локальной формулировки - один и тот же случай
		// обязан выглядеть одинаково в разных командах.
		title = untitledLabel
	}
	snippet = extractSnippet(a)
	return title, snippet
}

// untitledLabel - метка результата без названия. Совпадает с тем, что печатает
// poolsearch для записей пула без названия.
const untitledLabel = "(без названия)"

// snippetBoilerplate - признаки текста, который является оформлением сайта, а не
// описанием результата.
//
// Нужен потому, что извлечение сниппета по классу контейнера приносит и мусор:
// в живом прогоне deep-режима движок отдал в качестве описания результата строку
// «Results for search query: BOOK /// Search engine for searching hidden
// services on the TOR network. Search For: Marketplaces, Drugs, Porn, Adult,
// Cryptocurrency,...» - это шапка и навигация самого движка, а не описание
// найденной страницы. Другой результат дал «Read more in our article on
// <адрес>» - футерный призыв.
//
// Такой сниппет хуже пустого: он выглядит как описание, пользователь читает его
// и делает вывод о содержании страницы, которого там нет. Плюс score начисляет
// за найденные в нём токены, поэтому boilerplate с перечислением категорий
// («Marketplaces, Drugs, Porn, Cryptocurrency») искусственно поднимал нерелевантный
// результат по любому из этих слов.
var snippetBoilerplate = []string{
	// Навигация и меню.
	"search for:", "search engine for", "results for search", "skip to content",
	"toggle navigation", "main menu", "home about", "menu home",
	// Футерные призывы и самопрезентация.
	"read more in our article", "read more on", "learn more about us",
	"all rights reserved", "copyright", "privacy policy", "terms of service",
	"terms and conditions", "cookie policy", "powered by",
	// Призывы к действию и реклама.
	"subscribe to", "sign up for", "follow us on", "share this",
	"add to cart", "buy now", "click here to",
	// Самоописание поисковика. Живой прогон deep-режима показал, что Ahmia при
	// пустой выдаче отдаёт ссылки со своей главной, а описанием у всех
	// результатов идёт одна и та же шапка сайта: «Ahmia searches hidden services
	// on the Tor network... Abuse material is not allowed on Ahmia». Эти фразы
	// описывают движок, а не найденную страницу.
	"searches hidden services", "abuse material is not allowed",
	"you need the tor browser bundle", "see our service blacklist",
	"contribute to the source code",
}

// isBoilerplateSnippet сообщает, является ли текст оформлением сайта.
//
// Сравнение по подстрокам в нижнем регистре, а не по точному совпадению:
// boilerplate почти всегда окружён другим текстом («... Cryptocurrency,...»),
// поэтому точное совпадение не сработало бы ни на одном реальном случае.
func isBoilerplateSnippet(s string) bool {
	low := strings.ToLower(s)
	for _, mark := range snippetBoilerplate {
		if strings.Contains(low, mark) {
			return true
		}
	}
	return false
}

// DropRepeatedSnippets убирает сниппет, общий для всех результатов одного
// источника.
//
// Причина не косметическая. Когда движок не находит ничего по запросу, он отдаёт
// свою главную страницу, а универсальный парсер принимает её ссылки за
// результаты. Описание у таких «результатов» одно на всех - шапка сайта. Живой
// прогон deep-режима дал четыре результата Ahmia с дословно одинаковым сниппетом
// и заголовками «Tor browser bundle», «contribute to the source code», «The Tor
// Project»: это навигация движка, а не найденные страницы.
//
// Одинаковый сниппет у всех результатов источника - надёжный признак, потому что
// у настоящих результатов описания различаются: они берутся из разных страниц.
// Проверка идёт по источнику, а не по всей выдаче: разные движки законно могут
// дать пересекающийся текст, а вот один движок с одним описанием на все свои
// результаты - это шапка.
//
// Сниппет обнуляется, а не удаляется весь результат. Убрать результат целиком
// значило бы потерять и те случаи, когда движок честно отдал несколько страниц с
// одинаковым описанием по теме запроса; обнуление же убирает ложное описание и не
// даёт ему завышать релевантность через score, который начисляет балл за каждый
// найденный в сниппете токен.
//
// Функция чистая: возвращает новый срез и не меняет входной, потому что выдача
// кэшируется и мутирование на месте испортило бы кэш.
func DropRepeatedSnippets(in []Result) []Result {
	if len(in) == 0 {
		return in
	}

	type key struct {
		source  string
		snippet string
	}
	// counts - сколько результатов источника несут данный сниппет.
	// withSnippet - сколько всего результатов источника со сниппетом.
	counts := make(map[key]int)
	withSnippet := make(map[string]int)
	for _, r := range in {
		s := strings.TrimSpace(r.Snippet)
		if s == "" {
			continue
		}
		counts[key{r.Source, s}]++
		withSnippet[r.Source]++
	}

	out := make([]Result, len(in))
	copy(out, in)
	for i := range out {
		s := strings.TrimSpace(out[i].Snippet)
		if s == "" {
			continue
		}
		k := key{out[i].Source, s}
		// Обнуляем, только если сниппет повторяется и им покрыты все результаты
		// источника со сниппетом. Единственный результат источника не
		// обнуляется: повторять нечего, и описание может быть настоящим.
		if counts[k] > 1 && counts[k] == withSnippet[out[i].Source] {
			out[i].Snippet = ""
		}
	}
	return out
}

// minSnippetRunes - минимальная осмысленная длина сниппета.
//
// Одно-два слова описанием не являются: это огрызок заголовка или метка
// элемента разметки. Порог в 12 рун отсекает «Home», «Read more», «Search»,
// пропуская настоящие описания, которые практически никогда не короче
// предложения.
const minSnippetRunes = 12

// extractSnippet ищет описание результата рядом со ссылкой.
//
// Возвращает пустую строку, если описания нет: это нормальное состояние, а не
// ошибка, потому что часть движков отдаёт только заголовок и адрес. Пустой
// сниппет вызывающий переживает - score просто не начислит за него балл.
//
// Порядок перебора от точного к приблизительному:
//  1. атрибуты data-* самой ссылки;
//  2. элементы с описанием внутри контейнера результата;
//  3. соседние элементы того же контейнера;
//  4. текст контейнера за вычетом текста ссылки.
//
// Последний шаг намеренно последний: он даёт текст навигации и соседних
// результатов, если контейнер выбран неудачно.
// acceptSnippet - единая точка приёма кандидата в сниппеты.
//
// Все четыре шага extractSnippet прогоняют текст через неё, а не через
// разрозненные проверки: иначе фильтр на одном шаге пропускал бы boilerplate на
// остальных, и результат зависел от того, какой шаг сработал первым.
//
// Возвращает готовый сниппет или пустую строку, если кандидат непригоден.
func acceptSnippet(raw string) string {
	s := collapse(raw)
	if s == "" {
		return ""
	}
	if isBoilerplateSnippet(s) {
		return ""
	}
	// Короткий текст описанием не является: это огрызок заголовка или метка
	// элемента разметки вроде «Home» или «Read more».
	if len([]rune(s)) < minSnippetRunes {
		return ""
	}
	return clipRunes(s, maxSnippetRunes)
}

func extractSnippet(a *goquery.Selection) string {
	if a == nil {
		return ""
	}

	// 1. Атрибуты ссылки: часть движков кладёт описание туда, чтобы не
	//    смешивать его с видимой разметкой.
	for _, attr := range snippetAttrs {
		if v, ok := a.Attr(attr); ok {
			if s := acceptSnippet(v); s != "" {
				return s
			}
		}
	}

	// Контейнер результата: ближайший предок, который содержит и ссылку, и
	// описание. Без него поиск шёл бы по всему документу и приносил чужой текст.
	linkText := collapse(a.Text())

	// 2. Описание внутри контейнера - сначала в ближайшем предке, затем на
	//    уровень выше: разметка бывает <div class="result"><a/><div
	//    class="desc">...</div></div>, а бывает и с дополнительным слоем.
	for _, depth := range []int{1, 2, 3} {
		container := closestAncestor(a, depth)
		if container == nil {
			continue
		}
		if s := snippetFromContainer(container, linkText); s != "" {
			return s
		}
	}

	// 4. Текст контейнера за вычетом текста ссылки - последняя попытка. Именно
	// здесь чаще всего приносится навигация, поэтому фильтр обязателен.
	if container := closestAncestor(a, 2); container != nil {
		if s := acceptSnippet(subtractText(collapse(container.Text()), linkText)); s != "" {
			return s
		}
	}
	return ""
}

// closestAncestor поднимается от элемента на depth уровней вверх.
func closestAncestor(s *goquery.Selection, depth int) *goquery.Selection {
	cur := s
	for i := 0; i < depth; i++ {
		parent := cur.Parent()
		if parent == nil || parent.Length() == 0 {
			return nil
		}
		cur = parent
	}
	return cur
}

// snippetFromContainer ищет в контейнере элемент с описанием.
//
// Перебор продолжается после отвергнутого кандидата: контейнер результата
// нередко содержит несколько блоков с описание-подобными классами, и первым
// идёт навигационный. Остановка на первом совпадении класса возвращала бы
// boilerplate там, где ниже лежит настоящее описание.
func snippetFromContainer(container *goquery.Selection, linkText string) string {
	var found string
	container.Find("*").EachWithBreak(func(_ int, el *goquery.Selection) bool {
		if found != "" {
			return false
		}
		class, _ := el.Attr("class")
		id, _ := el.Attr("id")
		names := strings.ToLower(class + " " + id)
		for _, want := range snippetClasses {
			if !strings.Contains(names, want) {
				continue
			}
			text := collapse(el.Text())
			// Текст ссылки описанием не является: на движках со вложенной
			// разметкой <div class="content"><a>Заголовок</a></div> этот
			// блок вернул бы сам заголовок.
			if text == "" || text == linkText {
				continue
			}
			if s := acceptSnippet(subtractText(text, linkText)); s != "" {
				found = s
				return false
			}
		}
		return true
	})
	return found
}

// subtractText убирает из текста текст заголовка.
//
// Нужен потому, что контейнер результата почти всегда включает заголовок: без
// вычитания сниппет начинался бы с повторного заголовка, а CLI напечатал бы
// одну и ту же строку дважды.
func subtractText(text, linkText string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	if linkText == "" {
		return text
	}
	// Убираем первое вхождение заголовка, а не все: слово заголовка может
	// законно встречаться и в описании.
	if i := strings.Index(text, linkText); i >= 0 {
		text = strings.TrimSpace(text[:i] + " " + text[i+len(linkText):])
	}
	if collapse(text) == "" {
		return ""
	}
	return text
}

// clipRunes обрезает строку до n рун, не разрывая их.
//
// Обрезка по байтам разрезала бы многобайтовую кириллицу посередине символа и
// давала нечитаемый вывод.
func clipRunes(s string, n int) string {
	s = strings.TrimSpace(s)
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n]))
}

// meaningfulTitle решает, годится ли текст ссылки в заголовок.
//
// Onion-движки регулярно отдают ссылку, у которой видимый текст - сам адрес:
// <a href="http://abc...xyz.onion/">abc...xyz.onion</a>. Такой заголовок хуже
// отсутствующего. По нему нельзя искать (score сравнивает токены запроса с
// заголовком и не находит ничего), а в выдаче он занимает место, где должно
// быть название сервиса, и выглядит как поломанный парсер.
//
// Возвращает заголовок и признак, что это адрес, а не название: вызывающий
// помечает такой результат, чтобы пользователь понимал, почему заголовка нет.
func meaningfulTitle(text, rawURL string) (title string, isAddress bool) {
	t := collapse(text)
	if t == "" {
		return "", true
	}
	low := strings.ToLower(t)
	host := strings.ToLower(hostOf(rawURL))

	// Текст совпадает с адресом - с хостом, с полным URL или с хостом без
	// схемы и завершающего слэша.
	if host != "" {
		bare := strings.TrimSuffix(strings.TrimPrefix(host, "www."), "/")
		for _, candidate := range []string{low, strings.TrimSuffix(low, "/")} {
			if candidate == host || candidate == bare || strings.TrimSuffix(candidate, "/") == host {
				return "", true
			}
		}
	}
	// Адрес в любом виде: 56 символов base32 плюс .onion, либо с http(s)://.
	if strings.Contains(low, ".onion") && !strings.Contains(low, " ") {
		return "", true
	}
	if strings.HasPrefix(low, "http://") || strings.HasPrefix(low, "https://") {
		return "", true
	}
	return t, false
}

// fallbackTitle собирает заголовок, когда видимый текст ссылки - адрес.
//
// Используется последний осмысленный сегмент пути: у
// http://abc.onion/library/books он даёт «books». Пустой результат хуже любого
// осмысленного слова, потому что score штрафует пустой заголовок на 2 балла и
// такой результат тонет ниже результатов с адресом в заголовке.
func fallbackTitle(rawURL string) string {
	path := strings.TrimSpace(rawURL)
	if i := strings.Index(path, "://"); i >= 0 {
		path = path[i+3:]
	}
	if i := strings.Index(path, "/"); i >= 0 {
		path = path[i:]
	} else {
		return ""
	}
	// Отбрасываем строку запроса и фрагмент.
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	segs := strings.Split(strings.Trim(path, "/"), "/")
	for i := len(segs) - 1; i >= 0; i-- {
		s := collapse(strings.ReplaceAll(segs[i], "-", " "))
		s = strings.ReplaceAll(s, "_", " ")
		s = collapse(s)
		if len([]rune(s)) >= 3 {
			return clipRunes(s, 80)
		}
	}
	return ""
}

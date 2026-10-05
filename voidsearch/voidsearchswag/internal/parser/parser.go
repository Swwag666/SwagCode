// Package parser - самосборный парсер из плана v3 (этап 3).
//
// Сервер отдает очищенный DOM-скелет, mapping field->selector возвращает
// LLM-клиент через save_selector, извлечение идет с self-healing по цепочке:
// точный CSS -> нечеткое совпадение класса -> regex -> структурный контекст.
// Confidence точного попадания 1.0, нечеткого 0.8, запасных 0.5-0.6:
// значение ниже 0.6 означает, что селектор надо перегенерировать.
package parser

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"sync"

	"github.com/PuerkitoBio/goquery"
	"github.com/antchfx/htmlquery"
	"golang.org/x/net/html"

	"voidsearchswag/internal/store"
)

// Fetcher умеет забирать страницу. Его реализует поисковое ядро
// (Engine.FetchURL), в тестах - заглушка.
type Fetcher interface {
	FetchURL(ctx context.Context, rawURL string) (Response, error)
}

// Response - минимальный срез ответа, нужный парсеру.
type Response struct {
	Status int
	Body   []byte
	URL    string
}

type Parser struct {
	Store    *store.Store
	Fetch    Fetcher
	MaxChars int
}

// Confidence пороги стратегий.
const (
	ConfExact      = 1.0
	ConfFuzzy      = 0.8
	ConfRegex      = 0.6
	ConfStructural = 0.5
	ConfXPath      = 1.0
	// HealBelow - ниже этого порога селектор считается сломанным:
	// вызывающий должен перегенерировать его через LLM-клиент.
	HealBelow = 0.6
)

func (p *Parser) maxChars() int {
	if p.MaxChars > 0 {
		return p.MaxChars
	}
	return 20000
}

// HostPattern возвращает ключ реестра селекторов для URL: хост нижним
// регистром без порта. Селекторы привязаны к хосту, а не к странице:
// верстка у сайта обычно общая.
//
// Хост берётся через u.Hostname(), а не отсечением по последнему двоеточию.
// Прежний способ предполагал, что двоеточие в конце всегда отделяет порт, и
// ломал IPv6-адреса в скобках без порта: для http://[2001:db8::1]/ поле Host
// равно "[2001:db8::1]", и отсечение давало "[:", то есть мусорный ключ
// реестра. Сохранённые селекторы для такого хоста никогда не находились, и
// самовосстанавливающийся фолбэк отрабатывал на каждом запросе заново.
// Hostname() возвращает адрес уже без скобок и порта.
func HostPattern(rawURL string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "", fmt.Errorf("некорректный url %q: %w", rawURL, err)
	}
	h := strings.ToLower(strings.TrimSpace(u.Hostname()))
	if h == "" {
		return "", fmt.Errorf("некорректный url %q: пустой хост", rawURL)
	}
	return h, nil
}

// Skeleton забирает страницу и отдает DOM-скелет для LLM-клиента.
func (p *Parser) Skeleton(ctx context.Context, rawURL string, maxChars int) (pattern, skeleton string, err error) {
	if p.Fetch == nil {
		return "", "", fmt.Errorf("фетчер не задан")
	}
	pattern, err = HostPattern(rawURL)
	if err != nil {
		return "", "", err
	}
	resp, err := p.Fetch.FetchURL(ctx, rawURL)
	if err != nil {
		return "", "", err
	}
	if maxChars <= 0 {
		maxChars = p.maxChars()
	}
	return pattern, SkeletonOf(string(resp.Body), maxChars), nil
}

// maxSkeletonDepth ограничивает глубину рекурсивного обхода DOM.
//
// Значение выбрано равным собственному пределу html.Parse: парсер отказывается
// строить дерево глубже 512 открытых элементов и возвращает ошибку
// "open stack of elements exceeds 512 nodes". То есть вход глубже 512 до обхода
// не доходит в принципе, и этот потолок является страховкой, а не семантическим
// обрезанием - он никогда не отбрасывает узлы, которые парсер смог построить.
//
// Меньшее значение было бы ошибкой. При потолке 256 страница глубиной 300
// теряла содержимое ниже отметки: в проверке листовой узел на глубине 304
// исчезал из скелета, а скелет используется для генерации селекторов, то есть
// потерянные узлы означали неполные mapping-подсказки.
//
// Ограничение всё равно нужно: обход рекурсивен, а переполнение стека в Go -
// фатальная ошибка, которую recover() не перехватывает. Ранняя проверка
// maxChars стоит после рекурсивного вызова, поэтому без явного потолка
// глубокая цепочка успевала спуститься до конца раньше ограничения размера.
const maxSkeletonDepth = 512

// SkeletonOf строит очищенный DOM-скелет: тег, id и классы каждого узла,
// для текстовых узлов - обрезанный текст. script/style/noscript вырезаются:
// для mapping они мусор, а вес у них большой.
func SkeletonOf(body string, maxChars int) string {
	doc, err := html.Parse(strings.NewReader(body))
	if err != nil {
		return ""
	}
	var b strings.Builder
	var walk func(n *html.Node, depth int)
	walk = func(n *html.Node, depth int) {
		if n.Type == html.ElementNode {
			tag := strings.ToLower(n.Data)
			switch tag {
			case "script", "style", "noscript", "svg", "canvas":
				return
			}
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(strings.Repeat("  ", depth))
			b.WriteString(tag)
			for _, a := range n.Attr {
				switch strings.ToLower(a.Key) {
				case "id":
					if v := strings.TrimSpace(a.Val); v != "" {
						b.WriteString("#" + truncate(v, 40))
					}
				case "class":
					for _, c := range strings.Fields(a.Val) {
						b.WriteString("." + truncate(c, 40))
					}
				case "name", "itemprop", "property":
					if v := strings.TrimSpace(a.Val); v != "" {
						b.WriteString("[" + strings.ToLower(a.Key) + "=" + truncate(v, 30) + "]")
					}
				}
			}
			if tag == "title" || tag == "h1" || tag == "h2" || tag == "a" || tag == "button" {
				if t := truncate(inlineText(n), 60); t != "" {
					b.WriteString(` "` + t + `"`)
				}
			}
		}
		if depth >= maxSkeletonDepth {
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			next := depth
			if n.Type == html.ElementNode {
				next = depth + 1
			}
			walk(c, next)
			if maxChars > 0 && b.Len() >= maxChars {
				return
			}
		}
	}
	walk(doc, 0)
	out := b.String()
	if maxChars > 0 && len(out) > maxChars {
		out = out[:maxChars]
	}
	return out
}

func inlineText(n *html.Node) string {
	var parts []string
	var walk func(x *html.Node)
	walk = func(x *html.Node) {
		if x.Type == html.TextNode {
			if t := strings.TrimSpace(x.Data); t != "" {
				parts = append(parts, t)
			}
			return
		}
		if x.Type == html.ElementNode {
			switch strings.ToLower(x.Data) {
			case "script", "style":
				return
			}
		}
		for c := x.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	s := strings.Join(parts, " ")
	return strings.Join(strings.Fields(s), " ")
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// Result - извлечение одного поля.
type Result struct {
	Value      string  `json:"value"`
	Confidence float64 `json:"confidence"`
	Strategy   string  `json:"strategy"`
	Healed     bool    `json:"healed,omitempty"`
}

// Parse забирает страницу и извлекает поля по сохраненным селекторам хоста.
// Нет селектора - работает generic-фолбэк (title/h1/meta). Пустой результат
// по всем полям - ошибка, а не молчаливый пустой ответ.
//
// Обёртка над ParseDetailed для вызывающих, которым предупреждение о деградации
// не нужно.
func (p *Parser) Parse(ctx context.Context, rawURL string, fields []string) (map[string]Result, bool, error) {
	out, healed, _, err := p.ParseDetailed(ctx, rawURL, fields)
	return out, healed, err
}

// ParseDetailed разбирает страницу и возвращает четвёртым значением
// предупреждение о деградации разбора.
//
// Предупреждение отделено от ошибки, потому что описывает не провал разбора, а
// потерю точности: страница извлекается, значения находятся, но сохранённые
// селекторы хоста не прочитаны, поэтому вместо точного css со confidence 1.0
// работают generic и structural со confidence 0.5, а healed=true советует
// перегенерировать селекторы, которые на самом деле в порядке.
//
// Замер на фикстуре с сохранённым селектором .price-tag: на открытой базе
// strategy=css confidence=1.00 healed=false, после закрытия той же базы
// strategy=structural confidence=0.50 healed=true и err=nil, то есть без
// отдельного сигнала деградация неотличима от нормального исцеления.
func (p *Parser) ParseDetailed(ctx context.Context, rawURL string, fields []string) (map[string]Result, bool, string, error) {
	if p.Fetch == nil {
		return nil, false, "", fmt.Errorf("фетчер не задан")
	}
	if len(fields) == 0 {
		return nil, false, "", fmt.Errorf("поля не заданы")
	}
	// Число полей ограничено. Список приходит из MCP-инструмента parse и из
	// флага --fields, то есть извне, и ничем не ограничен: каждое поле - это
	// проход по всем стратегиям извлечения, поэтому запрос с тысячей полей
	// превращался бы в тысячу обходов одной страницы и держал соединение к базе
	// и к сети неопределённо долго.
	//
	// Порог 64 с запасом покрывает разумное число полей страницы (на практике их
	// единицы), а превышение отклоняется явно, а не молча урезается: урезанный
	// список вернул бы неполный результат без признака, что часть полей потеряна.
	if len(fields) > maxParseFields {
		return nil, false, "", fmt.Errorf("слишком много полей: %d, предел %d", len(fields), maxParseFields)
	}
	pattern, err := HostPattern(rawURL)
	if err != nil {
		return nil, false, "", err
	}
	resp, err := p.Fetch.FetchURL(ctx, rawURL)
	if err != nil {
		return nil, false, "", err
	}
	body := string(resp.Body)

	var saved map[string]store.Selector
	var warn string
	if p.Store != nil {
		sels, err := p.Store.SelectorsFor(ctx, pattern)
		if err != nil {
			// Ошибка чтения селекторов больше не выбрасывается: разбор без них
			// остаётся рабочим, а причина худшего результата обязана быть видна
			// вызывающему, иначе совет перегенерировать селекторы уводит от
			// настоящей причины - недоступной базы.
			warn = fmt.Sprintf("сохранённые селекторы хоста %s не прочитаны, разбор без них: %v",
				pattern, err)
		} else {
			saved = make(map[string]store.Selector, len(sels))
			for _, s := range sels {
				if _, ok := saved[s.Field]; !ok {
					saved[s.Field] = s
				}
			}
		}
	}

	out := make(map[string]Result, len(fields))
	anyHealed := false
	// Страница разбирается один раз на весь вызов, а не по разу на каждое поле.
	//
	// Прежняя версия передавала в extractField строку body, и каждая из пяти
	// стратегий внутри разбирала её самостоятельно: xpath через html.Parse, css
	// и fuzzy через goquery, generic и structural - тоже через goquery. При семи
	// полях это было до тридцати пяти полных разборов одной и той же страницы, и
	// на теле в несколько мегабайт разбор стоил дороже, чем весь остальной поиск.
	page := newPageDoc(body)
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		r := extractFieldDoc(page, f, saved[f])
		out[f] = r
		if r.Healed {
			anyHealed = true
		}
	}
	if len(out) == 0 {
		return nil, false, "", fmt.Errorf("поля пусты после нормализации")
	}
	ok := false
	for _, r := range out {
		if r.Value != "" {
			ok = true
			break
		}
	}
	if !ok {
		return out, anyHealed, warn, fmt.Errorf("ни одно поле не извлечено")
	}
	// Успешный точный разбор подтверждает селектор: поднимаем confidence
	// обратно до 1.0, чтобы единичный сбой не держал его в зоне исцеления.
	if p.Store != nil {
		for f, r := range out {
			if s, ok := saved[f]; ok && r.Strategy == "css" && s.Confidence < 1.0 && r.Value != "" {
				_ = p.Store.UpsertSelector(ctx, store.Selector{
					URLPattern: pattern, Field: f, Selector: s.Selector,
					Strategy: s.Strategy, Confidence: 1.0,
				})
			}
		}
	}
	return out, anyHealed, warn, nil
}

// Save сохраняет mapping от LLM-клиента в реестр селекторов.
func (p *Parser) Save(ctx context.Context, pattern, field, selector, strategy string) error {
	if p.Store == nil {
		return fmt.Errorf("хранилище не задано")
	}
	if strings.TrimSpace(pattern) == "" || strings.TrimSpace(field) == "" || strings.TrimSpace(selector) == "" {
		return fmt.Errorf("pattern, field и selector обязательны")
	}
	if strategy == "" {
		strategy = "css"
	}
	return p.Store.UpsertSelector(ctx, store.Selector{
		URLPattern: pattern, Field: field, Selector: selector,
		Strategy: strategy, Confidence: 1.0,
	})
}

// TitleOf достаёт заголовок страницы без сети: нужен классификатору,
// которому нужен только title, а не полный разбор.
func TitleOf(body string) string {
	return genericField(body, "title")
}

// maxParseFields ограничивает число полей в одном вызове Parse.
//
// Список полей приходит извне (MCP-инструмент parse, флаг --fields), а каждое
// поле - это проход по всей цепочке стратегий извлечения. Без предела запрос с
// тысячей полей превращался бы в тысячу обходов одной страницы.
const maxParseFields = 64

var classToken = regexp.MustCompile(`\.([A-Za-z0-9_-]+)`)

// stripTagsRE компилируется один раз на пакет.
//
// Прежняя stripTags вызывала regexp.MustCompile внутри функции, то есть
// компилировала одно и то же статическое выражение на каждый вызов. Вызовов
// много: regexField идёт по каждому полю, а полей у parse бывает несколько,
// поэтому на одном разборе страницы выражение компилировалось заново столько же
// раз. Компиляция регулярного выражения дороже его применения, и шаблон здесь
// постоянен - выносить его на уровень пакета требует и go vet (regexpinmain не
// ругается, но смысл тот же).
var stripTagsRE = regexp.MustCompile(`(?s)<script.*?</script>|<style.*?</style>|<[^>]+>`)

// pageDoc - одна страница, разобранная не более одного раза.
//
// Тип введён потому, что прежний код разбирал HTML заново на каждую стратегию и
// на каждое поле. extractField пробует до пяти стратегий подряд - xpath, точный
// CSS, нечёткий CSS, generic и structural, - и каждая из них самостоятельно
// вызывала html.Parse или goquery.NewDocumentFromReader над тем же телом. При
// нескольких полях это давало до пяти полных разборов на поле: на странице в
// несколько мегабайт и семи полях - десятки проходов по одному и тому же тексту.
//
// Разбор - самая дорогая операция в этом файле, и он идемпотентен, поэтому
// результат мемоизируется и переиспользуется всеми стратегиями и всеми полями
// одного вызова Parse.
//
// Все поля ленивые: страница, которую удалось взять точным CSS-селектором, не
// требует ни XPath-дерева, ни текста без тегов. Прежняя версия платила все
// представления сразу.
//
// pageDoc не потокобезопасен намеренно: он живёт внутри одного вызова Parse,
// который обрабатывает поля последовательно. Мьютекс здесь означал бы
// блокировку на каждой стратегии ради объекта, который никто не разделяет.
type pageDoc struct {
	body string

	docOnce  sync.Once
	doc      *goquery.Document
	docOK    bool
	nodeOnce sync.Once
	root     *html.Node
	textOnce sync.Once
	text     string
}

// newPageDoc создаёт обёртку над телом страницы. Разбор откладывается до первого
// обращения к нужному представлению.
func newPageDoc(body string) *pageDoc {
	return &pageDoc{body: body}
}

// Doc возвращает goquery-документ, разбирая тело один раз.
//
// Второе значение сообщает, удался ли разбор: html.Parse почти всегда возвращает
// документ даже на битом HTML, поэтому признак нужен не для ошибок синтаксиса, а
// для случая, когда тела вовсе нет.
func (p *pageDoc) Doc() (*goquery.Document, bool) {
	p.docOnce.Do(func() {
		doc, err := goquery.NewDocumentFromReader(strings.NewReader(p.body))
		if err != nil || doc == nil {
			return
		}
		p.doc = doc
		p.docOK = true
	})
	return p.doc, p.docOK
}

// Root возвращает корневой узел дерева для XPath-запросов.
//
// Используется тот же разбор, что и для goquery: goquery.Document хранит
// корневой узел в Nodes, поэтому отдельный html.Parse не нужен. Прежняя
// xpathText разбирала тело заново на каждый вызов.
func (p *pageDoc) Root() *html.Node {
	p.nodeOnce.Do(func() {
		doc, ok := p.Doc()
		if !ok || len(doc.Nodes) == 0 {
			return
		}
		p.root = doc.Nodes[0]
	})
	return p.root
}

// Text возвращает тело без тегов, вычисленное один раз.
//
// Прежняя regexField вызывала stripTags на каждый запрос, и каждый вызов
// прогонял регулярное выражение по всему телу. При нескольких полях это
// несколько полных проходов по мегабайтам текста ради одного и того же результата.
func (p *pageDoc) Text() string {
	p.textOnce.Do(func() {
		p.text = stripTagsRE.ReplaceAllString(p.body, " ")
	})
	return p.text
}

func extractField(body, field string, sel store.Selector) Result {
	return extractFieldDoc(newPageDoc(body), field, sel)
}

// extractFieldDoc - извлечение поля из уже разобранной страницы.
//
// Отдельная функция, а не изменение подписи extractField, потому что Parse
// передаёт один pageDoc на все поля, а одиночные вызовы (тесты, служебные пути)
// продолжают работать со строкой через обёртку.
func extractFieldDoc(p *pageDoc, field string, sel store.Selector) Result {
	if sel.Selector != "" {
		// XPath-селектор идёт своей веткой: для него нет нечёткого
		// ослабления, зато он переживает переименование всех классов -
		// путь по структуре от этого не меняется.
		if sel.Strategy == "xpath" {
			if v := xpathTextDoc(p, sel.Selector); v != "" {
				return Result{Value: v, Confidence: ConfXPath, Strategy: "xpath"}
			}
		} else {
			if v := cssTextDoc(p, sel.Selector); v != "" {
				return Result{Value: v, Confidence: ConfExact, Strategy: "css"}
			}
			if fz := fuzzySelector(sel.Selector); fz != "" && fz != sel.Selector {
				if v := cssTextDoc(p, fz); v != "" {
					return Result{Value: v, Confidence: ConfFuzzy, Strategy: "fuzzy", Healed: true}
				}
			}
		}
	}
	if v := regexFieldDoc(p, field); v != "" {
		return Result{Value: v, Confidence: ConfRegex, Strategy: "regex", Healed: true}
	}
	// Семантические теги точнее угадывания по подстроке в атрибуте:
	// класс shop-title содержит токен title, но ценой это не является.
	// Поэтому generic для известных полей идёт раньше structural.
	if v := genericFieldDoc(p, field); v != "" {
		return Result{Value: v, Confidence: ConfStructural, Strategy: "generic", Healed: true}
	}
	if v := structuralFieldDoc(p, field); v != "" {
		return Result{Value: v, Confidence: ConfStructural, Strategy: "structural", Healed: true}
	}
	return Result{Confidence: 0, Strategy: "none", Healed: true}
}

func cssText(body, selector string) string {
	return cssTextDoc(newPageDoc(body), selector)
}

func cssTextDoc(p *pageDoc, selector string) string {
	doc, ok := p.Doc()
	if !ok {
		return ""
	}
	sel := doc.Find(selector).First()
	if sel.Length() == 0 {
		return ""
	}
	if v, ok := sel.Attr("content"); ok && strings.TrimSpace(v) != "" &&
		strings.Contains(strings.ToLower(selector), "meta") {
		return strings.TrimSpace(v)
	}
	return strings.TrimSpace(sel.Text())
}

// fuzzySelector ослабляет CSS: точные классы .foo-bar превращаются в
// [class*="foo-bar"]. Верстка меняется чаще всего переименованием части
// классов, а не структуры: нечеткое совпадение переживает такой редизайн.
func fuzzySelector(sel string) string {
	toks := classToken.FindAllStringSubmatch(sel, -1)
	if len(toks) == 0 {
		return ""
	}
	out := classToken.ReplaceAllStringFunc(sel, func(m string) string {
		name := m[1:]
		return `[class*="` + name + `"]`
	})
	return out
}

// xpathText извлекает текст первого узла по XPath. Невалидное выражение
// не роняет процесс: htmlquery паникует на ошибке компиляции, поэтому здесь
// recover, а вызывающий падает дальше по цепочке стратегий.
func xpathText(body, expr string) (out string) {
	defer func() {
		_ = recover()
	}()
	return xpathTextDoc(newPageDoc(body), expr)
}

// xpathTextDoc извлекает текст первого узла по XPath из уже разобранной страницы.
//
// Корень берётся из общего goquery-разбора через Root(), поэтому отдельный
// html.Parse на каждый XPath-запрос не выполняется. Прежняя версия разбирала
// тело заново на каждый вызов, и при нескольких полях с xpath-селекторами это
// было несколько полных разборов одной страницы.
//
// recover остаётся в обёртке xpathText: htmlquery паникует на ошибке компиляции
// выражения, и вызывающий обязан падать дальше по цепочке стратегий, а не
// ронять процесс.
func xpathTextDoc(p *pageDoc, expr string) string {
	if strings.TrimSpace(expr) == "" {
		return ""
	}
	root := p.Root()
	if root == nil {
		return ""
	}
	node := htmlquery.FindOne(root, expr)
	if node == nil {
		return ""
	}
	return strings.TrimSpace(inlineText(node))
}

func regexField(body, field string) string {
	return regexFieldDoc(newPageDoc(body), field)
}

func regexFieldDoc(p *pageDoc, field string) string {
	// Пара "field: value" в тексте или JSON: самый частый вид данных,
	// которые не обернуты в удобный селектор.
	//
	// Шаблон строится из имени поля, поэтому скомпилировать его один раз на
	// пакет нельзя. Компиляция остаётся на каждый вызов, но текст без тегов
	// теперь мемоизирован: прежняя версия прогоняла регулярное выражение по
	// всему телу страницы на каждое поле, и при семи полях это было семь
	// полных проходов по мегабайтам текста ради одного и того же результата.
	pat := `(?i)` + regexp.QuoteMeta(field) + `\s*[:=]\s*["']?([^"'<>\n]{1,200})`
	re, err := regexp.Compile(pat)
	if err != nil {
		return ""
	}
	m := re.FindStringSubmatch(p.Text())
	if len(m) < 2 {
		return ""
	}
	return strings.TrimSpace(m[1])
}

// stripTags возвращает текст страницы без разметки.
//
// Выражение скомпилировано на уровне пакета (stripTagsRE), а не внутри функции:
// прежняя версия вызывала regexp.MustCompile на каждый вызов, то есть
// компилировала один и тот же постоянный шаблон по разу на каждое поле.
func stripTags(body string) string {
	return stripTagsRE.ReplaceAllString(body, " ")
}

func structuralField(body, field string) string {
	return structuralFieldDoc(newPageDoc(body), field)
}

// structuralFieldDoc ищет поле по подстроке в атрибутах элементов.
//
// Обход остаётся полным (doc.Find("*")), потому что критерий - подстрока в
// значении атрибута, а не имя тега или класса: по селектору такое не
// выразить. Прежняя оценка «O(n²)» относилась не к обходу, а к s.Text() внутри
// него: Text() собирает текст всего поддерева, поэтому на каждом совпадении
// атрибута обход начинался заново из этой точки.
//
// Совпадения редки (нужно вхождение токена поля в значение атрибута), поэтому
// полный пересбор текста на каждом из них приемлем, а вот повторный разбор HTML
// на каждое поле - нет: он устранён общим pageDoc.
func structuralFieldDoc(p *pageDoc, field string) string {
	tok := strings.ToLower(field)
	doc, ok := p.Doc()
	if !ok {
		return ""
	}
	// email почти всегда лежит в mailto-ссылке: структура стабильнее классов.
	if strings.Contains(tok, "email") || strings.Contains(tok, "mail") || strings.Contains(tok, "e-mail") {
		if href, ok := doc.Find(`a[href^="mailto:"]`).First().Attr("href"); ok {
			return strings.TrimSpace(strings.TrimPrefix(href, "mailto:"))
		}
	}
	// Токен короче трёх символов не ищется вовсе: «id» и «no» входят почти в
	// любое значение атрибута, и первое совпадение оказалось бы мусором.
	// Прежняя версия проверяла len(tok) >= 3 внутри цикла по атрибутам, то есть
	// на каждом элементе и каждом атрибуте, хотя результат от положения не
	// зависит.
	if len(tok) < 3 {
		return ""
	}
	var best string
	doc.Find("*").EachWithBreak(func(_ int, s *goquery.Selection) bool {
		n := s.Get(0)
		if n == nil || n.Type != html.ElementNode {
			return true
		}
		for _, a := range n.Attr {
			if strings.Contains(strings.ToLower(a.Val), tok) {
				if t := strings.TrimSpace(s.Text()); t != "" {
					best = firstLine(t)
					return false
				}
			}
		}
		return true
	})
	return best
}

// genericField - последний шанс без селектора: типовые поля страницы.
func genericField(body, field string) string {
	return genericFieldDoc(newPageDoc(body), field)
}

func genericFieldDoc(p *pageDoc, field string) string {
	tok := strings.ToLower(strings.TrimSpace(field))
	switch tok {
	case "title", "heading", "h1", "description":
	default:
		// Разбор страницы не нужен, если поле не из типовых: прежняя версия
		// разбирала HTML до switch и платила полным разбором ради немедленного
		// выхода с пустой строкой.
		return ""
	}
	doc, ok := p.Doc()
	if !ok {
		return ""
	}
	switch tok {
	case "title":
		if t := strings.TrimSpace(doc.Find("title").First().Text()); t != "" {
			return t
		}
		return strings.TrimSpace(doc.Find("h1").First().Text())
	case "heading", "h1":
		return strings.TrimSpace(doc.Find("h1").First().Text())
	case "description":
		if v, ok := doc.Find(`meta[name="description"]`).First().Attr("content"); ok {
			return strings.TrimSpace(v)
		}
		if v, ok := doc.Find(`meta[property="og:description"]`).First().Attr("content"); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func firstLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > 200 {
		return string(r[:200])
	}
	return s
}

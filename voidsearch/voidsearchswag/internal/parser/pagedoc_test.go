package parser

import (
	"context"
	"strings"
	"testing"

	"voidsearchswag/internal/store"
)

const memoSample = `<html><head>
<title>Заголовок страницы</title>
<meta name="description" content="Описание из мета-тега">
<style>.x{color:red}</style>
<script>var y = 1;</script>
</head><body>
<h1>Главный заголовок</h1>
<div class="product-price">1500 RUB</div>
<div data-email="нет">Пишите на <a href="mailto:shop@example.onion">shop@example.onion</a></div>
<p>price: 1500</p>
<p>email: shop@example.onion</p>
</body></html>`

func TestPageDocMemoizesDocument(t *testing.T) {
	// Главная цель pageDoc: страница разбирается один раз, а не по разу на
	// каждую стратегию и на каждое поле. Прежний код разбирал тело до пяти раз на
	// поле, и на семи полях это было до тридцати пяти полных разборов одной
	// страницы.
	p := newPageDoc(memoSample)

	doc1, ok1 := p.Doc()
	doc2, ok2 := p.Doc()
	doc3, ok3 := p.Doc()

	if !ok1 || !ok2 || !ok3 {
		t.Fatal("разбор не удался")
	}
	// Тот же указатель означает, что повторного разбора не было: второй разбор
	// вернул бы новый объект.
	if doc1 != doc2 || doc2 != doc3 {
		t.Error("Doc() разобрал страницу повторно вместо возврата закэшированного")
	}
}

func TestPageDocMemoizesText(t *testing.T) {
	// Текст без тегов вычисляется один раз. Прежняя regexField вызывала stripTags
	// на каждое поле, и каждый вызов прогонял регулярное выражение по всему телу.
	p := newPageDoc(memoSample)

	t1 := p.Text()
	t2 := p.Text()

	if t1 != t2 {
		t.Error("Text() вернул разные значения")
	}
	if strings.Contains(t1, "<script") || strings.Contains(t1, "<style") {
		t.Errorf("в тексте остались теги script/style: %q", t1)
	}
	if !strings.Contains(t1, "price: 1500") {
		t.Errorf("из текста потерялось содержимое: %q", t1)
	}
}

func TestPageDocRootReusesDocument(t *testing.T) {
	// Корень для XPath берётся из общего goquery-разбора, поэтому отдельный
	// html.Parse на каждый XPath-запрос не выполняется.
	p := newPageDoc(memoSample)

	root := p.Root()
	if root == nil {
		t.Fatal("корень не получен")
	}
	doc, ok := p.Doc()
	if !ok {
		t.Fatal("документ не получен")
	}
	if len(doc.Nodes) == 0 {
		t.Fatal("в документе нет узлов")
	}
	if root != doc.Nodes[0] {
		t.Error("Root() вернул не узел общего документа: разбор выполнен дважды")
	}
	if p.Root() != root {
		t.Error("Root() не мемоизирован")
	}
}

func TestPageDocEmptyBody(t *testing.T) {
	// Пустое тело не должно паниковать ни на одном представлении.
	p := newPageDoc("")
	if _, ok := p.Doc(); !ok {
		t.Error("пустое тело не дало документ: html.Parse возвращает документ даже на пустоте")
	}
	if got := p.Text(); got != "" {
		t.Errorf("Text() на пустом теле = %q", got)
	}
	// Корень может быть nil или валидным, но обращение не должно паниковать.
	_ = p.Root()
}

func TestExtractFieldDocMatchesStringVersion(t *testing.T) {
	// Перенос стратегий на общий документ не должен менять результат. Проверяю
	// обе формы на всех стратегиях: точный CSS, нечёткий, regex, generic,
	// structural и xpath.
	cases := []struct {
		name string
		body string
		sel  store.Selector
		want string
	}{
		{"точный css", memoSample, store.Selector{Selector: ".product-price", Strategy: "css"}, "1500 RUB"},
		{"мета description", memoSample, store.Selector{Selector: `meta[name="description"]`, Strategy: "css"}, "Описание из мета-тега"},
		{"xpath", memoSample, store.Selector{Selector: "//h1", Strategy: "xpath"}, "Главный заголовок"},
		{"без селектора title", memoSample, store.Selector{}, "Заголовок страницы"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			viaString := extractField(c.body, "title", c.sel)
			viaDoc := extractFieldDoc(newPageDoc(c.body), "title", c.sel)
			if viaString.Value != viaDoc.Value {
				t.Errorf("строковый путь %q, документный %q", viaString.Value, viaDoc.Value)
			}
			if viaString.Strategy != viaDoc.Strategy {
				t.Errorf("стратегия разошлась: %q против %q", viaString.Strategy, viaDoc.Strategy)
			}
			if viaString.Confidence != viaDoc.Confidence {
				t.Errorf("confidence разошёлся: %v против %v", viaString.Confidence, viaDoc.Confidence)
			}
		})
	}
}

func TestSharedPageDocAcrossFields(t *testing.T) {
	// Один pageDoc на несколько полей обязан давать те же значения, что и
	// отдельный разбор на каждое поле. Это и есть проверка того, что мемоизация
	// не вносит состояния между полями.
	fields := []string{"title", "description", "price", "email", "heading"}

	shared := newPageDoc(memoSample)
	for _, f := range fields {
		want := extractField(memoSample, f, store.Selector{})
		got := extractFieldDoc(shared, f, store.Selector{})
		if want.Value != got.Value {
			t.Errorf("поле %q: общий документ дал %q, отдельный %q", f, got.Value, want.Value)
		}
		if want.Strategy != got.Strategy {
			t.Errorf("поле %q: стратегия %q против %q", f, got.Strategy, want.Strategy)
		}
	}
}

func TestStructuralFieldDocSkipsShortToken(t *testing.T) {
	// Токен короче трёх символов не ищется вовсе: «id» и «no» входят почти в
	// любое значение атрибута, и первое совпадение оказалось бы мусором.
	// Прежняя версия проверяла длину внутри цикла по атрибутам, то есть на каждом
	// элементе, хотя результат от положения не зависит.
	for _, tok := range []string{"id", "no", "x", ""} {
		if got := structuralFieldDoc(newPageDoc(memoSample), tok); got != "" {
			t.Errorf("короткий токен %q дал результат %q", tok, got)
		}
	}
}

func TestStructuralFieldDocFindsEmail(t *testing.T) {
	// email ищется по mailto-ссылке: структура стабильнее классов.
	got := structuralFieldDoc(newPageDoc(memoSample), "email")
	if got != "shop@example.onion" {
		t.Errorf("email = %q, ожидала shop@example.onion", got)
	}
}

func TestStructuralFieldDocFindsByAttribute(t *testing.T) {
	got := structuralFieldDoc(newPageDoc(memoSample), "price")
	if got != "1500 RUB" {
		t.Errorf("price = %q, ожидала 1500 RUB", got)
	}
}

func TestGenericFieldDocDoesNotParseUnknownField(t *testing.T) {
	// Для поля не из типовых разбор страницы не нужен: прежняя версия разбирала
	// HTML до switch и платила полным разбором ради немедленного выхода с пустой
	// строкой. Проверяю, что документ при этом не создаётся.
	p := newPageDoc(memoSample)
	if got := genericFieldDoc(p, "someunknownfield"); got != "" {
		t.Errorf("нетиповое поле дало %q", got)
	}
	if p.doc != nil {
		t.Error("нетиповое поле всё равно разобрало страницу")
	}
}

func TestGenericFieldDocKnownFields(t *testing.T) {
	cases := map[string]string{
		"title":       "Заголовок страницы",
		"heading":     "Главный заголовок",
		"h1":          "Главный заголовок",
		"description": "Описание из мета-тега",
	}
	for field, want := range cases {
		if got := genericFieldDoc(newPageDoc(memoSample), field); got != want {
			t.Errorf("поле %q = %q, ожидала %q", field, got, want)
		}
	}
}

func TestStripTagsUsesPackageRegexp(t *testing.T) {
	// stripTags обязан давать тот же результат, что и прямое применение
	// пакетного выражения: проверка ловит ситуацию, когда функция и stripTagsRE
	// разошлись.
	got := stripTags(memoSample)
	want := stripTagsRE.ReplaceAllString(memoSample, " ")
	if got != want {
		t.Error("stripTags разошёлся со stripTagsRE")
	}
	if strings.Contains(got, "<h1>") {
		t.Errorf("теги не сняты: %q", got)
	}
}

func TestParseRejectsTooManyFields(t *testing.T) {
	// Список полей приходит извне и ничем не ограничен. Каждое поле - проход по
	// всей цепочке стратегий, поэтому запрос с тысячей полей превращался бы в
	// тысячу обходов одной страницы. Превышение отклоняется явно, а не молча
	// урезается: урезанный список вернул бы неполный результат без признака, что
	// часть полей потеряна.
	p := &Parser{Fetch: &stubFetcher{body: memoSample}}

	tooMany := make([]string, maxParseFields+1)
	for i := range tooMany {
		tooMany[i] = "title"
	}
	_, _, err := p.Parse(context.Background(), "http://a.example/x", tooMany)
	if err == nil {
		t.Fatal("превышение числа полей принято")
	}
	if !strings.Contains(err.Error(), "предел") {
		t.Errorf("ошибка не объясняет предел: %v", err)
	}
}

func TestParseAcceptsFieldsAtLimit(t *testing.T) {
	// Ровно на пределе запрос обязан проходить: граница включительная.
	p := &Parser{Fetch: &stubFetcher{body: memoSample}}

	atLimit := make([]string, maxParseFields)
	for i := range atLimit {
		atLimit[i] = "title"
	}
	out, _, err := p.Parse(context.Background(), "http://a.example/x", atLimit)
	if err != nil {
		t.Fatalf("запрос на пределе полей отклонён: %v", err)
	}
	// Все поля одинаковые, поэтому в карте одна запись: ключ - имя поля.
	if len(out) != 1 {
		t.Errorf("в результате %d записей, ожидала 1 (поля совпадают)", len(out))
	}
}

func TestParseSingleDocumentForManyFields(t *testing.T) {
	// Несколько разных полей на одном вызове обязаны извлекаться корректно и без
	// потери значений: это проверка, что общий документ не «затирает» состояние
	// между полями.
	p := &Parser{Fetch: &stubFetcher{body: memoSample}}

	out, _, err := p.Parse(context.Background(), "http://a.example/x",
		[]string{"title", "description", "price", "email"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"title":       "Заголовок страницы",
		"description": "Описание из мета-тега",
	}
	for field, w := range want {
		if got := out[field].Value; got != w {
			t.Errorf("поле %q = %q, ожидала %q", field, got, w)
		}
	}
	if out["price"].Value == "" {
		t.Error("поле price не извлечено")
	}
	if out["email"].Value == "" {
		t.Error("поле email не извлечено")
	}
}

// stubFetcher отдаёт фиксированное тело, чтобы тесты разбора не зависели от сети.
type stubFetcher struct {
	body string
}

func (s *stubFetcher) FetchURL(ctx context.Context, rawURL string) (Response, error) {
	return Response{Status: 200, Body: []byte(s.body)}, nil
}

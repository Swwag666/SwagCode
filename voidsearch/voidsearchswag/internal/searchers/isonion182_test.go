package searchers

import (
	"strings"
	"testing"
)

// Кэтчер этапа 182: IsOnion принимает только канонические длины onion-адреса
// - 16 (проприетарная схема) и 56 (v3). Прежний диапазон {16,56} пропускал
// любую промежуточную длину, и битые адреса с произвольным числом знаков
// получали onion=true: такие хосты не открываются в tor (контрольная сумма
// не сходится), а репорт называл их onion-сервисами.
func TestIsOnionStrictLengths(t *testing.T) {
	host := func(n int) string { return "http://" + strings.Repeat("a", n) + ".onion/" }

	cases := []struct {
		n    int
		want bool
		why  string
	}{
		{16, true, "каноническая короткая длина"},
		{56, true, "каноническая длина v3"},
		{15, false, "короче минимума"},
		{17, false, "промежуточная длина не существует"},
		{20, false, "промежуточная длина не существует"},
		{33, false, "промежуточная длина не существует"},
		{55, false, "промежуточная длина не существует"},
		{57, false, "длиннее максимума"},
		{8, false, "совсем короткий мусор"},
		{1, false, "один знак"},
	}
	for _, c := range cases {
		if got := IsOnion(host(c.n)); got != c.want {
			t.Errorf("IsOnion(%d знаков) = %v, хочу %v (%s)", c.n, got, c.want, c.why)
		}
	}

	// Канонические адреса с полным URL обязаны определяться так же.
	v3 := "http://" + strings.Repeat("a", 56) + ".onion/path?x=1"
	if !IsOnion(v3) {
		t.Error("IsOnion не узнал валидный v3-адрес в URL")
	}
	if IsOnion("http://example.com/a") {
		t.Error("clearnet-адрес принят за onion")
	}
	if IsOnion(strings.Repeat("a", 56) + ".onion.com") {
		t.Error("суффикс .onion.com принят за onion-хост")
	}
}

// Кэтчер этапа 182 (смоук-179, «дубли контента через каталог-листинг»):
// два результата одного движка с одинаковым путём и дословно одинаковым
// сниппетом - зеркала одного листинга, в выдаче остаётся первый. Дедуп по
// адресу их не ловил: хосты у зеркал разные, и живой прогон «forum»
// (BEFORE этапа 182) нёс две записи path=/forum/ с идентичной рекламной
// прокладкой «OnionDir - DeepLink ✔, ...».
func TestDedupeDropsListingMirrors(t *testing.T) {
	snip := "OnionDir - DeepLink, Porn, Counterfeits, Hire Professional Hackers"
	mk := func(host, title string) Result {
		return Result{Title: title, URL: "http://" + host + "/forum/", Snippet: snip, Source: "torch"}
	}
	in := []Result{
		mk("mirror1.onion", "Forum - Tor Stuff"),
		mk("mirror2.onion", "Forum - Tor Land"),
	}

	got := DedupeAll(in)
	if len(got) != 1 {
		t.Fatalf("зеркала листинга не дедупятся: %d результатов", len(got))
	}
	if got[0].URL != "http://mirror1.onion/forum/" {
		t.Errorf("остался не первый результат: %s", got[0].URL)
	}
}

// Негативный контроль: одинаковый путь, но РАЗНЫЕ сниппеты - это разные
// сайты (живой прогон «forum»: path=/forum у четырёх форумов с разными
// описаниями). Дедуп обязан оставить оба.
func TestDedupeKeepsSamePathWithDifferentSnippets(t *testing.T) {
	in := []Result{
		{Title: "TorMart Forum", URL: "http://deepmad.onion/forum", Snippet: "Tor Forum Carding Forum", Source: "torch"},
		{Title: "Tor Stuff", URL: "http://stuff.onion/forum/", Snippet: "marketplace of digital goods", Source: "torch"},
	}
	if got := DedupeAll(in); len(got) != 2 {
		t.Fatalf("разные сайты с одинаковым путём задвоены вырезанием: %d", len(got))
	}
}

// Негативный контроль: одинаковый путь и сниппет, но РАЗНЫЕ источники -
// склейка не идёт: разные движки могут законно дать пересекающийся текст,
// пара между движками не обязана быть зеркалом.
func TestDedupeKeepsSameSnippetAcrossEngines(t *testing.T) {
	snip := "одно описание на два движка"
	in := []Result{
		{Title: "A", URL: "http://a.onion/x", Snippet: snip, Source: "torch"},
		{Title: "B", URL: "http://b.onion/x", Snippet: snip, Source: "tornet"},
	}
	if got := DedupeAll(in); len(got) != 2 {
		t.Fatalf("результаты разных движков склеены: %d", len(got))
	}
}

// Негативный контроль: пустой сниппет не образует сигнатуру зеркала -
// без описания совпадение пути слишком слабое, и страницы одного каталога
// с одинаковым путём и без сниппетов задваивались бы без вины.
func TestDedupeKeepsEmptySnippetPairs(t *testing.T) {
	in := []Result{
		{Title: "A", URL: "http://a.onion/lib", Snippet: "", Source: "torch"},
		{Title: "B", URL: "http://b.onion/lib", Snippet: "", Source: "torch"},
	}
	if got := DedupeAll(in); len(got) != 2 {
		t.Fatalf("пустые сниппеты склеены как зеркала: %d", len(got))
	}
}

// Короткий огрызок сниппета (меньше minSnippetRunes) в сигнатуру не идёт:
// «Read more» у двух страниц одного пути - метка разметки, а не признак
// зеркала.
func TestDedupeKeepsShortSnippetPairs(t *testing.T) {
	in := []Result{
		{Title: "A", URL: "http://a.onion/p", Snippet: "read more", Source: "torch"},
		{Title: "B", URL: "http://b.onion/p", Snippet: "read more", Source: "torch"},
	}
	if got := DedupeAll(in); len(got) != 2 {
		t.Fatalf("короткие сниппеты склеены как зеркала: %d", len(got))
	}
}

// Дубли URL по-прежнему снимаются приоритетно: тот же адрес - та же
// страница, независимо от сниппета.
func TestDedupeStillDropsSameURL(t *testing.T) {
	in := []Result{
		{Title: "A", URL: "http://x.onion/f", Snippet: "first", Source: "torch"},
		{Title: "B", URL: "http://x.onion/f", Snippet: "second", Source: "torch"},
	}
	if got := DedupeAll(in); len(got) != 1 {
		t.Fatalf("одинаковые URL не дедупятся: %d", len(got))
	}
}

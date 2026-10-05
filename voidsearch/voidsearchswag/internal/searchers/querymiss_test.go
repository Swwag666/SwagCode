package searchers

import (
	"strings"
	"testing"
)

// Жалоба смоук-агентов на v0.1.0, этап 164: запрос «acme corp leak database»
// в deep приносил leak-маркеты без слова «acme» - выдача, в которой у части
// результатов нет ни одного токена запроса. Таблица ниже - сокращённая
// версия той выдачи.
func TestDropQueryMissSplitsByTokenHit(t *testing.T) {
	in := []Result{
		{Title: "ACME Corp internal files", URL: "http://acmecorp.onion/files", Snippet: "leak database of acme corp"},
		{Title: "онлайн аптека со скидками", URL: "http://pharma.example/shop", Snippet: "лучшие цены"},
		{Title: "leak market forum", URL: "http://market.onion/listing/9", Snippet: "продажа доступа к базам"},
	}
	kept, dropped := DropQueryMiss(in, "acme corp leak database")
	if len(kept) != 2 {
		t.Fatalf("осталось %d результатов, хочу 2 (совпадения по токенам)", len(kept))
	}
	if len(dropped) != 1 {
		t.Fatalf("выкинуто %d результатов, хочу 1 (аптека без токенов)", len(dropped))
	}
	if dropped[0].Title != "онлайн аптека со скидками" {
		t.Errorf("выкинут не тот результат: %q", dropped[0].Title)
	}
	if kept[0].Title != "ACME Corp internal files" {
		t.Errorf("первый оставленный не тот: %q", kept[0].Title)
	}
}

// Совпадение в адресе достаточно: каталоги отдают строки с пустыми
// заголовками, и релевантность живёт в самом URL.
func TestDropQueryMissCountsURLHit(t *testing.T) {
	in := []Result{
		{Title: "", URL: "http://acme.onion/dump"},
		{Title: "", URL: "http://random.onion/dump"},
	}
	kept, dropped := DropQueryMiss(in, "acme")
	if len(kept) != 1 || len(dropped) != 1 {
		t.Fatalf("осталось %d, выкинуто %d; хочу 1 и 1", len(kept), len(dropped))
	}
	if kept[0].URL != "http://acme.onion/dump" {
		t.Errorf("оставлен %q, хочу адрес с acme", kept[0].URL)
	}
}

// Совпадение в сниппете достаточно без заголовка и адреса.
func TestDropQueryMissCountsSnippetHit(t *testing.T) {
	in := []Result{
		{Title: "x", URL: "http://a.onion/1", Snippet: "база данных acme утекла"},
		{Title: "y", URL: "http://b.onion/2", Snippet: "совсем другое"},
	}
	kept, dropped := DropQueryMiss(in, "acme")
	if len(kept) != 1 || len(dropped) != 1 {
		t.Fatalf("осталось %d, выкинуто %d; хочу 1 и 1", len(kept), len(dropped))
	}
}

// Пустой kept при мусорной выдаче - не задача функции: снятие фильтра
// решает вызывающий, потому что «нет совпадений» и «выдача пуста» обязаны
// дойти до пользователя разными словами.
func TestDropQueryMissReturnsAllAsDroppedWhenNothingHits(t *testing.T) {
	in := []Result{
		{Title: "витрина", URL: "http://shop.example/"},
		{Title: "ещё витрина", URL: "http://mall.example/"},
	}
	kept, dropped := DropQueryMiss(in, "acme corp")
	if len(kept) != 0 {
		t.Fatalf("осталось %d, хочу 0: ничего не совпало", len(kept))
	}
	if len(dropped) != 2 {
		t.Fatalf("выкинуто %d, хочу 2", len(dropped))
	}
}

// Запрос из коротких слов и пунктуации не оставляет токенов: фильтр
// обязан пройти всё как есть, иначе одно-двухбуквенный запрос вычищал бы
// всю выдачу целиком.
func TestDropQueryMissPassesThroughWhenNoTokens(t *testing.T) {
	in := []Result{{Title: "a b", URL: "http://x.example/"}}
	kept, dropped := DropQueryMiss(in, "a? .,")
	if len(kept) != 1 || len(dropped) != 0 {
		t.Fatalf("осталось %d, выкинуто %d; хочу 1 и 0 при пустых токенах", len(kept), len(dropped))
	}
}

// Регистр не важен ни в одной стороне: запрос капсом, выдача маленькими.
func TestDropQueryMissIsCaseInsensitive(t *testing.T) {
	in := []Result{{Title: "the AcMe files", URL: "http://x.example/a"}}
	kept, dropped := DropQueryMiss(in, "ACME")
	if len(kept) != 1 || len(dropped) != 0 {
		t.Fatalf("осталось %d, выкинуто %d; хочу 1 и 0 без учёта регистра", len(kept), len(dropped))
	}
	if !strings.Contains(strings.ToLower(kept[0].Title), "acme") {
		t.Errorf("оставлен не тот: %q", kept[0].Title)
	}
}

// Пустой вход и пустая выдача не паникуют и не придумывают мусор.
func TestDropQueryMissEmptyInput(t *testing.T) {
	kept, dropped := DropQueryMiss(nil, "acme")
	if len(kept) != 0 || len(dropped) != 0 {
		t.Fatalf("nil вход дал kept=%d dropped=%d, хочу нули", len(kept), len(dropped))
	}
}

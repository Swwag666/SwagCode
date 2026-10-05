package parser

import (
	"context"
	"strings"
	"testing"

	"voidsearchswag/internal/store"
)

// Сохранённые селекторы читались одной строкой «if sels, err := SelectorsFor(...);
// err == nil», поэтому ошибка базы выбрасывалась целиком. Разбор при этом не
// падал: без селекторов он уходит в generic- и structural-стратегии, возвращает
// значения и healed=true с советом перегенерировать селекторы. Пользователь
// получал более слабый результат и подсказку, которая уводила от настоящей
// причины - селекторы были в порядке, недоступна была база.
//
// Контроль в начале теста доказывает, что деградация вызвана именно потерей
// чтения: пока база открыта, тот же разбор идёт точным селектором со стратегией
// css и confidence 1.0.
func TestParseDegradesWhenSelectorsUnreadable(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir + "/p.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	body := `<html><head><title>Магазин книг</title></head><body>` +
		`<div class="price-tag">1 990 ₽</div></body></html>`
	p := &Parser{Store: st, Fetch: &stubFetch{body: body}}
	if err := p.Save(ctx, "example.com", "price", ".price-tag", "css"); err != nil {
		t.Fatalf("Save: %v", err)
	}

	open, healedOpen, err := p.Parse(ctx, "http://example.com/x", []string{"price"})
	if err != nil {
		t.Fatalf("разбор на открытой базе: %v", err)
	}
	if open["price"].Strategy != "css" || open["price"].Confidence != ConfExact {
		t.Fatalf("контроль не сошёлся: на открытой базе ждал точный css, получил %+v", open["price"])
	}
	if healedOpen {
		t.Fatalf("контроль не сошёлся: точный разбор помечен исцелённым")
	}

	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	closed, healed, err := p.Parse(ctx, "http://example.com/x", []string{"price"})
	if err != nil {
		t.Fatalf("разбор на закрытой базе вернул ошибку: %v", err)
	}
	if closed["price"].Strategy == "css" {
		t.Errorf("селектор применён после закрытия базы: %+v", closed["price"])
	}
	// Значение всё ещё находится запасным путём, поэтому разбор выглядит
	// успешным: ни ошибка, ни healed не говорят, что база селекторов недоступна.
	if closed["price"].Value == "" {
		t.Errorf("значение потеряно: %+v", closed["price"])
	}
	if !healed {
		t.Errorf("деградация не помечена исцелением, совет о селекторах потерялся бы и здесь: %+v", closed["price"])
	}
	t.Logf("стратегия после закрытия базы: %q, confidence %.2f, healed %v",
		closed["price"].Strategy, closed["price"].Confidence, healed)
}

// Цель правки: предупреждение обязано доехать до вызывающего, потому что разбор
// остаётся успешным и ошибкой его не выразить.
func TestParseDetailedWarnsWhenSelectorsUnreadable(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir + "/p.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	body := `<html><head><title>Магазин книг</title></head><body>` +
		`<div class="price-tag">1 990 ₽</div></body></html>`
	p := &Parser{Store: st, Fetch: &stubFetch{body: body}}
	if err := p.Save(ctx, "example.com", "price", ".price-tag", "css"); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	out, _, warn, err := p.ParseDetailed(ctx, "http://example.com/x", []string{"price"})
	if err != nil {
		t.Fatalf("разбор на закрытой базе вернул ошибку: %v", err)
	}
	if out["price"].Value == "" {
		t.Errorf("значение потеряно: %+v", out["price"])
	}
	if warn == "" {
		t.Errorf("предупреждение о недоступных селекторах пустое")
	}
	if !strings.Contains(warn, "example.com") {
		t.Errorf("в предупреждении нет хоста: %q", warn)
	}
	if !strings.Contains(warn, "разбор без них") {
		t.Errorf("предупреждение не объясняет последствие: %q", warn)
	}
	if strings.Contains(warn, "%!") {
		t.Errorf("предупреждение собрано с ошибкой формата: %q", warn)
	}
}

// Обратная сторона: на живой базе предупреждения нет, иначе оно обесценится и
// перестанет отличать настоящую потерю селекторов.
func TestParseDetailedWarnsNothingWhenSelectorsReadable(t *testing.T) {
	p, _ := openParser(t, sampleHTML)
	ctx := context.Background()
	if err := p.Save(ctx, "example.com", "price", ".price-tag", "css"); err != nil {
		t.Fatalf("Save: %v", err)
	}

	out, healed, warn, err := p.ParseDetailed(ctx, "http://example.com/x", []string{"price"})
	if err != nil {
		t.Fatalf("ParseDetailed: %v", err)
	}
	if warn != "" {
		t.Errorf("предупреждение на живой базе: %q", warn)
	}
	if out["price"].Strategy != "css" || out["price"].Confidence != ConfExact || healed {
		t.Errorf("точный разбор изменился: %+v healed=%v", out["price"], healed)
	}
}

// Предупреждение не заменяет ошибку разбора: когда не извлечено ни одно поле,
// ошибка остаётся, а потеря селекторов доходит отдельно.
func TestParseDetailedKeepsErrorAndWarning(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir + "/p.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	p := &Parser{Store: st, Fetch: &stubFetch{body: `<html><body><p>текст без полей</p></body></html>`}}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	out, _, warn, err := p.ParseDetailed(ctx, "http://example.com/x", []string{"price"})
	if err == nil {
		t.Errorf("ошибка «ни одно поле не извлечено» потеряна")
	}
	if warn == "" {
		t.Errorf("предупреждение о недоступных селекторах потеряно при ошибке разбора")
	}
	if len(out) == 0 {
		t.Errorf("карта значений не вернулась вместе с ошибкой")
	}
}

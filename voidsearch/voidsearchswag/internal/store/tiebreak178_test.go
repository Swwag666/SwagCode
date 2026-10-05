package store

import (
	"context"
	"testing"
)

// Этап 178: развязка равных ключей сортировки. Смоуки видели пять live
// с одинаковыми rate и latency подряд и не могли назвать правило порядка:
// описание tools теперь говорит «url ASC», и это обязано быть правдой
// поведения, а не только текста. До этапа порядок держался на хвосте
// ORDER BY без своего теста: мутация, срезающая url ASC, меняла бы
// выдачу молча.
func TestListOnionsTieBreakByUrlAsc(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	// Вставки в обратном алфавиту порядке: если хвост сортировки
	// исчезнет, порядок выдачи поплывёт по insertion-порядку таблицы
	// или вернётся обратным - оба варианта поймает проверка ниже.
	rows := []Onion{
		{URL: "http://charlie.onion", Status: "live", SuccessRate: 0.5, LatencyAvg: 700},
		{URL: "http://alpha.onion", Status: "live", SuccessRate: 0.5, LatencyAvg: 700},
		{URL: "http://delta.onion", Status: "live", SuccessRate: 0.5, LatencyAvg: 700},
		{URL: "http://bravo.onion", Status: "live", SuccessRate: 0.5, LatencyAvg: 700},
	}
	for _, o := range rows {
		if err := st.UpsertOnion(ctx, o); err != nil {
			t.Fatal(err)
		}
	}

	got, err := st.ListOnions(ctx, "live", 10)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"http://alpha.onion", "http://bravo.onion", "http://charlie.onion", "http://delta.onion"}
	if len(got) != len(want) {
		t.Fatalf("записей %d, ожидала %d", len(got), len(want))
	}
	for i := range want {
		if got[i].URL != want[i] {
			t.Errorf("позиция %d: %q, ожидаю %q: равные rate и latency обязаны разрешаться адресом по алфавиту", i, got[i].URL, want[i])
		}
	}
}

// Та же развязка для очереди волны: описание probe_pool обещает, что при
// равных приоритетах охвата очередь доезжает по адресу. Проверяю на
// свежих unknown без единой пробы: приоритет у всех один, порядок обязан
// быть url ASC, иначе волна гоняет адреса в случайном порядке и оператор
// не может предсказать, кого возьмёт limit.
func TestNextProbeWaveTieBreakByUrlAsc(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	for _, u := range []string{
		"zulu00.onion",
		"alpha00.onion",
		"mike00.onion",
		"bravo00.onion",
	} {
		if err := st.UpsertOnion(ctx, Onion{URL: u, Status: "unknown"}); err != nil {
			t.Fatal(err)
		}
	}

	got, err := st.NextProbeWave(ctx, 4)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"alpha00.onion", "bravo00.onion", "mike00.onion", "zulu00.onion"}
	if len(got) != len(want) {
		t.Fatalf("адресов %d, ожидала %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("позиция %d: %q, ожидаю %q: равные приоритеты очереди обязаны разрешаться адресом по алфавиту", i, got[i], want[i])
		}
	}
}

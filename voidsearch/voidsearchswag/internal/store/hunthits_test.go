package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// newHunt заводит охоту и возвращает её id. История находок привязана к
// конкретной охоте, поэтому без строки в hunts нельзя проверить ни отбор по
// hunt_id, ни ErrNotFound при чистке несуществующей охоты.
func newHunt(t *testing.T, st *Store, query, mode string) int64 {
	t.Helper()
	id, err := st.CreateHunt(context.Background(), Hunt{Query: query, Mode: mode, ScheduleMin: 60})
	if err != nil {
		t.Fatalf("create hunt: %v", err)
	}
	return id
}

func urlsOf(hits []HuntHit) []string {
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.URL)
	}
	return out
}

func TestSaveHuntHitsCountsNewURLs(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	id := newHunt(t, st, "leak database", "fast")

	added, err := st.SaveHuntHits(ctx, id, "leak database", "fast",
		[]string{"http://a.onion/x", "http://b.onion/y"})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if added != 2 {
		t.Errorf("новых %d, хочу 2", added)
	}
	hits, err := st.ListHuntHits(ctx, id, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("строк %d, хочу 2: %+v", len(hits), hits)
	}
	for _, h := range hits {
		if h.HuntID != id || h.Query != "leak database" || h.Mode != "fast" {
			t.Errorf("строка потеряла привязку к охоте: %+v", h)
		}
		if h.TimesSeen != 1 {
			t.Errorf("times_seen %d, хочу 1 для первой находки", h.TimesSeen)
		}
		if h.FirstFound.IsZero() || h.LastFound.IsZero() {
			t.Errorf("даты не заполнены: %+v", h)
		}
		if h.ID <= 0 {
			t.Errorf("id строки %d: историю нельзя ни обновить, ни удалить построчно", h.ID)
		}
	}
}

// Повторная находка того же url не должна плодить строки: иначе история охоты,
// у которой выдача меняется туда-сюда, росла бы бесконечно и перестала отвечать
// на вопрос «что нового».
func TestSaveHuntHitsRepeatUpdatesInsteadOfDuplicating(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	id := newHunt(t, st, "leak", "fast")
	list := []string{"http://a.onion/x", "http://b.onion/y"}

	if _, err := st.SaveHuntHits(ctx, id, "leak", "fast", list); err != nil {
		t.Fatalf("первая запись: %v", err)
	}
	first, err := st.ListHuntHits(ctx, id, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	added, err := st.SaveHuntHits(ctx, id, "leak", "fast", list)
	if err != nil {
		t.Fatalf("вторая запись: %v", err)
	}
	if added != 0 {
		t.Errorf("новых %d, хочу 0: весь список уже был в истории", added)
	}
	second, err := st.ListHuntHits(ctx, id, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(second) != 2 {
		t.Fatalf("строк %d, хочу 2: повтор задублировал историю %+v", len(second), urlsOf(second))
	}
	byURL := map[string]HuntHit{}
	for _, h := range second {
		byURL[h.URL] = h
	}
	for _, before := range first {
		after, ok := byURL[before.URL]
		if !ok {
			t.Fatalf("строка %s пропала после повторной записи", before.URL)
		}
		if after.TimesSeen != before.TimesSeen+1 {
			t.Errorf("%s: times_seen %d, хочу %d", before.URL, after.TimesSeen, before.TimesSeen+1)
		}
		if after.LastFound.Before(before.LastFound) {
			t.Errorf("%s: last_found уехала назад: %v после %v", before.URL, after.LastFound, before.LastFound)
		}
		if !after.FirstFound.Equal(before.FirstFound) {
			t.Errorf("%s: first_found изменилась %v -> %v", before.URL, before.FirstFound, after.FirstFound)
		}
		if after.ID != before.ID {
			t.Errorf("%s: id строки сменился %d -> %d", before.URL, before.ID, after.ID)
		}
	}
}

func TestSaveHuntHitsPartialOverlap(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	id := newHunt(t, st, "leak", "fast")

	if _, err := st.SaveHuntHits(ctx, id, "leak", "fast", []string{"http://a.onion/x", "http://b.onion/y"}); err != nil {
		t.Fatalf("первая запись: %v", err)
	}
	added, err := st.SaveHuntHits(ctx, id, "leak", "fast", []string{"http://b.onion/y", "http://c.onion/z"})
	if err != nil {
		t.Fatalf("вторая запись: %v", err)
	}
	if added != 1 {
		t.Errorf("новых %d, хочу 1: b уже был в истории", added)
	}
	hits, err := st.ListHuntHits(ctx, id, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(hits) != 3 {
		t.Fatalf("строк %d, хочу 3: %+v", len(hits), urlsOf(hits))
	}
	for _, h := range hits {
		if h.URL == "http://b.onion/y" && h.TimesSeen != 2 {
			t.Errorf("b: times_seen %d, хочу 2", h.TimesSeen)
		}
		if h.URL != "http://b.onion/y" && h.TimesSeen != 1 {
			t.Errorf("%s: times_seen %d, хочу 1", h.URL, h.TimesSeen)
		}
	}
}

// Пустые url и дубли внутри одной выдачи не должны становиться событиями: UNIQUE
// превратил бы все пустые строки в одну, а times_seen начал бы расти от дублей в
// одном списке вместо роста от находки к находке.
func TestSaveHuntHitsSkipsEmptyAndDuplicates(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	id := newHunt(t, st, "leak", "fast")

	added, err := st.SaveHuntHits(ctx, id, "leak", "fast",
		[]string{"", "   ", "http://a.onion/x", "http://a.onion/x", " http://a.onion/x "})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if added != 1 {
		t.Errorf("новых %d, хочу 1", added)
	}
	hits, err := st.ListHuntHits(ctx, id, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("строк %d, хочу 1: %+v", len(hits), urlsOf(hits))
	}
	if hits[0].URL != "http://a.onion/x" {
		t.Errorf("url %q не обрезан от пробелов", hits[0].URL)
	}
	if hits[0].TimesSeen != 1 {
		t.Errorf("times_seen %d, хочу 1: дубли внутри выдачи считаются одной находкой", hits[0].TimesSeen)
	}
}

func TestSaveHuntHitsRejectsBadHuntID(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	for _, id := range []int64{0, -1} {
		if _, err := st.SaveHuntHits(ctx, id, "leak", "fast", []string{"http://a.onion/x"}); err == nil {
			t.Errorf("id %d принят", id)
		}
	}
}

// Один и тот же url у двух охот - разные события: история читается по охоте, и
// объединение строк показало бы находку одной охоты в чужой.
func TestSaveHuntHitsKeepsHuntsApart(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	first := newHunt(t, st, "leak", "fast")
	second := newHunt(t, st, "leak", "stealth")
	url := "http://a.onion/x"

	n1, err := st.SaveHuntHits(ctx, first, "leak", "fast", []string{url})
	if err != nil {
		t.Fatalf("save 1: %v", err)
	}
	n2, err := st.SaveHuntHits(ctx, second, "leak", "stealth", []string{url})
	if err != nil {
		t.Fatalf("save 2: %v", err)
	}
	if n1 != 1 || n2 != 1 {
		t.Errorf("новых %d и %d, хочу 1 и 1", n1, n2)
	}
	for _, id := range []int64{first, second} {
		hits, err := st.ListHuntHits(ctx, id, 0)
		if err != nil {
			t.Fatalf("list %d: %v", id, err)
		}
		if len(hits) != 1 || hits[0].HuntID != id {
			t.Errorf("охота %d: строк %d %+v, хочу одну свою", id, len(hits), hits)
		}
		if len(hits) == 1 && hits[0].TimesSeen != 1 {
			t.Errorf("охота %d: times_seen %d, хочу 1", id, hits[0].TimesSeen)
		}
	}
	all, err := st.ListHuntHits(ctx, 0, 0)
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("строк по всем охотам %d, хочу 2", len(all))
	}
}

func TestListHuntHitsRespectsLimit(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	id := newHunt(t, st, "leak", "fast")
	list := []string{"http://a.onion/1", "http://a.onion/2", "http://a.onion/3",
		"http://a.onion/4", "http://a.onion/5"}
	if _, err := st.SaveHuntHits(ctx, id, "leak", "fast", list); err != nil {
		t.Fatalf("save: %v", err)
	}
	two, err := st.ListHuntHits(ctx, id, 2)
	if err != nil {
		t.Fatalf("list 2: %v", err)
	}
	if len(two) != 2 {
		t.Errorf("limit=2 отдал %d строк", len(two))
	}
	all, err := st.ListHuntHits(ctx, id, 0)
	if err != nil {
		t.Fatalf("list 0: %v", err)
	}
	if len(all) != 5 {
		t.Errorf("limit=0 отдал %d строк, хочу все 5", len(all))
	}
	huge, err := st.ListHuntHits(ctx, id, 100000)
	if err != nil {
		t.Fatalf("list huge: %v", err)
	}
	if len(huge) != 5 {
		t.Errorf("необъятный limit отдал %d строк", len(huge))
	}
}

// Свежие раньше: историю читают глазами, и сортировка по возрастанию заставила бы
// листать к последней находке через всю историю охоты.
func TestListHuntHitsNewestFirst(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	id := newHunt(t, st, "leak", "fast")
	if _, err := st.SaveHuntHits(ctx, id, "leak", "fast", []string{"http://a.onion/old"}); err != nil {
		t.Fatalf("save old: %v", err)
	}
	if _, err := st.SaveHuntHits(ctx, id, "leak", "fast", []string{"http://a.onion/new"}); err != nil {
		t.Fatalf("save new: %v", err)
	}
	hits, err := st.ListHuntHits(ctx, id, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("строк %d, хочу 2", len(hits))
	}
	if hits[0].URL != "http://a.onion/new" {
		t.Errorf("первой идёт %q, хочу свежую находку", hits[0].URL)
	}
	if hits[1].URL != "http://a.onion/old" {
		t.Errorf("второй идёт %q, хочу прежнюю находку", hits[1].URL)
	}
}

func TestDeleteHuntHitsRemovesRows(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	first := newHunt(t, st, "leak", "fast")
	second := newHunt(t, st, "other", "fast")
	if _, err := st.SaveHuntHits(ctx, first, "leak", "fast",
		[]string{"http://a.onion/1", "http://a.onion/2", "http://a.onion/3"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := st.SaveHuntHits(ctx, second, "other", "fast", []string{"http://b.onion/1"}); err != nil {
		t.Fatalf("save 2: %v", err)
	}

	n, err := st.DeleteHuntHits(ctx, first)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n != 3 {
		t.Errorf("удалено %d строк, хочу 3", n)
	}
	rest, err := st.ListHuntHits(ctx, first, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rest) != 0 {
		t.Errorf("после чистки осталось %d строк: %+v", len(rest), urlsOf(rest))
	}
	other, err := st.ListHuntHits(ctx, second, 0)
	if err != nil {
		t.Fatalf("list other: %v", err)
	}
	if len(other) != 1 {
		t.Errorf("чистка одной охоты задела другую: строк %d", len(other))
	}
	again, err := st.DeleteHuntHits(ctx, first)
	if err != nil {
		t.Fatalf("повторная чистка: %v", err)
	}
	if again != 0 {
		t.Errorf("повторная чистка удалила %d строк", again)
	}
}

// Молчаливый ноль при опечатке в id не отличим от честно пустой истории, поэтому
// несуществующая охота обязана давать ErrNotFound.
func TestDeleteHuntHitsUnknownHuntIsNotFound(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	id := newHunt(t, st, "leak", "fast")
	if _, err := st.SaveHuntHits(ctx, id, "leak", "fast", []string{"http://a.onion/1"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := st.DeleteHuntHits(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("охота 999: err=%v, хочу ErrNotFound", err)
	}
	hits, err := st.ListHuntHits(ctx, id, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(hits) != 1 {
		t.Errorf("строк %d, хочу 1: неудачная чистка задела историю", len(hits))
	}
}

func TestDeleteHuntHitsRequiresID(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	id := newHunt(t, st, "leak", "fast")
	if _, err := st.SaveHuntHits(ctx, id, "leak", "fast", []string{"http://a.onion/1"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	for _, bad := range []int64{0, -1} {
		n, err := st.DeleteHuntHits(ctx, bad)
		if err == nil {
			t.Errorf("id %d принят, удалено %d строк", bad, n)
			continue
		}
		if errors.Is(err, ErrNotFound) {
			t.Errorf("id %d дал ErrNotFound вместо отказа чистить всё разом", bad)
		}
	}
	hits, err := st.ListHuntHits(ctx, id, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(hits) != 1 {
		t.Errorf("строк %d, хочу 1: вызов без id стёр историю", len(hits))
	}
}

// Ошибки базы обязаны выходить наружу с именем операции: hunt печатает SaveError
// оператору, и «database is closed» без префикса не говорит, какое именно действие
// не удалось.
func TestHuntHitsAPIReportsClosedStore(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "closed.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := st.SaveHuntHits(ctx, 1, "leak", "fast", []string{"http://a.onion/x"}); err == nil {
		t.Error("SaveHuntHits на закрытой базе не вернул ошибку")
	} else if !strings.Contains(err.Error(), "save hunt hits") {
		t.Errorf("SaveHuntHits: %v без имени операции", err)
	}
	if _, err := st.ListHuntHits(ctx, 1, 10); err == nil {
		t.Error("ListHuntHits на закрытой базе не вернул ошибку")
	} else if !strings.Contains(err.Error(), "list hunt hits") {
		t.Errorf("ListHuntHits: %v без имени операции", err)
	}
	if _, err := st.DeleteHuntHits(ctx, 1); err == nil {
		t.Error("DeleteHuntHits на закрытой базе не вернул ошибку")
	} else if !strings.Contains(err.Error(), "delete hunt hits") {
		t.Errorf("DeleteHuntHits: %v без имени операции", err)
	}
}

// Миграция с hunt_hits обязана переживать повторный Migrate на уже развёрнутой
// базе: список миграций применяется по версии, и второй проход не должен ни
// пересоздавать таблицу, ни терять записанную историю.
func TestHuntHitsMigrationIsIdempotent(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	id := newHunt(t, st, "leak", "fast")
	if _, err := st.SaveHuntHits(ctx, id, "leak", "fast", []string{"http://a.onion/1"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("повторный migrate: %v", err)
	}
	hits, err := st.ListHuntHits(ctx, id, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(hits) != 1 || hits[0].TimesSeen != 1 {
		t.Errorf("история после повторной миграции: %+v", hits)
	}
	if _, err := st.SaveHuntHits(ctx, id, "leak", "fast", []string{"http://a.onion/2"}); err != nil {
		t.Errorf("запись после повторной миграции: %v", err)
	}
}

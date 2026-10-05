package hunt

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"voidsearchswag/internal/store"
)

// openStoreAt открывает базу по известному пути. Путь нужен наружу: для проверки
// SaveError второй коннект выкидывает таблицу hunt_hits из-под прогона, и без
// адреса файла это не сделать.
func openStoreAt(t *testing.T, path string) *store.Store {
	t.Helper()
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// scriptedSearch отдаёт по раунду выдачи на каждый вызов. Последний раунд
// повторяется: прогон может опрашивать поиск чаще, чем записано сценарием, и
// паника на лишнем вызове маскировала бы проверяемое поведение.
func scriptedSearch(rounds ...[]string) SearchFunc {
	i := 0
	return func(context.Context, string, string, int) (SearchOutcome, error) {
		if i >= len(rounds) {
			i = len(rounds) - 1
		}
		urls := rounds[i]
		i++
		return SearchOutcome{URLs: urls}, nil
	}
}

func historyURLs(hits []store.HuntHit) []string {
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.URL)
	}
	return out
}

// perQuerySearch раздаёт раунды выдачи отдельно на каждый запрос. Общий скрипт на
// несколько охот здесь не годится: охоты съедали раунды по очереди, и вторая
// получала «базу» из выдачи первой, после чего находки у неё не случалось.
func perQuerySearch(rounds map[string][][]string) SearchFunc {
	i := map[string]int{}
	return func(_ context.Context, query, _ string, _ int) (SearchOutcome, error) {
		list := rounds[query]
		n := i[query]
		if n >= len(list) {
			n = len(list) - 1
		}
		i[query]++
		return SearchOutcome{URLs: list[n]}, nil
	}
}

// Первый прогон фиксирует базу и находкой не считается, поэтому в историю он
// попадать не должен: иначе история начиналась бы со снимка того, что охота
// видела всегда, и «новых url» в первой настоящей находке стало бы меньше, чем
// оператор видел в выдаче.
func TestBaselineRunWritesNoHistory(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	r := &Runner{Store: st, Search: scriptedSearch([]string{"http://a.onion/x", "http://b.onion/y"})}
	id, err := r.Create(ctx, "leak", "fast", 60)
	if err != nil {
		t.Fatal(err)
	}
	hit, err := r.RunOne(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if hit.Changed {
		t.Fatalf("первый прогон стал находкой: %+v", hit)
	}
	if hit.Saved != 0 || hit.SaveError != "" {
		t.Errorf("базовый прогон отчитался записью: saved=%d err=%q", hit.Saved, hit.SaveError)
	}
	hits, err := r.Hits(ctx, id, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Errorf("история после базового прогона: %+v", historyURLs(hits))
	}
}

// Saved считает url, которых в истории находок этой охоты ещё не было. У первой
// находки история пуста, поэтому новых столько же, сколько url в выдаче: до неё
// охота не записала ничего.
func TestFirstHitWritesWholeResultToHistory(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	r := &Runner{Store: st, Search: scriptedSearch(
		[]string{"http://a.onion/x"},
		[]string{"http://b.onion/y", "http://c.onion/z"},
	)}
	id, err := r.Create(ctx, "leak database", "stealth", 60)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.RunOne(ctx, id); err != nil {
		t.Fatal(err)
	}
	hit, err := r.RunOne(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !hit.Changed {
		t.Fatalf("смена выдачи не стала находкой: %+v", hit)
	}
	if hit.Saved != 2 {
		t.Errorf("saved=%d, хочу 2: история была пуста", hit.Saved)
	}
	if hit.SaveError != "" {
		t.Errorf("save_error=%q при успешной записи", hit.SaveError)
	}
	hits, err := r.Hits(ctx, id, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("строк %d, хочу 2: %+v", len(hits), historyURLs(hits))
	}
	for _, h := range hits {
		if h.HuntID != id {
			t.Errorf("строка %s привязана к охоте %d вместо %d", h.URL, h.HuntID, id)
		}
		// Запрос и режим берутся из находки, а не из hunts: история обязана
		// оставаться читаемой, даже если охоту потом переименуют.
		if h.Query != "leak database" || h.Mode != "stealth" {
			t.Errorf("строка %s потеряла привязку к запросу: %q/%q", h.URL, h.Query, h.Mode)
		}
		if h.TimesSeen != 1 {
			t.Errorf("строка %s: times_seen %d, хочу 1", h.URL, h.TimesSeen)
		}
	}
}

// Вторая находка с частичным пересечением: уже виденный url получает счётчик, а
// Saved считает только действительно новый.
func TestSecondHitCountsOnlyUnseenURLs(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	r := &Runner{Store: st, Search: scriptedSearch(
		[]string{"http://a.onion/x"},
		[]string{"http://b.onion/y", "http://c.onion/z"},
		[]string{"http://c.onion/z", "http://d.onion/w"},
	)}
	id, err := r.Create(ctx, "leak", "fast", 60)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := r.RunOne(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	hit, err := r.RunOne(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !hit.Changed {
		t.Fatalf("третья выдача не стала находкой: %+v", hit)
	}
	if hit.Saved != 1 {
		t.Errorf("saved=%d, хочу 1: c уже был в истории", hit.Saved)
	}
	hits, err := r.Hits(ctx, id, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 3 {
		t.Fatalf("строк %d, хочу 3: %+v", len(hits), historyURLs(hits))
	}
	byURL := map[string]store.HuntHit{}
	for _, h := range hits {
		byURL[h.URL] = h
	}
	if got := byURL["http://c.onion/z"].TimesSeen; got != 2 {
		t.Errorf("c: times_seen %d, хочу 2", got)
	}
	if got := byURL["http://d.onion/w"].TimesSeen; got != 1 {
		t.Errorf("d: times_seen %d, хочу 1", got)
	}
	if hits[0].URL != "http://d.onion/w" {
		t.Errorf("первой в истории идёт %q, хочу свежую находку", hits[0].URL)
	}
}

// Прогон по расписанию пишет историю так же, как RunOne: фоновый режим -
// основной способ получения находок, и потеря истории именно там была бы
// заметнее всего.
func TestRunDueWritesHistory(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	base := time.Now()
	r := &Runner{Store: st, Now: func() time.Time { return base },
		Search: scriptedSearch(
			[]string{"http://a.onion/x"},
			[]string{"http://a.onion/x", "http://b.onion/y"},
		)}
	id, err := r.Create(ctx, "leak", "fast", 60)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.RunDue(ctx); err != nil {
		t.Fatal(err)
	}
	// Расписание вышло: без перевода часов RunDue пропустил бы охоту и тест
	// проверял бы не запись истории, а собственную фикстуру.
	r.Now = func() time.Time { return base.Add(2 * time.Hour) }
	rep, err := r.RunDue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Hits) != 1 {
		t.Fatalf("находок %d, хочу 1: %+v", len(rep.Hits), rep)
	}
	if rep.Hits[0].Saved != 2 {
		t.Errorf("saved=%d, хочу 2", rep.Hits[0].Saved)
	}
	if rep.Hits[0].SaveError != "" {
		t.Errorf("save_error=%q", rep.Hits[0].SaveError)
	}
	hits, err := r.Hits(ctx, id, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Errorf("строк истории %d, хочу 2: %+v", len(hits), historyURLs(hits))
	}
}

// WatchDetailed идёт через тот же run(), поэтому находка из блокирующего
// ожидания обязана попасть в историю: именно watch чаще всего работает долго и
// перезапускается, и находки терялись бы пачками.
func TestWatchWritesHistory(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	r := &Runner{Store: st, Search: scriptedSearch(
		[]string{"http://a.onion/x"},
		[]string{"http://b.onion/y"},
	)}
	id, err := r.Create(ctx, "leak", "fast", 60)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.RunOne(ctx, id); err != nil {
		t.Fatal(err)
	}
	rep, err := r.WatchDetailed(ctx, id, 3*time.Second, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Hits) != 1 {
		t.Fatalf("находок %d, хочу 1: %+v", len(rep.Hits), rep)
	}
	if rep.Hits[0].Saved != 1 {
		t.Errorf("saved=%d, хочу 1", rep.Hits[0].Saved)
	}
	hits, err := r.Hits(ctx, id, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].URL != "http://b.onion/y" {
		t.Errorf("история после watch: %+v", historyURLs(hits))
	}
}

func TestUnchangedRunWritesNoHistory(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	r := &Runner{Store: st, Search: scriptedSearch([]string{"http://a.onion/x"})}
	id, err := r.Create(ctx, "leak", "fast", 60)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		hit, err := r.RunOne(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if i > 0 && (hit.Changed || hit.Saved != 0) {
			t.Errorf("прогон %d: changed=%v saved=%d при неизменной выдаче", i, hit.Changed, hit.Saved)
		}
	}
	hits, err := r.Hits(ctx, id, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Errorf("история при стабильной выдаче: %+v", historyURLs(hits))
	}
}

// Выдача может исчезнуть целиком: hash сменился, а url нет. Находка остаётся
// находкой, но писать в историю нечего, и это не ошибка записи.
func TestEmptyResultHitWritesNoHistory(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	r := &Runner{Store: st, Search: scriptedSearch(
		[]string{"http://a.onion/x"},
		nil,
	)}
	id, err := r.Create(ctx, "leak", "fast", 60)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.RunOne(ctx, id); err != nil {
		t.Fatal(err)
	}
	hit, err := r.RunOne(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !hit.Changed {
		t.Fatalf("опустевшая выдача не стала находкой: %+v", hit)
	}
	if hit.Saved != 0 {
		t.Errorf("saved=%d при пустой выдаче", hit.Saved)
	}
	if hit.SaveError != "" {
		t.Errorf("save_error=%q при пустой выдаче: писать нечего, это не ошибка", hit.SaveError)
	}
	hits, err := r.Hits(ctx, id, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Errorf("строк истории %d, хочу 0: %+v", len(hits), historyURLs(hits))
	}
}

// Ошибка записи истории не должна ни ронять прогон, ни терять находку: hash уже
// обновлён и откатить его нельзя, поэтому вызывающий получает находку с
// SaveError и печатает предупреждение.
func TestSaveErrorIsReportedAndHitSurvives(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hits.db")
	st := openStoreAt(t, path)
	ctx := context.Background()
	r := &Runner{Store: st, Search: scriptedSearch(
		[]string{"http://a.onion/x"},
		[]string{"http://b.onion/y"},
	)}
	id, err := r.Create(ctx, "leak", "fast", 60)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.RunOne(ctx, id); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE hunt_hits`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()

	hit, err := r.RunOne(ctx, id)
	if err != nil {
		t.Fatalf("прогон упал из-за незаписанной истории: %v", err)
	}
	if !hit.Changed {
		t.Errorf("находка потеряна: %+v", hit)
	}
	if hit.SaveError == "" {
		t.Errorf("save_error пуст: о потере истории никто бы не узнал, %+v", hit)
	}
	if !strings.Contains(hit.SaveError, "hunt_hits") {
		t.Errorf("save_error=%q не называет причину", hit.SaveError)
	}
	if hit.Saved != 0 {
		t.Errorf("saved=%d при незаписанной истории", hit.Saved)
	}
	// Охота при этом остаётся живой: следующий прогон работает как обычно.
	list, err := r.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != id {
		t.Errorf("охота пропала из списка: %+v", list)
	}
}

func TestHitsAndClearHitsWithoutStore(t *testing.T) {
	r := &Runner{}
	if _, err := r.Hits(context.Background(), 1, 10); err == nil {
		t.Error("Hits без стора не вернул ошибку")
	}
	if _, err := r.ClearHits(context.Background(), 1); err == nil {
		t.Error("ClearHits без стора не вернул ошибку")
	}
}

func TestClearHitsRemovesHistory(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	r := &Runner{Store: st, Search: scriptedSearch(
		[]string{"http://a.onion/x"},
		[]string{"http://b.onion/y"},
	)}
	id, err := r.Create(ctx, "leak", "fast", 60)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.RunOne(ctx, id); err != nil {
		t.Fatal(err)
	}
	hit, err := r.RunOne(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if hit.Saved != 1 {
		t.Fatalf("saved=%d, хочу 1", hit.Saved)
	}
	n, err := r.ClearHits(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("удалено %d строк, хочу 1", n)
	}
	hits, err := r.Hits(ctx, id, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Errorf("после чистки осталось %d строк: %+v", len(hits), historyURLs(hits))
	}
}

// id <= 0 читает историю всех охот: оператор держит несколько мониторингов и
// хочет видеть последние находки одним списком, а не обходить охоты по очереди.
func TestHitsWithoutIDReadsAllHunts(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	r := &Runner{Store: st, Search: perQuerySearch(map[string][][]string{
		"leak":   {{"http://a.onion/x"}, {"http://b.onion/y"}},
		"breach": {{"http://c.onion/z"}, {"http://d.onion/w"}},
	})}
	first, err := r.Create(ctx, "leak", "fast", 60)
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.Create(ctx, "breach", "stealth", 60)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{first, second} {
		if _, err := r.RunOne(ctx, id); err != nil {
			t.Fatal(err)
		}
		if _, err := r.RunOne(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	all, err := r.Hits(ctx, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("строк по всем охотам %d, хочу 2: %+v", len(all), historyURLs(all))
	}
	seen := map[int64]int{}
	for _, h := range all {
		seen[h.HuntID]++
	}
	if seen[first] != 1 || seen[second] != 1 {
		t.Errorf("история перемешала охоты: %+v", seen)
	}
	one, err := r.Hits(ctx, second, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(one) != 1 || one[0].Query != "breach" {
		t.Errorf("выборка по охоте %d: %+v", second, one)
	}
}

package hunt

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"voidsearchswag/internal/store"
)

// deadEngines возвращает поиск, у которого выдача пуста и все движки отказали.
// Ровно так выглядит прогон при мёртвом tor или мёртвом прокси: ядро отдаёт
// пустой результат и nil ошибку, поэтому до появления SearchOutcome охота не
// могла отличить отказ от честной пустой выдачи.
func deadEngines(total int) SearchFunc {
	return func(context.Context, string, string, int) (SearchOutcome, error) {
		return SearchOutcome{EnginesFailed: total, EnginesTotal: total}, nil
	}
}

// liveEngines возвращает поиск, у которого движки отвечают.
func liveEngines(urls []string, total int) SearchFunc {
	return func(context.Context, string, string, int) (SearchOutcome, error) {
		return SearchOutcome{URLs: urls, EnginesTotal: total}, nil
	}
}

// baselineHash заводит охоту и фиксирует базу живым прогоном.
func baselineHash(t *testing.T, r *Runner, query string, urls []string) (int64, string) {
	t.Helper()
	ctx := context.Background()
	id, err := r.Create(ctx, query, "deep", 60)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.RunOne(ctx, id); err != nil {
		t.Fatal(err)
	}
	hunts, err := r.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hunts {
		if h.ID == id {
			if h.LastHash == "" {
				t.Fatal("базовый hash не записан")
			}
			if len(urls) == 0 && h.LastHash != HashURLs(nil) {
				t.Errorf("hash пустой выдачи = %q, ожидала %q", h.LastHash, HashURLs(nil))
			}
			return id, h.LastHash
		}
	}
	t.Fatalf("охота %d не найдена в списке", id)
	return 0, ""
}

func hashOf(t *testing.T, r *Runner, id int64) string {
	t.Helper()
	hunts, err := r.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hunts {
		if h.ID == id {
			return h.LastHash
		}
	}
	t.Fatalf("охота %d не найдена", id)
	return ""
}

func TestRunDueCountsDeadEnginesAsFailed(t *testing.T) {
	// Отчёт «проверено 2, отказов 0» при полностью мёртвых движках утверждал,
	// что охота выполнена и выдача пуста. Измерено живым прогоном: checked=2,
	// failed=0, находка с нулём URL и changed=true.
	st := openStore(t)
	ctx := context.Background()
	r := &Runner{Store: st, Search: deadEngines(2)}
	for _, q := range []string{"leak", "dump"} {
		if _, err := r.Create(ctx, q, "deep", 60); err != nil {
			t.Fatal(err)
		}
	}

	rep, err := r.RunDue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Failed != 2 {
		t.Errorf("Failed = %d, ожидала 2: отказ всех движков не попал в счётчик (весь отчёт %+v)", rep.Failed, rep)
	}
	if rep.Checked != 0 {
		t.Errorf("Checked = %d, ожидала 0: прогон не выполнен", rep.Checked)
	}
	if rep.Skipped != 0 {
		t.Errorf("Skipped = %d, ожидала 0", rep.Skipped)
	}
	if !strings.Contains(rep.LastError, "все движки поиска отказали (2 из 2)") {
		t.Errorf("LastError = %q, ожидала причину отказа", rep.LastError)
	}
	if len(rep.Hits) != 0 {
		t.Errorf("находки при мёртвых движках: %+v", rep.Hits)
	}
}

func TestRunDueKeepsBaselineWhenEnginesDown(t *testing.T) {
	// Повреждение данных: прогон при мёртвых движках записывал sha256 пустой
	// строки вместо настоящего базового hash. Живой замер показал, как охота с
	// базой 117178d6002f... получила e3b0c44298fc... и находку с нулём URL.
	st := openStore(t)
	ctx := context.Background()
	now := time.Now()
	urls := []string{"http://a.onion", "http://b.onion"}
	r := &Runner{Store: st, Now: func() time.Time { return now }, Search: liveEngines(urls, 2)}
	id, before := baselineHash(t, r, "leak", urls)

	r.Search = deadEngines(2)
	now = now.Add(2 * time.Hour)
	rep, err := r.RunDue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Failed != 1 {
		t.Errorf("Failed = %d, ожидала 1 (%+v)", rep.Failed, rep)
	}
	if after := hashOf(t, r, id); after != before {
		t.Errorf("базовый hash затёрт прогоном без поиска: было %q, стало %q (sha256 пустой строки = %q)",
			before, after, HashURLs(nil))
	}
}

func TestRunDueNoFalseHitAfterRecovery(t *testing.T) {
	// Полный цикл ложной находки: живой прогон фиксирует базу, прогон при
	// мёртвых движках больше её не трогает, поэтому возвращение сети с той же
	// выдачей не выглядит изменением.
	st := openStore(t)
	ctx := context.Background()
	now := time.Now()
	urls := []string{"http://a.onion", "http://b.onion"}
	r := &Runner{Store: st, Now: func() time.Time { return now }, Search: liveEngines(urls, 2)}
	id, _ := baselineHash(t, r, "leak", urls)

	r.Search = deadEngines(2)
	now = now.Add(2 * time.Hour)
	if _, err := r.RunDue(ctx); err != nil {
		t.Fatal(err)
	}

	r.Search = liveEngines(urls, 2)
	now = now.Add(2 * time.Hour)
	rep, err := r.RunDue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Hits) != 0 {
		t.Errorf("ложная находка после восстановления сети: %+v", rep.Hits)
	}
	if rep.Failed != 0 {
		t.Errorf("Failed = %d, ожидала 0: сеть жива", rep.Failed)
	}
	hit, err := r.RunOne(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if hit.Changed {
		t.Errorf("та же выдача названа изменением: %+v", hit)
	}
}

func TestRunOneFailsWhenEnginesDown(t *testing.T) {
	// RunOne обязан вернуть ошибку: CLI печатает «прогон: ...» и выходит с
	// кодом 1, MCP отдаёт isError. Молчаливый Hit с changed=false оставил бы
	// затёртый hash и пустую находку в отчёте.
	st := openStore(t)
	ctx := context.Background()
	urls := []string{"http://a.onion"}
	r := &Runner{Store: st, Search: liveEngines(urls, 2)}
	id, before := baselineHash(t, r, "leak", urls)

	r.Search = deadEngines(2)
	hit, err := r.RunOne(ctx, id)
	if err == nil {
		t.Fatalf("RunOne принял прогон без поиска: %+v", hit)
	}
	if !strings.Contains(err.Error(), "все движки поиска отказали") {
		t.Errorf("ошибка %v не называет причину", err)
	}
	if after := hashOf(t, r, id); after != before {
		t.Errorf("hash затёрт отказавшим прогоном: %q -> %q", before, after)
	}
}

func TestRunOneStillRecordsHonestEmptyResult(t *testing.T) {
	// Семантика честной пустой выдачи сохраняется: движки отвечают, результатов
	// нет - это факт о мире, его и записывают базой.
	st := openStore(t)
	ctx := context.Background()
	urls := []string{"http://a.onion"}
	r := &Runner{Store: st, Search: liveEngines(urls, 2)}
	id, before := baselineHash(t, r, "leak", urls)

	r.Search = liveEngines(nil, 2)
	hit, err := r.RunOne(ctx, id)
	if err != nil {
		t.Fatalf("живой поиск с пустой выдачей отклонён: %v", err)
	}
	if !hit.Changed {
		t.Errorf("исчезновение выдачи не замечено: %+v", hit)
	}
	if after := hashOf(t, r, id); after == before || after != HashURLs(nil) {
		t.Errorf("hash = %q, ожидала %q (прежний %q)", after, HashURLs(nil), before)
	}
}

func TestRunDueBaselinesHonestEmptyResult(t *testing.T) {
	// Первый прогон с честной пустой выдачей обязан зафиксировать базу: без
	// этого охота, у которой поиск ничего не находит, никогда бы не стартовала
	// и каждое изменение выдачи терялось.
	st := openStore(t)
	ctx := context.Background()
	r := &Runner{Store: st, Search: liveEngines(nil, 3)}
	id, err := r.Create(ctx, "leak", "deep", 60)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := r.RunDue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Checked != 1 || rep.Failed != 0 {
		t.Errorf("честная пустая выдача принята за отказ: %+v", rep)
	}
	if got := hashOf(t, r, id); got != HashURLs(nil) {
		t.Errorf("hash = %q, ожидала %q", got, HashURLs(nil))
	}
}

func TestRunDuePartialFailureStillRuns(t *testing.T) {
	// Частичная деградация - не отказ: один движок из двух лёг, второй ответил.
	// Считать такой прогон невыполненным значит останавливать охоту всякий раз,
	// когда отвалился хотя бы один источник.
	st := openStore(t)
	ctx := context.Background()
	r := &Runner{Store: st, Search: func(context.Context, string, string, int) (SearchOutcome, error) {
		return SearchOutcome{
			URLs:          []string{"http://a.onion"},
			EnginesFailed: 1,
			EnginesTotal:  2,
		}, nil
	}}
	id, err := r.Create(ctx, "leak", "deep", 60)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := r.RunDue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Checked != 1 || rep.Failed != 0 {
		t.Errorf("частичный отказ принят за полный: %+v", rep)
	}
	if hashOf(t, r, id) == "" {
		t.Error("база не записана при частично живом поиске")
	}
}

func TestWatchNamesDeadEngines(t *testing.T) {
	// Пояснение ожидания при мёртвых движках уводило править запрос:
	// «охота не сломана по таймауту, она пуста». Живой замер роли показал ровно
	// эту строку при failed=0.
	st := openStore(t)
	ctx := context.Background()
	r := &Runner{Store: st, Search: deadEngines(4)}
	id, err := r.Create(ctx, "leak", "deep", 0)
	if err != nil {
		t.Fatal(err)
	}

	// Окно поднято с 200 мс: под полным пакетом холодное открытие базы
	// иногда съедало дедлайн до первого опроса, и отказ движков не успевал
	// попасть в отчёт.
	rep, err := r.WatchDetailed(ctx, id, 2*time.Second, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Failed < 1 {
		t.Errorf("Failed = %d, ожидала не меньше 1", rep.Failed)
	}
	if !strings.Contains(rep.LastError, "все движки поиска отказали") {
		t.Errorf("LastError = %q", rep.LastError)
	}
	note := rep.Note()
	if strings.Contains(note, "не сломана") {
		t.Errorf("Note() утверждает, что охота не сломана, при мёртвых движках: %q", note)
	}
	if !strings.Contains(note, "отказ") {
		t.Errorf("Note() не называет отказы: %q", note)
	}
}

func TestWatchDetailedKeepsEmptyWordingForLiveSearch(t *testing.T) {
	// Обратная сторона: живой поиск без результатов обязан остаться «пустой
	// охотой», иначе предупреждение об отказах обесценится.
	st := openStore(t)
	ctx := context.Background()
	r := &Runner{Store: st, Search: liveEngines(nil, 2)}
	id, err := r.Create(ctx, "leak", "deep", 0)
	if err != nil {
		t.Fatal(err)
	}

	// Тайминги не про гонку, а про дедлайн ожидания: под полным пакетом
	// (холодный диск, соседние пакеты только что закрыли свои базы) открытие
	// хранилища и первый list hunts иногда занимали больше 150 мс, и Watch
	// возвращал «context deadline exceeded» раньше первого опроса. Окно
	// поднято до двух секунд: смысл теста - формулировка Note(), а не
	// успеваемость за 150 мс.
	rep, err := r.WatchDetailed(ctx, id, 2*time.Second, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Failed != 0 {
		t.Errorf("Failed = %d, ожидала 0: движки отвечали", rep.Failed)
	}
	if note := rep.Note(); !strings.Contains(note, "пуста") {
		t.Errorf("Note() = %q, ожидала объяснение про пустую охоту", note)
	}
}

func TestEnginesDownNeedsKnownEngines(t *testing.T) {
	cases := []struct {
		name          string
		failed, total int
		want          bool
	}{
		{"отчёт движков не пришёл", 0, 0, false},
		{"все легли", 2, 2, true},
		{"один из двух лёг", 1, 2, false},
		{"все четыре легли", 4, 4, true},
		{"ни один не лёг", 0, 3, false},
		{"счётчик больше числа движков", 5, 2, true},
	}
	for _, c := range cases {
		got := SearchOutcome{EnginesFailed: c.failed, EnginesTotal: c.total}.EnginesDown()
		if got != c.want {
			t.Errorf("%s: EnginesDown(%d из %d) = %v, ожидала %v", c.name, c.failed, c.total, got, c.want)
		}
	}
}

func TestReasonCarriesNote(t *testing.T) {
	plain := SearchOutcome{EnginesFailed: 2, EnginesTotal: 2}.Reason()
	if plain != "все движки поиска отказали (2 из 2)" {
		t.Errorf("Reason() = %q", plain)
	}
	withNote := SearchOutcome{
		EnginesFailed: 3,
		EnginesTotal:  3,
		Note:          "onion-выдача пуста, отработал clearnet",
	}.Reason()
	if !strings.Contains(withNote, "(3 из 3)") || !strings.Contains(withNote, "onion-выдача пуста") {
		t.Errorf("Reason() = %q, ожидала счётчик и пояснение ядра", withNote)
	}
}

func TestWatchByIDSurvivesSearchFailure(t *testing.T) {
	// Режим конкретной охоты: отказ поиска не должен ронять ожидание. Пока любая
	// ошибка RunOne прерывала watch на первом опросе, оператор получал «прогон:
	// ...» вместо диагностики с числом отказов и пояснением.
	st := openStore(t)
	ctx := context.Background()
	r := &Runner{Store: st, Search: deadEngines(2)}
	id, err := r.Create(ctx, "leak", "deep", 0)
	if err != nil {
		t.Fatal(err)
	}

	// Окно поднято с 200 мс: под полным пакетом холодное открытие базы
	// иногда съедало весь дедлайн до первого опроса. Смысл теста -
	// формулировка причины, а не успеваемость.
	rep, err := r.WatchDetailed(ctx, id, 2*time.Second, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("ожидание прервано отказом поиска: %v", err)
	}
	if !rep.Timeout {
		t.Error("ожидание не дошло до таймаута")
	}
	if rep.Polls < 2 {
		t.Errorf("опросов %d, ожидала несколько: ожидание остановилось после первого отказа", rep.Polls)
	}
	if rep.Failed < 2 {
		t.Errorf("Failed = %d, ожидала не меньше 2", rep.Failed)
	}
	if !strings.Contains(rep.LastError, "все движки поиска отказали") {
		t.Errorf("LastError = %q", rep.LastError)
	}
	if note := rep.Note(); strings.Contains(note, "не сломана") {
		t.Errorf("Note() утверждает, что охота не сломана: %q", note)
	}
}

func TestWatchByIDRecoversAfterFailure(t *testing.T) {
	// Поиск ожил на следующем опросе: ожидание обязано заметить находку, а не
	// остаться в отказах до самого таймаута.
	st := openStore(t)
	ctx := context.Background()
	calls := 0
	urls := []string{"http://a.onion"}
	r := &Runner{Store: st, Search: func(context.Context, string, string, int) (SearchOutcome, error) {
		calls++
		switch calls {
		case 1:
			// Первый опрос фиксирует базу живым поиском.
			return SearchOutcome{URLs: urls, EnginesTotal: 2}, nil
		case 2:
			return SearchOutcome{EnginesFailed: 2, EnginesTotal: 2}, nil
		default:
			return SearchOutcome{
				URLs:         []string{"http://a.onion", "http://new.onion"},
				EnginesTotal: 2,
			}, nil
		}
	}}
	id, _ := baselineHash(t, r, "leak", urls)

	rep, err := r.WatchDetailed(ctx, id, 2*time.Second, 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Failed < 1 {
		t.Errorf("Failed = %d, ожидала не меньше 1: отказ на втором опросе потерян", rep.Failed)
	}
	if len(rep.Hits) != 1 {
		t.Fatalf("находок %d, ожидала 1 (%+v)", len(rep.Hits), rep)
	}
	if rep.Hits[0].Count != 2 {
		t.Errorf("count = %d, ожидала 2", rep.Hits[0].Count)
	}
}

func TestRunOneWrapsSearchError(t *testing.T) {
	// Метка ErrSearch обязана сохраняться и при настоящей ошибке поиска, иначе
	// ожидание прервётся на мёртвом tor так же, как на неизвестном id.
	st := openStore(t)
	ctx := context.Background()
	r := &Runner{Store: st, Search: failingSearch("tor мёртв")}
	id, err := r.Create(ctx, "leak", "deep", 0)
	if err != nil {
		t.Fatal(err)
	}

	_, err = r.RunOne(ctx, id)
	if !errors.Is(err, ErrSearch) {
		t.Errorf("ошибка поиска не помечена ErrSearch: %v", err)
	}
	if !strings.Contains(err.Error(), "tor мёртв") {
		t.Errorf("причина потеряна в обёртке: %v", err)
	}
}

func TestRunOneNotFoundIsNotSearchError(t *testing.T) {
	// Обратная сторона метки: неизвестный id обязан прерывать ожидание. Если
	// назвать его отказом поиска, watch будет до таймаута опрашивать охоту,
	// которой нет, и рапортовать «отказов N».
	r := &Runner{Store: openStore(t), Search: searchStub(func() []string { return nil })}
	_, err := r.RunOne(context.Background(), 424242)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("ожидала store.ErrNotFound, получила %v", err)
	}
	if errors.Is(err, ErrSearch) {
		t.Error("неизвестный id помечен отказом поиска: ожидание не прервётся")
	}
}

func TestWatchLastErrorHasNoDuplicatePrefix(t *testing.T) {
	// Note() уже говорит «поиск не выполнился ни разу», поэтому причина обязана
	// начинаться с сути. Живой замер показывал строку «поиск не выполнился ни
	// разу: отказов 4: поиск не выполнен: все движки поиска отказали (2 из 2)»:
	// префикс встречался в ней дважды.
	st := openStore(t)
	ctx := context.Background()
	r := &Runner{Store: st, Search: deadEngines(2)}
	id, err := r.Create(ctx, "leak", "deep", 0)
	if err != nil {
		t.Fatal(err)
	}

	// Окно поднято с 200 мс по той же причине, что у соседних watch-тестов:
	// формулировка причины важнее успеваемости за 200 мс. Интервал секунда:
	// круг ожидания включает две записи SQLite (ListHunts и TouchHunt), и при
	// холодном диске под полным пакетом круг занимал больше остатка до
	// дедлайна - код честно оборачивал причину в «поиск оборван: истёк срок
	// ожидания», и HasPrefix чистой причины не находил начало строки.
	// Секундный шаг оставляет кругу почти весь секундный остаток дедлайна -
	// запас больше любого реального IO записи.
	rep, err := r.WatchDetailed(ctx, id, 2*time.Second, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(rep.LastError, "все движки поиска отказали") {
		t.Errorf("LastError = %q, ожидала причину без служебного префикса", rep.LastError)
	}
	if strings.Contains(rep.LastError, ErrSearch.Error()) {
		t.Errorf("LastError дублирует префикс: %q", rep.LastError)
	}
	if strings.Contains(rep.Note(), ErrSearch.Error()+":") {
		t.Errorf("Note() дублирует префикс: %q", rep.Note())
	}
}

func TestSearchErrorKeepsMarkAndReason(t *testing.T) {
	// Метка нужна errors.Is в ожидании, а причина - оператору: оба слоя обязаны
	// переживать обёртку.
	err := &SearchError{Reason: "tor мёртв"}
	if !errors.Is(err, ErrSearch) {
		t.Error("SearchError не распаковывается в ErrSearch")
	}
	if got := err.Error(); got != "поиск не выполнен: tor мёртв" {
		t.Errorf("Error() = %q", got)
	}
	if got := searchReason(err); got != "tor мёртв" {
		t.Errorf("searchReason() = %q, ожидала чистую причину", got)
	}
	// Посторонняя ошибка отдаётся как есть: терять текст нельзя.
	other := errors.New("база не читается")
	if got := searchReason(other); got != "база не читается" {
		t.Errorf("searchReason(посторонняя) = %q", got)
	}
	if errors.Is(other, ErrSearch) {
		t.Error("посторонняя ошибка помечена отказом поиска")
	}
}

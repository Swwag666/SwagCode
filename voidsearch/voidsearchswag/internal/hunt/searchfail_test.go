package hunt

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// failingSearch отказывает на любой запрос: имитация мёртвого tor, недоступного
// движка или истёкшего контекста. Ни одна существующая заглушка в пакете ошибку
// не возвращала, поэтому путь отказа поиска не был проверен вовсе.
func failingSearch(msg string) SearchFunc {
	return func(ctx context.Context, query, mode string, limit int) (SearchOutcome, error) {
		return SearchOutcome{}, errors.New(msg)
	}
}

func TestRunDueCountsSearchFailureSeparately(t *testing.T) {
	// Три охоты, у всех вышло расписание, поиск отказывает каждой.
	//
	// До разделения отказ поиска увеличивал Skipped, Checked не рос, и отчёт
	// выглядел как «проверено 0, пропущено 3» - то есть «ещё не пора». Реально
	// это означало, что все три поиска легли. Причина отказа не сохранялась
	// нигде: ни в отчёте, ни в логе, ни в коде возврата.
	st := openStore(t)
	ctx := context.Background()
	r := &Runner{Store: st, Search: failingSearch("tor мёртв")}
	for _, q := range []string{"leak", "dump", "cards"} {
		if _, err := r.Create(ctx, q, "deep", 60); err != nil {
			t.Fatalf("создание охоты %s: %v", q, err)
		}
	}

	rep, err := r.RunDue(ctx)
	if err != nil {
		t.Fatalf("RunDue: %v", err)
	}
	if rep.Failed != 3 {
		t.Errorf("Failed = %d, ожидала 3: отказ поиска не отделён от пропуска по расписанию (весь отчёт: %+v)", rep.Failed, rep)
	}
	if rep.Skipped != 0 {
		t.Errorf("Skipped = %d, ожидала 0: расписание вышло у всех трёх охот, пропускать нечего", rep.Skipped)
	}
	if rep.Checked != 0 {
		t.Errorf("Checked = %d, ожидала 0: ни один поиск не выполнился", rep.Checked)
	}
	if !strings.Contains(rep.LastError, "tor мёртв") {
		t.Errorf("LastError = %q, ожидала причину отказа", rep.LastError)
	}
	if len(rep.Hits) != 0 {
		t.Errorf("находки при отказавшем поиске: %+v", rep.Hits)
	}
}

func TestRunDueKeepsScheduleSkip(t *testing.T) {
	// Обратная сторона: пропуск по расписанию обязан остаться в Skipped и не
	// должен считаться отказом. Иначе разделение счётчиков превратило бы
	// штатное «ещё не пора» в аварию.
	st := openStore(t)
	ctx := context.Background()
	now := time.Now()
	calls := 0
	r := &Runner{Store: st, Now: func() time.Time { return now },
		Search: searchStub(func() []string {
			calls++
			return []string{"http://a.onion"}
		})}
	if _, err := r.Create(ctx, "leak", "deep", 360); err != nil {
		t.Fatal(err)
	}
	if _, err := r.RunDue(ctx); err != nil {
		t.Fatal(err)
	}
	rep, err := r.RunDue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Skipped != 1 {
		t.Errorf("Skipped = %d, ожидала 1: свежая охота прогнана повторно", rep.Skipped)
	}
	if rep.Failed != 0 {
		t.Errorf("Failed = %d, ожидала 0: поиск не вызывался, отказываться нечему", rep.Failed)
	}
	if calls != 1 {
		t.Errorf("поиск вызван %d раз, ожидала 1", calls)
	}
}

func TestWatchReportsSearchFailuresInsteadOfClaimingEmpty(t *testing.T) {
	// Режим «все охоты» (id <= 0). До правки WatchDetailed брал из ответа
	// RunAll только Checked, а Skipped с отказами выбрасывал. При нулевом
	// Checked Note() выдавал положительное утверждение «поиск не вернул
	// результатов ни в одном опросе: охота не сломана по таймауту, она пуста» -
	// в ситуации, когда поиск не выполнился ни разу. Формулировка прямо уводила
	// от починки: пользователь шёл проверять запрос, а не tor.
	st := openStore(t)
	ctx := context.Background()
	r := &Runner{Store: st, Search: failingSearch("tor мёртв")}
	if _, err := r.Create(ctx, "leak", "deep", 0); err != nil {
		t.Fatal(err)
	}

	// Окно поднято с 200 мс: холодное открытие базы под полным пакетом
	// могло съесть дедлайн раньше первого опроса, и отчёт терял отказы.
	rep, err := r.WatchDetailed(ctx, 0, 2*time.Second, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("WatchDetailed: %v", err)
	}
	if !rep.Timeout {
		t.Error("ожидание должно было закончиться таймаутом: находок при отказавшем поиске нет")
	}
	if rep.Checked != 0 {
		t.Errorf("Checked = %d, ожидала 0", rep.Checked)
	}
	if rep.Failed < 1 {
		t.Errorf("Failed = %d, ожидала не меньше 1: отказы поиска потеряны в режиме «все охоты»", rep.Failed)
	}
	if !strings.Contains(rep.LastError, "tor мёртв") {
		t.Errorf("LastError = %q, ожидала причину отказа", rep.LastError)
	}
	note := rep.Note()
	if strings.Contains(note, "не сломана") {
		t.Errorf("Note() утверждает, что охота не сломана, хотя поиск не выполнился ни разу: %q", note)
	}
	if !strings.Contains(note, "отказ") {
		t.Errorf("Note() не говорит об отказах поиска: %q", note)
	}
}

func TestWatchStillReportsEmptyHunt(t *testing.T) {
	// Семантика пустой выдачи обязана сохраниться: поиск работает, результатов
	// действительно нет, и Note() по-прежнему объясняет, что охота пуста, а не
	// сломана. Без этой проверки правка могла бы заменить одну крайность
	// другой и начать пугать отказами там, где их нет.
	st := openStore(t)
	ctx := context.Background()
	r := &Runner{Store: st, Search: searchStub(func() []string { return nil })}
	id, err := r.Create(ctx, "leak", "deep", 0)
	if err != nil {
		t.Fatal(err)
	}

	// Окно поднято с 150 мс по той же причине: смысл теста - семантика
	// пустой выдачи, а не успеваемость.
	rep, err := r.WatchDetailed(ctx, id, 2*time.Second, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("WatchDetailed: %v", err)
	}
	if rep.Failed != 0 {
		t.Errorf("Failed = %d, ожидала 0: поиск отвечал успешно", rep.Failed)
	}
	if rep.LastCount != 0 {
		t.Errorf("LastCount = %d, ожидала 0", rep.LastCount)
	}
	if note := rep.Note(); !strings.Contains(note, "пуста") {
		t.Errorf("Note() = %q, ожидала объяснение про пустую охоту", note)
	}
}

func TestRunOneStillReturnsSearchError(t *testing.T) {
	// Режим конкретной охоты ошибку поиска и до правки отдавал вызывающему,
	// поэтому CLI завершался ненулевым кодом. Асимметрия с режимом «все охоты»
	// и была дефектом; здесь фиксируется, что правильный путь не сломан.
	st := openStore(t)
	ctx := context.Background()
	r := &Runner{Store: st, Search: failingSearch("tor мёртв")}
	id, err := r.Create(ctx, "leak", "deep", 0)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := r.RunOne(ctx, id); err == nil {
		t.Error("RunOne вернул nil при отказавшем поиске")
	} else if !strings.Contains(err.Error(), "tor мёртв") {
		t.Errorf("RunOne вернул %v, ожидала причину отказа", err)
	}
}

package discover

import (
	"context"
	"testing"

	"voidsearchswag/internal/store"
)

// Этап 174: поадресная запись - фолбэк батча, и она обязана жить своей
// жизнью, а не быть мёртвым довеском: счётчики совпадают с прежним
// циклом Run дословно, отказы считаются по одному адресу, мёртвая база
// не оставляет нулей-тишину.

func TestSavePoolOneByOneCountsLikeBatch(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.UpsertOnion(ctx, store.Onion{URL: cHost, Status: "unknown", Title: "Старый титул", Category: "market"}); err != nil {
		t.Fatalf("подготовка: %v", err)
	}

	p := &Pool{Store: st, Log: silentLog{}}
	res := &Result{}
	p.savePoolOneByOne(ctx, []Candidate{
		{Address: cHost, Title: "Новый титул", Category: "market"},
		{Address: v3a[:56] + ".onion"},
	}, res)

	if res.New != 1 || res.Updated != 1 {
		t.Fatalf("new=%d updated=%d, хочу 1/1: счётчики фолбэка разошлись с прежним циклом записи", res.New, res.Updated)
	}
	if res.SaveFailed != 0 {
		t.Fatalf("save_failed=%d на живой базе: фолбэк паникует без причины", res.SaveFailed)
	}
	got, err := st.GetOnion(ctx, cHost)
	if err != nil {
		t.Fatalf("GetOnion: %v", err)
	}
	if got.Title != "Новый титул" {
		t.Errorf("мета не доехала фолбэком: title=%q", got.Title)
	}
}

func TestSavePoolOneByOneCountsRefusesOnDeadStore(t *testing.T) {
	st := newStore(t)
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	p := &Pool{Store: st, Log: silentLog{}}
	res := &Result{}
	p.savePoolOneByOne(context.Background(), []Candidate{{Address: cHost}}, res)

	if res.New != 0 || res.Updated != 0 {
		t.Fatalf("new=%d updated=%d на мёртвой базе: запись обязана не состояться", res.New, res.Updated)
	}
	if res.SaveFailed == 0 {
		t.Fatal("save_failed=0 на мёртвой базе: отказ обязан быть виден, а не тих")
	}
	if res.LastError == "" {
		t.Error("last_error пуст: отказ без причины нечитаем")
	}
}

// TestRunBatchFailureFallsBackPerAddress: отказ батча обязан включать
// поадресный фолбэк, и его гранулярность видна без crawl-хвоста (recordCrawl
// молчит - его save_failed не маскирует пропавший фолбэк). Мёртвый
// saveBudget: батч падает синхронно (отрицательный таймаут), и без
// фолбэка save_failed остался бы нулём - отказ пула пропал бы молча.
func TestRunBatchFailureFallsBackPerAddress(t *testing.T) {
	st := newStore(t)
	p := &Pool{
		Store:      st,
		Finder:     &Finder{Client: &brokenWriteSource{n: 1}, Log: silentLog{}, Attempts: 1},
		Log:        silentLog{},
		saveBudget: -1,
	}

	res, err := p.Run(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.New != 0 || res.Updated != 0 {
		t.Fatalf("new=%d updated=%d при мёртвом бюджете: запись обязана не состояться", res.New, res.Updated)
	}
	if res.Found == 0 {
		t.Fatal("источники не дали адресов: сценарий не воспроизведён")
	}
	if res.SaveFailed != res.Found {
		t.Fatalf("save_failed=%d при found=%d: фолбэк обязан посчитать отказ по каждому адресу, а не исчезнуть вместе с батчем", res.SaveFailed, res.Found)
	}
	if res.LastError == "" {
		t.Error("last_error пуст: отказ батча без причины нечитаем")
	}
}

package store

import (
	"context"
	"fmt"
	"testing"
)

// Тесты батч-записи пула (этап 174). Контракты, которые обязаны совпасть с
// поадресным путём дословно: new - адресов, которых не было; updated -
// существующих с отличающейся метой; пустые поля мета-вызова не затирают
// того, что уже лежит; при ошибке - нули и откат, никаких частичных
// записей. Плюс собственные свойства батча: чанки WHERE url IN по 400 при
// списке больше лимита переменных SQLite и один коммит на весь список.

func TestSyncOnionsCountsAndMeta(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	// Существующий адрес без меты - как после прошлых прогонов, когда
	// discover завёл его голым адресом.
	if err := st.UpsertOnion(ctx, Onion{URL: "aaaaaaaaaaaaaaaa.onion", Status: "unknown"}); err != nil {
		t.Fatalf("подготовка существующего: %v", err)
	}

	n, u, err := st.SyncOnions(ctx, []OnionSync{
		{URL: "aaaaaaaaaaaaaaaa.onion", Title: "Старый сайт", Category: "market"},
		{URL: "bbbbbbbbbbbbbbbb.onion", Title: "Новый сайт", Category: "forum", Description: "свежая находка"},
		{URL: "cccccccccccccccc.onion"},
	})
	if err != nil {
		t.Fatalf("SyncOnions: %v", err)
	}
	if n != 2 || u != 1 {
		t.Fatalf("new=%d updated=%d, ожидаю 2/1: счётчики разошлись с поадресным путём", n, u)
	}

	old, err := st.GetOnion(ctx, "aaaaaaaaaaaaaaaa.onion")
	if err != nil {
		t.Fatalf("GetOnion существующего: %v", err)
	}
	if old.Title != "Старый сайт" || old.Category != "market" {
		t.Errorf("мета существующего не записана батчем: title=%q category=%q", old.Title, old.Category)
	}
	fresh, err := st.GetOnion(ctx, "bbbbbbbbbbbbbbbb.onion")
	if err != nil {
		t.Fatalf("GetOnion нового: %v", err)
	}
	if fresh.Status != "unknown" || fresh.Title != "Новый сайт" || fresh.Description != "свежая находка" {
		t.Errorf("новый адрес записан криво: %+v", fresh)
	}
	if _, err := st.GetOnion(ctx, "cccccccccccccccc.onion"); err != nil {
		t.Errorf("адрес без меты не записан: %v", err)
	}
}

func TestSyncOnionsIdempotentOnRerun(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	items := []OnionSync{
		{URL: "aaaaaaaaaaaaaaaa.onion", Title: "Титул", Category: "forum"},
		{URL: "bbbbbbbbbbbbbbbb.onion"},
	}
	if _, _, err := st.SyncOnions(ctx, items); err != nil {
		t.Fatalf("первый SyncOnions: %v", err)
	}
	n, u, err := st.SyncOnions(ctx, items)
	if err != nil {
		t.Fatalf("повторный SyncOnions: %v", err)
	}
	if n != 0 || u != 0 {
		t.Fatalf("повтор: new=%d updated=%d, ожидаю 0/0: повторный прогон не должен считать своё же запись новой", n, u)
	}
}

func TestSyncOnionsEmptyInput(t *testing.T) {
	st := newStore(t)
	n, u, err := st.SyncOnions(context.Background(), nil)
	if err != nil || n != 0 || u != 0 {
		t.Fatalf("пустой вход: new=%d updated=%d err=%v - пустой список не бывает ошибкой", n, u, err)
	}
}

func TestSyncOnionsDeadContextWritesNothing(t *testing.T) {
	st := newStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	n, u, err := st.SyncOnions(ctx, []OnionSync{{URL: "aaaaaaaaaaaaaaaa.onion", Title: "Титул"}})
	if err == nil {
		t.Fatal("мёртвый контекст не вернул ошибку")
	}
	if n != 0 || u != 0 {
		t.Fatalf("мёртвый контекст: new=%d updated=%d - счётчики обязаны быть нулями при отказе", n, u)
	}
	if _, err := st.GetOnion(context.Background(), "aaaaaaaaaaaaaaaa.onion"); err == nil {
		t.Fatal("мёртвый контекст что-то записал: откат не сработал")
	}
}

func TestSyncOnionsChunksBeyondSQLiteVariableLimit(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	// 401 адрес: первый чанк WHERE IN - 400, второй - 1. Предел
	// переменных SQLite - 999, и без чанков список упирается в него уже
	// на тысяче; границу 400/401 проверяем дёшево, смысл тот же.
	const total = 401
	items := make([]OnionSync, total)
	for i := range items {
		items[i] = OnionSync{URL: fmt.Sprintf("%056d.onion", i)}
	}
	n, u, err := st.SyncOnions(ctx, items)
	if err != nil {
		t.Fatalf("SyncOnions на %d адресах: %v", total, err)
	}
	if n != total || u != 0 {
		t.Fatalf("new=%d updated=%d, ожидаю %d/0: чанкование потеряло адреса", n, u, total)
	}
}

// TestSyncOnionsSnapshotCoversTail: хвост списка обязан попадать в снимок
// известности - чанк-цикл обязан дойти до конца, а не остановиться после
// первого куска. Потерянный хвост не роняет запись (INSERT с ON CONFLICT
// переиграет), но врёт счётчиками: известный адрес становится «новым».
func TestSyncOnionsSnapshotCoversTail(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	const total = 401
	tail := fmt.Sprintf("%056d.onion", total-1)
	if err := st.UpsertOnion(ctx, Onion{URL: tail, Status: "unknown", Title: "Хвост", Category: "market"}); err != nil {
		t.Fatalf("подготовка хвостового: %v", err)
	}
	items := make([]OnionSync, total)
	for i := range items {
		items[i] = OnionSync{URL: fmt.Sprintf("%056d.onion", i)}
	}
	// Последний элемент - известный адрес с изменившейся метой: он обязан
	// доехать до снимка вторым чанком и посчитаться обновлением, а не
	// новой вставкой.
	items[total-1] = OnionSync{URL: tail, Title: "Хвост новый", Category: "market"}

	n, u, err := st.SyncOnions(ctx, items)
	if err != nil {
		t.Fatalf("SyncOnions: %v", err)
	}
	if n != total-1 || u != 1 {
		t.Fatalf("new=%d updated=%d, ожидаю %d/1: снимок известности не покрыл хвост списка", n, u, total-1)
	}
}

func TestSyncOnionsKeepsExistingMetaOnEmptyFields(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.UpsertOnion(ctx, Onion{URL: "aaaaaaaaaaaaaaaa.onion", Status: "unknown", Title: "Старый титул", Category: "market"}); err != nil {
		t.Fatalf("подготовка: %v", err)
	}

	// Прогон собрал адрес без меты: пустые поля не имеют права затирать
	// то, что уже лежит, - это же правило у SetOnionMeta поадресного
	// пути, и расхождение означало бы, что батч вычищает пул.
	if _, _, err := st.SyncOnions(ctx, []OnionSync{{URL: "aaaaaaaaaaaaaaaa.onion"}}); err != nil {
		t.Fatalf("SyncOnions с пустой метой: %v", err)
	}
	got, err := st.GetOnion(ctx, "aaaaaaaaaaaaaaaa.onion")
	if err != nil {
		t.Fatalf("GetOnion: %v", err)
	}
	if got.Title != "Старый титул" || got.Category != "market" {
		t.Fatalf("пустая мета затёрла существующую: title=%q category=%q", got.Title, got.Category)
	}
}

func TestSyncOnionsFailureRollsBackEverything(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	// Убиваем базу посреди батча: закрытое соединение валит транзакцию,
	// и после отказа пул обязан остаться пустым - ни новых, ни меты.
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	n, u, err := st.SyncOnions(ctx, []OnionSync{{URL: "aaaaaaaaaaaaaaaa.onion", Title: "Титул"}})
	if err == nil {
		t.Fatal("закрытая база не отказала батчу")
	}
	if n != 0 || u != 0 {
		t.Fatalf("отказ батча: new=%d updated=%d - счётчики обязаны быть нулями", n, u)
	}
}

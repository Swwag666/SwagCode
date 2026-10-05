package searchers

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestOnionCatalogConcurrentAttachAndSnapshot проверяет, что одновременное
// добавление движков и чтение каталога не дают гонки.
//
// До исправления promote.Attach делал append в Engines без мьютекса, а
// liveEngines, обработчики MCP и фоновый тик обходили тот же срез из своих
// горутин. Итогом были потерянные движки или порванный заголовок среза.
// Тест рассчитан на запуск под -race.
func TestOnionCatalogConcurrentAttachAndSnapshot(t *testing.T) {
	cat := &OnionCatalog{}
	const writers = 8
	const perWriter = 25
	const readers = 8

	var wg sync.WaitGroup
	wg.Add(writers + readers)

	for w := 0; w < writers; w++ {
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				eng := &OnionEngine{Name_: fmt.Sprintf("eng-%d-%d", w, i), Base: fmt.Sprintf("http://e%d%d.onion", w, i)}
				if !cat.AttachEngine(eng) {
					t.Errorf("движок %s не добавился", eng.Name())
				}
			}
		}(w)
	}

	stop := make(chan struct{})
	for r := 0; r < readers; r++ {
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				// Обход копии обязан быть безопасным даже пока писатели
				// добавляют движки.
				for _, e := range cat.Snapshot() {
					_ = e.Name()
				}
			}
		}()
	}

	// Писатели завершаются, затем останавливаем читателей.
	go func() {
		time.Sleep(150 * time.Millisecond)
		close(stop)
	}()
	wg.Wait()

	want := writers * perWriter
	if got := len(cat.Snapshot()); got != want {
		t.Errorf("в каталоге %d движков, ожидала %d: часть потерялась", got, want)
	}
}

// TestOnionCatalogAttachDedupesConcurrently проверяет, что проверка дубликата
// и добавление атомарны.
//
// Прежняя версия в promote.Attach делала их раздельно: два одновременных
// вызова могли оба пройти проверку по имени и оба добавить один и тот же
// движок. Здесь десять горутин добавляют одно и то же имя - принять его
// должна ровно одна.
func TestOnionCatalogAttachDedupesConcurrently(t *testing.T) {
	cat := &OnionCatalog{}
	const racers = 10
	var accepted int64
	var mu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(racers)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		go func() {
			defer wg.Done()
			<-start
			if cat.AttachEngine(&OnionEngine{Name_: "dup", Base: "http://dup.onion"}) {
				mu.Lock()
				accepted++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	if accepted != 1 {
		t.Errorf("дубликат принят %d раз, ожидала ровно 1", accepted)
	}
	if got := len(cat.Snapshot()); got != 1 {
		t.Errorf("в каталоге %d движков, ожидала 1", got)
	}
}

// TestSearchAllDoesNotDeadlockOnConcurrentAttach закрывает конкретный сценарий
// зависания: SearchAll вычислял вместимость канала по len(c.Engines), а затем
// ещё раз обходил тот же срез. Если между двумя чтениями список вырастал, цикл
// приёма ждал больше элементов, чем было отправлено, и блокировался навсегда.
func TestSearchAllDoesNotDeadlockOnConcurrentAttach(t *testing.T) {
	cat := &OnionCatalog{}
	cat.AttachEngine(&OnionEngine{Name_: "seed", Base: "http://seed.onion"})

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Клиент не задан, движки вернут ошибку, но SearchAll обязан
		// отработать свой цикл приёма до конца и вернуться.
		_ = cat.SearchAll(context.Background(), "запрос", 5)
	}()

	// Одновременно добавляем движки: при прежнем коде это меняло len(c.Engines)
	// между двумя чтениями внутри SearchAll.
	for i := 0; i < 5; i++ {
		cat.AttachEngine(&OnionEngine{Name_: fmt.Sprintf("late-%d", i), Base: fmt.Sprintf("http://late%d.onion", i)})
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("SearchAll заблокировался на приёме из канала")
	}
}

func TestOnionCatalogSnapshotIsCopy(t *testing.T) {
	cat := &OnionCatalog{}
	cat.AttachEngine(&OnionEngine{Name_: "a", Base: "http://a.onion"})
	snap := cat.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("снимок: %d движков", len(snap))
	}
	// Изменение снимка не должно влиять на каталог.
	snap[0] = &OnionEngine{Name_: "подменён"}
	cat.AttachEngine(&OnionEngine{Name_: "b", Base: "http://b.onion"})

	after := cat.Snapshot()
	if len(after) != 2 {
		t.Fatalf("после добавления %d движков, ожидала 2", len(after))
	}
	if after[0].Name() != "a" {
		t.Errorf("первый движок подменён через снимок: %s", after[0].Name())
	}
}

func TestOnionCatalogNilSafety(t *testing.T) {
	var cat *OnionCatalog
	if got := cat.Snapshot(); got != nil {
		t.Errorf("Snapshot на nil вернул %v", got)
	}
	if cat.AttachEngine(&OnionEngine{Name_: "x"}) {
		t.Error("AttachEngine на nil сообщил об успехе")
	}
	// Пустой каталог тоже не должен падать.
	empty := &OnionCatalog{}
	if got := empty.Snapshot(); got != nil {
		t.Errorf("Snapshot пустого каталога вернул %v", got)
	}
	if got := empty.SearchAll(context.Background(), "q", 5); got != nil {
		t.Errorf("SearchAll на пустом каталоге вернул %v", got)
	}
}

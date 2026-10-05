package main

import (
	"errors"
	"sync"
	"testing"
)

func TestAtExitRunsOnShutdown(t *testing.T) {
	ResetShutdownForTest()
	defer ResetShutdownForTest()

	var ran bool
	AtExit("тест", func() error {
		ran = true
		return nil
	})

	if PendingShutdown() != 1 {
		t.Fatalf("в стеке %d хуков, ожидала 1", PendingShutdown())
	}
	RunShutdown()
	if !ran {
		t.Error("хук не выполнен при аварийном выходе")
	}
	if PendingShutdown() != 0 {
		t.Errorf("после опустошения в стеке %d хуков", PendingShutdown())
	}
}

func TestAtExitDeferRunsCleanupExactlyOnce(t *testing.T) {
	// Главный смысл реестра: при нормальном завершении defer ВЫПОЛНЯЕТ очистку и
	// снимает регистрацию, а RunShutdown не запускает её второй раз.
	//
	// Первая версия возвращала только снятие регистрации, и
	// `defer AtExit("база", st.Close)()` оказывался ловушкой: st.Close не
	// вызывался вовсе, база оставалась открытой. На тестах это выглядело как
	// «TempDir RemoveAll cleanup: файл используется другим процессом».
	ResetShutdownForTest()
	defer ResetShutdownForTest()

	calls := 0
	cleanup := func() error {
		calls++
		return nil
	}

	func() {
		defer AtExit("ресурс", cleanup)()
	}()

	if calls != 1 {
		t.Errorf("после defer очистка выполнена %d раз, ожидала 1", calls)
	}
	if PendingShutdown() != 0 {
		t.Errorf("после defer в стеке осталось %d хуков", PendingShutdown())
	}
	RunShutdown()
	if calls != 1 {
		t.Errorf("RunShutdown запустил очистку повторно: всего %d вызовов", calls)
	}
}

func TestAtExitRunsOnFatalPath(t *testing.T) {
	// Аварийный путь: defer не сработал, запись осталась в стеке, и RunShutdown
	// обязан её выполнить. Это ровно тот сценарий, ради которого реестр
	// существует - os.Exit пропускает defer.
	ResetShutdownForTest()
	defer ResetShutdownForTest()

	calls := 0
	AtExit("ресурс", func() error { calls++; return nil })

	RunShutdown()
	if calls != 1 {
		t.Errorf("на аварийном пути выполнено %d раз, ожидала 1", calls)
	}
}

func TestAtExitLIFOOrder(t *testing.T) {
	// Порядок LIFO, как у defer: tor закрывается раньше базы, потому что
	// фоновая запись здоровья движков обращается к базе. Прямой порядок
	// закрыл бы базу первой, и запись здоровья упала бы на закрытом соединении.
	ResetShutdownForTest()
	defer ResetShutdownForTest()

	var order []string
	AtExit("первый", func() error { order = append(order, "первый"); return nil })
	AtExit("второй", func() error { order = append(order, "второй"); return nil })
	AtExit("третий", func() error { order = append(order, "третий"); return nil })

	RunShutdown()

	want := []string{"третий", "второй", "первый"}
	if len(order) != len(want) {
		t.Fatalf("выполнено %d хуков, ожидала %d", len(order), len(want))
	}
	for i := range want {
		if order[i] != want[i] {
			t.Errorf("позиция %d: %q, ожидала %q", i, order[i], want[i])
		}
	}
}

func TestRunShutdownIdempotent(t *testing.T) {
	// Аварийный выход мог случиться после того, как очистка уже отработала.
	// Повторное закрытие tor или базы вернуло бы ошибку, которая напугала бы
	// пользователя на ровном месте.
	ResetShutdownForTest()
	defer ResetShutdownForTest()

	calls := 0
	AtExit("ресурс", func() error { calls++; return nil })

	RunShutdown()
	RunShutdown()
	RunShutdown()

	if calls != 1 {
		t.Errorf("хук выполнен %d раз, ожидала 1", calls)
	}
}

func TestAtExitErrorDoesNotStopOthers(t *testing.T) {
	// Сиротский tor-процесс и несброшенная база хуже, чем незакрытый один из
	// нескольких ресурсов, поэтому цикл идёт до конца.
	ResetShutdownForTest()
	defer ResetShutdownForTest()

	var ran []string
	AtExit("первый", func() error { ran = append(ran, "первый"); return nil })
	AtExit("падающий", func() error { return errors.New("не закрылось") })
	AtExit("третий", func() error { ran = append(ran, "третий"); return nil })

	RunShutdown()

	// LIFO: третий, падающий, первый. Оба остальных обязаны выполниться.
	if len(ran) != 2 {
		t.Fatalf("выполнено %d хуков (%v), ожидала 2", len(ran), ran)
	}
	if ran[0] != "третий" || ran[1] != "первый" {
		t.Errorf("порядок неверен: %v", ran)
	}
}

func TestAtExitDeregisterIdempotent(t *testing.T) {
	// Возвращаемая функция может сработать дважды: defer в нескольких местах или
	// повторный вызов. Второй вызов обязан быть пустой операцией, а не паникой,
	// не двойным закрытием и не удалением чужого хука.
	ResetShutdownForTest()
	defer ResetShutdownForTest()

	calls := 0
	off := AtExit("ресурс", func() error { calls++; return nil })
	off()
	off()
	off()

	if calls != 1 {
		t.Errorf("очистка выполнена %d раз, ожидала 1", calls)
	}
	if PendingShutdown() != 0 {
		t.Errorf("в стеке %d хуков", PendingShutdown())
	}
	RunShutdown()
	if calls != 1 {
		t.Errorf("после RunShutdown выполнено %d раз, ожидала 1", calls)
	}
}

func TestAtExitNilFunctionIgnored(t *testing.T) {
	// Регистрация заведомо пустого хука означала бы лишнюю запись в стеке и
	// панику при опустошении.
	ResetShutdownForTest()
	defer ResetShutdownForTest()

	off := AtExit("пустой", nil)
	if off == nil {
		t.Fatal("AtExit вернул nil вместо пустой функции")
	}
	off() // не должна паниковать
	if PendingShutdown() != 0 {
		t.Errorf("nil-хук попал в стек: %d", PendingShutdown())
	}
	RunShutdown()
}

func TestAtExitSameResourceTwiceIndependent(t *testing.T) {
	// Один и тот же ресурс, зарегистрированный дважды (например, две базы в
	// разных командах), обязан закрыться дважды: снятие по указателю на запись, а
	// не по имени или по функции, иначе закрытие одной базы сняло бы регистрацию
	// другой и вторая осталась бы открытой.
	ResetShutdownForTest()
	defer ResetShutdownForTest()

	calls := 0
	fn := func() error { calls++; return nil }

	off1 := AtExit("база", fn)
	AtExit("база", fn)

	off1()
	if calls != 1 {
		t.Errorf("первый хук выполнен %d раз, ожидала 1", calls)
	}
	if PendingShutdown() != 1 {
		t.Errorf("в стеке %d хуков, ожидала 1 (второй не должен был сняться)", PendingShutdown())
	}

	RunShutdown()
	if calls != 2 {
		t.Errorf("всего выполнено %d раз, ожидала 2", calls)
	}
}

func TestAsErrorAdapts(t *testing.T) {
	// cleanup поискового ядра возвращает func(), а реестр хранит func() error.
	var ran bool
	adapted := AsError(func() { ran = true })
	if err := adapted(); err != nil {
		t.Errorf("адаптер вернул ошибку: %v", err)
	}
	if !ran {
		t.Error("обёрнутая функция не выполнена")
	}
}

func TestAsErrorNil(t *testing.T) {
	if got := AsError(nil); got != nil {
		t.Error("AsError(nil) вернул не-nil вместо nil")
	}
}

func TestAtExitConcurrentRegistration(t *testing.T) {
	// Команды регистрируют хуки из одной горутины, но фоновая запись здоровья
	// движков работает параллельно, и обработчик сигналов живёт в своей
	// горутине. Стек обязан выдерживать конкурентную регистрацию без гонки.
	ResetShutdownForTest()
	defer ResetShutdownForTest()

	const n = 50
	var wg sync.WaitGroup
	var mu sync.Mutex
	executed := 0

	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			AtExit("конкурентный", func() error {
				mu.Lock()
				executed++
				mu.Unlock()
				return nil
			})
		}()
	}
	wg.Wait()

	if got := PendingShutdown(); got != n {
		t.Errorf("в стеке %d хуков, ожидала %d", got, n)
	}

	RunShutdown()

	mu.Lock()
	defer mu.Unlock()
	if executed != n {
		t.Errorf("выполнено %d хуков, ожидала %d", executed, n)
	}
}

func TestAtExitConcurrentDeregistration(t *testing.T) {
	// Регистрация и снятие одновременно: стек перестраивается, и индексная
	// арифметика съехала бы. Идентичность по указателю держит это корректным.
	ResetShutdownForTest()
	defer ResetShutdownForTest()

	const n = 50
	var wg sync.WaitGroup
	offs := make([]func(), n)

	for i := 0; i < n; i++ {
		offs[i] = AtExit("ресурс", func() error { return nil })
	}

	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			offs[i]()
		}(i)
	}
	wg.Wait()

	if got := PendingShutdown(); got != 0 {
		t.Errorf("в стеке осталось %d хуков, ожидала 0", got)
	}
	RunShutdown()
}

func TestRunShutdownOnEmptyStack(t *testing.T) {
	// Нормальное завершение команды, которая ресурсов не открывала (version,
	// help), тоже вызывает RunShutdown. Пустой стек не должен ни паниковать, ни
	// мешать последующим вызовам.
	ResetShutdownForTest()
	defer ResetShutdownForTest()

	RunShutdown()
	if PendingShutdown() != 0 {
		t.Errorf("в стеке %d хуков", PendingShutdown())
	}

	// После опустошения регистрация всё ещё работает (Once не блокирует стек).
	var ran bool
	AtExit("поздний", func() error { ran = true; return nil })
	if PendingShutdown() != 1 {
		t.Errorf("поздняя регистрация не попала в стек: %d", PendingShutdown())
	}
	// Но повторный RunShutdown уже не сработает: Once израсходован. Это
	// намеренно - аварийный выход происходит один раз.
	RunShutdown()
	if ran {
		t.Error("RunShutdown сработал дважды")
	}
}

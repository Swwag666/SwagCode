package netx

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// failingConfig возвращает конфигурацию, при которой StartTor падает быстро и
// без запуска процесса tor.
//
// Tor-адрес указывает на закрытый порт: attachTor пытается подключиться,
// получает отказ соединения и возвращает ошибку. TorNoReuse отключает поиск уже
// запущенного демона в файле эндпоинта, иначе тест зависел бы от состояния
// машины.
func failingConfig() Config {
	return Config{
		TorSocksAddr:   "127.0.0.1:1",
		TorNoReuse:     true,
		RequestTimeout: 300 * time.Millisecond,
	}
}

func TestLazyTorDoesNotStartUntilUsed(t *testing.T) {
	// Главный смысл обёртки: быстрый режим не должен платить bootstrap.
	// Создание ротатора обязано быть мгновенным и не трогать tor.
	l := LazyTor(failingConfig()).(*lazyRotator)
	if l.Started() {
		t.Error("tor запущен при создании ротатора")
	}
	if l.inner != nil {
		t.Error("внутренний ротатор создан заранее")
	}
}

func TestLazyTorStartsOnTransportSpec(t *testing.T) {
	// TransportSpec вызывается при создании сессии tor-клиента, поэтому именно
	// он обязан быть триггером запуска.
	l := LazyTor(failingConfig()).(*lazyRotator)
	spec := l.TransportSpec()
	if !l.Started() {
		t.Error("TransportSpec не запустил tor")
	}
	// Запуск не удался, поэтому транспорт пустой: onion-запросы уйдут в
	// clearnet и упадут, поиск деградирует. Это прежнее поведение при
	// неудачном StartTor, сохранённое намеренно.
	if spec != "" {
		t.Errorf("транспорт при неудачном запуске = %q, ожидала пустой", spec)
	}
}

func TestLazyTorStartsOnKindAndHealthyAndRotate(t *testing.T) {
	// Каждый метод, которому нужен транспорт, обязан запускать tor.
	t.Run("Kind", func(t *testing.T) {
		l := LazyTor(failingConfig()).(*lazyRotator)
		if got := l.Kind(); got != "" {
			t.Errorf("Kind при неудачном запуске = %q", got)
		}
		if !l.Started() {
			t.Error("Kind не запустил tor")
		}
	})
	t.Run("Healthy", func(t *testing.T) {
		l := LazyTor(failingConfig()).(*lazyRotator)
		if l.Healthy() {
			t.Error("Healthy вернул true при неподнятом tor")
		}
		if !l.Started() {
			t.Error("Healthy не запустил tor")
		}
	})
	t.Run("Rotate", func(t *testing.T) {
		l := LazyTor(failingConfig()).(*lazyRotator)
		if err := l.Rotate(context.Background()); err == nil {
			t.Error("Rotate не вернул ошибку при неудачном запуске")
		}
		if !l.Started() {
			t.Error("Rotate не запустил tor")
		}
	})
	t.Run("ControlStatus", func(t *testing.T) {
		l := LazyTor(failingConfig()).(*lazyRotator)
		got := l.ControlStatus()
		if !l.Started() {
			t.Error("ControlStatus не запустил tor")
		}
		if got == "" {
			t.Error("ControlStatus вернул пустую строку")
		}
	})
}

func TestLazyTorMemoizesFailure(t *testing.T) {
	// StartTor стоит 20-25 секунд. Повторная попытка на каждый вызов
	// превратила бы быстрый поиск в медленный, поэтому ошибка запоминается.
	l := LazyTor(failingConfig()).(*lazyRotator)

	start := time.Now()
	for i := 0; i < 5; i++ {
		l.TransportSpec()
		l.Kind()
		_ = l.Healthy()
		_ = l.Rotate(context.Background())
	}
	elapsed := time.Since(start)

	if !l.Started() {
		t.Fatal("tor не был запущен ни разу")
	}
	if l.err == nil {
		t.Fatal("ошибка запуска не запомнена")
	}
	// Пятнадцать обращений к уже запомненной ошибке должны занимать
	// миллисекунды, а не минуты.
	if elapsed > 5*time.Second {
		t.Errorf("%d обращений заняли %v: ошибка не мемоизируется", 15, elapsed)
	}
}

func TestLazyTorCloseDoesNotStart(t *testing.T) {
	// Close обязан быть безопасным до первого использования: закрывать нечего,
	// а поднимать процесс ради немедленного закрытия бессмысленно.
	l := LazyTor(failingConfig()).(*lazyRotator)
	if err := l.Close(); err != nil {
		t.Errorf("Close до запуска вернул ошибку: %v", err)
	}
	if l.Started() {
		t.Error("Close запустил tor")
	}
}

func TestLazyTorCloseAfterFailureIsSafe(t *testing.T) {
	// После неудачного запуска закрывать тоже нечего: внутренний ротатор nil.
	l := LazyTor(failingConfig()).(*lazyRotator)
	l.TransportSpec()
	if err := l.Close(); err != nil {
		t.Errorf("Close после неудачи вернул ошибку: %v", err)
	}
	// Повторный Close не должен паниковать: движок закрывается из defer и из
	// явного вызова.
	if err := l.Close(); err != nil {
		t.Errorf("второй Close вернул ошибку: %v", err)
	}
}

func TestLazyTorControlAddrWithoutStart(t *testing.T) {
	// Адрес control-канала до запуска неизвестен. Пустая строка - правильный
	// ответ, и torStatusLine в CLI трактует её как «control не задан».
	l := LazyTor(failingConfig()).(*lazyRotator)
	if got := l.ControlAddr(); got != "" {
		t.Errorf("ControlAddr до запуска = %q", got)
	}
	if l.Started() {
		t.Error("ControlAddr запустил tor, хотя он только читает состояние")
	}
}

func TestLazyTorControlAddrAfterFailure(t *testing.T) {
	l := LazyTor(failingConfig()).(*lazyRotator)
	l.TransportSpec()
	if got := l.ControlAddr(); got != "" {
		t.Errorf("ControlAddr после неудачного запуска = %q", got)
	}
}

func TestLazyTorConcurrentStartOnce(t *testing.T) {
	// Ядро строится один раз, но сессии создаются из разных goroutine
	// параллельных поисков. Гонка не должна привести к нескольким запускам tor:
	// два демона боролись бы за tor-data/lock.
	l := LazyTor(failingConfig()).(*lazyRotator)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l.TransportSpec()
			l.Kind()
			_ = l.Healthy()
		}()
	}
	wg.Wait()

	if !l.Started() {
		t.Fatal("tor не запущен ни разу")
	}
	l.mu.Lock()
	tried := l.tried
	l.mu.Unlock()
	if !tried {
		t.Error("флаг запуска сброшен")
	}
}

func TestLazyTorSatisfiesRotatorInterface(t *testing.T) {
	// Обёртка обязана быть полноценным ротатором, иначе её нельзя передать в
	// search.Build и httpc.NewClient.
	var r Rotator = LazyTor(failingConfig())
	if r == nil {
		t.Fatal("LazyTor вернул nil")
	}
}

func TestLazyTorControlStatusImplementsOptionalInterface(t *testing.T) {
	// torStatusLine в CLI ищет метод ControlStatus через опциональный
	// интерфейс. Без него ленивый ротатор показал бы только «control не задан»,
	// хотя способен дать проверенный статус.
	r := LazyTor(failingConfig())
	if _, ok := r.(interface{ ControlStatus() string }); !ok {
		t.Error("ленивый ротатор не реализует ControlStatus")
	}
}

func TestLazyTorControlStatusExplainsFailure(t *testing.T) {
	// Статус обязан объяснять, что tor не поднят, а не молчать: иначе
	// onioncheck показал бы пустое место вместо причины.
	l := LazyTor(failingConfig()).(*lazyRotator)
	got := l.ControlStatus()
	if got == "" {
		t.Fatal("пустой статус")
	}
	if !strings.Contains(got, "не поднят") {
		t.Errorf("статус не объясняет неудачу: %q", got)
	}
}

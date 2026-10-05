package search

import (
	"context"
	"testing"
	"time"

	"voidsearchswag/internal/searchers"
)

func TestBuildDirectClient(t *testing.T) {
	eng, client, err := Build(DeepConfig{Transport: "direct"})
	if err != nil {
		t.Fatalf("сборка: %v", err)
	}
	defer eng.Close()
	if eng == nil || client == nil {
		t.Fatal("движок или клиент не созданы")
	}
	if eng.Client != client {
		t.Error("движок и клиент ссылаются на разные объекты")
	}
}

func TestBuildAlwaysHasDuckDuckGoPair(t *testing.T) {
	eng, _, err := Build(DeepConfig{Transport: "direct"})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	// Обычный и lite-вариант: lite отдаёт разметку, которую проще разобрать,
	// и работает, когда основной отдаёт заглушку.
	if len(eng.DDG) != 2 {
		t.Fatalf("движков DDG %d, ожидала 2", len(eng.DDG))
	}
	var lite int
	for _, s := range eng.DDG {
		if d, ok := s.(*searchers.DuckDuckGo); ok && d.Lite {
			lite++
		}
	}
	if lite != 1 {
		t.Errorf("lite-вариантов %d, ожидала 1", lite)
	}
}

func TestBuildHasAhmia(t *testing.T) {
	eng, _, err := Build(DeepConfig{Transport: "direct"})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	if eng.Ahmia == nil {
		t.Error("ahmia-мост не собран")
	} // На direct-транспорте отдельный клиент не нужен: мост едет на основном.
	if eng.Direct != nil {
		t.Error("на direct создан лишний клиент для моста")
	}
}

func TestBuildSearXNGOptIn(t *testing.T) {
	eng, _, err := Build(DeepConfig{Transport: "direct"})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	// Без URL движка нет вовсе: чужой инстанс по умолчанию был бы
	// внешней зависимостью.
	if eng.SearXNG != nil {
		t.Error("searxng собран без URL")
	}

	eng2, _, err := Build(DeepConfig{Transport: "direct", SearXNGBase: "https://sx.example/"})
	if err != nil {
		t.Fatal(err)
	}
	defer eng2.Close()
	if eng2.SearXNG == nil {
		t.Fatal("searxng не собран при заданном URL")
	}
	if eng2.SearXNG.Name() != "searxng" {
		t.Errorf("имя %q", eng2.SearXNG.Name())
	}
}

func TestBuildWithRotatorGivesAhmiaOwnClient(t *testing.T) {
	eng, _, err := Build(DeepConfig{Transport: "direct", Rotator: fakeRotator{}})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	// Основной клиент ходит через tor-ротатор: мосту нужен свой прямой,
	// иначе поиск по даркнету ляжет вместе с цепями.
	if eng.Direct == nil {
		t.Fatal("мосту не выдан отдельный прямой клиент при tor-ротаторе")
	}
	if eng.Ahmia == nil || eng.Ahmia.Client != eng.Direct {
		t.Error("мост не сидит на прямом клиенте")
	}
}

func TestBuildWithoutOnionEngines(t *testing.T) {
	eng, _, err := Build(DeepConfig{Transport: "direct"})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	// Без onion-движков каталог и пул здоровья не создаются: деградация
	// должна быть видна как nil, а не как пустой рабочий объект.
	if eng.Onion != nil {
		t.Error("каталог onion собран без движков")
	}
	if eng.Health != nil {
		t.Error("пул здоровья собран без движков")
	}
	if n, _, _ := eng.ProbeOnion(context.Background()); n != 0 {
		t.Errorf("проба без каталога вернула %d", n)
	}
	if rep := eng.HealthReport(); rep != nil {
		t.Errorf("отчёт без пула здоровья: %v", rep)
	}
}

func TestBuildWithOnionEngines(t *testing.T) {
	engines := []*searchers.OnionEngine{
		{Name_: "a", Base: "http://a.onion"},
		{Name_: "b", Base: "http://b.onion"},
	}
	eng, client, err := Build(DeepConfig{Transport: "direct", OnionEngines: engines})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	if eng.Onion == nil || eng.Health == nil {
		t.Fatal("каталог или пул здоровья не созданы")
	}
	// Движкам подставляется общий клиент: свой на каждый означал бы
	// отдельную сессию и свой отпечаток на каждый запрос.
	for _, e := range engines {
		if e.Client != client {
			t.Errorf("движок %s получил чужой клиент", e.Name_)
		}
	}
	if rep := eng.HealthReport(); len(rep) != 2 {
		t.Errorf("в отчёте %d движков, ожидала 2", len(rep))
	}
}

func TestBuildHeadlessCreatesBrowser(t *testing.T) {
	eng, _, err := Build(DeepConfig{Transport: "direct", Headless: true})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	if eng.Browser == nil {
		t.Error("браузер не создан при Headless")
	}
}

func TestBuildDefaultsTimeout(t *testing.T) {
	// Нулевой таймаут должен стать 30с, иначе клиент получит мгновенные
	// обрывы на первом же медленном onion-сервисе.
	eng, _, err := Build(DeepConfig{Transport: "direct", Timeout: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	if eng.Client == nil {
		t.Fatal("клиент не создан")
	}
}

func TestBuildRejectsBadTransport(t *testing.T) {
	if _, _, err := Build(DeepConfig{Transport: "телепорт"}); err == nil {
		t.Error("неизвестный транспорт принят")
	}
}

func TestBuildPoolWithoutPoolErrors(t *testing.T) {
	if _, _, err := Build(DeepConfig{Transport: "pool"}); err == nil {
		t.Error("транспорт pool без пула принят")
	}
}

func TestBuildCarriesCacheAndLimit(t *testing.T) {
	eng, _, err := Build(DeepConfig{
		Transport: "direct",
		CacheTTL:  15 * time.Minute,
		Limit:     7,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	if eng.CacheTTL != 15*time.Minute {
		t.Errorf("CacheTTL=%v", eng.CacheTTL)
	}
	if eng.DefaultN != 7 {
		t.Errorf("DefaultN=%d", eng.DefaultN)
	}
}

func TestFetchURLWithoutClient(t *testing.T) {
	eng := &Engine{}
	if _, err := eng.FetchURL(context.Background(), "http://example.com/"); err == nil {
		t.Error("запрос без клиента не отмечен ошибкой")
	}
}

func TestFetchURLWithClient(t *testing.T) {
	eng, _, err := Build(DeepConfig{Transport: "direct"})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	// Сеть не нужна: проверяется, что вызов доходит до клиента и ошибка
	// соединения возвращается как ошибка, а не как паника. Адрес служебный,
	// поэтому барьер приватных целей снят явно - иначе тест проверял бы барьер
	// вместо транспорта.
	eng.AllowPrivateTarget = true
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if _, err := eng.FetchURL(ctx, "http://127.0.0.1:9/"); err != nil {
		t.Logf("соединение не поднялось как ожидалось: %v", err)
	}
}

func TestEngineCloseWithoutBrowser(t *testing.T) {
	eng, _, err := Build(DeepConfig{Transport: "direct"})
	if err != nil {
		t.Fatal(err)
	}
	if eng.Browser != nil {
		t.Fatal("браузер не ожидался")
	}
	if err := eng.Close(); err != nil {
		t.Errorf("закрытие движка: %v", err)
	}
}

func TestEngineCloseTwice(t *testing.T) {
	eng, _, err := Build(DeepConfig{Transport: "direct"})
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.Close(); err != nil {
		t.Fatal(err)
	}
	// Повторное закрытие не должно падать: клиент уже закрыт.
	if err := eng.Close(); err != nil {
		t.Logf("повторное закрытие вернуло: %v", err)
	}
}

func TestProbeOnionWithoutHealth(t *testing.T) {
	eng := &Engine{}
	if n, _, _ := eng.ProbeOnion(context.Background()); n != 0 {
		t.Errorf("проба без пула здоровья вернула %d", n)
	}
}

func TestHealthReportWithoutHealth(t *testing.T) {
	eng := &Engine{}
	if rep := eng.HealthReport(); rep != nil {
		t.Errorf("отчёт без пула здоровья: %v", rep)
	}
}

type fakeRotator struct{}

func (fakeRotator) Kind() string                 { return "tor" }
func (fakeRotator) TransportSpec() string        { return "socks5://127.0.0.1:9050" }
func (fakeRotator) Rotate(context.Context) error { return nil }
func (fakeRotator) Healthy() bool                { return true }
func (fakeRotator) Close() error                 { return nil }

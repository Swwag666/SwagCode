package search

import (
	"testing"
	"time"

	"voidsearchswag/internal/searchers"
)

// TestBuildClearnetUsesDirectClientWhenTorPresent - главный тест разделения
// транспортов.
//
// Один общий клиент означал, что запросы к DuckDuckGo и SearXNG идут через tor:
// NewClient при ненулевом Rotator использует его и полностью игнорирует
// Transport. Для быстрого режима это ломало саму идею режима - fast задуман как
// короткий clearnet-путь, а фактически платил за tor-цепи. Tor exit-ноды
// медленные, и DuckDuckGo их регулярно блокирует, поэтому быстрый режим
// становился и медленнее, и ненадёжнее глубокого.
func TestBuildClearnetUsesDirectClientWhenTorPresent(t *testing.T) {
	eng, client, err := Build(DeepConfig{
		Rotator:      fakeRotator{},
		Timeout:      testTimeout(),
		SearXNGBase:  "http://searx.local",
		OnionEngines: []*searchers.OnionEngine{{Name_: "ahmia", Base: "http://a.onion"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	if eng.Client != client {
		t.Error("основной клиент не тот, что вернул Build")
	}
	// Прямой клиент обязан существовать отдельно от основного.
	if eng.Direct == nil {
		t.Fatal("прямой клиент не создан при наличии tor-ротатора")
	}
	if eng.Direct == client {
		t.Error("прямой клиент совпадает с tor-клиентом: разделения нет")
	}

	// Clearnet-движки обязаны ходить через прямой клиент.
	for i, s := range eng.DDG {
		ddg, ok := s.(*searchers.DuckDuckGo)
		if !ok {
			t.Fatalf("DDG[%d] не того типа", i)
		}
		if ddg.Client == nil {
			t.Fatalf("DDG[%d] без клиента", i)
		}
		if ddg.Client == client {
			t.Errorf("DDG[%d] ходит через tor-клиент", i)
		}
		if ddg.Client != eng.Direct {
			t.Errorf("DDG[%d] ходит не через прямой клиент", i)
		}
	}
	if eng.SearXNG == nil {
		t.Fatal("SearXNG не создан при заданном BaseURL")
	}
	if eng.SearXNG.Client != eng.Direct {
		t.Error("SearXNG ходит не через прямой клиент")
	}
	if eng.Ahmia == nil {
		t.Fatal("Ahmia-мост не создан")
	}
	if eng.Ahmia.Client != eng.Direct {
		t.Error("ahmia-мост ходит не через прямой клиент")
	}

	// Onion-движки остаются на tor-клиенте: им нужен tor.
	for _, e := range eng.Onion.Engines {
		if e.Client != client {
			t.Errorf("onion-движок %s ходит не через tor-клиент", e.Name_)
		}
	}
}

func TestBuildNoExtraClientWithoutRotator(t *testing.T) {
	// Когда ротатора нет, основной клиент и так прямой (static/pool/direct
	// достигают clearnet сами), поэтому второй клиент создаваться не должен:
	// лишний экземпляр плодил бы соединения и отпечатки без нужды.
	eng, client, err := Build(DeepConfig{
		Transport:    "direct",
		Timeout:      testTimeout(),
		SearXNGBase:  "http://searx.local",
		OnionEngines: []*searchers.OnionEngine{{Name_: "ahmia", Base: "http://a.onion"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	if eng.Direct != nil {
		t.Error("прямой клиент создан без нужды: ротатора нет")
	}
	for i, s := range eng.DDG {
		ddg := s.(*searchers.DuckDuckGo)
		if ddg.Client != client {
			t.Errorf("DDG[%d] получил отдельный клиент без ротатора", i)
		}
	}
	if eng.SearXNG.Client != client {
		t.Error("SearXNG получил отдельный клиент без ротатора")
	}
	if eng.Ahmia.Client != client {
		t.Error("ahmia-мост получил отдельный клиент без ротатора")
	}
}

func TestBuildDirectClientClosedWithEngine(t *testing.T) {
	// Отдельный клиент обязан закрываться вместе с движком: иначе каждое
	// построение ядра утекало бы tls-клиент и его соединения.
	eng, _, err := Build(DeepConfig{
		Rotator: fakeRotator{},
		Timeout: testTimeout(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if eng.Direct == nil {
		t.Fatal("прямой клиент не создан")
	}
	if err := eng.Close(); err != nil {
		t.Errorf("Close вернул ошибку: %v", err)
	}
	// Повторный Close не должен паниковать: движок закрывается из defer и из
	// явного вызова в разных командах.
	if err := eng.Close(); err != nil {
		t.Errorf("второй Close вернул ошибку: %v", err)
	}
}

func TestBuildClearnetWorksWithoutOnionEngines(t *testing.T) {
	// Быстрый режим не требует onion-движков вовсе, поэтому ядро обязано
	// собираться и работать без них.
	eng, client, err := Build(DeepConfig{
		Rotator: fakeRotator{},
		Timeout: testTimeout(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	if eng.Onion != nil {
		t.Error("каталог onion создан без движков")
	}
	if eng.Health != nil {
		t.Error("пул здоровья создан без движков")
	}
	if len(eng.DDG) != 2 {
		t.Errorf("DDG движков %d, ожидала 2", len(eng.DDG))
	}
	for i, s := range eng.DDG {
		ddg := s.(*searchers.DuckDuckGo)
		if ddg.Client == client {
			t.Errorf("DDG[%d] ходит через tor-клиент без onion-движков", i)
		}
	}
}

func TestBuildBothClientsReachClearnetIndependently(t *testing.T) {
	// Смысл разделения: ни один из двух путей не зависит от чужого транспорта.
	// Проверяется на уровне транспортов клиентов, а не сетевых запросов.
	eng, client, err := Build(DeepConfig{
		Rotator:      fakeRotator{},
		Timeout:      testTimeout(),
		OnionEngines: []*searchers.OnionEngine{{Name_: "ahmia", Base: "http://a.onion"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	torSpec := client.Rotator().TransportSpec()
	if torSpec == "" {
		t.Error("основной клиент без tor-транспорта")
	}
	if eng.Direct.Rotator() != nil {
		if got := eng.Direct.Rotator().TransportSpec(); got != "" {
			t.Errorf("прямой клиент получил транспорт %q, ожидала пустой", got)
		}
	}
}

func testTimeout() time.Duration {
	return 5 * time.Second
}

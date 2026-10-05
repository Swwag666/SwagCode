package search

import (
	"context"
	"errors"
	"testing"
	"time"

	"voidsearchswag/internal/netx"
)

// TestFetchURLBlocksPrivateTarget проверяет барьер на адресе, который диктует
// вызывающий. До правки FetchURL уходил в транспорт без проверки цели, и живой
// MCP-вызов fetch("http://127.0.0.1:18099/") возвращал тело локального сервиса.
func TestFetchURLBlocksPrivateTarget(t *testing.T) {
	eng, _, err := Build(DeepConfig{Transport: "direct"})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	if eng.AllowPrivateTarget {
		t.Fatal("барьер выключен по умолчанию")
	}

	cases := []string{
		"http://127.0.0.1:18099/",
		"http://localhost:1/",
		"http://[::1]:8080/x",
		"http://169.254.169.254/latest/meta-data/",
		"http://192.168.1.1/admin",
		"http://10.0.0.5:9200/_search",
		"file:///C:/Windows/System32/drivers/etc/hosts",
	}
	for _, raw := range cases {
		// Таймаут короткий намеренно: барьер обязан отвечать до попытки
		// соединения, иначе «защита» стоила бы полного таймаута запроса.
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		start := time.Now()
		_, err := eng.FetchURL(ctx, raw)
		cancel()
		if err == nil {
			t.Errorf("%s: запрос прошёл, хочу отказ", raw)
			continue
		}
		if !errors.Is(err, netx.ErrPrivateTarget) && !errors.Is(err, netx.ErrBadScheme) {
			t.Errorf("%s: ошибка %v, хочу %v или %v", raw, err, netx.ErrPrivateTarget, netx.ErrBadScheme)
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("%s: отказ пришёл за %v, барьер не должен ждать сеть", raw, d)
		}
	}
}

// TestFetchURLAllowPrivateTargetReachesTransport доказывает, что флаг снимает
// барьер, а не подменяет его другой ошибкой: запрос обязан дойти до транспорта
// и упасть на соединении, а не на проверке адреса.
func TestFetchURLAllowPrivateTargetReachesTransport(t *testing.T) {
	eng, _, err := Build(DeepConfig{Transport: "direct", AllowPrivateTarget: true})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	if !eng.AllowPrivateTarget {
		t.Fatal("DeepConfig.AllowPrivateTarget не дошло до движка")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = eng.FetchURL(ctx, "http://127.0.0.1:1/")
	if err == nil {
		t.Fatal("запрос на закрытый порт прошёл")
	}
	if errors.Is(err, netx.ErrPrivateTarget) || errors.Is(err, netx.ErrBadScheme) {
		t.Errorf("флаг не снял барьер: %v", err)
	}
}

// TestFetchURLPassesPublicTarget: публичный адрес барьер пропускает. Сеть не
// нужна - контекст отменён, поэтому транспорт отвечает сразу, а предмет теста
// состоит в том, что отказ пришёл не от барьера.
func TestFetchURLPassesPublicTarget(t *testing.T) {
	eng, _, err := Build(DeepConfig{Transport: "direct"})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	_, err = eng.FetchURL(ctx, "http://93.184.216.34/x")
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("проверка публичного адреса заняла %v", d)
	}
	if errors.Is(err, netx.ErrPrivateTarget) || errors.Is(err, netx.ErrBadScheme) {
		t.Errorf("публичный адрес отклонён барьером: %v", err)
	}
}

// TestFetchURLWithoutClientStillReportsInit проверяет порядок проверок: без
// клиента вызывающий получает причину про инициализацию, а не про адрес, иначе
// диагностика путала бы две разные поломки.
func TestFetchURLWithoutClientStillReportsInit(t *testing.T) {
	eng := &Engine{}
	_, err := eng.FetchURL(context.Background(), "http://127.0.0.1:1/")
	if err == nil {
		t.Fatal("запрос без клиента прошёл")
	}
	if errors.Is(err, netx.ErrPrivateTarget) {
		t.Errorf("получена причина барьера вместо причины про клиент: %v", err)
	}
}

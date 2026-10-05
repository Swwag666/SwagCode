package netx

import (
	"context"
	"errors"
	"net"
	"testing"
)

// stubLookup подменяет разрешение имён и возвращает прежнее значение, чтобы
// тесты не зависели от DNS машины.
func stubLookup(t *testing.T, fn func(ctx context.Context, host string) ([]net.IP, error)) {
	t.Helper()
	prev := lookupIPs
	lookupIPs = fn
	t.Cleanup(func() { lookupIPs = prev })
}

func noLookup(t *testing.T) {
	stubLookup(t, func(_ context.Context, host string) ([]net.IP, error) {
		t.Fatalf("резолвинг вызван для %q, хотя адрес задан литералом", host)
		return nil, nil
	})
}

func TestBlockedReasonTable(t *testing.T) {
	blocked := []struct {
		addr string
		why  string
	}{
		{"127.0.0.1", "петлевой адрес"},
		{"127.5.5.5", "петлевой адрес"},
		{"::1", "петлевой адрес"},
		{"::ffff:127.0.0.1", "петлевой адрес"},
		{"10.1.2.3", "приватная сеть"},
		{"172.16.0.1", "приватная сеть"},
		{"172.31.255.255", "приватная сеть"},
		{"192.168.1.1", "приватная сеть"},
		{"fd12:3456::1", "приватная сеть"},
		{"fe80::1", "link-local адрес"},
		{"169.254.169.254", "link-local адрес"},
		{"0.0.0.0", "неопределённый адрес"},
		{"::", "неопределённый адрес"},
		{"0.1.2.3", "блок «эта сеть»"},
		{"100.64.0.1", "CGNAT провайдера"},
		{"100.127.255.255", "CGNAT провайдера"},
		{"192.0.0.1", "служебный блок IETF"},
		{"192.0.2.1", "тестовая сеть документации"},
		{"198.18.0.1", "блок измерений"},
		{"198.51.100.1", "тестовая сеть документации"},
		{"203.0.113.1", "тестовая сеть документации"},
		{"224.0.0.1", "локальный multicast"},
		{"ff02::1", "локальный multicast"},
		{"240.1.1.1", "зарезервированный блок"},
		{"255.255.255.255", "широковещательный адрес"},
		{"64:ff9b::1", "префикс NAT64"},
		{"100::1", "блок Discard-Only"},
		{"2001:db8::1", "тестовая сеть документации IPv6"},
		{"2001:2::1", "служебный блок IETF для Teredo и измерений"},
	}
	for _, c := range blocked {
		ip := net.ParseIP(c.addr)
		if ip == nil {
			t.Fatalf("адрес %q не разобран", c.addr)
		}
		got := BlockedReason(ip)
		if got != c.why {
			t.Errorf("%s: причина %q, хочу %q", c.addr, got, c.why)
		}
	}

	allowed := []string{
		"8.8.8.8",
		"1.1.1.1",
		"93.184.216.34",
		"172.32.0.1",
		"100.128.0.1",
		"192.0.1.1",
		"198.20.0.1",
		"2606:2800:220:1:248:1893:25c8:1946",
		"2a00:1450:4010:c0f::67",
	}
	for _, addr := range allowed {
		ip := net.ParseIP(addr)
		if ip == nil {
			t.Fatalf("адрес %q не разобран", addr)
		}
		if why := BlockedReason(ip); why != "" {
			t.Errorf("%s: публичный адрес заблокирован как %q", addr, why)
		}
	}
	if why := BlockedReason(nil); why == "" {
		t.Error("nil-адрес получил разрешение")
	}
}

func TestCheckPublicTargetSchemes(t *testing.T) {
	ctx := context.Background()
	noLookup(t)

	cases := []struct {
		raw string
		err error
	}{
		{"", ErrBadScheme},
		{"   ", ErrBadScheme},
		{"file:///C:/Windows/System32/drivers/etc/hosts", ErrBadScheme},
		{"ftp://example.com/x", ErrBadScheme},
		{"gopher://127.0.0.1:70/", ErrBadScheme},
		{"http:///only-path", ErrBadScheme},
		{"example.com/no-scheme", ErrBadScheme},
	}
	for _, c := range cases {
		err := CheckPublicTarget(ctx, c.raw)
		if err == nil {
			t.Errorf("%q: ошибки нет", c.raw)
			continue
		}
		if !errors.Is(err, c.err) {
			t.Errorf("%q: ошибка %v, хочу %v", c.raw, err, c.err)
		}
	}
	if err := CheckPublicTarget(ctx, "://мусор"); err == nil {
		t.Error("нечитаемый URL принят")
	}
}

// Литерал IP обязан проверяться без резолвинга: лишний DNS-запрос на адрес,
// который уже разобран, это и задержка, и утечка имени.
func TestCheckPublicTargetIPLiteralSkipsLookup(t *testing.T) {
	ctx := context.Background()
	noLookup(t)

	if err := CheckPublicTarget(ctx, "https://8.8.8.8/x"); err != nil {
		t.Errorf("публичный литерал отклонён: %v", err)
	}
	err := CheckPublicTarget(ctx, "http://127.0.0.1:18099/")
	if !errors.Is(err, ErrPrivateTarget) {
		t.Errorf("127.0.0.1: ошибка %v, хочу %v", err, ErrPrivateTarget)
	}
	err = CheckPublicTarget(ctx, "http://[::1]:8080/")
	if !errors.Is(err, ErrPrivateTarget) {
		t.Errorf("::1: ошибка %v, хочу %v", err, ErrPrivateTarget)
	}
	// Порт и путь не влияют на решение.
	if err := CheckPublicTarget(ctx, "https://93.184.216.34:8443/a/b?c=d#e"); err != nil {
		t.Errorf("публичный литерал с портом отклонён: %v", err)
	}
}

// Имя резолвится, и проверяются все полученные адреса: запись с публичным и
// петлевым адресом обязана быть отклонена, иначе выбор удобной A-записи снимал
// бы барьер.
func TestCheckPublicTargetResolvesEveryAddress(t *testing.T) {
	ctx := context.Background()
	table := map[string][]net.IP{
		"localhost":   {net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		"mixed.test":  {net.ParseIP("93.184.216.34"), net.ParseIP("127.0.0.1")},
		"public.test": {net.ParseIP("93.184.216.34")},
		"empty.test":  {},
		"meta.test":   {net.ParseIP("169.254.169.254")},
		"lan.test":    {net.ParseIP("192.168.1.1")},
	}
	stubLookup(t, func(_ context.Context, host string) ([]net.IP, error) {
		if host == "fail.test" {
			return nil, errors.New("DNS недоступен")
		}
		ips, ok := table[host]
		if !ok {
			return nil, errors.New("неизвестное имя " + host)
		}
		return ips, nil
	})

	for _, raw := range []string{"http://localhost:18099/", "http://mixed.test/x", "http://meta.test/latest", "http://lan.test/"} {
		if err := CheckPublicTarget(ctx, raw); !errors.Is(err, ErrPrivateTarget) {
			t.Errorf("%s: ошибка %v, хочу %v", raw, err, ErrPrivateTarget)
		}
	}
	if err := CheckPublicTarget(ctx, "https://public.test/x"); err != nil {
		t.Errorf("публичное имя отклонено: %v", err)
	}
	err := CheckPublicTarget(ctx, "https://empty.test/")
	if err == nil {
		t.Error("пустой список адресов принят")
	}
	if errors.Is(err, ErrPrivateTarget) || errors.Is(err, ErrBadScheme) {
		t.Errorf("пустой список адресов дал %v, хочу отдельную причину", err)
	}
	// Сбой разрешения имени - это ошибка, а не разрешение пройти: молчаливый
	// пропуск означал бы, что недоступный DNS снимает барьер.
	err = CheckPublicTarget(ctx, "https://fail.test/")
	if err == nil {
		t.Error("сбой DNS принят как успех")
	}
	if errors.Is(err, ErrPrivateTarget) || errors.Is(err, ErrBadScheme) {
		t.Errorf("сбой DNS получил причину %v, хочу текст про разрешение имени", err)
	}
}

// Настоящий резолвинг без подмены: localhost обязан блокироваться на живой
// машине, а не только в тестовой таблице.
func TestCheckPublicTargetBlocksRealLocalhost(t *testing.T) {
	ctx := context.Background()
	if err := CheckPublicTarget(ctx, "http://localhost:18099/internal"); !errors.Is(err, ErrPrivateTarget) {
		t.Errorf("localhost: ошибка %v, хочу %v", err, ErrPrivateTarget)
	}
	if err := CheckPublicTarget(ctx, "http://LOCALHOST:18099/"); !errors.Is(err, ErrPrivateTarget) {
		t.Errorf("LOCALHOST в верхнем регистре: ошибка %v, хочу %v", err, ErrPrivateTarget)
	}
}

// Отменённый контекст не превращается в разрешение: резолвинг обязан вернуть
// ошибку, а не пустой список, который барьер принял бы за «адресов нет».
func TestCheckPublicTargetCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stubLookup(t, func(ctx context.Context, host string) ([]net.IP, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return []net.IP{net.ParseIP("93.184.216.34")}, nil
	})
	if err := CheckPublicTarget(ctx, "https://public.test/"); err == nil {
		t.Error("отменённый контекст принят как успех")
	}
}

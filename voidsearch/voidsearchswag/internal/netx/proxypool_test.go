package netx

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStaticProviderFetch(t *testing.T) {
	p := StaticProvider{"1.2.3.4:1080", "  ", "#коммент", "http://5.6.7.8:8080", "мусор"}
	got, err := p.Fetch(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name() != "static" {
		t.Errorf("Name=%q", p.Name())
	}
	// Мусор и комментарии отбрасываются, адрес без схемы получает socks5://.
	if len(got) != 2 {
		t.Fatalf("адресов %d, ожидала 2: %v", len(got), got)
	}
	if got[0] != "socks5://1.2.3.4:1080" {
		t.Errorf("первый %q", got[0])
	}
	if got[1] != "http://5.6.7.8:8080" {
		t.Errorf("второй %q", got[1])
	}
}

func TestStaticProviderLimit(t *testing.T) {
	p := StaticProvider{"1.1.1.1:1", "2.2.2.2:2", "3.3.3.3:3"}
	got, err := p.Fetch(context.Background(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("лимит не соблюдён: %d адресов", len(got))
	}
}

func TestStaticProviderEmpty(t *testing.T) {
	for _, p := range []StaticProvider{nil, {}, {"", "  ", "#только-коммент", "мусор"}} {
		if _, err := p.Fetch(context.Background(), 0); err == nil {
			t.Errorf("пустой провайдер %v не вернул ошибку", p)
		}
	}
}

func TestProxyScrapeName(t *testing.T) {
	if got := DefaultProxyScrape().Name(); got != "proxyscrape/http" {
		t.Errorf("Name=%q", got)
	}
	if got := (ProxyScrape{Protocol: "SOCKS5"}).Name(); got != "proxyscrape/socks5" {
		t.Errorf("регистр не нормализован: %q", got)
	}
	if got := (ProxyScrape{}).Name(); got != "proxyscrape/http" {
		t.Errorf("дефолт протокола потерян: %q", got)
	}
}

func TestProxyScrapeEndpointVariants(t *testing.T) {
	cases := []struct {
		endpoint string
		want     string
	}{
		{"https://api.test/", "https://api.test/?request=getproxies"},
		{"https://api.test/?", "https://api.test/?request=getproxies"},
		{"https://api.test/?x=1", "https://api.test/?x=1&request=getproxies"},
		{"", "https://api.proxyscrape.com/v2/?request=getproxies"},
	}
	for _, c := range cases {
		u := ProxyScrape{Endpoint: c.endpoint}.url()
		if !strings.HasPrefix(u, c.want) {
			t.Errorf("endpoint %q дал %q, ожидала префикс %q", c.endpoint, u, c.want)
		}
	}
}

func TestProxyScrapeFetchAgainstStubServer(t *testing.T) {
	srv := listServer(t, proxyList)
	p := ProxyScrape{Protocol: "http", Country: "all", TimeoutMS: 5000, Endpoint: srv.URL + "/"}

	got, err := p.Fetch(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	// Дубликат, комментарий, пустая строка и мусор без порта отброшены.
	if len(got) != 3 {
		t.Fatalf("адресов %d, ожидала 3: %v", len(got), got)
	}
	for _, spec := range got {
		if !strings.HasPrefix(spec, "http://") {
			t.Errorf("схема не подставлена: %q", spec)
		}
		if _, err := parseProxySpec(spec); err != nil {
			t.Errorf("spec %q не парсится: %v", spec, err)
		}
	}
}

func TestProxyScrapeFetchLimit(t *testing.T) {
	srv := listServer(t, proxyList)
	p := ProxyScrape{Protocol: "socks5", Endpoint: srv.URL + "/"}

	got, err := p.Fetch(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("лимит не соблюдён: %d", len(got))
	}
	if !strings.HasPrefix(got[0], "socks5://") {
		t.Errorf("схема %q, ожидала socks5://", got[0])
	}
}

func TestProxyScrapeFetchHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	p := ProxyScrape{Endpoint: srv.URL + "/"}
	_, err := p.Fetch(context.Background(), 0)
	if err == nil {
		t.Fatal("HTTP 403 принят")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("код ответа не назван: %v", err)
	}
}

func TestProxyScrapeFetchEmptyAnswer(t *testing.T) {
	srv := listServer(t, "#только комментарии\n\nмусор\n")
	p := ProxyScrape{Endpoint: srv.URL + "/"}

	_, err := p.Fetch(context.Background(), 0)
	if err == nil {
		t.Fatal("пустой ответ принят")
	}
	if !strings.Contains(err.Error(), "пустой ответ") {
		t.Errorf("ошибка не объясняет причину: %v", err)
	}
}

func TestProxyScrapeFetchUnreachable(t *testing.T) {
	p := ProxyScrape{Endpoint: "http://127.0.0.1:1/"}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := p.Fetch(ctx, 0); err == nil {
		t.Fatal("недоступный endpoint принят")
	}
}

func TestProbeProxyBadSpec(t *testing.T) {
	// Невалидный адрес обязан отказать до попытки соединения: иначе проба
	// тратит весь таймаут на заведомо мёртвую строку.
	if _, _, err := ProbeProxy(context.Background(), "ftp://мусор", time.Second); err == nil {
		t.Error("невалидный spec принят")
	}
}

func TestProbeProxyUnreachableSocks(t *testing.T) {
	ip, lat, err := ProbeProxy(context.Background(), "socks5://127.0.0.1:1", 1500*time.Millisecond)
	if err == nil {
		t.Skip("проба неожиданно прошла через несуществующий прокси")
	}
	if ip != "" {
		t.Errorf("при ошибке вернули IP %q", ip)
	}
	if lat < 0 {
		t.Errorf("отрицательная латентность %v", lat)
	}
}

func TestProbeProxyUnreachableHTTP(t *testing.T) {
	if _, _, err := ProbeProxy(context.Background(), "http://127.0.0.1:1", 1500*time.Millisecond); err == nil {
		t.Error("недоступный http-прокси принят")
	}
}

func TestFilterLiveMarksDeadInState(t *testing.T) {
	st := LoadPoolState("", nil)
	// Оба адреса недоступны: FilterLive обязан отметить их мёртвыми в
	// состоянии, иначе каждый прогон заново тратит время на те же трупы.
	list := []string{"http://127.0.0.1:1", "socks5://127.0.0.1:2"}
	live := FilterLive(context.Background(), list, 2, 800*time.Millisecond, nopLogger{}, st)

	if len(live) != 0 {
		t.Errorf("живых %d, ожидала 0", len(live))
	}
	for _, spec := range list {
		if why := st.Skip(spec, ""); why == "" {
			t.Errorf("%s не помечен мёртвым", spec)
		}
	}
}

func TestFilterLiveSkipsKnownDead(t *testing.T) {
	st := LoadPoolState("", nil)
	st.MarkDead("http://127.0.0.1:1", "ранее умер")

	list := []string{"http://127.0.0.1:1"}
	live := FilterLive(context.Background(), list, 4, 500*time.Millisecond, nopLogger{}, st)
	if len(live) != 0 {
		t.Errorf("живых %d", len(live))
	}
}

func TestFilterLiveEmptyList(t *testing.T) {
	if got := FilterLive(context.Background(), nil, 4, time.Second, nil, nil); len(got) != 0 {
		t.Errorf("пустой список дал %d живых", len(got))
	}
}

func TestFilterLiveSortsKnownIPFirst(t *testing.T) {
	// Сортировка обязана ставить записи с известным exit IP раньше слепых:
	// слепой прокси нельзя проверить на бан по IP, и он менее полезен.
	live := []LiveProxy{
		{Spec: "blind", Latency: time.Millisecond},
		{Spec: "known-slow", ExitIP: "8.8.8.8", Latency: time.Hour},
		{Spec: "known-fast", ExitIP: "1.1.1.1", Latency: time.Second},
	}
	sortLive(live)
	if live[0].Spec != "known-fast" || live[1].Spec != "known-slow" || live[2].Spec != "blind" {
		t.Errorf("порядок неверен: %v, %v, %v", live[0].Spec, live[1].Spec, live[2].Spec)
	}
}

// sortLive повторяет порядок, который FilterLive применяет к результату.
// Вынесено в тест, чтобы порядок проверялся напрямую и не зависел от сети.
func sortLive(live []LiveProxy) {
	for i := 1; i < len(live); i++ {
		for j := i; j > 0; j-- {
			a, b := live[j-1], live[j]
			better := false
			if (a.ExitIP == "") != (b.ExitIP == "") {
				better = b.ExitIP != ""
			} else {
				better = b.Latency < a.Latency
			}
			if better {
				live[j-1], live[j] = b, a
			}
		}
	}
}

func TestDefaultPoolConfig(t *testing.T) {
	p := StaticProvider{"1.1.1.1:1"}
	cfg := DefaultPoolConfig(p)
	if cfg.Provider == nil {
		t.Fatal("провайдер потерян")
	}
	if !cfg.Verify {
		t.Error("проверка живости по умолчанию выключена: пул набьётся трупами")
	}
	for name, v := range map[string]int{
		"Concurrency": cfg.Concurrency,
		"FetchLimit":  cfg.FetchLimit,
		"MinLive":     cfg.MinLive,
		"MaxLive":     cfg.MaxLive,
	} {
		if v <= 0 {
			t.Errorf("%s=%d", name, v)
		}
	}
	if cfg.ProbeTimeout <= 0 || cfg.RefillWait <= 0 {
		t.Errorf("нулевые интервалы: %v / %v", cfg.ProbeTimeout, cfg.RefillWait)
	}
	if cfg.MinLive > cfg.MaxLive {
		t.Errorf("MinLive %d больше MaxLive %d", cfg.MinLive, cfg.MaxLive)
	}
}

func TestNewProxyPoolWithoutProvider(t *testing.T) {
	if _, err := NewProxyPool(context.Background(), PoolConfig{}, nil); err == nil {
		t.Fatal("пул без провайдера собран")
	}
}

func TestNewProxyPoolFillsFromProvider(t *testing.T) {
	srv := listServer(t, proxyList)
	p := ProxyScrape{Endpoint: srv.URL + "/"}

	pool, err := NewProxyPool(context.Background(), PoolConfig{
		Provider:   p,
		Verify:     false,
		MaxLive:    5,
		MinLive:    1,
		RefillWait: time.Hour,
		FetchLimit: 10,
	}, nopLogger{})
	if err != nil {
		t.Fatal(err)
	}

	if pool.ProviderName() != "proxyscrape/http" {
		t.Errorf("ProviderName=%q", pool.ProviderName())
	}
	live, total, killed := pool.Stats()
	if live != 3 || total != 3 {
		t.Errorf("live=%d total=%d killed=%d, ожидала 3/3/0", live, total, killed)
	}
	if pool.State() != nil {
		t.Error("состояние создано без StatePath")
	}
	// ExitIP для адреса без проверки живости неизвестен.
	if got := pool.ExitIP("http://1.2.3.4:8080"); got != "" {
		t.Errorf("ExitIP=%q", got)
	}
	if err := pool.SaveState(); err != nil {
		t.Errorf("SaveState без состояния: %v", err)
	}
}

func TestNewProxyPoolVerifyFailsOnDeadProxies(t *testing.T) {
	srv := listServer(t, "http://127.0.0.1:1\nhttp://127.0.0.1:2\n")
	p := ProxyScrape{Endpoint: srv.URL + "/"}

	// Все адреса мертвы, поэтому пул не соберётся: пустой пул в работе
	// означает, что каждый запрос падает, а ротатор нечего выбирать.
	_, err := NewProxyPool(context.Background(), PoolConfig{
		Provider:     p,
		Verify:       true,
		ProbeTimeout: 800 * time.Millisecond,
		Concurrency:  2,
		MaxLive:      5,
		MinLive:      1,
		RefillWait:   time.Hour,
	}, nopLogger{})
	if err == nil {
		t.Fatal("пул из мёртвых прокси собран")
	}
	if !strings.Contains(err.Error(), "ни одного живого") {
		t.Errorf("ошибка не объясняет причину: %v", err)
	}
}

func TestNewProxyPoolWithStatePath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "state.json")
	srv := listServer(t, proxyList)
	p := ProxyScrape{Endpoint: srv.URL + "/"}

	pool, err := NewProxyPool(context.Background(), PoolConfig{
		Provider:   p,
		Verify:     false,
		MaxLive:    5,
		MinLive:    1,
		RefillWait: time.Hour,
		StatePath:  path,
	}, nopLogger{})
	if err != nil {
		t.Fatal(err)
	}
	if pool.State() == nil {
		t.Fatal("состояние не загружено")
	}
	// MarkBanned и MarkSuspect берут (spec, reason): exit IP пул достаёт
	// сам из записи, а при отсутствии - из состояния.
	pool.MarkDead("http://1.2.3.4:8080", "тест")
	pool.MarkBanned("http://5.6.7.8:3128", "бан")
	pool.MarkSuspect("http://9.9.9.9:1080", "подозрителен")

	if err := pool.SaveState(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("файл состояния не создан: %v", err)
	}

	// Перезагрузка обязана вернуть вердикты: иначе каждый прогон заново
	// пробует забаненные цели и получает блокировки. При Verify=false
	// exit IP не известен, поэтому бан идёт только по адресу, а не по IP.
	st2 := LoadPoolState(path, nil)
	if why := st2.Skip("http://1.2.3.4:8080", ""); why == "" {
		t.Error("мёртвый адрес не пережил перезагрузку")
	}
	if why := st2.Skip("http://5.6.7.8:3128", ""); why == "" {
		t.Error("забаненный адрес не пережил перезагрузку")
	}
}

func TestPoolStateMarkBannedRecordsExitIP(t *testing.T) {
	// С известным exit IP бан обязан закрыть и сам IP, и всю сеть /24:
	// иначе тот же выход сменит порт и продолжит блокировать цель.
	ps := LoadPoolState("", nil)
	ps.MarkBanned("http://a:1", "9.9.9.9", "бан")

	if why := ps.Skip("http://other:1", "9.9.9.9"); why == "" {
		t.Error("бан по IP не сработал")
	}
	if why := ps.Skip("http://other:2", "9.9.9.200"); why == "" {
		t.Error("бан по сети /24 не сработал")
	}
	if why := ps.Skip("http://c:3", "8.8.8.8"); why != "" {
		t.Errorf("чужой IP забанен: %s", why)
	}
	// MarkBanned ставит срок бана, но не помечает адрес мёртвым: это разные
	// вердикты, и смерть означает «не отвечает», а бан - «отвечает, но цель
	// нас блокирует».
	dead, banned, _ := ps.Counts()
	if dead != 0 || banned != 1 {
		t.Errorf("dead=%d banned=%d, ожидала 0/1", dead, banned)
	}
}

func TestPoolStateCountsVerdictPrecedence(t *testing.T) {
	// Counts относит запись к одному вердикту: смерть важнее бана, бан
	// важнее страйка. Иначе сумма счётчиков превысит число записей и
	// статистика начнёт врать.
	ps := LoadPoolState("", nil)
	ps.MarkDead("http://dead-only:1", "умер")
	ps.MarkBanned("http://banned-only:1", "9.9.9.9", "бан")
	ps.MarkSuspect("http://suspect-only:1", "8.8.8.8", "страйк")
	// Запись с обоими вердиктами считается один раз - как мёртвая.
	ps.MarkDead("http://both:1", "умер")
	ps.MarkBanned("http://both:1", "7.7.7.7", "бан")

	dead, banned, striking := ps.Counts()
	if dead != 2 || banned != 1 || striking != 1 {
		t.Errorf("вердикты %d/%d/%d, ожидала 2/1/1", dead, banned, striking)
	}
	if dead+banned+striking != 4 {
		t.Errorf("сумма %d при 4 записях", dead+banned+striking)
	}
}

func TestProxyPoolMarkSuspectWithoutState(t *testing.T) {
	pool := &ProxyPool{cfg: PoolConfig{MaxLive: 2, MinLive: 1, RefillWait: time.Hour}, dead: map[string]int{}}
	pool.add(LiveProxy{Spec: "http://a:1", ExitIP: "1.1.1.1"})
	// Без состояния MarkSuspect обязан выйти молча, а не паниковать.
	pool.MarkSuspect("http://a:1", "причина")
	if live, _, _ := pool.Stats(); live != 1 {
		t.Errorf("живых %d, ожидала 1", live)
	}
}

func TestProxyPoolMarkBannedWithoutEntry(t *testing.T) {
	pool := &ProxyPool{cfg: PoolConfig{MaxLive: 2, MinLive: 1, RefillWait: time.Hour}, dead: map[string]int{}, log: nopLogger{}}
	// Адреса нет в пуле: отметка не должна ни паниковать, ни менять счётчики.
	pool.MarkBanned("http://absent:1", "бан")
	if _, _, killed := pool.Stats(); killed != 0 {
		t.Errorf("killed=%d", killed)
	}
}

func TestProxyPoolRefillWhenBelowMinLive(t *testing.T) {
	srv := listServer(t, proxyList)
	p := ProxyScrape{Endpoint: srv.URL + "/"}

	pool := &ProxyPool{
		cfg: PoolConfig{
			Provider: p, Verify: false, MaxLive: 5, MinLive: 3,
			RefillWait: time.Millisecond, FetchLimit: 10,
		},
		log:  nopLogger{},
		dead: map[string]int{},
	}
	pool.add(LiveProxy{Spec: "http://a:1"})
	pool.add(LiveProxy{Spec: "http://b:2"})

	// Живых 2 при MinLive 3: выброс одного обязан запустить дозаправку.
	pool.MarkDead("http://a:1", "тест")
	pool.refillAsync()

	// Дозаправка асинхронная, поэтому ждём появления адресов провайдера.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, total, _ := pool.Stats(); total > 2 {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	if _, total, _ := pool.Stats(); total <= 2 {
		t.Errorf("дозаправка не сработала: total=%d", total)
	}
}

func TestProxyPoolRefillThrottled(t *testing.T) {
	calls := 0
	p := countingProvider{fn: func(context.Context, int) ([]string, error) {
		calls++
		return []string{"http://1.2.3.4:8080"}, nil
	}}
	pool := &ProxyPool{
		cfg:  PoolConfig{Provider: p, Verify: false, MaxLive: 5, MinLive: 1, RefillWait: time.Hour},
		log:  nopLogger{},
		dead: map[string]int{},
	}
	pool.lastRef = time.Now()
	// Внутри окна дозаправки новый запрос обязан игнорироваться: иначе
	// каждый выброшенный прокси тянет за собой обращение к провайдеру.
	pool.refillAsync()
	time.Sleep(100 * time.Millisecond)
	if calls != 0 {
		t.Errorf("дозаправка запущена внутри окна: вызовов %d", calls)
	}
}

type countingProvider struct {
	fn func(context.Context, int) ([]string, error)
}

func (c countingProvider) Name() string { return "counter" }

func (c countingProvider) Fetch(ctx context.Context, limit int) ([]string, error) {
	return c.fn(ctx, limit)
}

func TestNewPoolRotatorEmptyPool(t *testing.T) {
	pool := &ProxyPool{cfg: PoolConfig{MaxLive: 2, MinLive: 1, RefillWait: time.Hour}, dead: map[string]int{}}
	if _, err := NewPoolRotator(pool); err == nil {
		t.Fatal("ротатор на пустом пуле собран")
	}
}

func TestPoolRotatorRotatesAndReportsHealth(t *testing.T) {
	pool := &ProxyPool{
		cfg:  PoolConfig{Provider: StaticProvider{"1.1.1.1:1"}, Verify: false, MaxLive: 5, MinLive: 1, RefillWait: time.Hour},
		log:  nopLogger{},
		dead: map[string]int{},
	}
	pool.add(LiveProxy{Spec: "http://a:1", ExitIP: "1.1.1.1"})
	pool.add(LiveProxy{Spec: "http://b:2", ExitIP: "2.2.2.2"})

	r, err := NewPoolRotator(pool)
	if err != nil {
		t.Fatal(err)
	}
	pr := r.(*poolRotator)
	if r.Kind() != "proxy" {
		t.Errorf("Kind=%q", r.Kind())
	}
	first := r.TransportSpec()
	if first == "" {
		t.Fatal("spec пуст")
	}
	if err := r.Rotate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.TransportSpec() == first {
		t.Error("Rotate не сменил прокси")
	}
	if !r.Healthy() {
		t.Error("пул с живыми записями не здоров")
	}
	if pr.Pool() != pool {
		t.Error("Pool() вернул чужой пул")
	}
	if got := pr.ExitIPFor("http://a:1"); got != "1.1.1.1" {
		t.Errorf("ExitIPFor=%q", got)
	}
	// Отметки обязаны доходить до пула, иначе ротатор продолжает давать
	// прокси, который уже забанил цель.
	pr.MarkFailed("http://a:1", "тест")
	if _, _, killed := pool.Stats(); killed != 1 {
		t.Errorf("killed=%d", killed)
	}
	pr.MarkBanned("http://b:2", "бан")
	pr.MarkSuspect("http://a:1", "подозрение")
	if err := r.Close(); err != nil {
		t.Errorf("закрытие: %v", err)
	}
}

func TestPoolRotatorRefillsWhenAllDead(t *testing.T) {
	srv := listServer(t, proxyList)
	p := ProxyScrape{Endpoint: srv.URL + "/"}

	pool := &ProxyPool{
		cfg:  PoolConfig{Provider: p, Verify: false, MaxLive: 5, MinLive: 1, RefillWait: time.Millisecond, FetchLimit: 10},
		log:  nopLogger{},
		dead: map[string]int{},
	}
	pool.add(LiveProxy{Spec: "http://old:1"})

	r, err := NewPoolRotator(pool)
	if err != nil {
		t.Fatal(err)
	}
	pool.MarkDead("http://old:1", "умер")
	if r.Healthy() {
		t.Error("пул без живых записей назван здоровым")
	}
	// Все записи мертвы: Rotate обязан дозаправить пул и вернуть адрес,
	// а не сдаться с ошибкой.
	if err := r.Rotate(context.Background()); err != nil {
		t.Fatalf("Rotate не восстановил пул: %v", err)
	}
	if r.TransportSpec() == "" {
		t.Error("после дозаправки spec пуст")
	}
	if !r.Healthy() {
		t.Error("после дозаправки пул не здоров")
	}
}

func TestPoolRotatorRotateFailsWhenProviderEmpty(t *testing.T) {
	p := countingProvider{fn: func(context.Context, int) ([]string, error) {
		return nil, nil
	}}
	pool := &ProxyPool{
		cfg:  PoolConfig{Provider: p, Verify: false, MaxLive: 5, MinLive: 1, RefillWait: time.Millisecond},
		log:  nopLogger{},
		dead: map[string]int{},
	}
	pool.add(LiveProxy{Spec: "http://a:1"})
	r, err := NewPoolRotator(pool)
	if err != nil {
		t.Fatal(err)
	}
	pool.MarkDead("http://a:1", "умер")
	if err := r.Rotate(context.Background()); err == nil {
		t.Error("Rotate на пустом провайдере не вернул ошибку")
	}
}

func TestPoolStateCounts(t *testing.T) {
	ps := LoadPoolState("", nil)
	ps.MarkDead("http://dead:1", "умер")
	ps.MarkBanned("http://ban:1", "9.9.9.9", "бан")
	ps.MarkSuspect("http://susp:1", "8.8.8.8", "подозрителен")

	dead, banned, striking := ps.Counts()
	if dead != 1 {
		t.Errorf("dead=%d", dead)
	}
	if banned != 1 {
		t.Errorf("banned=%d", banned)
	}
	if striking != 1 {
		t.Errorf("striking=%d", striking)
	}
}

func TestPoolStateCountsEmpty(t *testing.T) {
	ps := LoadPoolState("", nil)
	if d, b, s := ps.Counts(); d != 0 || b != 0 || s != 0 {
		t.Errorf("пустое состояние: %d/%d/%d", d, b, s)
	}
}

func TestPoolStatePath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.json")
	if got := LoadPoolState(path, nil).Path(); got != path {
		t.Errorf("Path=%q, ожидала %q", got, path)
	}
	if got := LoadPoolState("", nil).Path(); got != "" {
		t.Errorf("Path без файла=%q", got)
	}
}

func TestPoolStatePruneDropsExpired(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.json")

	// Файл пишется напрямую, чтобы подставить старые метки времени:
	// через MarkDead получить запись старше TTL нельзя.
	old := time.Now().Add(-40 * 24 * time.Hour)
	f := stateFile{
		Updated: time.Now(),
		Proxies: map[string]entryState{
			"http://старый:1": {DeadAt: old, Reason: "умер давно"},
			"http://свежий:1": {DeadAt: time.Now(), Reason: "умер сейчас"},
		},
		BannedIP:  map[string]time.Time{"9.9.9.9": time.Now().Add(-time.Hour)},
		BannedNet: map[string]time.Time{"1.2.3.0/24": time.Now().Add(time.Hour)},
	}
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}

	ps := LoadPoolState(path, nopLogger{})
	// Старая запись вычищена уже при загрузке.
	if why := ps.Skip("http://старый:1", ""); why != "" {
		t.Errorf("просроченная запись не вычищена: %s", why)
	}
	if why := ps.Skip("http://свежий:1", ""); why == "" {
		t.Error("свежая запись вычищена")
	}
	// Просроченный бан по IP снят, действующий бан по диапазону остался.
	if why := ps.Skip("http://x:1", "9.9.9.9"); why != "" {
		t.Errorf("просроченный бан по IP не снят: %s", why)
	}
	if why := ps.Skip("http://y:1", "1.2.3.4"); why == "" {
		t.Error("действующий бан по диапазону снят")
	}
	if n := ps.Prune(); n != 1 {
		t.Errorf("Prune вернул %d, ожидала 1", n)
	}
}

func TestPoolStatePruneCapsEntries(t *testing.T) {
	ps := LoadPoolState("", nil)
	now := time.Now()
	// Записей больше потолка: pruning обязан оставить ровно stateMaxEntries,
	// иначе файл состояния растёт бесконечно.
	for i := 0; i < stateMaxEntries+500; i++ {
		ps.st.Proxies[fmt.Sprintf("http://p%05d:1", i)] = entryState{
			DeadAt: now.Add(-time.Duration(i) * time.Second),
			Reason: "умер",
		}
	}
	if n := ps.Prune(); n != stateMaxEntries {
		t.Errorf("после pruning записей %d, ожидала %d", n, stateMaxEntries)
	}
}

func TestPoolStateSaveWithoutPath(t *testing.T) {
	ps := LoadPoolState("", nil)
	ps.MarkDead("http://a:1", "умер")
	if err := ps.Save(); err != nil {
		t.Errorf("Save без пути вернул ошибку: %v", err)
	}
}

func TestPoolStateSaveNotDirty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.json")
	ps := LoadPoolState(path, nil)
	// Ничего не менялось: файл создаваться не должен.
	if err := ps.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("файл создан без изменений: %v", err)
	}
}

func TestPoolStateBrokenFileStartsClean(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.json")
	if err := os.WriteFile(path, []byte("{это не json"), 0o600); err != nil {
		t.Fatal(err)
	}

	ps := LoadPoolState(path, nopLogger{})
	if ps == nil {
		t.Fatal("состояние не создано")
	}
	if d, b, s := ps.Counts(); d != 0 || b != 0 || s != 0 {
		t.Errorf("битый файл дал записи: %d/%d/%d", d, b, s)
	}
	// Состояние обязано остаться рабочим, а не залипнуть на битом файле.
	ps.MarkDead("http://a:1", "умер")
	if why := ps.Skip("http://a:1", ""); why == "" {
		t.Error("после битого файла состояние не работает")
	}
}

func TestPoolStateMissingFile(t *testing.T) {
	ps := LoadPoolState(filepath.Join(t.TempDir(), "нет.json"), nil)
	if ps == nil {
		t.Fatal("состояние не создано")
	}
	if d, b, s := ps.Counts(); d != 0 || b != 0 || s != 0 {
		t.Errorf("записи из ниоткуда: %d/%d/%d", d, b, s)
	}
}

func TestPoolStateRememberExitIdempotent(t *testing.T) {
	ps := LoadPoolState("", nil)
	ps.RememberExit("http://a:1", "")
	if got := ps.ExitIP("http://a:1"); got != "" {
		t.Errorf("пустой IP записан: %q", got)
	}
	ps.RememberExit("http://a:1", "1.2.3.4")
	if got := ps.ExitIP("http://a:1"); got != "1.2.3.4" {
		t.Errorf("ExitIP=%q", got)
	}
	// Повтор той же записи не должен менять метку времени: иначе запись
	// никогда не прунится как устаревшая.
	ps.RememberExit("http://a:1", "1.2.3.4")
	if got := ps.ExitIP("http://a:1"); got != "1.2.3.4" {
		t.Errorf("ExitIP после повтора=%q", got)
	}
	// Чужой адрес не должен наследовать exit IP соседа.
	if got := ps.ExitIP("http://b:2"); got != "" {
		t.Errorf("чужому адресу приписан IP %q", got)
	}
}

func TestPoolStateMarkDeadClearsStrikes(t *testing.T) {
	ps := LoadPoolState("", nil)
	ps.MarkSuspect("http://a:1", "1.2.3.4", "раз")
	ps.MarkSuspect("http://a:1", "1.2.3.4", "два")
	// Смерть перекрывает страйки: адрес уже мёртв, копить strikes дальше
	// бессмысленно, а старый счётчик дал бы мгновенный бан после оживления.
	ps.MarkDead("http://a:1", "умер")

	e := ps.st.Proxies["http://a:1"]
	if e.Strikes != 0 || !e.StrikeAt.IsZero() {
		t.Errorf("страйки не сброшены: %d", e.Strikes)
	}
	if e.DeadAt.IsZero() {
		t.Error("метка смерти не проставлена")
	}
}

func TestPoolStateSuspectStrikeResetAfterTTL(t *testing.T) {
	ps := LoadPoolState("", nil)
	ps.MarkSuspect("http://a:1", "1.2.3.4", "старый страйк")
	// Страйк устарел: следующий должен начать счёт заново, иначе два
	// нарушения с разницей в неделю дают бан.
	e := ps.st.Proxies["http://a:1"]
	e.StrikeAt = time.Now().Add(-stateStrikeTTL - time.Hour)
	ps.st.Proxies["http://a:1"] = e

	ps.MarkSuspect("http://a:1", "1.2.3.4", "новый страйк")
	if got := ps.st.Proxies["http://a:1"].Strikes; got != 1 {
		t.Errorf("страйков %d, ожидала 1", got)
	}
	if why := ps.Skip("http://a:1", ""); why != "" {
		t.Errorf("бан после одного страйка: %s", why)
	}
}

func TestNetSlash24(t *testing.T) {
	cases := map[string]string{
		"1.2.3.4":     "1.2.3.0/24",
		"9.9.9.255":   "9.9.9.0/24",
		"мусор":       "",
		"":            "",
		"::1":         "",
		"2001:db8::1": "",
	}
	for in, want := range cases {
		if got := netSlash24(in); got != want {
			t.Errorf("netSlash24(%q)=%q, ожидала %q", in, got, want)
		}
	}
}

func TestEntryStateExpiredLocked(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		e    entryState
		want bool
	}{
		{"пустая", entryState{}, true},
		{"свежая смерть", entryState{DeadAt: now.Add(-time.Hour)}, false},
		{"старая смерть", entryState{DeadAt: now.Add(-stateDeadTTL - time.Hour)}, true},
		{"действующий бан", entryState{BanUntil: now.Add(time.Hour)}, false},
		{"просроченный бан", entryState{BanUntil: now.Add(-time.Hour)}, true},
		{"свежий страйк", entryState{Strikes: 1, StrikeAt: now.Add(-time.Hour)}, false},
		{"старый страйк", entryState{Strikes: 1, StrikeAt: now.Add(-stateStrikeTTL - time.Hour)}, true},
		{"свежий exit IP", entryState{ExitIP: "1.1.1.1", SeenAt: now.Add(-time.Hour)}, false},
		{"старый exit IP", entryState{ExitIP: "1.1.1.1", SeenAt: now.Add(-stateSeenTTL - time.Hour)}, true},
	}
	for _, c := range cases {
		if got := c.e.expiredLocked(now); got != c.want {
			t.Errorf("%s: expired=%v, ожидала %v", c.name, got, c.want)
		}
	}
}

func TestProxyPoolExitIPMatchesWithCredentials(t *testing.T) {
	// Один и тот же прокси может прийти с логином и без: сравнение идёт по
	// маске, иначе exit IP теряется и бан по IP не срабатывает.
	pool := &ProxyPool{cfg: PoolConfig{MaxLive: 5, MinLive: 1, RefillWait: time.Hour}, dead: map[string]int{}}
	pool.add(LiveProxy{Spec: "socks5://user:pass@1.2.3.4:1080", ExitIP: "8.8.8.8"})

	if got := pool.ExitIP("socks5://1.2.3.4:1080"); got != "8.8.8.8" {
		t.Errorf("ExitIP без кредов=%q", got)
	}
	if got := pool.ExitIP("socks5://other:9999"); got != "" {
		t.Errorf("чужой адрес дал %q", got)
	}
}

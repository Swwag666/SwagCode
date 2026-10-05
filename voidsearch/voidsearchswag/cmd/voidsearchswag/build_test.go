package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"voidsearchswag/internal/catalog"
	"voidsearchswag/internal/config"
	"voidsearchswag/internal/discover"
	"voidsearchswag/internal/netx"
	"voidsearchswag/internal/search"
	"voidsearchswag/internal/store"
)

// directConfig собирает конфиг, в котором ядро поднимается без tor и без
// сети: транспорт direct, rotator не создаётся, поэтому тесты детерминированы.
func directConfig() config.Config {
	cfg, err := config.Load()
	if err != nil {
		panic(err)
	}
	cfg.Tor = "off"
	cfg.Transport = "direct"
	cfg.RequestTimeout = 2 * time.Second
	cfg.Headless = false
	cfg.DiscoverConcurrency = 2
	cfg.DiscoverMaxHosts = 5
	cfg.DiscoverPerHostDelay = time.Millisecond
	cfg.DiscoverDepth = 1
	return cfg
}

func TestBuildEngineDirect(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)

	cfg := directConfig()
	st := openTestStore(t, dir)
	defer closeStore(st)

	eng, cleanup := buildEngine(cfg, stderrLogger{}, st)
	defer cleanup()

	if eng == nil {
		t.Fatal("движок не собран")
	}
	if eng.Client == nil {
		t.Error("клиент не собран")
	}
	// Tor выключен, поэтому ротатора быть не должно: иначе запросы ушли бы
	// через несуществующий SOCKS и упали бы на соединении.
	if eng.Rotator() != nil {
		t.Error("ротатор создан при выключенном tor")
	}
	if eng.Store != st {
		t.Error("база не проброшена в движок")
	}
	// Дефолтные onion-сиды дают движки даже без сети: они понадобятся в
	// deep-режиме, но сами по себе запросов не делают.
	if got := onionCount(eng); got == 0 {
		t.Error("onion-движки не собраны из дефолтных сидов")
	}
	if eng.Browser != nil {
		t.Error("браузер собран при Headless=false")
	}
	// Ahmia-мост собирается всегда и без ключей (на direct - без отдельного клиента).
	if eng.Ahmia == nil {
		t.Error("ahmia-мост не собран")
	}
}

func TestBuildEngineTorTransportFallsBackToDirect(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)

	cfg := directConfig()
	// Транспорт tor без поднятого rotator не имеет смысла: buildEngine
	// обязан заменить его на direct, иначе клиент не соберётся вовсе.
	cfg.Transport = "tor"
	st := openTestStore(t, dir)
	defer closeStore(st)

	eng, cleanup := buildEngine(cfg, stderrLogger{}, st)
	defer cleanup()

	if eng == nil || eng.Client == nil {
		t.Fatal("движок не собрался на транспорте tor без rotator")
	}
	if eng.Rotator() != nil {
		t.Error("ротатор появился при выключенном tor")
	}
}

func TestBuildEngineWithCustomSeeds(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)

	cfg := directConfig()
	cfg.OnionEngines = "мой|http://abcdefghijklmnop.onion"
	st := openTestStore(t, dir)
	defer closeStore(st)

	eng, cleanup := buildEngine(cfg, stderrLogger{}, st)
	defer cleanup()

	if got := onionCount(eng); got != 1 {
		t.Errorf("движков %d, ожидала 1 из своего сида", got)
	}
	if eng.Onion.Engines[0].Name_ != "мой" {
		t.Errorf("имя сида не проброшено: %q", eng.Onion.Engines[0].Name_)
	}
}

func TestBuildEngineBadSeedsFallsBackToDefaults(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)

	cfg := directConfig()
	// Мусорный сид не должен ронять сборку: ParseSeeds возвращает дефолты.
	cfg.OnionEngines = "битый-сид-без-базы"
	st := openTestStore(t, dir)
	defer closeStore(st)

	eng, cleanup := buildEngine(cfg, stderrLogger{}, st)
	defer cleanup()

	if got := onionCount(eng); got == 0 {
		t.Error("на мусорном сиде движки пропали вместо отката к дефолтам")
	}
}

func TestBuildDiscoverWithEngine(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)

	cfg := directConfig()
	st := openTestStore(t, dir)
	defer closeStore(st)

	eng, cleanup := buildEngine(cfg, stderrLogger{}, st)
	defer cleanup()

	pool := buildDiscover(cfg, stderrLogger{}, st, eng)
	if pool == nil {
		t.Fatal("discovery-слой не собрался")
	}
	if pool.Store != st {
		t.Error("база не проброшена в пул")
	}
	if pool.Finder == nil {
		t.Error("Finder не собран")
	}
	if pool.Crawl == nil {
		t.Error("Crawler не собран")
	}
	// Finder обязан получить повтор на транзиентные сбои: один таймаут на
	// тяжёлом каталоге не должен выбрасывать источник из прогона.
	if got := pool.Finder.Attempts; got != 0 && got < 2 {
		t.Errorf("попыток у Finder %d", got)
	}
}

func TestBuildDiscoverUsesOwnDirectClient(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)

	cfg := directConfig()
	st := openTestStore(t, dir)
	defer closeStore(st)

	eng, cleanup := buildEngine(cfg, stderrLogger{}, st)
	defer cleanup()

	pool := buildDiscover(cfg, stderrLogger{}, st, eng)
	if pool == nil {
		t.Fatal("пул не собрался")
	}
	// Каталог-источники лежат в clearnet: через tor exit-ноды отдают
	// challenge Cloudflare вместо страницы. Поэтому Finder обязан получить
	// свой прямой клиент, а не тор-клиент ядра.
	if pool.Finder.Client == nil {
		t.Error("у Finder нет клиента")
	}
	if pool.Crawl.Client == nil {
		t.Error("у Crawler нет клиента")
	}
}

func TestBuildProberWithEngine(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)

	cfg := directConfig()
	// Значения намеренно разные у обхода и у проб. Если пробер снова возьмёт
	// обходные настройки, тест это поймает: при одинаковых числах регресс
	// прошёл бы незамеченным.
	cfg.DiscoverConcurrency = 3
	cfg.RequestTimeout = 41 * time.Second
	cfg.ProbeConcurrency = 14
	cfg.ProbeTimeout = 17 * time.Second

	st := openTestStore(t, dir)
	defer closeStore(st)

	eng, cleanup := buildEngine(cfg, stderrLogger{}, st)
	defer cleanup()

	p := buildProber(cfg, stderrLogger{}, st, eng, 0)
	if p == nil {
		t.Fatal("пробер не собрался")
	}
	if p.Store != st {
		t.Error("база не проброшена в пробер")
	}
	if p.Cfg.Concurrency != cfg.ProbeConcurrency {
		t.Errorf("конкурентность %d, ожидала пробную %d", p.Cfg.Concurrency, cfg.ProbeConcurrency)
	}
	if p.Cfg.Timeout != cfg.ProbeTimeout {
		t.Errorf("таймаут %v, ожидала пробный %v", p.Cfg.Timeout, cfg.ProbeTimeout)
	}
	if p.Cfg.Delay != cfg.DiscoverPerHostDelay {
		t.Errorf("пауза %v, ожидала значение конфига %v: ноль флага означает «взять из конфига»",
			p.Cfg.Delay, cfg.DiscoverPerHostDelay)
	}
	if p.Cfg.Concurrency == cfg.DiscoverConcurrency {
		t.Error("пробер снова привязан к обходной конкурентности")
	}
	if p.Cfg.Timeout == cfg.RequestTimeout {
		t.Error("пробер снова привязан к полному таймауту запроса")
	}
}

func TestProbeSettingsHaveOwnDefaults(t *testing.T) {
	// Проба не должна наследовать обходные значения по умолчанию: обход идёт
	// осторожно (4 воркера) и с полным таймаутом запроса, а проба проверяет
	// только живость. Совпадение дефолтов означало бы, что отдельная настройка
	// ничего не меняет.
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ProbeConcurrency <= 0 {
		t.Errorf("пробная конкурентность %d", cfg.ProbeConcurrency)
	}
	if cfg.ProbeTimeout <= 0 {
		t.Errorf("пробный таймаут %v", cfg.ProbeTimeout)
	}
	if cfg.ProbeConcurrency < cfg.DiscoverConcurrency {
		t.Errorf("пробный параллелизм %d ниже обходного %d: изолированные цепи tor позволяют больше",
			cfg.ProbeConcurrency, cfg.DiscoverConcurrency)
	}
	if cfg.ProbeTimeout >= cfg.RequestTimeout {
		t.Errorf("пробный таймаут %v не меньше полного %v: мёртвый хост займёт воркер надолго",
			cfg.ProbeTimeout, cfg.RequestTimeout)
	}
}

func TestBuildCollectorWithEngine(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)

	cfg := directConfig()
	st := openTestStore(t, dir)
	defer closeStore(st)

	eng, cleanup := buildEngine(cfg, stderrLogger{}, st)
	defer cleanup()

	col := buildCollector(cfg, stderrLogger{}, st, eng)
	if col == nil {
		t.Fatal("сборщик не собрался")
	}
	if col.Store != st {
		t.Error("база не проброшена в сборщик")
	}
}

func TestCmdCollectEmptyPool(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)
	t.Setenv("VOIDSEARCH_TOR", "off")
	t.Setenv("VOIDSEARCH_TRANSPORT", "direct")
	t.Setenv("VOIDSEARCH_REQUEST_TIMEOUT", "2s")

	// Пул пуст: команда обязана объяснить, что делать, а не молча выйти
	// или начать обход несуществующих хостов.
	out := captureStdout(t, func() { cmdCollect(nil) })
	if !strings.Contains(out, "пул пуст") {
		t.Errorf("пустой пул не объяснён: %q", out)
	}
	if !strings.Contains(out, "discover") {
		t.Errorf("подсказка про discover не показана: %q", out)
	}
}

func TestCmdCollectEmptyPoolJSON(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)
	t.Setenv("VOIDSEARCH_TOR", "off")
	t.Setenv("VOIDSEARCH_TRANSPORT", "direct")

	// До этапа 89 тест требовал только наличия подстроки «пул пуст», поэтому он
	// одинаково проходил и при тексте, и при JSON: страж не отличал одно от
	// другого, а команда печатала подсказку в stdout вместо отчёта.
	out := captureStdout(t, func() { cmdCollect([]string{"--json"}) })
	var rep catalog.Report
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("JSON-режим на пустом пуле не отдаёт отчёт: %v (%.200s)", err, out)
	}
	if !strings.Contains(rep.Note, "пул пуст") {
		t.Errorf("note = %q, хочу причину про пустой пул", rep.Note)
	}
}

func TestCmdProbeWithoutTorSkipsAddress(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)
	t.Setenv("VOIDSEARCH_TOR", "off")
	t.Setenv("VOIDSEARCH_TRANSPORT", "direct")
	t.Setenv("VOIDSEARCH_REQUEST_TIMEOUT", "2s")

	// Транспорт direct и onion-адрес: запрос не может уйти в сеть. Отчёт обязан
	// назвать это пропуском, а не смертью адреса, и не писать вердикт в базу.
	// Прогон идёт в подпроцессе: несостоявшаяся волна завершает команду через
	// os.Exit, и in-process вызов убил бы тестовый процесс.
	out, _, code := runMainSplit(t, "probe", "--addr", "abcdefghijklmnop.onion", "--timeout", "20s")
	if code != 1 {
		t.Errorf("код возврата %d, хочу 1: %s", code, firstN(out, 300))
	}
	if !strings.Contains(out, "пробы:") {
		t.Errorf("заголовок отчёта не напечатан: %q", out)
	}
	if !strings.Contains(out, "живых 0 из 1") {
		t.Errorf("счётчик неверен: %q", out)
	}
	if !strings.Contains(out, "пропущено 1: нужен tor или прокси") {
		t.Errorf("пропуск не назван в итоговой строке: %q", out)
	}
	// Прежняя формулировка «нет ответа» утверждала, что сервис промолчал.
	// Вместе с ней проба писала в пул отказ, и три таких запуска переводили
	// живой адрес в dead: порог fail_streak срабатывает на третьей неудаче.
	if !strings.Contains(out, "нет tor") {
		t.Errorf("исход пробы не показан: %q", out)
	}
	if strings.Contains(out, "нет ответа") || strings.Contains(out, "мертв") {
		t.Errorf("вывод утверждает вердикт о сервисе, которого не спрашивали: %q", out)
	}
	if strings.Contains(out, "пул:") {
		t.Errorf("напечатан статус пула для пробы, которой не было: %q", out)
	}
	if !strings.Contains(out, "abcdefghijklmnop.onion") {
		t.Errorf("адрес не показан: %q", out)
	}

	// В базу не должно попасть ничего: запись «unknown» с растущим fail_streak
	// и обнулённой долей успехов портила статистику живости.
	st, err := store.Open(dir + "/voidsearchswag.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	statuses, err := st.KnownStatuses(t.Context(), []string{"abcdefghijklmnop.onion"})
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 0 {
		t.Errorf("проба без tor записана в базу: %v", statuses)
	}
}

func TestCmdProbeThroughDeadProxyWritesFailure(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)
	t.Setenv("VOIDSEARCH_TOR", "off")
	t.Setenv("VOIDSEARCH_TRANSPORT", "static")
	t.Setenv("VOIDSEARCH_PROXIES", "127.0.0.1:1")
	t.Setenv("VOIDSEARCH_REQUEST_TIMEOUT", "2s")

	// Обратная сторона: прокси задан, onion-барьер пройден, запрос ушёл и упал на
	// соединении. Это отказ, и он обязан печататься и записываться как раньше.
	out := captureStdout(t, func() {
		cmdProbe([]string{"--addr", "abcdefghijklmnop.onion", "--timeout", "20s"})
	})
	if !strings.Contains(out, "живых 0 из 1") {
		t.Errorf("счётчик неверен: %q", out)
	}
	if strings.Contains(out, "пропущено") || strings.Contains(out, "нет tor") {
		t.Errorf("настоящий отказ записан в пропуски: %q", out)
	}
	if !strings.Contains(out, "нет ответа") {
		t.Errorf("исход пробы не показан: %q", out)
	}

	st, err := store.Open(dir + "/voidsearchswag.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	statuses, err := st.KnownStatuses(t.Context(), []string{"abcdefghijklmnop.onion"})
	if err != nil {
		t.Fatal(err)
	}
	if statuses["abcdefghijklmnop.onion"] == "" {
		t.Errorf("отказ не записан в базу: %v", statuses)
	}
}

func TestCmdProbeEmptyPool(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)
	t.Setenv("VOIDSEARCH_TOR", "off")
	t.Setenv("VOIDSEARCH_TRANSPORT", "direct")
	t.Setenv("VOIDSEARCH_REQUEST_TIMEOUT", "2s")

	// Подпроцесс: пустая волна завершается нулём, но правило должно оставаться
	// проверяемым и на мутациях, которые дают единицу. In-process вызов в таком
	// случае убивает тестовый процесс через os.Exit, и вместо внятного падения
	// остаётся оборванный вывод.
	out, _, code := runMainSplit(t, "probe", "--limit", "5", "--timeout", "20s")
	if code != 0 {
		t.Errorf("код возврата %d при пустом пуле: %s", code, firstN(out, 300))
	}
	if !strings.Contains(out, "пробы:") {
		t.Errorf("отчёт на пустом пуле не напечатан: %q", out)
	}
	if !strings.Contains(out, "живых 0 из 0") {
		t.Errorf("счётчик на пустом пуле неверен: %q", out)
	}
}

func TestCmdProbeJSON(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)
	t.Setenv("VOIDSEARCH_TOR", "off")
	t.Setenv("VOIDSEARCH_TRANSPORT", "direct")
	t.Setenv("VOIDSEARCH_REQUEST_TIMEOUT", "2s")

	// Подпроцесс по той же причине: команда завершается os.Exit. Поток разделён,
	// потому что предмет проверки - JSON именно в stdout.
	out, _, code := runMainSplit(t, "probe", "--addr", "abcdefghijklmnop.onion", "--json", "--timeout", "20s")
	if code != 1 {
		t.Errorf("код возврата %d, хочу 1: %s", code, firstN(out, 300))
	}
	if !strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Errorf("JSON-отчёт не объект: %q", out)
	}
	// В discover JSON-теги snake_case: ключи проверяются по тегам, а не по
	// именам полей, иначе тест соврал бы о формате вывода.
	for _, key := range []string{`"live"`, `"dead"`, `"total"`, `"results"`} {
		if !strings.Contains(out, key) {
			t.Errorf("в отчёте нет %s: %q", key, out)
		}
	}
	if !strings.Contains(out, "abcdefghijklmnop.onion") {
		t.Errorf("адрес пропал из JSON: %q", out)
	}
}

func TestCmdOnioncheckNoTorReportsDead(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)
	t.Setenv("VOIDSEARCH_TRANSPORT", "direct")
	t.Setenv("VOIDSEARCH_REQUEST_TIMEOUT", "2s")

	// Без tor onion-движки не разрешаются: отчёт обязан показать их
	// мёртвыми, а не зависнуть и не соврать, что всё живо. Прогон идёт в
	// подпроцессе: несостоявшаяся волна завершает команду через os.Exit, и
	// in-process вызов убил бы тестовый процесс.
	out, _, code := runMainSplit(t, "onioncheck", "--no-tor", "--timeout", "25s")
	if code != 1 {
		t.Errorf("код возврата %d, хочу 1: %s", code, firstN(out, 300))
	}
	if !strings.Contains(out, "onion-поисковики:") {
		t.Errorf("заголовок отчёта не напечатан: %q", out)
	}
	if !strings.Contains(out, "живых 0 из") {
		t.Errorf("без tor движки не отчитаны как мёртвые: %q", out)
	}
	if !strings.Contains(out, "torch") {
		t.Errorf("имена движков не показаны: %q", out)
	}
}

func TestProbeReportShape(t *testing.T) {
	// Форма отчёта, который печатает cmdProbe: проверяется напрямую, чтобы
	// смена полей не прошла мимо тестов.
	rep := discover.ProbeReport{
		Total: 3,
		Live:  1,
		Dead:  2,
		Results: []discover.ProbeResult{
			{URL: "a.onion", OK: true, LatencyMS: 120},
			{URL: "b.onion", OK: false, Error: "нет соединения"},
			{URL: "c.onion", OK: false, Status: 503, Error: "HTTP Service Unavailable"},
		},
	}
	if len(rep.Results) != rep.Total {
		t.Errorf("Total=%d при %d результатах", rep.Total, len(rep.Results))
	}
	if rep.Live+rep.Dead != rep.Total {
		t.Errorf("живые %d + мёртвые %d != %d", rep.Live, rep.Dead, rep.Total)
	}
}

func TestNetxLoggerInterfaceSatisfied(t *testing.T) {
	// Оба логгера обязаны удовлетворять netx.Logger: иначе сборка ядра
	// не компилируется на месте вызова.
	var _ netx.Logger = stderrLogger{}
	var _ netx.Logger = zlogAdapter{}
}

func TestEngineCleanupIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)

	cfg := directConfig()
	st := openTestStore(t, dir)
	defer closeStore(st)

	eng, cleanup := buildEngine(cfg, stderrLogger{}, st)
	if eng == nil {
		t.Fatal("движок не собран")
	}
	cleanup()
	// Повторный вызов не должен паниковать: клиент уже закрыт, а rotator nil.
	cleanup()
	var _ *search.Engine = eng
}

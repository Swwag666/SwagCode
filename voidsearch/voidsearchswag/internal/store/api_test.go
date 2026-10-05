package store

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"math/rand"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func nullString(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}

func newStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return st
}

func TestCacheKeyStableAndDistinct(t *testing.T) {
	a := CacheKey("Leak Test", "fast")
	b := CacheKey("leak test", "fast")
	if a != b {
		t.Error("ключ должен игнорировать регистр и пробелы по краям")
	}
	if a == CacheKey("leak test", "deep") {
		t.Error("разные режимы должны давать разные ключи")
	}
	if a == CacheKey("another query", "fast") {
		t.Error("разные запросы не должны совпадать")
	}
	if len(a) != 64 {
		t.Errorf("длина ключа %d, ожидала 64 (sha256 hex)", len(a))
	}
}

func TestCacheKeyIgnoresLimitByDesign(t *testing.T) {
	// limit намеренно не входит в ключ: один запрос в одном режиме - одна
	// запись, а запись на 20 результатов законно обслуживает запрос на 5
	// обрезкой. Плодить копию выдачи под каждый limit незачем.
	//
	// Защиту от недостачи держит не ключ, а сравнение Outcome.Limit при
	// чтении - это проверяется в internal/search.
	if CacheKey("leak test", "fast") != CacheKey("leak test", "fast") {
		t.Error("ключ нестабилен для одинаковых аргументов")
	}
	if CacheKey("leak test", "fast") == CacheKey("leak test", "deep") {
		t.Error("режим перестал влиять на ключ")
	}
}

func TestQueryHashFormatIsFrozen(t *testing.T) {
	// QueryHash - идентификатор запроса для голосов. Его схема заморожена
	// золотым значением: хеши уже лежат в таблице голосов, и любое изменение
	// формулы молча обнулит накопленные оценки - старые записи перестанут
	// находиться, а новые лягут рядом как другие запросы.
	//
	// Тест на совпадение с CacheKey здесь бессмыслен: сейчас формулы одинаковы,
	// и это нормально. Важно другое - если кто-то решит добавить в CacheKey
	// параметр (например limit), он обязан сделать это отдельной функцией, а
	// не правкой общей, иначе голоса поедут. Золотое значение это и ловит.
	const golden = "c88e6ed186d7affdd3bd80da489868704856e775ceb29971d1004eb3d824e6b2"
	if got := QueryHash("leak database", "judge"); got != golden {
		t.Errorf("схема QueryHash изменилась: %s, ожидала %s", got, golden)
	}
	if QueryHash("Leak Test", "judge") != QueryHash("leak test", "judge") {
		t.Error("QueryHash должен игнорировать регистр и пробелы")
	}
	if QueryHash("q", "judge") == QueryHash("q", "fast") {
		t.Error("разные области дали один хеш")
	}
}

func TestCachePutGetRoundtrip(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	key := CacheKey("q", "fast")

	if _, ok, err := st.CacheGet(ctx, key); err != nil || ok {
		t.Fatalf("пустой кэш: ok=%v err=%v, ожидала false/nil", ok, err)
	}
	if err := st.CachePut(ctx, key, "fast", `{"hits":3}`, time.Hour); err != nil {
		t.Fatalf("put: %v", err)
	}
	payload, ok, err := st.CacheGet(ctx, key)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !ok {
		t.Fatal("запись не найдена после put")
	}
	if payload != `{"hits":3}` {
		t.Errorf("payload=%q", payload)
	}
}

func TestCachePutOverwrites(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	key := CacheKey("q", "fast")
	if err := st.CachePut(ctx, key, "fast", "first", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := st.CachePut(ctx, key, "fast", "second", time.Hour); err != nil {
		t.Fatal(err)
	}
	payload, ok, err := st.CacheGet(ctx, key)
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if payload != "second" {
		t.Errorf("payload=%q, ожидала second (upsert)", payload)
	}
	var n int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM cache`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("строк %d, ожидала 1", n)
	}
}

func TestCacheExpiredIsMiss(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	key := CacheKey("q", "fast")
	if err := st.CachePut(ctx, key, "fast", "stale", -time.Second); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := st.CacheGet(ctx, key); err != nil || ok {
		t.Fatalf("протухшая запись: ok=%v err=%v, ожидала false/nil", ok, err)
	}
}

func TestOnionUpsertAndGet(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.UpsertOnion(ctx, Onion{
		URL: "http://x.onion", Status: "live", Category: "market",
		LatencyAvg: 120, SuccessRate: 0.9,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := st.GetOnion(ctx, "http://x.onion")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != "live" || got.Category != "market" {
		t.Errorf("status=%q category=%q", got.Status, got.Category)
	}
	if got.LatencyAvg != 120 {
		t.Errorf("latency=%d, ожидала 120", got.LatencyAvg)
	}
	if got.FirstSeen.IsZero() {
		t.Error("first_seen не проставлен")
	}
}

func TestOnionUpdateKeepsCategoryWhenEmpty(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.UpsertOnion(ctx, Onion{URL: "http://x.onion", Status: "live", Category: "forum"}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertOnion(ctx, Onion{URL: "http://x.onion", Status: "dead", SuccessRate: 0.1}); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetOnion(ctx, "http://x.onion")
	if err != nil {
		t.Fatal(err)
	}
	if got.Category != "forum" {
		t.Errorf("category=%q, ожидала forum (пустая не должна затирать)", got.Category)
	}
	if got.Status != "dead" {
		t.Errorf("status=%q, ожидала dead", got.Status)
	}
}

func TestOnionGetMissing(t *testing.T) {
	st := newStore(t)
	if _, err := st.GetOnion(context.Background(), "http://nope.onion"); err != ErrNotFound {
		t.Errorf("err=%v, ожидала ErrNotFound", err)
	}
}

func TestOnionEmptyURLRejected(t *testing.T) {
	st := newStore(t)
	if err := st.UpsertOnion(context.Background(), Onion{URL: "  "}); err == nil {
		t.Error("пустой url должен отклоняться")
	}
}

func TestNextUnprobedCoversQueue(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	for _, u := range []string{"a.onion", "b.onion", "c.onion", "d.onion", "e.onion"} {
		if err := st.UpsertOnion(ctx, Onion{URL: u, Status: "unknown"}); err != nil {
			t.Fatal(err)
		}
	}

	// Первая волна берёт начало очереди.
	first, err := st.NextProbeWave(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 {
		t.Fatalf("первая волна: %d адресов", len(first))
	}

	// Проверяем их: неудача растит fail_streak, но статус остаётся unknown.
	for _, u := range first {
		if err := st.RecordProbe(ctx, u, false, 0); err != nil {
			t.Fatal(err)
		}
	}

	// Вторая волна не должна повторять уже проверенные: иначе хвост пула
	// не проверяется никогда.
	second, err := st.NextProbeWave(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range second {
		for _, done := range first {
			if u == done {
				t.Errorf("адрес %s выдан повторно", u)
			}
		}
	}
}

func TestNextUnprobedPrefersNeverProbed(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	if err := st.UpsertOnion(ctx, Onion{URL: "probed.onion", Status: "unknown"}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordProbe(ctx, "probed.onion", false, 0); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertOnion(ctx, Onion{URL: "fresh.onion", Status: "unknown"}); err != nil {
		t.Fatal(err)
	}

	got, err := st.NextProbeWave(ctx, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("адресов %d, ожидала 2: %+v", len(got), got)
	}
	if got[0] != "fresh.onion" {
		t.Errorf("первым %q, ожидала fresh.onion (ни разу не проверялся)", got[0])
	}
}

// Этап 176: контракт развёрнут. Прежний тест (SkipsLiveAndDead) закреплял
// выборку только по unknown: проверенные записи волна не видела, и пул
// заморожен навечно. Теперь live и dead входят в очередь охвата после
// unknown - по старшинству last_probe.
func TestNextUnprobedRefreshesLiveAndDead(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	if err := st.UpsertOnion(ctx, Onion{URL: "unknown.onion", Status: "unknown"}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordProbe(ctx, "unknown.onion", false, 0); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertOnion(ctx, Onion{URL: "live.onion"}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordProbe(ctx, "live.onion", true, 500); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertOnion(ctx, Onion{URL: "dead.onion"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := st.RecordProbe(ctx, "dead.onion", false, 0); err != nil {
			t.Fatal(err)
		}
	}

	got, err := st.NextProbeWave(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"unknown.onion", "live.onion", "dead.onion"}
	if len(got) != len(want) {
		t.Fatalf("адресов %d, ожидала %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("позиция %d: %q, ожидала %q (порядок: unknown, затем live с меньшим fail_streak, затем dead)", i, got[i], want[i])
		}
	}
}

// Этап 176: освежение берёт записи по старшинству. Две live с разным
// last_probe - вперёд та, что не пробовалась дольше; peer-слитая запись
// без last_probe - впереди всех, как ни разу не проверенная.
func TestNextUnprobedTakesStaleByAge(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	for _, u := range []string{"old.onion", "new.onion"} {
		if err := st.UpsertOnion(ctx, Onion{URL: u}); err != nil {
			t.Fatal(err)
		}
		if err := st.RecordProbe(ctx, u, true, 500); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.db.Exec(`UPDATE onion_pool SET last_probe='2021-01-01 00:00:00' WHERE url='old.onion'`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`UPDATE onion_pool SET last_probe='2023-01-01 00:00:00' WHERE url='new.onion'`); err != nil {
		t.Fatal(err)
	}
	if err := st.MergePeerOnion(ctx, "peer.onion", "unknown", "", ""); err != nil {
		t.Fatal(err)
	}

	got, err := st.NextProbeWave(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "peer.onion" || got[1] != "old.onion" {
		t.Fatalf("выборка %+v, ожидала [peer.onion old.onion]: peer без last_probe впереди, из устаревших - старейшая", got)
	}
}

// Этап 176: прогрев вперёд освежения. Пока в пуле есть непроверенные,
// волна тратит потолок на них, а не на устаревшие live - иначе живой пул
// в тысячи unknown месяцами не разбирался бы вовсе.
func TestNextUnprobedKeepsUnprobedPriority(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		u := fmt.Sprintf("u%02d.onion", i)
		if err := st.UpsertOnion(ctx, Onion{URL: u, Status: "unknown"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.UpsertOnion(ctx, Onion{URL: "stale.onion"}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordProbe(ctx, "stale.onion", true, 500); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`UPDATE onion_pool SET last_probe='2020-01-01 00:00:00' WHERE url='stale.onion'`); err != nil {
		t.Fatal(err)
	}

	got, err := st.NextProbeWave(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("адресов %d, ожидала 3: %+v", len(got), got)
	}
	for _, u := range got {
		if u == "stale.onion" {
			t.Errorf("устаревшая live взята при живой очереди неизвестных %+v", got)
		}
	}
}

func TestNextUnprobedEmpty(t *testing.T) {
	st := newStore(t)
	got, err := st.NextProbeWave(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("на пустом пуле %+v", got)
	}
}

func TestNextUnprobedDefaultLimit(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	for i := 0; i < 120; i++ {
		u := fmt.Sprintf("h%03d.onion", i)
		if err := st.UpsertOnion(ctx, Onion{URL: u, Status: "unknown"}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.NextProbeWave(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 50 {
		t.Errorf("дефолтный потолок дал %d, ожидала 50", len(got))
	}
}

func TestListOnionsFilterAndOrder(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	rows := []Onion{
		{URL: "http://slow.onion", Status: "live", SuccessRate: 0.5, LatencyAvg: 900},
		{URL: "http://fast.onion", Status: "live", SuccessRate: 0.9, LatencyAvg: 100},
		{URL: "http://mid.onion", Status: "live", SuccessRate: 0.9, LatencyAvg: 300},
		{URL: "http://dead.onion", Status: "dead", SuccessRate: 0.1},
	}
	for _, o := range rows {
		if err := st.UpsertOnion(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	live, err := st.ListOnions(ctx, "live", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 3 {
		t.Fatalf("живых %d, ожидала 3", len(live))
	}
	if live[0].URL != "http://fast.onion" {
		t.Errorf("первый %q, ожидала fast (сортировка по score/latency)", live[0].URL)
	}
	if live[2].URL != "http://slow.onion" {
		t.Errorf("последний %q, ожидала slow", live[2].URL)
	}
	all, err := st.ListOnions(ctx, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Errorf("всего %d, ожидала 4", len(all))
	}
	limited, err := st.ListOnions(ctx, "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 2 {
		t.Errorf("лимит не сработал: %d", len(limited))
	}
}

func TestOnionStatusBreakdown(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	rows := []Onion{
		{URL: "http://a.onion", Status: "live"},
		{URL: "http://b.onion", Status: "live"},
		{URL: "http://c.onion", Status: "dead"},
		{URL: "http://d.onion", Status: "unknown"},
	}
	for _, o := range rows {
		if err := st.UpsertOnion(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.OnionStatusBreakdown(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got["live"] != 2 {
		t.Errorf("live=%d, ожидала 2", got["live"])
	}
	if got["dead"] != 1 {
		t.Errorf("dead=%d, ожидала 1", got["dead"])
	}
	if got["unknown"] != 1 {
		t.Errorf("unknown=%d, ожидала 1", got["unknown"])
	}
	if len(got) != 3 {
		t.Errorf("статусов %d, ожидала 3: %v", len(got), got)
	}
}

func TestOnionStatusBreakdownEmpty(t *testing.T) {
	st := newStore(t)
	got, err := st.OnionStatusBreakdown(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("на пустой базе %d статусов: %v", len(got), got)
	}
}

func TestOnionStatusBreakdownUsesStatusField(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.UpsertOnion(ctx, Onion{URL: "http://a.onion"}); err != nil {
		t.Fatal(err)
	}
	got, err := st.OnionStatusBreakdown(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got["unknown"] != 1 {
		t.Errorf("запись без статуса должна считаться unknown: %v", got)
	}
}

func TestRecordProbeSuccessMarksLive(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.UpsertOnion(ctx, Onion{URL: "http://a.onion", Status: "unknown"}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordProbe(ctx, "http://a.onion", true, 500); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetOnion(ctx, "http://a.onion")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "live" {
		t.Errorf("status=%q, ожидала live", got.Status)
	}
	if got.LatencyAvg != 500 {
		t.Errorf("latency=%d, ожидала 500 (первый замер берётся как есть)", got.LatencyAvg)
	}
	if got.FailStreak != 0 {
		t.Errorf("fail_streak=%d, ожидала 0", got.FailStreak)
	}
	if got.SuccessRate <= 0 {
		t.Errorf("success_rate=%v, ожидала больше нуля", got.SuccessRate)
	}
}

func TestRecordProbeLatencyIsSmoothed(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.UpsertOnion(ctx, Onion{URL: "http://a.onion", LatencyAvg: 1000, Status: "live"}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordProbe(ctx, "http://a.onion", true, 2000); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetOnion(ctx, "http://a.onion")
	if err != nil {
		t.Fatal(err)
	}
	if got.LatencyAvg != 1250 {
		t.Errorf("latency=%d, ожидала 1250 ((1000*3+2000)/4)", got.LatencyAvg)
	}
}

func TestRecordProbeOneFailureKeepsLive(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.UpsertOnion(ctx, Onion{URL: "http://a.onion", Status: "live"}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordProbe(ctx, "http://a.onion", false, 0); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetOnion(ctx, "http://a.onion")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "live" {
		t.Errorf("одна неудача выбила сервис из пула: status=%q", got.Status)
	}
	if got.FailStreak != 1 {
		t.Errorf("fail_streak=%d, ожидала 1", got.FailStreak)
	}
}

func TestRecordProbeThreeFailuresMarkDead(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.UpsertOnion(ctx, Onion{URL: "http://a.onion", Status: "live"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := st.RecordProbe(ctx, "http://a.onion", false, 0); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.GetOnion(ctx, "http://a.onion")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "dead" {
		t.Errorf("три провала не пометили сервис мёртвым: status=%q", got.Status)
	}
	if got.FailStreak != 3 {
		t.Errorf("fail_streak=%d, ожидала 3", got.FailStreak)
	}
}

func TestRecordProbeSuccessResetsStreak(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.UpsertOnion(ctx, Onion{URL: "http://a.onion", Status: "live"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := st.RecordProbe(ctx, "http://a.onion", false, 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.RecordProbe(ctx, "http://a.onion", true, 300); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetOnion(ctx, "http://a.onion")
	if err != nil {
		t.Fatal(err)
	}
	if got.FailStreak != 0 {
		t.Errorf("успех не сбросил серию провалов: %d", got.FailStreak)
	}
	if got.Status != "live" {
		t.Errorf("status=%q, ожидала live", got.Status)
	}
}

func TestRecordProbeCreatesUnknownURL(t *testing.T) {
	// probe --addr принимает любой валидный onion, а не только ранее
	// собранный. Пропускать такой результат нельзя: иначе адрес никогда не
	// появится в пуле и каждый следующий прогон проверит его заново.
	st := newStore(t)
	ctx := context.Background()

	if err := st.RecordProbe(ctx, "http://nope.onion", true, 100); err != nil {
		t.Fatalf("проба неизвестного адреса: %v", err)
	}

	o, err := st.GetOnion(ctx, "http://nope.onion")
	if err != nil {
		t.Fatalf("запись не появилась в пуле: %v", err)
	}
	if o.Status != "live" {
		t.Errorf("статус %q, ожидала live", o.Status)
	}
	if o.LatencyAvg != 100 {
		t.Errorf("латентность %d, ожидала 100", o.LatencyAvg)
	}
	if o.SuccessRate <= 0 {
		t.Errorf("рейтинг не начислен: %v", o.SuccessRate)
	}
	if o.LastProbe.IsZero() {
		t.Error("время пробы не записано")
	}
}

func TestRecordProbeCreatesUnknownURLThenAccumulates(t *testing.T) {
	// Вторая проба того же адреса обязана идти как обычное накопление, а не
	// как новая вставка: иначе счётчик серии сбросится и мёртвый адрес
	// никогда не достигнет трёх отказов.
	st := newStore(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if err := st.RecordProbe(ctx, "http://dead.onion", false, 0); err != nil {
			t.Fatalf("проба %d: %v", i, err)
		}
	}

	o, err := st.GetOnion(ctx, "http://dead.onion")
	if err != nil {
		t.Fatal(err)
	}
	if o.FailStreak != 3 {
		t.Errorf("серия %d, ожидала 3", o.FailStreak)
	}
	if o.Status != "dead" {
		t.Errorf("статус %q, ожидала dead после трёх отказов", o.Status)
	}
}

func TestRecordProbePreservesMetadata(t *testing.T) {
	// Проба обновляет только живость: заголовок, описание и категория
	// принадлежат каталогу и не должны стираться пустыми значениями.
	st := newStore(t)
	ctx := context.Background()

	if err := st.UpsertOnion(ctx, Onion{
		URL: "http://keep.onion", Status: "unknown",
		Title: "Каталог", Description: "описание", Category: "files",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordProbe(ctx, "http://keep.onion", true, 50); err != nil {
		t.Fatal(err)
	}

	o, err := st.GetOnion(ctx, "http://keep.onion")
	if err != nil {
		t.Fatal(err)
	}
	if o.Title != "Каталог" || o.Description != "описание" || o.Category != "files" {
		t.Errorf("метаданные стёрты пробой: %+v", o)
	}
	if o.Status != "live" {
		t.Errorf("статус %q, ожидала live", o.Status)
	}
}

// TestRecordProbeRejectsEmptyURL проверяет отказ на пустом адресе.
func TestRecordProbeRejectsEmptyURL(t *testing.T) {
	st := newStore(t)
	if err := st.RecordProbe(context.Background(), "   ", true, 10); err == nil {
		t.Error("пустой адрес записан в пул")
	}
}

// До этапа 166 файлы неизвестного размера (size=0) не доставались никаким
// фильтром: min их отсекал, max исключал сознательно, а отдельного входа не
// было. Смоук-агент поймал каталог из 7 нулевых записей и пустой ответ на
// max_size=100. UnknownSize - явный вход к ним.
func TestSearchFilesUnknownSize(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	for _, u := range []string{
		"http://a.onion/known.zip",
		"http://a.onion/unknown1.zip",
		"http://a.onion/unknown2.zip",
	} {
		size := int64(100)
		if strings.Contains(u, "unknown") {
			size = 0
		}
		if err := st.AddFile(ctx, FileEntry{
			TaskID: "t1", URL: u, Filename: filepath.Base(u), Ext: "zip", Size: size,
		}); err != nil {
			t.Fatal(err)
		}
	}

	files, err := st.SearchFiles(ctx, FileQuery{UnknownSize: true, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("unknown_size вернул %d записей, хочу 2 (только нулевые)", len(files))
	}
	for _, f := range files {
		if f.Size != 0 {
			t.Errorf("в выдачу unknown_size попал файл с известным размером: %s (%d)", f.URL, f.Size)
		}
	}

	n, err := st.FileUnknownCount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("FileUnknownCount=%d, хочу 2", n)
	}

	// Size-фильтр как раньше исключает неизвестных: семантика не поменялась,
	// поменялась достижимость.
	files, err = st.SearchFiles(ctx, FileQuery{MaxSize: 1000, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("max_size=1000 вернул %d записей, хочу 1 (неизвестные исключены)", len(files))
	}
}

// UnknownSize отключает размерные фильтры, а не молча комбинирует их:
// сравнивать неизвестный размер с диапазоном нечего.
func TestSearchFilesUnknownSizeIgnoresSizeBounds(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.AddFile(ctx, FileEntry{
		TaskID: "t1", URL: "http://a.onion/u.zip", Filename: "u.zip", Ext: "zip",
	}); err != nil {
		t.Fatal(err)
	}
	files, err := st.SearchFiles(ctx, FileQuery{UnknownSize: true, MinSize: 500, MaxSize: 600, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("unknown_size с min/max вернул %d записей, хочу 1: границы не применимы к неизвестному размеру", len(files))
	}
}

// FileKnown - глаза отчёта сбора на происхождение: до его появления задача
// не могла отличить «файл лёг сейчас» от «файл был известен до меня», и
// saved врало агенту (жалоба смоук-агента этапа 165). Проверяется вся
// тройка исходов: неизвестен, известен, пустой адрес.
func TestFileKnown(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	known, err := st.FileKnown(ctx, "http://a.onion/x.zip")
	if err != nil {
		t.Fatal(err)
	}
	if known {
		t.Fatal("пустой каталог не может знать файл")
	}
	if err := st.AddFile(ctx, FileEntry{
		TaskID: "t1", URL: "http://a.onion/x.zip", Filename: "x.zip", Ext: "zip", Size: 10,
	}); err != nil {
		t.Fatal(err)
	}
	known, err = st.FileKnown(ctx, "http://a.onion/x.zip")
	if err != nil {
		t.Fatal(err)
	}
	if !known {
		t.Fatal("файл в каталоге, но FileKnown его не видит")
	}
	known, err = st.FileKnown(ctx, "")
	if err != nil {
		t.Fatalf("пустой адрес: %v", err)
	}
	if known {
		t.Fatal("пустой адрес не должен считаться известным")
	}
}

// FileSize - источник обогащения повторных находок (этап 169): discover на
// повторном прогоне обязан подставить в file_refs размер, который каталог
// знает с первой записи, - иначе собранное выглядит хуже, чем оно есть.
// Проверяется вся тройка исходов: не записан, записан с размером, записан
// без замера (size=0 обязан читаться как «известен, но не замерен», а не
// подтягиваться как размер).
func TestFileSize(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	size, ok, err := st.FileSize(ctx, "http://a.onion/x.zip")
	if err != nil {
		t.Fatal(err)
	}
	if ok || size != 0 {
		t.Fatalf("пустой каталог: ok=%v size=%d, хочу false/0", ok, size)
	}
	if err := st.AddFile(ctx, FileEntry{
		TaskID: "t1", URL: "http://a.onion/x.zip", Filename: "x.zip", Ext: "zip", Size: 777,
	}); err != nil {
		t.Fatal(err)
	}
	size, ok, err = st.FileSize(ctx, "http://a.onion/x.zip")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || size != 777 {
		t.Fatalf("файл с размером 777: ok=%v size=%d, хочу true/777", ok, size)
	}
	if err := st.AddFile(ctx, FileEntry{
		TaskID: "t1", URL: "http://a.onion/nosize.zip", Filename: "nosize.zip", Ext: "zip",
	}); err != nil {
		t.Fatal(err)
	}
	size, ok, err = st.FileSize(ctx, "http://a.onion/nosize.zip")
	if err != nil {
		t.Fatalf("файл без замера: %v", err)
	}
	if !ok || size != 0 {
		t.Fatalf("известен, но не замерен: ok=%v size=%d, хочу true/0 - ноль не подставляется как размер", ok, size)
	}
	size, ok, err = st.FileSize(ctx, "")
	if err != nil {
		t.Fatalf("пустой адрес: %v", err)
	}
	if ok || size != 0 {
		t.Fatalf("пустой адрес: ok=%v size=%d, хочу false/0", ok, size)
	}
}

// TestFileSnapshots проверяет батч-справку файловой фазы discover (этап
// 170): один заход по списку адресов вместо FileKnown и FileSize по
// одному. Три исхода обязаны читаться так же, как поодиночке: не
// записан, записан с размером, известен без замера (ноль не
// подтягивается как размер). Список длиннее порции 400 - каталог
// обязан собираться из нескольких заходов без потерь на швах, а
// эквивалентность с поодиночным lookup-ом проверяется напрямую по
// каждой строке: расхождение любых двух путей - это свежесть или
// размер, молча посчитанные не так.
func TestFileSnapshots(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	// 410 адресов: порция 400 + хвост 10 на шве.
	const withSize, withoutSize, missing = 400, 6, 4
	urls := make([]string, 0, withSize+withoutSize+missing+1)
	want := map[string]FileSnapshot{}
	for i := 0; i < withSize; i++ {
		u := fmt.Sprintf("http://a.onion/sized%03d.zip", i)
		urls = append(urls, u)
		want[u] = FileSnapshot{Known: true, Size: int64(1000 + i)}
	}
	for i := 0; i < withoutSize; i++ {
		u := fmt.Sprintf("http://a.onion/nosize%03d.zip", i)
		urls = append(urls, u)
		want[u] = FileSnapshot{Known: true}
	}
	for i := 0; i < missing; i++ {
		urls = append(urls, fmt.Sprintf("http://a.onion/missing%03d.zip", i))
	}
	urls = append(urls, "") // пустой адрес - просто пропускается

	// Записи ложатся вразброс по порядку опроса: попадание в карту
	// не должно зависеть от того, в какой порции адрес оказался.
	known := append(append([]string{}, urls[:withSize]...), urls[withSize:withSize+withoutSize]...)
	rand.Shuffle(len(known), func(i, j int) { known[i], known[j] = known[j], known[i] })
	for _, u := range known {
		snap := want[u]
		if err := st.AddFile(ctx, FileEntry{
			TaskID:   "t-batch",
			URL:      u,
			Filename: path.Base(u),
			Ext:      "zip",
			Size:     snap.Size,
		}); err != nil {
			t.Fatalf("подготовка %s: %v", u, err)
		}
	}

	got, err := st.FileSnapshots(ctx, urls)
	if err != nil {
		t.Fatalf("FileSnapshots: %v", err)
	}
	if len(got) != withSize+withoutSize {
		t.Fatalf("карта знает %d адресов, хочу %d: пустой адрес и отсутствующие не попадают", len(got), withSize+withoutSize)
	}
	for _, u := range urls {
		if u == "" {
			if _, ok := got[u]; ok {
				t.Error("пустой адрес попал в карту")
			}
			continue
		}
		g := got[u]
		w := want[u]
		if g.Known != w.Known || g.Size != w.Size {
			t.Errorf("%s: батч дал known=%v size=%d, хочу known=%v size=%d", u, g.Known, g.Size, w.Known, w.Size)
			continue
		}
		// Эквивалентность с поодиночным путём - прямо по строке.
		k, kerr := st.FileKnown(ctx, u)
		if kerr != nil {
			t.Fatalf("FileKnown %s: %v", u, kerr)
		}
		fsz, fok, ferr := st.FileSize(ctx, u)
		if ferr != nil {
			t.Fatalf("FileSize %s: %v", u, ferr)
		}
		if k != w.Known || fok != w.Known || (fok && fsz != w.Size) {
			t.Errorf("%s: одиночный путь дал known=%v size=%d/%v, а батч known=%v size=%d", u, k, fsz, fok, w.Known, w.Size)
		}
	}

	if _, err := st.FileSnapshots(ctx, nil); err != nil {
		t.Errorf("пустой список: %v", err)
	}
}

func TestAddFileSameURLDifferentTasks(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.AddFile(ctx, FileEntry{
		TaskID: "t1", URL: "http://a.onion/x.zip", Filename: "x.zip", Ext: "zip", Size: 10,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddFile(ctx, FileEntry{
		TaskID: "t2", URL: "http://a.onion/x.zip", Filename: "x.zip", Ext: "zip",
	}); err != nil {
		t.Fatal(err)
	}
	files, err := st.SearchFiles(ctx, FileQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("файл размножен по задачам: %d записей", len(files))
	}
	if files[0].Size != 10 {
		t.Errorf("размер потерян при повторной записи: %d", files[0].Size)
	}
	// task_id сохраняет первую задачу, а не перезаписывается последней.
	// Прежнее поведение переносило файл в задачу, которая увидела его позже,
	// из-за чего ListFiles("t1") возвращал пустоту, хотя t1 этот файл нашла.
	if files[0].TaskID != "t1" {
		t.Errorf("происхождение перезаписано: %q, ожидала первую задачу t1", files[0].TaskID)
	}
}

// TestAddFileKeepsFirstTaskProvenance проверяет учёт по задачам на двух
// последовательных прогонах - сценарий, в котором дефект и проявлялся.
//
// Каждый прогон collect_files подъедал происхождение предыдущего: файл,
// найденный задачей A и повторно увиденный задачей B, переезжал в B, и
// `files --task A` показывал пустой каталог. После второго запуска учёт по
// задачам становился непригодным.
func TestAddFileKeepsFirstTaskProvenance(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	for _, u := range []string{"http://a.onion/one.zip", "http://a.onion/two.zip"} {
		if err := st.AddFile(ctx, FileEntry{
			TaskID: "taskA", URL: u, Filename: filepath.Base(u), Ext: "zip", Size: 100,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Второй прогон видит те же файлы и находит новый.
	for _, u := range []string{"http://a.onion/one.zip", "http://a.onion/three.zip"} {
		if err := st.AddFile(ctx, FileEntry{
			TaskID: "taskB", URL: u, Filename: filepath.Base(u), Ext: "zip", Size: 200,
		}); err != nil {
			t.Fatal(err)
		}
	}

	inA, err := st.ListFiles(ctx, "taskA", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(inA) != 2 {
		t.Errorf("в задаче A %d файлов, ожидала 2: происхождение потеряно", len(inA))
	}
	inB, err := st.ListFiles(ctx, "taskB", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(inB) != 1 {
		t.Errorf("в задаче B %d файлов, ожидала 1 (только новый)", len(inB))
	}

	// Метаданные при этом уточняются: размер из второго прогона заменяет
	// прежний, потому что это не происхождение, а свойство файла.
	all, err := st.SearchFiles(ctx, FileQuery{Text: "one.zip"})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("найдено %d записей, ожидала 1", len(all))
	}
	if all[0].Size != 200 {
		t.Errorf("размер не уточнён: %d, ожидала 200", all[0].Size)
	}
	if all[0].TaskID != "taskA" {
		t.Errorf("происхождение перезаписано: %q", all[0].TaskID)
	}
}

func TestAddFileFoundAtIsFirstSighting(t *testing.T) {
	// found_at не обновлялся никогда, поэтому после перезаписи task_id запись
	// становилась внутренне противоречивой: задача свежая, а время находки
	// самое старое. Теперь оба поля означают первую находку.
	st := newStore(t)
	ctx := context.Background()
	if err := st.AddFile(ctx, FileEntry{TaskID: "t1", URL: "http://a.onion/f.pdf", Filename: "f.pdf", Ext: "pdf", Size: 5}); err != nil {
		t.Fatal(err)
	}
	before, err := st.SearchFiles(ctx, FileQuery{Text: "f.pdf"})
	if err != nil || len(before) != 1 {
		t.Fatalf("первая запись: %v err=%v", before, err)
	}
	if err := st.AddFile(ctx, FileEntry{TaskID: "t2", URL: "http://a.onion/f.pdf", Filename: "f.pdf", Ext: "pdf", Size: 9}); err != nil {
		t.Fatal(err)
	}
	after, err := st.SearchFiles(ctx, FileQuery{Text: "f.pdf"})
	if err != nil || len(after) != 1 {
		t.Fatalf("вторая запись: %v err=%v", after, err)
	}
	if after[0].TaskID != before[0].TaskID {
		t.Errorf("task_id изменился: %q -> %q", before[0].TaskID, after[0].TaskID)
	}
}

func TestAddFileKeepsSizeOnUnknown(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.AddFile(ctx, FileEntry{
		TaskID: "t", URL: "http://a.onion/x.zip", Filename: "x.zip", Ext: "zip", Size: 500,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddFile(ctx, FileEntry{
		TaskID: "t", URL: "http://a.onion/x.zip", Filename: "x.zip", Ext: "zip", Size: 0,
	}); err != nil {
		t.Fatal(err)
	}
	files, err := st.SearchFiles(ctx, FileQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Size != 500 {
		t.Fatalf("нулевой размер затёр известный: %+v", files)
	}
}

func TestAddFileFillsMissingMeta(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.AddFile(ctx, FileEntry{
		TaskID: "t", URL: "http://a.onion/x.zip", Filename: "", Ext: "",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddFile(ctx, FileEntry{
		TaskID: "t", URL: "http://a.onion/x.zip", Filename: "x.zip", Ext: "zip",
		MIME: "application/zip", SourcePage: "http://a.onion/",
	}); err != nil {
		t.Fatal(err)
	}
	files, err := st.SearchFiles(ctx, FileQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("записей %d", len(files))
	}
	f := files[0]
	if f.Filename != "x.zip" || f.Ext != "zip" || f.MIME == "" || f.SourcePage == "" {
		t.Errorf("мета не дописана: %+v", f)
	}
}

func TestAddFileUniquePerURL(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if err := st.AddFile(ctx, FileEntry{
			TaskID: "t", URL: "http://a.onion/same.pdf", Filename: "same.pdf", Ext: "pdf",
		}); err != nil {
			t.Fatal(err)
		}
	}
	total, _, _, err := st.FileStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Errorf("дубли по url: %d записей", total)
	}
}

func TestCleanFileCatalogRemovesMarkup(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	keep := []FileEntry{
		{TaskID: "t", URL: "http://a.onion/dump.sql", Filename: "dump.sql", Ext: "sql", Size: 10},
		{TaskID: "t", URL: "http://a.onion/backup.zip", Filename: "backup.zip", Ext: "zip", Size: 20},
		{TaskID: "t", URL: "http://a.onion/doc.pdf", Filename: "doc.pdf", Ext: "pdf", Size: 30},
		// Самостоятельное изображение: с этапа 61 графика в каталоге
		// содержится, поэтому тест обязан отличать её от оформления сайта.
		{TaskID: "t", URL: "http://a.onion/photo.jpg", Filename: "photo.jpg", Ext: "jpg", Size: 40},
	}
	junk := []FileEntry{
		// Изображения с признаками оформления: отбраковываются правилом по
		// имени, а не расширением.
		{TaskID: "t", URL: "http://a.onion/logo.png", Filename: "logo.png", Ext: "png"},
		{TaskID: "t", URL: "http://a.onion/s.css", Filename: "s.css", Ext: "css"},
		{TaskID: "t", URL: "http://a.onion/a.js", Filename: "a.js", Ext: "js"},
		{TaskID: "t", URL: "http://a.onion/spinner.gif", Filename: "spinner.gif", Ext: "gif"},
		{TaskID: "t", URL: "http://a.onion/f.woff2", Filename: "f.woff2", Ext: "woff2"},
		{TaskID: "t", URL: "http://a.onion/page.html", Filename: "page.html", Ext: "html"},
		{TaskID: "t", URL: "http://a.onion/map.json", Filename: "", Ext: "json"},
		{TaskID: "t", URL: "http://a.onion/dir/", Filename: "dir", Ext: ""},
	}
	for _, f := range append(append([]FileEntry{}, keep...), junk...) {
		if err := st.AddFile(ctx, f); err != nil {
			t.Fatal(err)
		}
	}

	n, err := st.CleanFileCatalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len(junk)) {
		t.Errorf("вычищено %d, ожидала %d", n, len(junk))
	}

	files, err := st.SearchFiles(ctx, FileQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != len(keep) {
		t.Fatalf("осталось %d записей, ожидала %d: %+v", len(files), len(keep), files)
	}
	for _, f := range files {
		if f.Ext != "sql" && f.Ext != "zip" && f.Ext != "pdf" && f.Ext != "jpg" {
			t.Errorf("мусор остался: %+v", f)
		}
	}
}

func TestCleanFileCatalogRemovesUnknownExt(t *testing.T) {
	// Регрессия: «onion» лежал в filex.KnownExts, и само-ссылка сайта на
	// собственный адрес попадала в каталог как файл. После удаления из списка
	// чистка обязана убрать и уже накопленную строку - иначе мусор, который
	// больше не собирается, остаётся в базе навсегда.
	st := newStore(t)
	ctx := context.Background()

	const addr = "blackpasspn7734jqltjj2qx4qez5gcpcwujuugymky3lzcmmcfpzbyd.onion"
	entries := []FileEntry{
		{TaskID: "t", URL: "http://" + addr + "/" + addr, Filename: addr, Ext: "onion"},
		{TaskID: "t", URL: "http://a.onion/x.i2p", Filename: "x.i2p", Ext: "i2p"},
		{TaskID: "t", URL: "http://a.onion/s.php", Filename: "s.php", Ext: "php"},
		{TaskID: "t", URL: "http://a.onion/keep.zip", Filename: "keep.zip", Ext: "zip", Size: 5},
	}
	for _, f := range entries {
		if err := st.AddFile(ctx, f); err != nil {
			t.Fatal(err)
		}
	}

	n, err := st.CleanFileCatalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("вычищено %d, ожидала 3", n)
	}

	files, err := st.SearchFiles(ctx, FileQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("осталось %d записей, ожидала 1: %+v", len(files), files)
	}
	if files[0].Ext != "zip" {
		t.Errorf("выжил не тот файл: %+v", files[0])
	}
}

func TestCleanFileCatalogRemovesMetadataFiles(t *testing.T) {
	// Подписи и сертификаты - не содержимое: они описывают другой файл и сами
	// по себе бесполезны. В каталоге они раздувают счётчик и создают видимость
	// раздачи, которой нет. При этом MIME для них обязан остаться известным.
	st := newStore(t)
	ctx := context.Background()

	entries := []FileEntry{
		{TaskID: "t", URL: "http://a.onion/dist.iso.asc", Filename: "dist.iso.asc", Ext: "asc", Size: 1},
		{TaskID: "t", URL: "http://a.onion/ca.pem", Filename: "ca.pem", Ext: "pem", Size: 2},
		{TaskID: "t", URL: "http://a.onion/host.crt", Filename: "host.crt", Ext: "crt", Size: 3},
		{TaskID: "t", URL: "http://a.onion/priv.key", Filename: "priv.key", Ext: "key", Size: 4},
		{TaskID: "t", URL: "http://a.onion/release.torrent", Filename: "release.torrent", Ext: "torrent", Size: 5},
		{TaskID: "t", URL: "http://a.onion/backup.zip", Filename: "backup.zip", Ext: "zip", Size: 6},
	}
	for _, f := range entries {
		if err := st.AddFile(ctx, f); err != nil {
			t.Fatal(err)
		}
	}

	n, err := st.CleanFileCatalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Errorf("вычищено %d, ожидала 4 (asc, pem, crt, key)", n)
	}

	files, err := st.SearchFiles(ctx, FileQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("осталось %d записей, ожидала 2: %+v", len(files), files)
	}
	kept := map[string]bool{}
	for _, f := range files {
		kept[f.Ext] = true
	}
	// torrent - самостоятельный объект раздачи, zip - архив: оба остаются.
	if !kept["torrent"] || !kept["zip"] {
		t.Errorf("выжили не те расширения: %+v", files)
	}
}

func TestCleanFileCatalogChunksLargeJunkSet(t *testing.T) {
	// Мусора больше размера порции: удаление обязано пройти все записи, а не
	// упереться в предел числа параметров SQLite.
	st := newStore(t)
	ctx := context.Background()

	const total = 950
	for i := 0; i < total; i++ {
		// Мусор взят по расширению, а не по признаку оформления: тест
		// проверяет порционность удаления, и он не должен зависеть от
		// эвристики по имени.
		if err := st.AddFile(ctx, FileEntry{
			TaskID:   "t",
			URL:      fmt.Sprintf("http://a.onion/junk%04d.ico", i),
			Filename: fmt.Sprintf("junk%04d.ico", i),
			Ext:      "ico",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.AddFile(ctx, FileEntry{
		TaskID: "t", URL: "http://a.onion/keep.zip", Filename: "keep.zip", Ext: "zip", Size: 5,
	}); err != nil {
		t.Fatal(err)
	}

	n, err := st.CleanFileCatalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != total {
		t.Errorf("вычищено %d, ожидала %d", n, total)
	}
	files, err := st.SearchFiles(ctx, FileQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Ext != "zip" {
		t.Errorf("осталось %+v", files)
	}
}

func TestCleanFileCatalogKeepsUppercaseExt(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	// Оба случая в верхнем регистре: мусорное расширение и изображение с
	// признаком оформления. Приведение регистра обязано срабатывать в обоих,
	// иначе запись в верхнем регистре обошла бы чистку.
	for _, f := range []FileEntry{
		{TaskID: "t", URL: "http://a.onion/X.CSS", Filename: "X.CSS", Ext: "CSS"},
		{TaskID: "t", URL: "http://a.onion/LOGO.PNG", Filename: "LOGO.PNG", Ext: "PNG"},
	} {
		if err := st.AddFile(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.CleanFileCatalog(ctx); err != nil {
		t.Fatal(err)
	}
	files, err := st.SearchFiles(ctx, FileQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Errorf("регистр расширения не учтён: %+v", files)
	}
}

func TestCleanFileCatalogEmpty(t *testing.T) {
	st := newStore(t)
	n, err := st.CleanFileCatalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("на пустом каталоге вычищено %d", n)
	}
}

func TestCleanFileCatalogKeepsArchivesAndKeys(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	exts := []string{"zip", "rar", "7z", "tar", "gz", "kdbx", "pgp", "apk", "iso", "torrent"}
	for _, e := range exts {
		if err := st.AddFile(ctx, FileEntry{
			TaskID: "t", URL: "http://a.onion/f." + e, Filename: "f." + e, Ext: e,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.CleanFileCatalog(ctx); err != nil {
		t.Fatal(err)
	}
	files, err := st.SearchFiles(ctx, FileQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != len(exts) {
		t.Errorf("полезные расширения вычищены: осталось %d из %d", len(files), len(exts))
	}
}

func TestSearchFilesByText(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	rows := []FileEntry{
		{URL: "http://a.onion/leak.zip", Filename: "leak.zip", Ext: "zip", Size: 500},
		{URL: "http://a.onion/report.pdf", Filename: "report.pdf", Ext: "pdf", Size: 300},
		{URL: "http://a.onion/notes.txt", Filename: "notes.txt", Ext: "txt", Size: 100},
	}
	for _, f := range rows {
		if err := st.AddFile(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.SearchFiles(ctx, FileQuery{Text: "leak"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("найдено %d, ожидала 1", len(got))
	}
	if got[0].Filename != "leak.zip" {
		t.Errorf("вернулся %q", got[0].Filename)
	}
}

func TestSearchFilesByExt(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	for _, f := range []FileEntry{
		{URL: "http://a.onion/1.zip", Filename: "1.zip", Ext: "zip"},
		{URL: "http://a.onion/2.pdf", Filename: "2.pdf", Ext: "pdf"},
		{URL: "http://a.onion/3.zip", Filename: "3.zip", Ext: "zip"},
	} {
		if err := st.AddFile(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.SearchFiles(ctx, FileQuery{Ext: "zip"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("zip-файлов %d, ожидала 2", len(got))
	}
	got, err = st.SearchFiles(ctx, FileQuery{Ext: ".ZIP"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("расширение с точкой и в верхнем регистре не нормализовано: %d", len(got))
	}
}

func TestSearchFilesBySize(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	for _, f := range []FileEntry{
		{URL: "http://a.onion/small", Filename: "small", Size: 100},
		{URL: "http://a.onion/mid", Filename: "mid", Size: 1000},
		{URL: "http://a.onion/big", Filename: "big", Size: 10000},
	} {
		if err := st.AddFile(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.SearchFiles(ctx, FileQuery{MinSize: 500, MaxSize: 5000})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Filename != "mid" {
		t.Errorf("диапазон размера не сработал: %+v", got)
	}
}

func TestSearchFilesIgnoresNegativeSizeBounds(t *testing.T) {
	// Отрицательная граница не применяется: предикат добавляется только при
	// значении больше нуля. Тест фиксирует это поведение, потому что именно оно
	// сделало невидимым переполнение в parseSize на этапе 62. Флаг
	// -min-size 8388608tib превращался в -9223372036854775808, граница молча
	// отбрасывалась, и команда печатала полную выдачу, как если бы флага не
	// было вовсе: ни ошибки, ни пустого результата, просто неотфильтрованный
	// список. Если это поведение когда-нибудь изменят на строгое сравнение,
	// тест укажет, что вместе с ним меняется и смысл отброшенной границы.
	st := newStore(t)
	ctx := context.Background()
	for _, f := range []FileEntry{
		{URL: "http://a.onion/small", Filename: "small", Ext: "txt", Size: 100},
		{URL: "http://a.onion/mid", Filename: "mid", Ext: "pdf", Size: 1000},
		{URL: "http://a.onion/big", Filename: "big", Ext: "mp4", Size: 10000},
	} {
		if err := st.AddFile(ctx, f); err != nil {
			t.Fatal(err)
		}
	}

	for _, q := range []FileQuery{
		{MinSize: math.MinInt64},
		{MaxSize: math.MinInt64},
		{MinSize: -1},
		{MaxSize: -1},
	} {
		got, err := st.SearchFiles(ctx, q)
		if err != nil {
			t.Fatalf("%+v: %v", q, err)
		}
		if len(got) != 3 {
			t.Errorf("%+v: возвращено %d записей, ожидала все 3 - граница должна игнорироваться", q, len(got))
		}
	}
}

func TestSearchFilesMaxSizeExcludesUnknownSize(t *testing.T) {
	// Регресс на дефект, найденный пользовательским тестом каталога. Ноль в
	// колонке size означает «размер неизвестен», а не «нулевой байт», но
	// условие `size <= ?` трактовало его как очень маленький файл. Запрос
	// «файлы до 5 КБ» возвращал 23 записи из 34, и 17 из них были без размера,
	// включая видео, которое могло весить сотни мегабайт.
	//
	// При этом `-min-size 1` те же 17 записей честно отсекал: фильтры
	// трактовали неизвестный размер противоположным образом.
	st := newStore(t)
	ctx := context.Background()
	for _, f := range []FileEntry{
		{URL: "http://a.onion/tiny.txt", Filename: "tiny.txt", Ext: "txt", Size: 100},
		{URL: "http://a.onion/mid.pdf", Filename: "mid.pdf", Ext: "pdf", Size: 4000},
		{URL: "http://a.onion/huge.mp4", Filename: "huge.mp4", Ext: "mp4", Size: 9000},
		{URL: "http://a.onion/unknown1.mp4", Filename: "unknown1.mp4", Ext: "mp4", Size: 0},
		{URL: "http://a.onion/unknown2.epub", Filename: "unknown2.epub", Ext: "epub", Size: 0},
	} {
		if err := st.AddFile(ctx, f); err != nil {
			t.Fatal(err)
		}
	}

	got, err := st.SearchFiles(ctx, FileQuery{MaxSize: 5000})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("max-size вернул %d записей, ожидала 2: %+v", len(got), got)
	}
	for _, f := range got {
		if f.Size == 0 {
			t.Errorf("запись с неизвестным размером прошла max-size: %s", f.Filename)
		}
		if f.Size > 5000 {
			t.Errorf("запись больше границы прошла max-size: %s size=%d", f.Filename, f.Size)
		}
	}

	// Симметрия: оба фильтра должны отсеивать неизвестный размер одинаково.
	byMin, err := st.SearchFiles(ctx, FileQuery{MinSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(byMin) != 3 {
		t.Errorf("min-size вернул %d записей, ожидала 3", len(byMin))
	}
	byBoth, err := st.SearchFiles(ctx, FileQuery{MinSize: 1, MaxSize: 5000})
	if err != nil {
		t.Fatal(err)
	}
	if len(byBoth) != 2 {
		t.Errorf("диапазон вернул %d записей, ожидала 2", len(byBoth))
	}
}

func TestSearchFilesWithoutSizeFilterStillReturnsUnknown(t *testing.T) {
	// Обратная сторона: если пользователь про размеры не спрашивал, записи с
	// неизвестным размером обязаны оставаться в выдаче. Иначе половина
	// каталога исчезла бы из списка без всякого фильтра.
	st := newStore(t)
	ctx := context.Background()
	for _, f := range []FileEntry{
		{URL: "http://a.onion/known.pdf", Filename: "known.pdf", Ext: "pdf", Size: 4000},
		{URL: "http://a.onion/unknown.epub", Filename: "unknown.epub", Ext: "epub", Size: 0},
	} {
		if err := st.AddFile(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.SearchFiles(ctx, FileQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("без фильтра вернулось %d записей, ожидала 2", len(got))
	}
}

func TestSearchFilesOrdersBySizeDesc(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	for _, f := range []FileEntry{
		{URL: "http://a.onion/small", Filename: "small", Size: 100},
		{URL: "http://a.onion/big", Filename: "big", Size: 10000},
		{URL: "http://a.onion/mid", Filename: "mid", Size: 1000},
	} {
		if err := st.AddFile(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.SearchFiles(ctx, FileQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("файлов %d, ожидала 3", len(got))
	}
	if got[0].Filename != "big" {
		t.Errorf("первый %q, ожидала big (сортировка по размеру)", got[0].Filename)
	}
}

func TestSearchFilesEscapeLike(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	for _, f := range []FileEntry{
		{URL: "http://a.onion/percent.txt", Filename: "100%pass.txt", Ext: "txt"},
		{URL: "http://a.onion/plain.txt", Filename: "plain.txt", Ext: "txt"},
	} {
		if err := st.AddFile(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.SearchFiles(ctx, FileQuery{Text: "%"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("символ %% не экранирован: найдено %d, ожидала 1", len(got))
	}
	if got[0].Filename != "100%pass.txt" {
		t.Errorf("вернулся %q", got[0].Filename)
	}

	got, err = st.SearchFiles(ctx, FileQuery{Text: "_"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("подчёркивание подействовало как шаблон: %+v", got)
	}
}

func TestSearchFilesByTaskID(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	for _, f := range []FileEntry{
		{TaskID: "t1", URL: "http://a.onion/1", Filename: "1"},
		{TaskID: "t2", URL: "http://a.onion/2", Filename: "2"},
	} {
		if err := st.AddFile(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.SearchFiles(ctx, FileQuery{TaskID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Filename != "1" {
		t.Errorf("фильтр по задаче не сработал: %+v", got)
	}
}

func TestSearchFilesLimit(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		if err := st.AddFile(ctx, FileEntry{
			URL: "http://a.onion/" + string(rune('a'+i)), Filename: string(rune('a' + i)),
		}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.SearchFiles(ctx, FileQuery{Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Errorf("лимит не сработал: %d", len(got))
	}
}

func TestSearchFilesEmpty(t *testing.T) {
	st := newStore(t)
	got, err := st.SearchFiles(context.Background(), FileQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("на пустом каталоге %d записей", len(got))
	}
}

func TestFileStats(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	for _, f := range []FileEntry{
		{URL: "http://a.onion/1.zip", Filename: "1.zip", Ext: "zip", Size: 1000},
		{URL: "http://a.onion/2.zip", Filename: "2.zip", Ext: "zip", Size: 2000},
		{URL: "http://a.onion/3.pdf", Filename: "3.pdf", Ext: "pdf", Size: 500},
	} {
		if err := st.AddFile(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	total, bytes, byExt, err := st.FileStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 {
		t.Errorf("total=%d, ожидала 3", total)
	}
	if bytes != 3500 {
		t.Errorf("bytes=%d, ожидала 3500", bytes)
	}
	if byExt["zip"] != 2 {
		t.Errorf("zip=%d, ожидала 2", byExt["zip"])
	}
	if byExt["pdf"] != 1 {
		t.Errorf("pdf=%d, ожидала 1", byExt["pdf"])
	}
}

func TestFileStatsEmpty(t *testing.T) {
	st := newStore(t)
	total, bytes, byExt, err := st.FileStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if total != 0 || bytes != 0 {
		t.Errorf("на пустом каталоге total=%d bytes=%d", total, bytes)
	}
	if len(byExt) != 0 {
		t.Errorf("разбивка не пуста: %v", byExt)
	}
}

func TestSearchOnionsByTitle(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	for _, o := range []Onion{
		{URL: "http://a.onion", Status: "live", Title: "Hidden Market"},
		{URL: "http://b.onion", Status: "live", Title: "Crypto Forum"},
	} {
		if err := st.UpsertOnion(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.SearchOnions(ctx, "market", "", 10, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Title != "Hidden Market" {
		t.Errorf("поиск по заголовку не сработал: %+v", got)
	}
}

func TestSearchOnionsByDescription(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.UpsertOnion(ctx, Onion{
		URL: "http://a.onion", Status: "live", Title: "Shop", Description: "продажа документов",
	}); err != nil {
		t.Fatal(err)
	}
	got, err := st.SearchOnions(ctx, "документов", "", 10, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("поиск по описанию не сработал: %d", len(got))
	}
}

func TestSearchOnionsByURL(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.UpsertOnion(ctx, Onion{URL: "abcdefghijklmnop.onion", Status: "live"}); err != nil {
		t.Fatal(err)
	}
	got, err := st.SearchOnions(ctx, "abcdefghij", "", 10, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("поиск по адресу не сработал: %d", len(got))
	}
}

func TestSearchOnionsHidesDeadByDefault(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	for _, o := range []Onion{
		{URL: "http://a.onion", Status: "live", Title: "Market"},
		{URL: "http://b.onion", Status: "dead", Title: "Market dead"},
	} {
		if err := st.UpsertOnion(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.SearchOnions(ctx, "market", "", 10, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("мёртвый сервис не отсеян: %d записей", len(got))
	}
	if got[0].Status != "live" {
		t.Errorf("вернулся статус %q", got[0].Status)
	}

	got, err = st.SearchOnions(ctx, "market", "", 10, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("includeDead не сработал: %d", len(got))
	}
}

func TestSearchOnionsStatusFilter(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	for _, o := range []Onion{
		{URL: "http://a.onion", Status: "live", Title: "Market"},
		{URL: "http://b.onion", Status: "unknown", Title: "Market two"},
	} {
		if err := st.UpsertOnion(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.SearchOnions(ctx, "market", "unknown", 10, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Status != "unknown" {
		t.Errorf("фильтр по статусу не сработал: %+v", got)
	}
}

func TestSearchOnionsLimit(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	for i := 0; i < 8; i++ {
		if err := st.UpsertOnion(ctx, Onion{
			URL: "http://" + string(rune('a'+i)) + ".onion", Status: "live", Title: "Market",
		}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.SearchOnions(ctx, "market", "", 3, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Errorf("лимит не сработал: %d", len(got))
	}
}

func TestSearchOnionsEscapesWildcards(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	for _, o := range []Onion{
		{URL: "http://a.onion", Status: "live", Title: "100% uptime"},
		{URL: "http://b.onion", Status: "live", Title: "other"},
	} {
		if err := st.UpsertOnion(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.SearchOnions(ctx, "%", "", 10, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("символ %% не экранирован: %d", len(got))
	}
	if got[0].Title != "100% uptime" {
		t.Errorf("вернулся %q", got[0].Title)
	}
}

func TestSetOnionMeta(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.UpsertOnion(ctx, Onion{URL: "http://a.onion", Status: "live"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetOnionMeta(ctx, "http://a.onion", "My Title", "My Desc", "market"); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetOnion(ctx, "http://a.onion")
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "My Title" {
		t.Errorf("title=%q", got.Title)
	}
	if got.Description != "My Desc" {
		t.Errorf("description=%q", got.Description)
	}
	if got.Category != "market" {
		t.Errorf("category=%q", got.Category)
	}
}

func TestSetOnionMetaKeepsKnownOnEmpty(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.UpsertOnion(ctx, Onion{URL: "http://a.onion", Status: "live", Title: "Keep"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetOnionMeta(ctx, "http://a.onion", "", "", ""); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetOnion(ctx, "http://a.onion")
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "Keep" {
		t.Errorf("пустое значение затёрло известный заголовок: %q", got.Title)
	}
}

func TestSetOnionMetaUnknownURL(t *testing.T) {
	st := newStore(t)
	if err := st.SetOnionMeta(context.Background(), "http://nope.onion", "t", "", ""); err == nil {
		t.Error("мета для неизвестного адреса должна вернуть ошибку")
	}
}

func TestUpsertOnionKeepsTitleOnEmpty(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.UpsertOnion(ctx, Onion{URL: "http://a.onion", Status: "live", Title: "First"}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertOnion(ctx, Onion{URL: "http://a.onion", Status: "live"}); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetOnion(ctx, "http://a.onion")
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "First" {
		t.Errorf("повторный upsert без заголовка затёр известный: %q", got.Title)
	}
}

func TestCleanOnionTitles(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	addr := "abcdefghijklmnop.onion"
	rows := []Onion{
		{URL: addr, Status: "live", Title: addr},
		{URL: "http://b.onion", Status: "live", Title: "http://b.onion"},
		{URL: "http://c.onion", Status: "live", Title: "c"},
		{URL: "http://d.onion", Status: "live", Title: "Drug Hub http://d.onion"},
		{URL: "http://e.onion", Status: "live", Title: "Real Name"},
	}
	for _, o := range rows {
		if err := st.UpsertOnion(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	n, err := st.CleanOnionTitles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Errorf("вычищено %d, ожидала 4", n)
	}
	got, err := st.GetOnion(ctx, "http://e.onion")
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "Real Name" {
		t.Errorf("настоящее название потеряно: %q", got.Title)
	}
	for _, u := range []string{addr, "http://b.onion", "http://c.onion", "http://d.onion"} {
		o, err := st.GetOnion(ctx, u)
		if err != nil {
			t.Fatal(err)
		}
		if o.Title != "" {
			t.Errorf("мусорный заголовок у %s остался: %q", u, o.Title)
		}
	}
}

func TestCleanOnionTitlesEmpty(t *testing.T) {
	st := newStore(t)
	n, err := st.CleanOnionTitles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("на пустом пуле вычищено %d", n)
	}
}

func TestCleanOnionTitlesStripsLatencyPrefix(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.UpsertOnion(ctx, Onion{
		URL: "http://a.onion", Status: "live", Title: "4.0s An index of clearnet sites",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CleanOnionTitles(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetOnion(ctx, "http://a.onion")
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "An index of clearnet sites" {
		t.Errorf("префикс латентности не срезан: %q", got.Title)
	}
}

func TestCleanOnionTitlesKeepsLegitNumbers(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	for _, o := range []Onion{
		{URL: "http://a.onion", Status: "live", Title: "3xploit Forum"},
		{URL: "http://b.onion", Status: "live", Title: "4chan archive"},
	} {
		if err := st.UpsertOnion(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.CleanOnionTitles(ctx); err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{"http://a.onion", "http://b.onion"} {
		got, err := st.GetOnion(ctx, u)
		if err != nil {
			t.Fatal(err)
		}
		if got.Title == "" {
			t.Errorf("настоящее название у %s вычищено", u)
		}
	}
}

func TestMarkOnionStatus(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.UpsertOnion(ctx, Onion{URL: "http://x.onion", Status: "live"}); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkOnionStatus(ctx, "http://x.onion", "dead", 3); err != nil {
		t.Fatalf("mark: %v", err)
	}
	got, err := st.GetOnion(ctx, "http://x.onion")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "dead" || got.FailStreak != 3 {
		t.Errorf("status=%q fail_streak=%d", got.Status, got.FailStreak)
	}
	if got.LastProbe.IsZero() {
		t.Error("last_probe не обновлён")
	}
	if err := st.MarkOnionStatus(ctx, "http://missing.onion", "live", 0); err != ErrNotFound {
		t.Errorf("err=%v, ожидала ErrNotFound", err)
	}
}

func TestSourceUpsert(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.UpsertSource(ctx, Source{URL: "http://s.example", Type: "public", Quality: 0.7}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertSource(ctx, Source{URL: "http://s.example", Quality: 0.9, Popularity: 0.5}); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetSource(ctx, "http://s.example")
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != "public" {
		t.Errorf("type=%q, ожидала public (пустая не затирает)", got.Type)
	}
	if got.Quality != 0.9 {
		t.Errorf("quality=%v, ожидала 0.9", got.Quality)
	}
	if _, err := st.GetSource(ctx, "http://none.example"); err != ErrNotFound {
		t.Errorf("err=%v, ожидала ErrNotFound", err)
	}
}

func TestSelectorUpsertAndList(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	sel := Selector{URLPattern: "*://shop/*", Field: "price", Selector: ".price"}
	if err := st.UpsertSelector(ctx, sel); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertSelector(ctx, Selector{
		URLPattern: "*://shop/*", Field: "price", Selector: ".cost", Strategy: "fuzzy", Confidence: 0.6,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertSelector(ctx, Selector{
		URLPattern: "*://shop/*", Field: "title", Selector: "h1",
	}); err != nil {
		t.Fatal(err)
	}
	got, err := st.SelectorsFor(ctx, "*://shop/*")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("селекторов %d, ожидала 2", len(got))
	}
	if got[0].Field != "title" {
		t.Errorf("первый %q, ожидала title (сортировка по confidence убыв.)", got[0].Field)
	}
	byField := map[string]Selector{}
	for _, s := range got {
		byField[s.Field] = s
	}
	price, ok := byField["price"]
	if !ok {
		t.Fatal("селектор price не найден")
	}
	if price.Selector != ".cost" {
		t.Errorf("price.selector=%q, ожидала .cost (upsert)", price.Selector)
	}
	if price.Strategy != "fuzzy" || price.Confidence != 0.6 {
		t.Errorf("price: strategy=%q confidence=%v, ожидала fuzzy/0.6", price.Strategy, price.Confidence)
	}
	if byField["title"].Selector != "h1" {
		t.Errorf("title.selector=%q, ожидала h1", byField["title"].Selector)
	}
	none, err := st.SelectorsFor(ctx, "*://other/*")
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Errorf("для чужого паттерна найдено %d", len(none))
	}
}

func TestSelectorDefaults(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.UpsertSelector(ctx, Selector{URLPattern: "p", Field: "f", Selector: ".x"}); err != nil {
		t.Fatal(err)
	}
	got, err := st.SelectorsFor(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Strategy != "css" {
		t.Errorf("strategy=%q, ожидала css", got[0].Strategy)
	}
	if got[0].Confidence != 1.0 {
		t.Errorf("confidence=%v, ожидала 1.0", got[0].Confidence)
	}
}

func TestTaskLifecycle(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.CreateTask(ctx, Task{ID: "t1", Kind: "search"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := st.GetTask(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "pending" {
		t.Errorf("status=%q, ожидала pending по умолчанию", got.Status)
	}
	if got.CreatedAt.IsZero() {
		t.Error("created_at пуст")
	}
	if err := st.UpdateTask(ctx, "t1", "running", 40, "ищу", ""); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err = st.GetTask(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "running" || got.Progress != 40 || got.Message != "ищу" {
		t.Errorf("после update: %+v", got)
	}
	if err := st.UpdateTask(ctx, "t1", "done", 100, "", "res://out"); err != nil {
		t.Fatal(err)
	}
	got, err = st.GetTask(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.ResultRef != "res://out" {
		t.Errorf("result_ref=%q", got.ResultRef)
	}
	if err := st.UpdateTask(ctx, "t1", "done", 100, "", ""); err != nil {
		t.Fatal(err)
	}
	got, _ = st.GetTask(ctx, "t1")
	if got.ResultRef != "res://out" {
		t.Errorf("пустой result_ref затёр предыдущий: %q", got.ResultRef)
	}
	if err := st.UpdateTask(ctx, "missing", "done", 0, "", ""); err != ErrNotFound {
		t.Errorf("err=%v, ожидала ErrNotFound", err)
	}
	if _, err := st.GetTask(ctx, "missing"); err != ErrNotFound {
		t.Errorf("err=%v, ожидала ErrNotFound", err)
	}
}

func TestTaskCreateRejectsEmptyID(t *testing.T) {
	st := newStore(t)
	if err := st.CreateTask(context.Background(), Task{Kind: "search"}); err == nil {
		t.Error("пустой id должен отклоняться")
	}
}

func TestListTasks(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	for _, row := range []Task{
		{ID: "a", Kind: "search", Status: "running"},
		{ID: "b", Kind: "crawl", Status: "done"},
		{ID: "c", Kind: "parse", Status: "running"},
	} {
		if err := st.CreateTask(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	running, err := st.ListTasks(ctx, "running", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(running) != 2 {
		t.Errorf("running=%d, ожидала 2", len(running))
	}
	all, err := st.ListTasks(ctx, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Errorf("всего=%d, ожидала 3", len(all))
	}
}

func TestHuntLifecycle(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	id, err := st.CreateHunt(ctx, Hunt{Query: "leak x"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if id == 0 {
		t.Fatal("id не вернулся")
	}
	hunts, err := st.ListHunts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(hunts) != 1 {
		t.Fatalf("охот %d, ожидала 1", len(hunts))
	}
	if hunts[0].Mode != "auto" {
		t.Errorf("mode=%q, ожидала auto по умолчанию", hunts[0].Mode)
	}
	if hunts[0].ScheduleMin != 360 {
		t.Errorf("schedule=%d, ожидала 360 по умолчанию", hunts[0].ScheduleMin)
	}
	if err := st.TouchHunt(ctx, id, "hash1"); err != nil {
		t.Fatalf("touch: %v", err)
	}
	hunts, _ = st.ListHunts(ctx)
	if hunts[0].LastHash != "hash1" {
		t.Errorf("last_hash=%q", hunts[0].LastHash)
	}
	if hunts[0].LastRun.IsZero() {
		t.Error("last_run не проставлен")
	}
	if err := st.TouchHunt(ctx, 9999, "x"); err != ErrNotFound {
		t.Errorf("err=%v, ожидала ErrNotFound", err)
	}
}

func TestHuntCustomSchedule(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if _, err := st.CreateHunt(ctx, Hunt{Query: "q", Mode: "deep", ScheduleMin: 30}); err != nil {
		t.Fatal(err)
	}
	hunts, _ := st.ListHunts(ctx)
	if hunts[0].Mode != "deep" || hunts[0].ScheduleMin != 30 {
		t.Errorf("mode=%q schedule=%d", hunts[0].Mode, hunts[0].ScheduleMin)
	}
}

func TestFileCatalog(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.AddFile(ctx, FileEntry{
		TaskID: "t1", URL: "http://x/file.zip", Filename: "file.zip",
		Ext: "zip", Size: 1024, Verdict: "archive",
	}); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := st.AddFile(ctx, FileEntry{
		TaskID: "t1", URL: "http://x/other.exe", Ext: "exe", Verdict: "executable-risk",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddFile(ctx, FileEntry{
		TaskID: "t2", URL: "http://y/doc.pdf", Ext: "pdf", Verdict: "safe-doc",
	}); err != nil {
		t.Fatal(err)
	}
	t1, err := st.ListFiles(ctx, "t1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(t1) != 2 {
		t.Errorf("файлов задачи t1: %d, ожидала 2", len(t1))
	}
	all, err := st.ListFiles(ctx, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Errorf("всего файлов %d, ожидала 3", len(all))
	}
	if err := st.AddFile(ctx, FileEntry{
		TaskID: "t1", URL: "http://x/file.zip", Size: 2048, Verdict: "archive",
	}); err != nil {
		t.Fatal(err)
	}
	t1, _ = st.ListFiles(ctx, "t1", 10)
	if len(t1) != 2 {
		t.Errorf("дубликат url создал новую запись: %d", len(t1))
	}
	for _, f := range t1 {
		if f.URL == "http://x/file.zip" && f.Size != 2048 {
			t.Errorf("upsert не обновил size: %d", f.Size)
		}
	}
}

func TestAddFileRejectsEmptyURL(t *testing.T) {
	st := newStore(t)
	if err := st.AddFile(context.Background(), FileEntry{TaskID: "t"}); err == nil {
		t.Error("пустой url должен отклоняться")
	}
}

func TestParseTSHandlesFormats(t *testing.T) {
	cases := map[string]bool{
		"2024-01-15 10:30:00":           true,
		"2024-01-15 10:30:00.123456789": true,
		"2024-01-15T10:30:00Z":          true,
		"2024-01-15 10:30:00+03:00":     true,
		"garbage":                       false,
		"":                              false,
	}
	for in, wantValid := range cases {
		got := parseTS(nullString(in))
		if wantValid && got.IsZero() {
			t.Errorf("%q не распарсилось", in)
		}
		if !wantValid && !got.IsZero() {
			t.Errorf("%q распарсилось как %v, ожидала zero", in, got)
		}
	}
}

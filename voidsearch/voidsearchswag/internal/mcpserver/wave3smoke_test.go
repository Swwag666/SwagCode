package mcpserver

// Этап 178, третий пучок правок по смокам G/H/I: юнит-покрытие каждой ветки.
// Правило прежнее - тест рождается из живого дефекта смока и называет его в
// комментарии, чтобы мутация правки ловилась адресно.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"

	"voidsearchswag/internal/catalog"
	"voidsearchswag/internal/httpc"
	"voidsearchswag/internal/hunt"
	"voidsearchswag/internal/search"
	"voidsearchswag/internal/searchers"
	"voidsearchswag/internal/store"
)

// callToolRaw вызывает инструмент и возвращает текст ответа вместе с флагом
// isError: часть проверок ниже именно про ошибку инструмента, и терять её
// в callToolArgs нельзя.
func callToolRaw(t *testing.T, c *client.Client, name string, args map[string]any) (string, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: name, Arguments: args},
	})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return textOf(t, res), res.IsError
}

// callToolJSONErr аналог callToolArgs, не падающий на isError: текст ответа
// разбирается и при ошибке инструмента - проверяются её тело и причина.
func callToolJSONErr(t *testing.T, c *client.Client, name string, args map[string]any) (map[string]any, bool) {
	t.Helper()
	text, isErr := callToolRaw(t, c, name, args)
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("%s: ответ не JSON: %v\n%s", name, err, text)
	}
	return out, isErr
}

// Смоук-H: описание зовёт фильтр status_filter, схема принимала только
// status - переданный по описанию фильтр молча не применялся. Теперь
// status_filter фильтрует, а status остаётся рабочим алиасом.
func TestPoolStatusFilterAppliesAndAliases(t *testing.T) {
	dir := t.TempDir()
	st := exportStore(t, dir)
	seedMCPBase(t, st, dir)
	// Мёртвая запись: без неё пул из одной live-записи не отличает
	// «фильтр применён» от «фильтр потерялся» - обе ветки отдают 1.
	if err := st.UpsertOnion(context.Background(), store.Onion{
		URL: "http://dead.onion/", Status: "dead",
	}); err != nil {
		t.Fatal(err)
	}

	srv := New(Deps{Version: "test", Store: st, Started: time.Now()})
	c := inProcess(t, srv)
	initClient(t, c)

	out := callToolArgs(t, c, "pool_status", map[string]any{"status_filter": "live"})
	if n, _ := out["returned"].(float64); n != 1 {
		t.Fatalf("status_filter=live: returned=%v, хочу 1 живую запись: %+v", out["returned"], out)
	}
	if f, _ := out["status_filter"].(string); f != "live" {
		t.Errorf("эхо status_filter=%q, хочу live", f)
	}

	alias := callToolArgs(t, c, "pool_status", map[string]any{"status": "live"})
	if n, _ := alias["returned"].(float64); n != 1 {
		t.Errorf("алиас status=live: returned=%v, хочу 1: алиас молча потерялся", alias["returned"])
	}
}

// Смоук-I: неверное значение фильтра отдавало весь пул как
// «отфильтрованный» - агент делал выводы о пуле из сырых данных. Теперь
// это ошибка вызова с перечнем допустимых значений.
func TestPoolStatusFilterRejectsUnknownValue(t *testing.T) {
	dir := t.TempDir()
	st := exportStore(t, dir)
	seedMCPBase(t, st, dir)

	srv := New(Deps{Version: "test", Store: st, Started: time.Now()})
	c := inProcess(t, srv)
	initClient(t, c)

	text, isErr := callToolRaw(t, c, "pool_status", map[string]any{"status_filter": "alive"})
	if !isErr {
		t.Fatalf("status_filter=alive не отвергнут: %s", text)
	}
	for _, want := range []string{"alive", "live|unknown|dead"} {
		if !strings.Contains(text, want) {
			t.Errorf("в отказе нет %q: %s", want, clip(text, 300))
		}
	}
}

// Смоук-G: success_rate приходил с полным двоичным хвостом
// (0.9166666666666666) - четыре знака, как обещано описанием.
func TestPoolStatusSuccessRateRoundedToFourDecimals(t *testing.T) {
	dir := t.TempDir()
	st := exportStore(t, dir)
	seedMCPBase(t, st, dir)
	ctx := context.Background()
	if err := st.UpsertOnion(ctx, store.Onion{
		URL: "http://rate.onion/", Status: "live", SuccessRate: 11.0 / 12.0,
	}); err != nil {
		t.Fatal(err)
	}

	srv := New(Deps{Version: "test", Store: st, Started: time.Now()})
	c := inProcess(t, srv)
	initClient(t, c)

	out := callToolArgs(t, c, "pool_status", map[string]any{"status_filter": "live", "limit": 10})
	entries, ok := out["entries"].([]any)
	if !ok || len(entries) != 2 {
		t.Fatalf("entries=%v, хочу 2 записи", out["entries"])
	}
	for _, raw := range entries {
		row, _ := raw.(map[string]any)
		if row["url"] == "http://rate.onion/" {
			if got, ok := row["success_rate"].(float64); !ok || got != 0.9167 {
				t.Errorf("success_rate=%v (%T), хочу 0.9167: округление до четырёх знаков", row["success_rate"], row["success_rate"])
			}
		}
	}
}

// Смоук-G/I: пустые списки hunts и hits обязаны приходить [] - строгий
// клиент получает один тип поля при любой пустоте, null ломает парсинг.
func TestHuntToolsEmptyListsAreArraysNotNull(t *testing.T) {
	st, h := newHitsServer(t, map[string][][]string{"leak": {{"http://a.onion/x"}}})
	srv := New(Deps{Version: "test", Store: st, Hunter: h, Started: time.Now()})
	c := inProcess(t, srv)
	initClient(t, c)

	text, _ := callToolRaw(t, c, "hunt_list", map[string]any{})
	if strings.Contains(text, `"hunts":null`) {
		t.Errorf("hunt_list отдал hunts:null: %s", clip(text, 200))
	}
	hitsText, _ := callToolRaw(t, c, "hunt_hits", map[string]any{})
	if strings.Contains(hitsText, `"hits":null`) {
		t.Errorf("hunt_hits отдал hits:null: %s", clip(hitsText, 200))
	}
	out := callToolArgs(t, c, "hunt_hits", map[string]any{})
	hits, ok := out["hits"].([]any)
	if !ok || len(hits) != 0 {
		t.Errorf("hits=%v (%T), хочу пустой массив", out["hits"], out["hits"])
	}
}

// Смоук-G: hunt_run с id при отказе поиска отдавал текст с isError, а без id
// ту же причину структурным JSON - инструмент был не согласован сам с собой.
// Теперь структурный JSON с failed/last_error и без isError.
func TestHuntRunSearchErrorIsStructuredJSON(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/runerr.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	h := &hunt.Runner{Store: st, Search: func(context.Context, string, string, int) (hunt.SearchOutcome, error) {
		return hunt.SearchOutcome{}, errors.New("все движки легли")
	}}
	srv := New(Deps{Version: "test", Store: st, Hunter: h, Started: time.Now()})
	c := inProcess(t, srv)
	initClient(t, c)

	created := callToolArgs(t, c, "hunt_create", map[string]any{"query": "leak"})
	id := int(created["hunt_id"].(float64))

	text, isErr := callToolRaw(t, c, "hunt_run", map[string]any{"id": id})
	if isErr {
		t.Fatalf("отказ поиска с id ушёл в isError, а не в результат: %s", clip(text, 300))
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("ответ не JSON: %v\n%s", err, text)
	}
	if out["failed"] != float64(1) {
		t.Errorf("failed=%v, хочу 1", out["failed"])
	}
	le, _ := out["last_error"].(string)
	if !strings.Contains(le, "все движки легли") {
		t.Errorf("last_error=%q не называет причину", le)
	}
	if hid, _ := out["hunt_id"].(float64); int(hid) != id {
		t.Errorf("hunt_id=%v, хочу %d: ответ не привязан к охоте", out["hunt_id"], id)
	}
}

// Смоук-G: last_run не двигался после отказавших прогонов - «последний
// прогон» врал нулевым временем. Прогон, окончившийся ошибкой поиска,
// всё равно состоялся, и TouchHunt обязан фиксировать его время.
func TestHuntLastRunAdvancesAfterFailedRun(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/lastrun.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	h := &hunt.Runner{Store: st, Search: func(context.Context, string, string, int) (hunt.SearchOutcome, error) {
		return hunt.SearchOutcome{}, errors.New("отказ")
	}}
	srv := New(Deps{Version: "test", Store: st, Hunter: h, Started: time.Now()})
	c := inProcess(t, srv)
	initClient(t, c)

	created := callToolArgs(t, c, "hunt_create", map[string]any{"query": "leak"})
	id := int(created["hunt_id"].(float64))
	list := callToolArgs(t, c, "hunt_list", map[string]any{})
	hunts, _ := list["hunts"].([]any)
	if len(hunts) != 1 {
		t.Fatalf("охот в списке %d, хочу 1", len(hunts))
	}

	_, isErr := callToolRaw(t, c, "hunt_run", map[string]any{"id": id})
	if isErr {
		t.Fatal("hunt_run с отказом поиска не должен быть isError")
	}
	list2 := callToolArgs(t, c, "hunt_list", map[string]any{})
	hunts2, _ := list2["hunts"].([]any)
	row2, _ := hunts2[0].(map[string]any)
	lr, _ := row2["last_run"].(string)
	if lr == "" || strings.HasPrefix(lr, "0001-") {
		t.Errorf("last_run=%q после отказавшего прогона: отказ съел факт прогона", lr)
	}
}

// Смоук-G: задачи collect_files - таблица tasks не имела ни одного
// вызывающего, tasks_total стоял нулём после успешного сбора при обещании
// описания. Проверяется полный жизненный цикл: регистрация, done, счётчик.
func TestCollectFilesRegistersTaskLifecycle(t *testing.T) {
	dir := t.TempDir()
	st := exportStore(t, dir)
	seedMCPBase(t, st, dir)

	col := catalog.NewCollector(&stubFetcher{body: `<a href="/dump.sql">d</a>`}, st, nil, nil,
		catalog.Config{PerHostDelay: time.Millisecond})
	srv := New(Deps{Version: "test", Store: st, Collector: col, Started: time.Now()})
	c := inProcess(t, srv)
	initClient(t, c)

	statusBefore := callToolArgs(t, c, "status", map[string]any{})
	tasksBefore, _ := statusBefore["db"].(map[string]any)["tasks_total"].(float64)

	out := callToolArgs(t, c, "collect_files", map[string]any{"hosts": []any{"abcdefghijklmnop.onion"}})
	if _, ok := out["task_id"]; !ok {
		t.Fatalf("task_id не отдан: %+v", out)
	}

	status := callToolArgs(t, c, "status", map[string]any{})
	db, _ := status["db"].(map[string]any)
	total, _ := db["tasks_total"].(float64)
	if total != tasksBefore+1 {
		t.Errorf("tasks_total=%v после сбора, хочу %v: задача не зарегистрирована", total, tasksBefore+1)
	}
	running, _ := db["tasks_running"].(float64)
	if running != 0 {
		t.Errorf("tasks_running=%v после завершённого сбора, хочу 0: задача не закрыта", running)
	}

	// Сама запись: статус done, сообщение называет объём сбора.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tasks, _, err := st.TaskStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if tasks != int(total) {
		t.Errorf("TaskStats=%d не сходится с tasks_total=%v", tasks, total)
	}
	taskID, _ := out["task_id"].(string)
	row := mcpDB(t, dir).QueryRowContext(ctx,
		`SELECT status, message FROM tasks WHERE id = ?`, taskID)
	var status2, msg string
	if err := row.Scan(&status2, &msg); err != nil {
		t.Fatalf("задача %s не найдена в таблице tasks: %v", taskID, err)
	}
	if status2 != "done" {
		t.Errorf("статус задачи=%q, хочу done", status2)
	}
	// Стартовое сообщение называет хостов, финальное - объём сбора.
	if !strings.Contains(msg, "файлов 1") {
		t.Errorf("сообщение задачи=%q не называет число файлов", msg)
	}
}

// Смоук-G: metrics обязан называть hunt_hits_total из базы - счётчик в
// памяти рождался бы нулём после перезапуска при непустой истории. Отказ
// базы не подменяется нулём, а попадает в problems.
func TestMetricsHuntHitsTotalFromStore(t *testing.T) {
	dir := t.TempDir()
	st := exportStore(t, dir)
	seedMCPBase(t, st, dir)
	ctx := context.Background()
	if err := st.UpsertOnion(ctx, store.Onion{URL: "http://a.onion/", Status: "live"}); err != nil {
		t.Fatal(err)
	}
	huntID, err := st.CreateHunt(ctx, store.Hunt{Query: "q", Mode: "auto", ScheduleMin: 60})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SaveHuntHits(ctx, huntID, "q", "auto", []string{"http://a.onion/1", "http://a.onion/2"}); err != nil {
		t.Fatalf("вставка находок: %v", err)
	}

	srv := New(Deps{Version: "test", Store: st, Started: time.Now()})
	c := inProcess(t, srv)
	initClient(t, c)

	out := callToolArgs(t, c, "metrics", map[string]any{})
	if n, _ := out["hunt_hits_total"].(float64); n != 2 {
		t.Errorf("hunt_hits_total=%v, хочу 2 из базы", out["hunt_hits_total"])
	}

	// Повреждённая таблица - не молчаливый ноль, а ключ problems.
	dropMCPTable(t, dir, "hunt_hits")
	out2 := callToolArgs(t, c, "metrics", map[string]any{})
	if _, ok := out2["hunt_hits_total"]; ok {
		t.Errorf("hunt_hits_total присутствует при нечитаемой таблице: число взято не из базы")
	}
	requireProblem(t, out2, "находки охот")
}

// CountHuntHits - прямое имя счётчика: пустая база ноль, вставки видны.
func TestStoreCountHuntHits(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/cnt.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	n, err := st.CountHuntHits(ctx)
	if err != nil || n != 0 {
		t.Fatalf("пустая база: n=%v err=%v, хочу 0/nil", n, err)
	}
	hid, err := st.CreateHunt(ctx, store.Hunt{Query: "q", Mode: "auto"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SaveHuntHits(ctx, hid, "q", "auto", []string{"http://a.onion/1"}); err != nil {
		t.Fatal(err)
	}
	// Повторная запись тех же url - times_seen, а не вторая строка.
	if _, err := st.SaveHuntHits(ctx, hid, "q", "auto", []string{"http://a.onion/1"}); err != nil {
		t.Fatal(err)
	}
	n, err = st.CountHuntHits(ctx)
	if err != nil || n != 1 {
		t.Errorf("n=%v err=%v, хочу 1: дубликат url не должен давать вторую строку", n, err)
	}
}

// Смоук-G-8/I-9: last_probe шёл RFC3339Nano в локальной зоне - не сравним с
// UTC-метками остальных ответов, наносекунды прятали секунды. Нулевое время -
// null, а не «1 год от рождества».
func TestEngineHealthOutUTCSecondsAndNull(t *testing.T) {
	loc := time.FixedZone("мск", 3*3600)
	probed := time.Date(2026, 2, 5, 12, 44, 6, 945999999, loc)
	list := []searchers.EngineHealth{
		{Name: "ahmia", URL: "https://ahmia", Live: true, Probes: 3, LastProbe: probed, LastError: ""},
		{Name: "fresh", URL: "https://fresh", Live: false, Probes: 0, LastProbe: time.Time{}, LastError: ""},
		{Name: "broken", URL: "https://broken", Live: false, Probes: 2, LastProbe: probed, LastError: "timeout"},
	}
	out := engineHealthOut(list)
	if len(out) != 3 {
		t.Fatalf("движков %d, хочу 3", len(out))
	}
	got, _ := out[0]["last_probe"].(string)
	if !strings.HasSuffix(got, "Z") || strings.Contains(got, ".") {
		t.Errorf("last_probe=%q, хочу UTC-секунды RFC3339 (суффикс Z, без дроби)", got)
	}
	want := probed.UTC().Format(time.RFC3339)
	if got != want {
		t.Errorf("last_probe=%q, хочу %q: зона не приведена к UTC", got, want)
	}
	if v, ok := out[1]["last_probe"]; !ok || v != nil {
		t.Errorf("last_probe непроверенного = %v (%T), хочу отсутствующий null", v, v)
	}
	if _, ok := out[0]["last_error"]; ok {
		t.Error("last_error присутствует при пустой причине: ключ-обещание без события")
	}
	if v, _ := out[2]["last_error"].(string); v != "timeout" {
		t.Errorf("last_error=%q, хочу причину отказа", v)
	}
	for _, row := range out {
		for _, k := range []string{"name", "url", "live", "latency_avg_ms", "success_rate", "fail_streak", "probes", "successes", "disabled"} {
			if _, ok := row[k]; !ok {
				t.Errorf("у движка нет поля %q: набор полей зависит от данных - клиент не может разобрать ответ одним типом", k)
			}
		}
	}
}

// Смоук-I: onion_search без ключа сортировки отдавал адреса в порядке,
// зависящем от строк SQLite - два одинаковых вызова могли расходиться. Хвост
// url ASC делает порядок детерминированным.
func TestSearchOnionsDeterministicOrder(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/ord.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// Одинаковые метрики и статус: все ключи сортировки, кроме url, совпадают.
	for _, u := range []string{
		"http://m.onion/", "http://a.onion/", "http://z.onion/", "http://c.onion/",
	} {
		if err := st.UpsertOnion(ctx, store.Onion{URL: u, Status: "live", LatencyAvg: 100, SuccessRate: 0.5}); err != nil {
			t.Fatal(err)
		}
	}
	first, err := st.SearchOnions(ctx, "onion", "live", 50, false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := st.SearchOnions(ctx, "onion", "live", 50, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 4 {
		t.Fatalf("найдено %d, хочу 4", len(first))
	}
	same := true
	for i := range first {
		if first[i].URL != second[i].URL {
			same = false
		}
	}
	if !same {
		t.Fatalf("два одинаковых вызова разошлись порядком: %v vs %v", urlsOf(first), urlsOf(second))
	}
	// Релевантность одинакова (нет текста в заголовках), порядок - url ASC.
	for i := 1; i < len(first); i++ {
		if first[i-1].URL > first[i].URL {
			t.Errorf("порядок не url ASC: %v", urlsOf(first))
			break
		}
	}
}

func urlsOf(list []store.Onion) []string {
	out := make([]string, 0, len(list))
	for _, o := range list {
		out = append(out, o.URL)
	}
	return out
}

// Смоук-I: явный max_addresses=0 клампился в 1 - «не возвращай адреса, только
// счётчики» подсовывал одну запись. Ноль - легальное пустое окно.
func TestDiscoverMaxAddressesZeroGivesEmptyWindow(t *testing.T) {
	pool := discoverPool(t, &stubFetcher{body: addrPage})
	c := newClient(t, Deps{Version: "test", Store: pool.Store, Discover: pool, Started: time.Now()})
	defer c.Close()

	out := callToolArgs(t, c, "discover_onions", map[string]any{"crawl": false, "max_addresses": 0})
	if got := anyLen(out["addresses"]); got != 0 {
		t.Fatalf("max_addresses=0 вернул %d адресов, хочу пустое окно", got)
	}
	if total, _ := out["addresses_total"].(float64); total != 5 {
		t.Errorf("addresses_total=%v, хочу 5: счётчики живут без окна", out["addresses_total"])
	}
	if trunc, _ := out["truncated"].(bool); !trunc {
		t.Error("truncated не выставлен: окно меньше пула обязано называться обрезкой")
	}
}

// Смоук-G: peer_sync без VOIDSEARCH_PEERS отдавал isError - слепой агент
// начинал лечить здоровую одиночную ноду. Пустой список - состояние.
func TestPeerSyncNoPeersIsStateNotError(t *testing.T) {
	dir := t.TempDir()
	st := exportStore(t, dir)
	seedMCPBase(t, st, dir)

	srv := New(Deps{Version: "test", Store: st, Started: time.Now()})
	c := inProcess(t, srv)
	initClient(t, c)

	text, isErr := callToolRaw(t, c, "peer_sync", map[string]any{})
	if isErr {
		t.Fatalf("peer_sync без пиров - ошибка вызова: %s", clip(text, 300))
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("ответ не JSON: %v\n%s", err, text)
	}
	peers, ok := out["peers"].([]any)
	if !ok || len(peers) != 0 {
		t.Errorf("peers=%v (%T), хочу пустой массив", out["peers"], out["peers"])
	}
	if n, _ := out["merged_onions"].(float64); n != 0 {
		t.Errorf("merged_onions=%v, хочу 0", out["merged_onions"])
	}
	if note, _ := out["note"].(string); !strings.Contains(note, "VOIDSEARCH_PEERS") {
		t.Errorf("note=%q не называет источник состояния", note)
	}
}

// httpProbeSession - мок HTTP-сессии mcp-go: реализует интерфейс, по которому
// notifyHunt узнаёт streamable HTTP. Отправка уведомления в такую сессию
// переключает её в SSE навсегда - протокол обязан видеть это до отправки.
type httpProbeSession struct {
	notifications []string
	upgraded      bool
}

func (s *httpProbeSession) Initialize()       {}
func (s *httpProbeSession) Initialized() bool { return true }
func (s *httpProbeSession) NotificationChannel() chan<- mcp.JSONRPCNotification {
	ch := make(chan mcp.JSONRPCNotification, 4)
	go func() {
		for n := range ch {
			s.notifications = append(s.notifications, n.Method)
		}
	}()
	return ch
}
func (s *httpProbeSession) SessionID() string { return "http-probe" }
func (s *httpProbeSession) UpgradeToSSEWhenReceiveNotification() {
	s.upgraded = true
}

// stdioProbeSession - мок обычной сессии без HTTP-расширения: канал stdio
// безопасен, уведомление уходит.
type stdioProbeSession struct {
	notifications []string
}

func (s *stdioProbeSession) Initialize()       {}
func (s *stdioProbeSession) Initialized() bool { return true }
func (s *stdioProbeSession) NotificationChannel() chan<- mcp.JSONRPCNotification {
	ch := make(chan mcp.JSONRPCNotification, 4)
	go func() {
		for n := range ch {
			s.notifications = append(s.notifications, n.Method)
		}
	}()
	return ch
}
func (s *stdioProbeSession) SessionID() string { return "stdio-probe" }

// Смоук-I (критичный): отправка уведомления из контекста HTTP-сессии
// помечает её upgradeToSSE навсегда, и клиент на plain JSON ломался об
// event-строки. В HTTP-транспорте push не отправляется вовсе.
func TestNotifyHuntSkipsHTTPSessions(t *testing.T) {
	d := Deps{Version: "test", Started: time.Now()}
	srv := New(d)
	d.Srv = srv

	httpSess := &httpProbeSession{}
	ctx := srv.WithContext(context.Background(), httpSess)
	d.notifyHunt(ctx, []hunt.Hit{{
		HuntID: 1, Query: "q", Changed: true, URLs: []string{"http://a.onion/x"},
	}})
	if httpSess.upgraded {
		t.Error("HTTP-сессия переведена в SSE: уведомление обязано пропускаться в HTTP-транспорте")
	}
	if len(httpSess.notifications) != 0 {
		t.Errorf("в HTTP-сессию ушли уведомления: %v", httpSess.notifications)
	}

	stdioSess := &stdioProbeSession{}
	ctx2 := srv.WithContext(context.Background(), stdioSess)
	d.notifyHunt(ctx2, []hunt.Hit{{
		HuntID: 1, Query: "q", Changed: true, URLs: []string{"http://a.onion/x"},
	}})
	// Канал читает горутина мока: доставка асинхронна, ждём её.
	deadline := time.Now().Add(2 * time.Second)
	for len(stdioSess.notifications) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(stdioSess.notifications) != 1 {
		t.Errorf("stdio-сессия не получила уведомление: push обязан работать вне HTTP: %v", stdioSess.notifications)
	}
}

// Смоук-G: fetch обещал «статус, заголовки, распакованное тело», а заголовков
// в ответе не было. Живой httptest-сервер: заголовки обязаны доезжать.
func TestFetchCarriesHeaders(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Test-Header", "значение")
		fmt.Fprint(w, "тело")
	}))
	t.Cleanup(ts.Close)

	st, err := store.Open(t.TempDir() + "/fetch.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	cl, err := httpc.NewClient(context.Background(), httpc.Options{Transport: "direct", Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cl.Close() })
	eng := &search.Engine{Store: st, Client: cl, AllowPrivateTarget: true}
	c := inProcess(t, New(Deps{Version: "test", Store: st, Search: eng, Started: time.Now()}))
	initClient(t, c)

	out := callToolArgs(t, c, "fetch", map[string]any{"url": ts.URL + "/doc"})
	headers, ok := out["headers"].(map[string]any)
	if !ok {
		t.Fatalf("headers=%v (%T), хочу map: описание обещает заголовки", out["headers"], out["headers"])
	}
	// http.Header сериализуется map[string][]string: значения - массивы.
	ct, _ := headers["Content-Type"].([]any)
	if len(ct) < 1 || !strings.Contains(ct[0].(string), "text/plain") {
		t.Errorf("Content-Type=%v не дошёл до ответа", headers["Content-Type"])
	}
	x, _ := headers["X-Test-Header"].([]any)
	if len(x) < 1 || x[0] != "значение" {
		t.Errorf("X-Test-Header=%v: нестандартный заголовок потерян", headers["X-Test-Header"])
	}
}

// Фиксация смоук-G-12: engines["onion_total"] исчез из status (переехал в
// db.onion_total и в stats с пояснением) - статус не должен отдавать
// один ключ с двумя смыслами.
func TestStatusSearchBlockHasNoOnionTotal(t *testing.T) {
	eng, st := testEngineWithStub(t)
	srv := New(Deps{Version: "test", Store: st, Search: eng, Started: time.Now()})
	c := inProcess(t, srv)
	initClient(t, c)

	out := callToolArgs(t, c, "status", map[string]any{})
	engines, ok := out["search"].(map[string]any)
	if !ok {
		t.Fatalf("search=%v (%T), хочу map", out["search"], out["search"])
	}
	if _, ok := engines["onion_total"]; ok {
		t.Error("search.onion_total присутствует: один ключ - два смысла (число движков и размер пула)")
	}
	if _, ok := engines["onion_engines"]; !ok {
		t.Error("у блока search нет ключа onion_engines: вычистка лишнего не должна трогать живые ключи")
	}
}

// Тот же контракт при живых замерах: onionCounts с total>0 - единственная
// ветка, где onion_total когда-то жил. Без движка в HealthPool ветка
// молчит, и проверка выше не видит возврата вычищенного ключа.
func TestStatusSearchBlockHasNoOnionTotalWithEngines(t *testing.T) {
	eng, st := testEngineWithStub(t)
	eng.Health = searchers.NewHealthPool(nil, []*searchers.OnionEngine{
		{Name_: "stub-onion", Base: "https://stub.onion"},
	})
	srv := New(Deps{Version: "test", Store: st, Search: eng, Started: time.Now()})
	c := inProcess(t, srv)
	initClient(t, c)

	out := callToolArgs(t, c, "status", map[string]any{})
	engines, ok := out["search"].(map[string]any)
	if !ok {
		t.Fatalf("search=%v (%T), хочу map", out["search"], out["search"])
	}
	if _, ok := engines["onion_total"]; ok {
		t.Error("search.onion_total присутствует при живых замерах: один ключ - два смысла (число движков и размер пула)")
	}
	if _, ok := engines["onion_live"]; !ok {
		t.Error("у блока search нет ключа onion_live при непустом onion_engines")
	}
}

// Смоук-I-Q10/H-Q11: лог шума ротации. Пустой control-адрес - свойство
// конфигурации, а не событие: причина называется один раз за жизнь
// процесса, Rotate молча возвращает nil. Полное покрытие - в
// internal/netx (TestExternalRotateNoControlQuiet), здесь фиксируется,
// что статус по-прежнему жив без движков и называет версию.
func TestStatusWithoutEngineNamesVersion(t *testing.T) {
	srv := New(Deps{Version: "vss178", Started: time.Now()})
	c := inProcess(t, srv)
	initClient(t, c)
	out := callToolArgs(t, c, "status", map[string]any{})
	if v, _ := out["version"].(string); v != "vss178" {
		t.Errorf("version=%q, хочу vss178", v)
	}
}

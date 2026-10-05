package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"voidsearchswag/internal/store"
)

// Замер до правки на HEAD ffb3312, копия боевой базы: пул 11088 записей, живых
// 1773, совпадений со словом leak - 11.
//
//	poolsearch --limit 3 leak
//	  rc=0, «пул: всего 11088, живых 1773; найдено 11, показано 3: потолок выдачи 500»
//	poolsearch --limit 3 leak --json
//	  rc=0, count=3, stderr: WARN: совпадений 11, показано 3: потолок выдачи 500
//	poolsearch --limit 1000 leak
//	  rc=0, «найдено 11», оговорки нет
//
// Потолок в 500 записей не участвовал ни в одном из двух первых прогонов: три
// записи показал сам оператор. Текст утверждал обратное.
func TestPoolTruncationReasonNamesOperatorLimit(t *testing.T) {
	got := poolTruncationReason(11, 3, 3)
	if got != "действует --limit 3" {
		t.Errorf("причина %q, хочу %q", got, "действует --limit 3")
	}
	if strings.Contains(got, "потолок") {
		t.Errorf("причина ссылается на потолок, которого не было: %q", got)
	}
}

func TestPoolTruncationReasonNamesStoreCeiling(t *testing.T) {
	ceiling := store.OnionSearchLimit()
	got := poolTruncationReason(1200, ceiling, 1000)
	want := fmt.Sprintf("потолок выдачи %d", ceiling)
	if got != want {
		t.Errorf("причина %q, хочу %q", got, want)
	}
}

// --limit выше потолка всё равно упирается в хранилище: виноват потолок, а не
// флаг, и текст обязан это сказать.
func TestPoolTruncationReasonCeilingWinsAboveCeiling(t *testing.T) {
	ceiling := store.OnionSearchLimit()
	got := poolTruncationReason(1200, ceiling, ceiling*10)
	if !strings.Contains(got, fmt.Sprintf("потолок выдачи %d", ceiling)) {
		t.Errorf("причина %q, хочу упоминание потолка", got)
	}
	if strings.Contains(got, "--limit") {
		t.Errorf("причина винит --limit при запросе сверх потолка: %q", got)
	}
}

func TestPoolTruncationReasonEmptyWhenFull(t *testing.T) {
	for _, tc := range []struct{ matched, shown, limit int }{
		{11, 11, 30},
		{0, 0, 30},
		{5, 30, 30},
	} {
		if got := poolTruncationReason(tc.matched, tc.shown, tc.limit); got != "" {
			t.Errorf("matched=%d shown=%d limit=%d: причина %q, хочу пустую",
				tc.matched, tc.shown, tc.limit, got)
		}
	}
}

// Флаг не задан или пришёл мусором, а хранилище подставило свой дефолт: винить
// --limit нельзя, и потолок тоже ни при чём, если записей меньше потолка.
func TestPoolTruncationReasonWithoutFlag(t *testing.T) {
	got := poolTruncationReason(150, 100, 0)
	if got == "" {
		t.Fatal("усечение есть, а причина пустая")
	}
	if strings.Contains(got, "--limit") {
		t.Errorf("причина винит флаг, который не задан: %q", got)
	}
	if strings.Contains(got, fmt.Sprintf("потолок выдачи %d", store.OnionSearchLimit())) {
		t.Errorf("причина винит потолок, до которого далеко: %q", got)
	}
	if !strings.Contains(got, "дефолт выборки") {
		t.Errorf("причина не называет дефолт хранилища: %q", got)
	}
	if strings.Contains(got, "показано") {
		t.Errorf("причина повторяет число, которое уже напечатано рядом: %q", got)
	}
	if same := poolTruncationReason(150, 100, -5); same != got {
		t.Errorf("мусорный --limit -5 дал %q, а незаданный флаг %q: причина одна", same, got)
	}
}

func TestPrintPoolHeaderNamesLimitReason(t *testing.T) {
	out := captureStdout(t, func() {
		printPoolHeader(11088, 1773, nil, 11, nil, 3, 3)
	})
	if !strings.Contains(out, "найдено 11, показано 3: действует --limit 3") {
		t.Errorf("шапка не назвала волю оператора:\n%s", out)
	}
	if strings.Contains(out, "потолок") {
		t.Errorf("шапка снова винит потолок:\n%s", out)
	}
}

func TestPrintPoolHeaderKeepsCeilingReason(t *testing.T) {
	ceiling := store.OnionSearchLimit()
	out := captureStdout(t, func() {
		printPoolHeader(1500, 900, nil, 1200, nil, ceiling, 1000)
	})
	if !strings.Contains(out, fmt.Sprintf("найдено 1200, показано %d: потолок выдачи %d", ceiling, ceiling)) {
		t.Errorf("шапка не назвала потолок:\n%s", out)
	}
}

// fillPool пишет в базу n записей, у которых в названии есть слово clip, чтобы
// запрос «clip host» совпал со всеми.
func fillPool(t *testing.T, dir, prefix string, n int) {
	t.Helper()
	st, err := store.Open(filepath.Join(dir, "voidsearchswag.db"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		o := store.Onion{
			URL:    fmt.Sprintf("http://%s%04dhostaaaa.onion/", prefix, i),
			Title:  fmt.Sprintf("clip host %d", i),
			Status: "live",
		}
		if err := st.UpsertOnion(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPoolSearchCLILimitReasonInRealRun(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)
	t.Setenv("VOIDSEARCH_BACKUP_DIR", "")
	t.Setenv("VOIDSEARCH_TOR", "off")
	t.Setenv("VOIDSEARCH_TRANSPORT", "direct")
	fillPool(t, dir, "lim", 11)

	out, code := runMain(t, "poolsearch", "-limit", "3", "clip host")
	if code != 0 {
		t.Fatalf("код возврата %d: %s", code, firstN(out, 300))
	}
	if !strings.Contains(out, "найдено 11, показано 3: действует --limit 3") {
		t.Errorf("шапка не назвала причину усечения:\n%s", firstN(out, 300))
	}
	if strings.Contains(out, "потолок выдачи") {
		t.Errorf("шапка винит потолок при --limit 3:\n%s", firstN(out, 300))
	}
}

func TestPoolSearchCLIJsonLimitReasonInRealRun(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)
	t.Setenv("VOIDSEARCH_BACKUP_DIR", "")
	t.Setenv("VOIDSEARCH_TOR", "off")
	t.Setenv("VOIDSEARCH_TRANSPORT", "direct")
	fillPool(t, dir, "jsn", 11)

	stdout, stderr, code := runMainSplit(t, "poolsearch", "-limit", "3", "-json", "clip host")
	if code != 0 {
		t.Fatalf("код возврата %d: %s %s", code, firstN(stdout, 200), firstN(stderr, 200))
	}
	if !strings.Contains(stderr, "WARN: совпадений 11, показано 3: действует --limit 3") {
		t.Errorf("предупреждение не назвало причину, фактически %q", stderr)
	}
	if strings.Contains(stderr, "потолок выдачи") {
		t.Errorf("предупреждение винит потолок при --limit 3: %q", stderr)
	}
	if strings.Contains(stdout, "WARN") {
		t.Errorf("предупреждение ушло в stdout и испортило JSON: %s", firstN(stdout, 200))
	}
}

func TestPoolSearchCLICeilingReasonInRealRun(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)
	t.Setenv("VOIDSEARCH_BACKUP_DIR", "")
	t.Setenv("VOIDSEARCH_TOR", "off")
	t.Setenv("VOIDSEARCH_TRANSPORT", "direct")
	ceiling := store.OnionSearchLimit()
	fillPool(t, dir, "ceil", ceiling+100)

	_, stderr, code := runMainSplit(t, "poolsearch", "-limit", "1000", "-json", "clip host")
	if code != 0 {
		t.Fatalf("код возврата %d: %s", code, firstN(stderr, 300))
	}
	want := fmt.Sprintf("WARN: совпадений %d, показано %d: потолок выдачи %d", ceiling+100, ceiling, ceiling)
	if !strings.Contains(stderr, want) {
		t.Errorf("предупреждение не назвало потолок, хочу %q, фактически %q", want, stderr)
	}
	if strings.Contains(stderr, "--limit") {
		t.Errorf("предупреждение винит --limit, хотя сработал потолок: %q", stderr)
	}
}

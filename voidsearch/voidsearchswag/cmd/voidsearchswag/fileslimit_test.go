package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"voidsearchswag/internal/store"
)

// Замер до правки на HEAD 36e4cae, пересобранный бинарник, копия боевой базы:
// каталог 19 файлов, 27.9 MiB.
//
//	files --limit 3    --json  rc=0, ключи [by_ext,catalog_bytes,catalog_total,files,returned], returned=3
//	files --limit 50   --json  rc=0, returned=19
//	files --limit 0    --json  rc=0, returned=19
//	files --limit -5   --json  rc=0, returned=19
//	files --limit 5000 --json  rc=0, returned=19
//	files --limit -5           rc=0, «каталог: 19 файлов, 27.9 MiB», «показано 19:»
//
// Поля limit в отчёте не было ни в одном из прогонов: заявленное значение вообще
// не отражалось, а ноль и мусор молча превращались в дефолт хранилища.
//
// Проба на временной базе из 1200 файлов показала, что именно подменялось:
// SearchFiles(Limit=5000) вернул 1000 строк, Limit=1500 - 1000, Limit=1001 -
// 1000, Limit=1000 - 1000, Limit=999 - 999, Limit=0 - 100, Limit=-5 - 100.
// Потолок 1000 жил в теле функции и вызывающему был недоступен.

// fillCatalog пишет в каталог n файлов, чтобы проверка returned имела предмет:
// на пустой базе returned равен нулю при любом пределе.
func fillCatalog(t *testing.T, dir string, n int) {
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
		f := store.FileEntry{
			URL:      fmt.Sprintf("http://abcdefghijklmnop.onion/f/%04d.bin", i),
			Filename: fmt.Sprintf("f%04d.bin", i),
			Ext:      "bin",
			Size:     10,
		}
		if err := st.AddFile(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestFilesCeilingIsStoreCeiling(t *testing.T) {
	if store.MaxFileSearchLimit != 1000 {
		t.Errorf("store.MaxFileSearchLimit = %d, хочу 1000: замер до правки снят на этом числе",
			store.MaxFileSearchLimit)
	}
}

func TestFilesRejectsNonPositiveLimitInRealRun(t *testing.T) {
	t.Setenv("VOIDSEARCH_DATA_DIR", t.TempDir())
	t.Setenv("VOIDSEARCH_BACKUP_DIR", "")
	t.Setenv("VOIDSEARCH_TOR", "off")
	t.Setenv("VOIDSEARCH_TRANSPORT", "direct")

	for _, value := range []string{"0", "-5", "-1000"} {
		_, stderr, code := runMainSplit(t, "files", "--limit", value, "--json")
		if code == 0 {
			t.Errorf("--limit %s: прогон вернул ноль, хочу отказ", value)
		}
		if !strings.Contains(stderr, "--limit должен быть положительным, получено "+value) {
			t.Errorf("--limit %s: stderr не называет причину, фактически %q", value, stderr)
		}
	}
}

func TestFilesRejectsLimitAboveStoreCeilingInRealRun(t *testing.T) {
	t.Setenv("VOIDSEARCH_DATA_DIR", t.TempDir())
	t.Setenv("VOIDSEARCH_BACKUP_DIR", "")
	t.Setenv("VOIDSEARCH_TOR", "off")
	t.Setenv("VOIDSEARCH_TRANSPORT", "direct")

	for _, value := range []string{"1001", "5000"} {
		_, stderr, code := runMainSplit(t, "files", "--limit", value, "--json")
		if code == 0 {
			t.Errorf("--limit %s: прогон вернул ноль, хочу отказ", value)
		}
		want := "слишком большой: " + value + " (предел " + strconv.Itoa(store.MaxFileSearchLimit) + ")"
		if !strings.Contains(stderr, want) {
			t.Errorf("--limit %s: stderr не называет значение и предел, хочу %q, фактически %q", value, want, stderr)
		}
	}
}

// Рабочие значения проходят проверку, отчёт несёт то число, которое реально ушло
// в запрос, и строк больше этого числа не появляется.
func TestFilesReportsCheckedLimitInRealRun(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)
	t.Setenv("VOIDSEARCH_BACKUP_DIR", "")
	t.Setenv("VOIDSEARCH_TOR", "off")
	t.Setenv("VOIDSEARCH_TRANSPORT", "direct")
	fillCatalog(t, dir, 20)

	for _, value := range []string{"1", "7", "20", "1000"} {
		want, err := strconv.Atoi(value)
		if err != nil {
			t.Fatalf("в тесте опечатка: %v", err)
		}
		stdout, stderr, code := runMainSplit(t, "files", "--limit", value, "--json")
		if code != 0 {
			t.Fatalf("--limit %s: прогон вернул %d, stderr %q", value, code, stderr)
		}
		if strings.Contains(stderr, "--limit") {
			t.Errorf("--limit %s: рабочее значение вызвало предупреждение %q", value, stderr)
		}
		var out struct {
			Returned int `json:"returned"`
			Limit    int `json:"limit"`
			Total    int `json:"catalog_total"`
		}
		if err := json.Unmarshal([]byte(stdout), &out); err != nil {
			t.Fatalf("--limit %s: разбор JSON: %v (%q)", value, err, stdout)
		}
		if out.Limit != want {
			t.Errorf("--limit %s: отчёт несёт limit=%d, хочу %d", value, out.Limit, want)
		}
		if out.Returned > out.Limit {
			t.Errorf("--limit %s: возвращено %d строк больше заявленных %d", value, out.Returned, out.Limit)
		}
		if out.Total != 20 {
			t.Errorf("--limit %s: catalog_total=%d, хочу 20", value, out.Total)
		}
	}
}

// Предел обязан называться числом хранилища, а не общим потолком флагов: 5000
// строк каталог всё равно не отдаст, и пропускать такое значение значит обещать
// оператору больше, чем существует.
func TestFilesLimitCeilingIsNarrowerThanFlagCeiling(t *testing.T) {
	if store.MaxFileSearchLimit >= maxFlagLimit {
		t.Errorf("потолок каталога %d не уже общего потолка флагов %d",
			store.MaxFileSearchLimit, maxFlagLimit)
	}
	if err := checkLimitCeiling("limit", store.MaxFileSearchLimit+1, store.MaxFileSearchLimit); err == nil {
		t.Error("значение сверх потолка каталога принято")
	}
	if err := checkLimitCeiling("limit", store.MaxFileSearchLimit, store.MaxFileSearchLimit); err != nil {
		t.Errorf("потолок каталога отклонён: %v", err)
	}
}

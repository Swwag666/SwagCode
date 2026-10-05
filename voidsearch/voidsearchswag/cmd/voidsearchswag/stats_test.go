package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"voidsearchswag/internal/store"
)

// sqlDB открывает базу напрямую, минуя store.
//
// Прямое соединение нужно потому, что тест ломает схему: поле db у store не
// экспортируется, а публичный API не даёт ни удалить таблицу, ни прочитать базу,
// которую не удаётся прочитать целиком.
func sqlDB(t *testing.T, dir string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", dir+"/voidsearchswag.db")
	if err != nil {
		t.Fatalf("открытие базы: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// seedHunts вносит n охот напрямую: store.CreateHunt проставляет режим и
// расписание по своим правилам, а для сводки важно только число строк.
func seedHunts(t *testing.T, dir string, n int) {
	t.Helper()
	db := sqlDB(t, dir)
	ctx := context.Background()
	for i := 0; i < n; i++ {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO hunts(query, mode, schedule_min) VALUES (?, 'normal', 60)`,
			fmt.Sprintf("query %d", i)); err != nil {
			t.Fatalf("вставка охоты %d: %v", i, err)
		}
	}
}

// dropTable удаляет таблицу, имитируя повреждение базы. Именно так выглядит
// ситуация, в которой счётчик невозможно прочитать, и именно в ней прежняя
// версия stats печатала ноль без единого признака проблемы.
func dropTable(t *testing.T, dir, table string) {
	t.Helper()
	db := sqlDB(t, dir)
	if _, err := db.ExecContext(context.Background(), `DROP TABLE `+table); err != nil {
		t.Fatalf("удаление таблицы %s: %v", table, err)
	}
}

// seedStatsBase создаёт базу с двумя файлами, одной живой записью пула и тремя
// охотами, закрывает её и возвращает каталог.
func seedStatsBase(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	st := openTestStore(t, dir)
	ctx := context.Background()
	mustAddFile(t, st, "http://a.onion/book.epub", "book.epub", "epub", 2048, "t", "")
	mustAddFile(t, st, "http://a.onion/photo.jpg", "photo.jpg", "jpg", 4096, "t", "")
	if err := st.UpsertOnion(ctx, store.Onion{
		URL: "http://a.onion/", Title: "живой", Status: "live",
	}); err != nil {
		t.Fatalf("запись пула: %v", err)
	}
	closeStore(st)
	seedHunts(t, dir, 3)
	return dir
}

func TestStatsDataReadsEverySource(t *testing.T) {
	dir := seedStatsBase(t)

	st := openTestStore(t, dir)
	defer closeStore(st)

	out, problems := statsData(context.Background(), st)
	if len(problems) != 0 {
		t.Errorf("на здоровой базе есть непрочитанные источники: %v", problems)
	}
	if out.FilesTotal != 2 {
		t.Errorf("FilesTotal = %d, ожидала 2", out.FilesTotal)
	}
	if out.FilesBytes != 2048+4096 {
		t.Errorf("FilesBytes = %d, ожидала %d", out.FilesBytes, 2048+4096)
	}
	if out.FilesByExt["epub"] != 1 || out.FilesByExt["jpg"] != 1 {
		t.Errorf("FilesByExt = %v", out.FilesByExt)
	}
	if out.OnionTotal != 1 || out.OnionLive != 1 {
		t.Errorf("пул = %d всего / %d живых, ожидала 1 / 1", out.OnionTotal, out.OnionLive)
	}
	if out.HuntsTotal != 3 {
		t.Errorf("HuntsTotal = %d, ожидала 3", out.HuntsTotal)
	}
}

func TestStatsDataCollectsEveryFailure(t *testing.T) {
	// Ошибки собираются по всем источникам, а не прерывают сбор на первой:
	// иначе поломка одной таблицы спрятала бы поломку второй, и после её
	// починки пользователь нашёл бы новую проблему, о которой команда знала, но
	// промолчала.
	dir := seedStatsBase(t)
	dropTable(t, dir, "hunts")
	dropTable(t, dir, "tasks")

	st := openTestStore(t, dir)
	defer closeStore(st)

	out, problems := statsData(context.Background(), st)
	if len(problems) != 2 {
		t.Fatalf("собрано %d проблем, ожидала 2: %v", len(problems), problems)
	}
	joined := strings.Join(problems, "; ")
	for _, want := range []string{"охоты", "задачи"} {
		if !strings.Contains(joined, want) {
			t.Errorf("источник %q не назван среди проблем: %s", want, joined)
		}
	}
	// Прочие источники остались читаемыми, и их числа обязаны быть точными:
	// частичная поломка не повод обнулять то, что прочитать удалось.
	if out.FilesTotal != 2 || out.FilesBytes != 6144 {
		t.Errorf("файлы = %d (%d байт), ожидала 2 (6144)", out.FilesTotal, out.FilesBytes)
	}
	if out.OnionTotal != 1 {
		t.Errorf("пул = %d, ожидала 1", out.OnionTotal)
	}
	if out.HuntsTotal != 0 {
		t.Errorf("охоты = %d, ожидала 0 при непрочитанной таблице", out.HuntsTotal)
	}
}

func TestStatsReportsUnreadableSource(t *testing.T) {
	dir := seedStatsBase(t)
	dropTable(t, dir, "hunts")
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)

	out, code := runMain(t, "stats")
	if code != 1 {
		t.Errorf("код возврата %d, ожидала 1: неполная сводка не является успехом", code)
	}
	if !strings.Contains(out, "ERROR: stats: охоты") {
		t.Errorf("непрочитанный источник не назван: %s", out)
	}
	if !strings.Contains(out, "no such table") {
		t.Errorf("причина не приведена: %s", out)
	}
	// Точные числа остаются в выводе: потеря одной таблицы не повод молчать об
	// остальном.
	if !strings.Contains(out, "файлы: 2") || !strings.Contains(out, "пул: 1") {
		t.Errorf("точные числа потеряны из вывода: %s", out)
	}
	if !strings.Contains(out, "охоты: 0") {
		t.Errorf("счётчик охот не напечатан: %s", out)
	}
}

func TestStatsJSONCarriesErrors(t *testing.T) {
	dir := seedStatsBase(t)
	dropTable(t, dir, "hunts")
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)

	out, code := runMain(t, "stats", "--json")
	if code != 1 {
		t.Errorf("код возврата %d, ожидала 1", code)
	}

	// JSON обязан оставаться разбираемым: предупреждения в этом режиме не
	// печатаются в stderr, иначе поток, который читает машина, смешался бы с
	// текстом. Если вывод не разберётся, значит это решение нарушено.
	var rep struct {
		FilesTotal int      `json:"files_total"`
		FilesBytes int64    `json:"files_bytes"`
		HuntsTotal int      `json:"hunts_total"`
		OnionTotal int      `json:"onion_total"`
		Errors     []string `json:"errors"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("JSON не разобран: %v\nвывод: %s", err, out)
	}
	if len(rep.Errors) != 1 {
		t.Errorf("в JSON %d ошибок, ожидала 1: %v", len(rep.Errors), rep.Errors)
	}
	if len(rep.Errors) > 0 && !strings.Contains(rep.Errors[0], "охоты") {
		t.Errorf("источник не назван в JSON: %v", rep.Errors)
	}
	if rep.FilesTotal != 2 || rep.FilesBytes != 6144 || rep.OnionTotal != 1 {
		t.Errorf("точные числа в JSON искажены: %+v", rep)
	}
	if rep.HuntsTotal != 0 {
		t.Errorf("hunts_total = %d, ожидала 0", rep.HuntsTotal)
	}
}

func TestStatsHealthyExitsZero(t *testing.T) {
	// На здоровой базе поведение прежнее: нулевой код возврата и никаких
	// предупреждений. Тест нужен, чтобы новое правило «неполная сводка - это
	// ошибка» не сработало там, где ошибки нет.
	dir := seedStatsBase(t)
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)

	out, code := runMain(t, "stats")
	if code != 0 {
		t.Errorf("код возврата %d на здоровой базе, ожидала 0\nвывод: %s", code, out)
	}
	if strings.Contains(out, "ERROR") {
		t.Errorf("на здоровой базе напечатана ошибка: %s", out)
	}
	if !strings.Contains(out, "охоты: 3") {
		t.Errorf("число охот неверно: %s", out)
	}
	if !strings.Contains(out, "файлы: 2 (6.0 KiB)") {
		t.Errorf("число и объём файлов неверны: %s", out)
	}

	jsonOut, jsonCode := runMain(t, "stats", "--json")
	if jsonCode != 0 {
		t.Errorf("код возврата JSON %d, ожидала 0", jsonCode)
	}
	if strings.Contains(jsonOut, `"errors"`) {
		t.Errorf("на здоровой базе в JSON появилось поле errors: %s", jsonOut)
	}
}

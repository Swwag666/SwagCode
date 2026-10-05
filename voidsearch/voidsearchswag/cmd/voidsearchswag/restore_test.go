package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"voidsearchswag/internal/store"
)

// makeDatabase собирает базу с n адресами в пуле. Разное n нужно, чтобы по числу
// адресов после восстановления было видно, какой именно снимок лёг на место базы:
// одинаковые базы не отличили бы восстановление от отказа.
func makeDatabase(t *testing.T, path string, n int) {
	t.Helper()
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := st.Migrate(ctx); err != nil {
		st.Close()
		t.Fatalf("migrate %s: %v", path, err)
	}
	for i := 0; i < n; i++ {
		u := fmt.Sprintf("http://host%d.onion", i)
		if err := st.UpsertOnion(ctx, store.Onion{URL: u, Status: "live"}); err != nil {
			st.Close()
			t.Fatalf("upsert %s: %v", u, err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close %s: %v", path, err)
	}
}

// restoreFixture готовит каталог данных: рабочая база с одним адресом и два
// снимка в backups/ - старый с двумя адресами и новый с тремя.
func restoreFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	backups := filepath.Join(dir, "backups")
	if err := os.MkdirAll(backups, 0o755); err != nil {
		t.Fatalf("каталог снимков: %v", err)
	}
	makeDatabase(t, filepath.Join(dir, "voidsearchswag.db"), 1)
	makeDatabase(t, filepath.Join(dir, "snap-old.db"), 2)
	makeDatabase(t, filepath.Join(dir, "snap-new.db"), 3)
	if err := os.Rename(filepath.Join(dir, "snap-old.db"), filepath.Join(backups, "voidsearchswag-20260926-090000.000.db")); err != nil {
		t.Fatalf("старый снимок: %v", err)
	}
	if err := os.Rename(filepath.Join(dir, "snap-new.db"), filepath.Join(backups, "voidsearchswag-20260927-120000.000.db")); err != nil {
		t.Fatalf("новый снимок: %v", err)
	}
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)
	t.Setenv("VOIDSEARCH_BACKUP_DIR", "")
	t.Setenv("VOIDSEARCH_TOR", "off")
	t.Setenv("VOIDSEARCH_TRANSPORT", "direct")
	return dir
}

// onionTotal читает число адресов в пуле той же функцией, которой пользуется
// stats: подпроцесс для этого поднимать незачем.
func onionTotal(t *testing.T, dbPath string) int {
	t.Helper()
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open %s: %v", dbPath, err)
	}
	defer st.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, _ := statsData(ctx, st)
	return out.OnionTotal
}

func decodeRestore(t *testing.T, stdout string) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal([]byte(stdout), &body); err != nil {
		t.Fatalf("stdout не JSON: %v (%.300s)", err, stdout)
	}
	return body
}

// Восстановление перезаписывает рабочую базу, поэтому без подтверждения команда
// обязана напечатать план и выйти с кодом 1, ничего не тронув.
func TestCmdRestoreNeedsConfirmation(t *testing.T) {
	dir := restoreFixture(t)
	dbPath := filepath.Join(dir, "voidsearchswag.db")

	stdout, stderr, code := runMainSplit(t, "restore", "--json")
	if code != 1 {
		t.Fatalf("код возврата %d, хочу 1", code)
	}
	if got := onionTotal(t, dbPath); got != 1 {
		t.Errorf("пул %d, хочу 1: отказ не имеет права менять базу", got)
	}
	body := decodeRestore(t, stdout)
	reason, _ := body["error"].(string)
	for _, want := range []string{"--yes", "voidsearchswag-20260927-120000.000.db", dbPath} {
		if !strings.Contains(reason, want) {
			t.Errorf("в плане нет %q: %s", want, reason)
		}
	}
	if !strings.Contains(stderr, "--yes") {
		t.Errorf("в stderr нет способа подтвердить: %q", firstN(stderr, 200))
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "pre-restore-") {
			t.Errorf("отказ создал %s", e.Name())
		}
	}
}

func TestCmdRestoreLatestWithYes(t *testing.T) {
	dir := restoreFixture(t)
	dbPath := filepath.Join(dir, "voidsearchswag.db")

	stdout, stderr, code := runMainSplit(t, "restore", "--yes", "--json")
	if code != 0 {
		t.Fatalf("код возврата %d, хочу 0 (stderr %q)", code, firstN(stderr, 300))
	}
	body := decodeRestore(t, stdout)
	if body["integrity"] != "ok" {
		t.Errorf("integrity = %v, хочу ok", body["integrity"])
	}
	snap, _ := body["snapshot"].(string)
	if filepath.Base(snap) != "voidsearchswag-20260927-120000.000.db" {
		t.Errorf("взят не последний снимок: %v", body["snapshot"])
	}
	if body["restored_to"] != dbPath {
		t.Errorf("restored_to = %v, хочу %s", body["restored_to"], dbPath)
	}
	saved, _ := body["previous_saved_as"].(string)
	if filepath.Base(saved) == "" || strings.HasPrefix(filepath.Base(saved), "voidsearchswag-") {
		t.Errorf("прежняя база не сохранена под безопасным именем: %v", body["previous_saved_as"])
	}
	if _, err := os.Stat(saved); err != nil {
		t.Errorf("файл прежней базы не создан: %v", err)
	}
	if got := onionTotal(t, dbPath); got != 3 {
		t.Errorf("пул %d, хочу 3: снимок не лёг на место базы", got)
	}
	// Прежняя база обязана остаться читаемой: откат назад - та же команда.
	if got := onionTotal(t, saved); got != 1 {
		t.Errorf("в сохранённой прежней базе пул %d, хочу 1", got)
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Stat(dbPath + suffix); err == nil {
			t.Errorf("рядом с восстановленной базой остался %s", suffix)
		}
	}
}

func TestCmdRestoreNamedSnapshot(t *testing.T) {
	dir := restoreFixture(t)
	dbPath := filepath.Join(dir, "voidsearchswag.db")

	stdout, stderr, code := runMainSplit(t, "restore", "--snapshot", "voidsearchswag-20260926-090000.000.db", "--yes", "--json")
	if code != 0 {
		t.Fatalf("код возврата %d, хочу 0 (stderr %q)", code, firstN(stderr, 300))
	}
	body := decodeRestore(t, stdout)
	if filepath.Base(fmt.Sprint(body["snapshot"])) != "voidsearchswag-20260926-090000.000.db" {
		t.Errorf("взят не тот снимок: %v", body["snapshot"])
	}
	if got := onionTotal(t, dbPath); got != 2 {
		t.Errorf("пул %d, хочу 2", got)
	}
}

// Полный путь тоже принимается: снимки хранят и вне каталога бэкапов.
func TestCmdRestoreByFullPath(t *testing.T) {
	dir := restoreFixture(t)
	snap := filepath.Join(dir, "backups", "voidsearchswag-20260926-090000.000.db")

	_, stderr, code := runMainSplit(t, "restore", "--snapshot", snap, "--yes", "--json")
	if code != 0 {
		t.Fatalf("код возврата %d, хочу 0 (stderr %q)", code, firstN(stderr, 300))
	}
	if got := onionTotal(t, filepath.Join(dir, "voidsearchswag.db")); got != 2 {
		t.Errorf("пул %d, хочу 2", got)
	}
}

func TestCmdRestoreUnknownSnapshot(t *testing.T) {
	dir := restoreFixture(t)
	dbPath := filepath.Join(dir, "voidsearchswag.db")

	stdout, _, code := runMainSplit(t, "restore", "--snapshot", "нет-такого.db", "--yes", "--json")
	if code != 1 {
		t.Fatalf("код возврата %d, хочу 1", code)
	}
	if got := onionTotal(t, dbPath); got != 1 {
		t.Errorf("пул %d, хочу 1: отказ не меняет базу", got)
	}
	reason, _ := decodeRestore(t, stdout)["error"].(string)
	if !strings.Contains(reason, "не найден") || !strings.Contains(reason, "voidsearchswag-20260927-120000.000.db") {
		t.Errorf("в отказе нет ни факта, ни подсказки: %s", reason)
	}
}

// Побитая копия хуже отсутствия восстановления: она стирает и ту базу, которая
// ещё подавала признаки жизни. Поэтому снимок проверяется до замены.
func TestCmdRestoreRejectsCorruptSnapshot(t *testing.T) {
	dir := restoreFixture(t)
	dbPath := filepath.Join(dir, "voidsearchswag.db")
	bad := filepath.Join(dir, "backups", "voidsearchswag-20260929-010101.000.db")
	if err := os.WriteFile(bad, []byte(strings.Repeat("это не база sqlite ", 60)), 0o644); err != nil {
		t.Fatalf("фикстура битого снимка: %v", err)
	}

	stdout, _, code := runMainSplit(t, "restore", "--yes", "--json")
	if code != 1 {
		t.Fatalf("код возврата %d, хочу 1", code)
	}
	reason, _ := decodeRestore(t, stdout)["error"].(string)
	if !strings.Contains(reason, "не читается как база") {
		t.Errorf("в отказе нет причины: %s", reason)
	}
	if got := onionTotal(t, dbPath); got != 1 {
		t.Errorf("пул %d, хочу 1: база обязана уцелеть", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "pre-restore-") {
			t.Errorf("отказ по целостности создал %s", e.Name())
		}
	}
}

// Отдельный случай - снимок, который открывается как база, но побит внутри.
// Именно он опаснее мусорного файла: копирование прошло бы без ошибки открытия,
// и на месте рабочей базы оказалась бы нечитаемая схема.
func TestCmdRestoreRejectsDamagedSnapshot(t *testing.T) {
	dir := restoreFixture(t)
	dbPath := filepath.Join(dir, "voidsearchswag.db")
	good := filepath.Join(dir, "backups", "voidsearchswag-20260926-090000.000.db")
	bad := filepath.Join(dir, "backups", "voidsearchswag-20260929-010101.000.db")
	body, err := os.ReadFile(good)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, body, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(bad, os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	// Нули поверх второй страницы: там корень sqlite_master.
	if _, err := f.WriteAt(make([]byte, 512), 4096); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	stdout, _, code := runMainSplit(t, "restore", "--yes", "--json")
	if code != 1 {
		t.Fatalf("код возврата %d, хочу 1", code)
	}
	reason, _ := decodeRestore(t, stdout)["error"].(string)
	if !strings.Contains(reason, "повреждён") || !strings.Contains(reason, "integrity_check") {
		t.Errorf("в отказе нет ни факта порчи, ни результата проверки: %s", reason)
	}
	if got := onionTotal(t, dbPath); got != 1 {
		t.Errorf("пул %d, хочу 1: база обязана уцелеть", got)
	}
}

func TestCmdRestoreNoSnapshots(t *testing.T) {
	restoreFixture(t)
	empty := t.TempDir()

	stdout, _, code := runMainSplit(t, "restore", "--dir", empty, "--yes", "--json")
	if code != 1 {
		t.Fatalf("код возврата %d, хочу 1", code)
	}
	reason, _ := decodeRestore(t, stdout)["error"].(string)
	if !strings.Contains(reason, "снимков нет") {
		t.Errorf("в отказе не названа причина: %s", reason)
	}
}

// Восстановление базы саму в себя означало бы удалить её и положить обрывок на
// место, поэтому совпадение путей отлавливается до любых изменений.
func TestCmdRestoreRefusesSelfSnapshot(t *testing.T) {
	dir := restoreFixture(t)
	dbPath := filepath.Join(dir, "voidsearchswag.db")

	stdout, _, code := runMainSplit(t, "restore", "--snapshot", dbPath, "--yes", "--json")
	if code != 1 {
		t.Fatalf("код возврата %d, хочу 1", code)
	}
	reason, _ := decodeRestore(t, stdout)["error"].(string)
	if !strings.Contains(reason, "и есть рабочая база") {
		t.Errorf("в отказе нет причины: %s", reason)
	}
	if got := onionTotal(t, dbPath); got != 1 {
		t.Errorf("пул %d, хочу 1", got)
	}
}

// Журнал прежней базы команда убирает вместе с ней; наблюдаемо это в
// TestCmdRestoreLatestWithYes, который требует отсутствия -wal, -shm и -journal
// после восстановления. Отдельный тест на «чужой -wal пережил замену базы»
// написать не удалось: SQLite при открытии сбрасывает журнал с несовпавшим salt,
// а после закрытия соединения файлы журнала удаляются, то есть к моменту возврата
// из команды различие между удалённым и неудалённым мусором исчезает. Утверждать,
// что удаление чужого журнала исправляет дефект, без такого замера нельзя: это
// гигиена, а не починка.
func TestCmdRestoreTextMode(t *testing.T) {
	restoreFixture(t)

	stdout, stderr, code := runMainSplit(t, "restore", "--yes")
	if code != 0 {
		t.Fatalf("код возврата %d, хочу 0 (stderr %q)", code, firstN(stderr, 300))
	}
	for _, want := range []string{
		"восстановлено из voidsearchswag-20260927-120000.000.db",
		"прежняя база сохранена как ",
		"pre-restore-",
		"целостность: ok",
		"пул: 3 всего",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("в тексте нет %q: %s", want, stdout)
		}
	}
	if strings.Contains(stdout, "{") {
		t.Errorf("без --json появился JSON: %s", stdout)
	}
}

// Восстановление без рабочей базы - штатный сценарий каталога, который перенесли
// без базы: команда обязана работать и не обещать сохранить несуществующее.
func TestCmdRestoreWithoutCurrentDatabase(t *testing.T) {
	dir := restoreFixture(t)
	dbPath := filepath.Join(dir, "voidsearchswag.db")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(dbPath + suffix); err != nil && !os.IsNotExist(err) {
			t.Fatalf("удаление %s: %v", dbPath+suffix, err)
		}
	}

	stdout, _, code := runMainSplit(t, "restore", "--yes", "--json")
	if code != 0 {
		t.Fatalf("код возврата %d, хочу 0 (%.300s)", code, stdout)
	}
	body := decodeRestore(t, stdout)
	if _, ok := body["previous_saved_as"]; ok {
		t.Errorf("сохранять было нечего, а поле есть: %+v", body)
	}
	if got := onionTotal(t, dbPath); got != 3 {
		t.Errorf("пул %d, хочу 3", got)
	}
}

func TestPickSnapshot(t *testing.T) {
	snaps := []string{
		filepath.Join("backups", "voidsearchswag-20260926-090000.000.db"),
		filepath.Join("backups", "voidsearchswag-20260927-120000.000.db"),
	}

	for _, want := range []string{"", "  ", "latest", "LATEST"} {
		got, err := pickSnapshot(snaps, want)
		if err != nil {
			t.Fatalf("want=%q: %v", want, err)
		}
		if filepath.Base(got) != "voidsearchswag-20260927-120000.000.db" {
			t.Errorf("want=%q: выбран %s, хочу последний", want, filepath.Base(got))
		}
	}

	// Имя ищется без учёта регистра: Windows его не различает.
	got, err := pickSnapshot(snaps, "VOIDSEARCHSWAG-20260926-090000.000.DB")
	if err != nil {
		t.Fatalf("поиск по имени: %v", err)
	}
	if filepath.Base(got) != "voidsearchswag-20260926-090000.000.db" {
		t.Errorf("выбран %s", filepath.Base(got))
	}

	// Существующий путь принимается, даже если маска снимка не совпадает.
	file := filepath.Join(t.TempDir(), "копия-базы.db")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := pickSnapshot(snaps, file); err != nil || got != file {
		t.Errorf("путь: %s, %v", got, err)
	}
	if _, err := pickSnapshot(snaps, filepath.Join(t.TempDir(), "нет-такого.db")); err == nil {
		t.Error("несуществующий путь обязан давать ошибку")
	}
	if _, err := pickSnapshot(snaps, t.TempDir()); err == nil {
		t.Error("каталог вместо снимка обязан давать ошибку")
	}
	if _, err := pickSnapshot(snaps, "нет-такого.db"); err == nil {
		t.Error("неизвестное имя обязано давать ошибку")
	}
}

func TestSamePath(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "base.db")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !samePath(file, file) {
		t.Error("один и тот же путь не опознан")
	}
	upper := strings.ToUpper(file)
	if !samePath(file, upper) {
		t.Errorf("%s и %s - один файл, а samePath сказал нет", file, upper)
	}
	if samePath(file, filepath.Join(dir, "other.db")) {
		t.Error("разные файлы признаны одинаковыми")
	}
	if samePath(filepath.Join(dir, "нет-такого.db"), file) {
		t.Error("несуществующий путь совпал с существующим")
	}
}

func TestCopyFileContents(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.db")
	body := strings.Repeat("данные базы ", 500)
	if err := os.WriteFile(src, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "dst.db")
	if err := copyFileContents(src, dst); err != nil {
		t.Fatalf("копирование: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Errorf("копия отличается: %d байт вместо %d", len(got), len(body))
	}
	// Временный файл не должен оставаться рядом: его подхватит любой обход
	// каталога, а маска снимков его не ловит.
	if _, err := os.Stat(dst + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("временный файл остался: %v", err)
	}
	// Перезапись существующего файла разрешена: на место базы встаёт снимок.
	if err := copyFileContents(src, dst); err != nil {
		t.Errorf("перезапись: %v", err)
	}
	if err := copyFileContents(filepath.Join(dir, "нет-такого.db"), dst); err == nil {
		t.Error("копирование несуществующего файла обязано давать ошибку")
	}
}

func TestClipText(t *testing.T) {
	if got := clipText("коротко", 300); got != "коротко" {
		t.Errorf("короткий текст изменён: %q", got)
	}
	long := strings.Repeat("находка integrity_check ", 40)
	got := clipText(long, 100)
	if len([]rune(got)) != 103 || !strings.HasSuffix(got, "...") {
		t.Errorf("обрезка неверна: %d рун, %q", len([]rune(got)), got)
	}
	if !strings.HasPrefix(got, "находка integrity_check ") {
		t.Errorf("начало находки потеряно: %q", got)
	}
}

// Предупреждение о каталоге со снимками и без базы обязано называть команду:
// совет «скопируйте файл вручную» без restore оставлял оператора один на один с
// -wal и -shm.
func TestWarnMentionsRestoreCommand(t *testing.T) {
	dir := restoreFixture(t)
	dbPath := filepath.Join(dir, "voidsearchswag.db")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(dbPath + suffix); err != nil && !os.IsNotExist(err) {
			t.Fatalf("удаление %s: %v", dbPath+suffix, err)
		}
	}

	stdout := captureStdout(t, func() {
		stderrText := captureStderr(t, func() {
			warnSnapshotOnlyDataDir("stats")
		})
		for _, want := range []string{"restore --yes", "voidsearchswag-20260927-120000.000.db", "-wal"} {
			if !strings.Contains(stderrText, want) {
				t.Errorf("в предупреждении нет %q: %s", want, stderrText)
			}
		}
	})
	if strings.TrimSpace(stdout) != "" {
		t.Errorf("предупреждение попало в stdout: %q", stdout)
	}
}

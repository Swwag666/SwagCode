package store

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestBackupKeepsMainPoolResponsive проверяет главное утверждение правки: снимок
// базы не блокирует основной пул соединений.
//
// Прежняя версия выполняла VACUUM INTO на s.db, а пул открыт с
// SetMaxOpenConns(1). Снимок читает всю базу и пишет копию, то есть занимает
// единственное соединение на всё это время, и поисковый сервер не отвечает ни на
// один запрос, пока копия не завершится. Комментарий при этом обещал «без
// остановки сервера», что было верно для консистентности файла и неверно для
// доступности.
//
// Тест запускает снимок и параллельно серию запросов к основному пулу. Если
// снимок занимает общее соединение, запросы выстроятся в очередь и суммарное
// время превысит время снимка на порядок.
func TestBackupKeepsMainPoolResponsive(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	// Наполняем базу, чтобы снимок занял измеримое время: на пустой базе он
	// завершается мгновенно, и тест не отличал бы блокировку от её отсутствия.
	for i := 0; i < 300; i++ {
		if err := st.AddFile(ctx, FileEntry{
			TaskID:   "t",
			URL:      "http://a.onion/f" + string(rune('a'+i%26)) + string(rune('a'+i/26)) + ".epub",
			Filename: "book.epub",
			Ext:      "epub",
			Size:     int64(i * 1024),
			Verdict:  "ebook",
		}); err != nil {
			t.Fatal(err)
		}
	}

	dest := filepath.Join(t.TempDir(), "snapshot.db")

	backupDone := make(chan error, 1)
	go func() {
		backupDone <- st.Backup(ctx, dest)
	}()

	// Запросы к основному пулу идут, пока снимок выполняется.
	var wg sync.WaitGroup
	const queries = 20
	errs := make([]error, queries)
	start := time.Now()
	for i := 0; i < queries; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := st.SearchFiles(ctx, FileQuery{Verdict: "ebook", Limit: 5})
			errs[i] = err
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(start)

	if err := <-backupDone; err != nil {
		t.Fatalf("снимок: %v", err)
	}

	for i, err := range errs {
		if err != nil {
			t.Errorf("запрос %d во время снимка: %v", i, err)
		}
	}

	// Порог намеренно свободный: цель - поймать порядок величины, а не
	// измерить производительность. При блокировке единственного соединения
	// двадцать запросов ждали бы завершения снимка последовательно.
	if elapsed > 10*time.Second {
		t.Errorf("%d запросов во время снимка заняли %v: основной пул заблокирован", queries, elapsed)
	}
}

// TestBackupConcurrentWritesSucceed проверяет, что запись во время снимка не
// падает с SQLITE_BUSY.
//
// WAL допускает одного писателя и неограниченное число читателей, а
// busy_timeout(5000) даёт ждать разблокировки. Без этого любая запись здоровья
// движков во время ночного бэкапа падала бы, и сервер терял статистику.
func TestBackupConcurrentWritesSucceed(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	dest := filepath.Join(t.TempDir(), "snapshot.db")

	backupDone := make(chan error, 1)
	go func() {
		backupDone <- st.Backup(ctx, dest)
	}()

	// Даём снимку начаться, чтобы запись попала именно на время его выполнения.
	time.Sleep(20 * time.Millisecond)

	var wg sync.WaitGroup
	errs := make([]error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = st.RecordProbe(ctx,
				"http://2222222222222222222222222222222222222222222222222222.onion", true, int64(50+i))
		}(i)
	}
	wg.Wait()

	if err := <-backupDone; err != nil {
		t.Fatalf("снимок: %v", err)
	}
	for i, err := range errs {
		if err != nil {
			t.Errorf("запись %d во время снимка: %v", i, err)
		}
	}
}

func TestBackupDoesNotUseSharedPoolWhenPathKnown(t *testing.T) {
	// Штатный путь: Store создан через Open, путь сохранён, и снимок идёт на
	// выделенном соединении. Проверяется косвенно - если бы снимок использовал
	// общий пул, занятый другим долгим запросом, он встал бы в очередь.
	st := newStore(t)
	ctx := context.Background()

	if st.Path() == "" {
		t.Fatal("Store не сохранил путь: снимок уйдёт на общий пул")
	}

	dest := filepath.Join(t.TempDir(), "snapshot.db")
	if err := st.Backup(ctx, dest); err != nil {
		t.Fatal(err)
	}
	assertFileExists(t, dest)
}

func TestBackupFallsBackToSharedPoolWithoutPath(t *testing.T) {
	// Store, созданный не через Open (подмена в тестах), пути не имеет. Снимок
	// обязан всё равно состояться на общем пуле: лучше заблокировать сервер, чем
	// не сделать резервную копию.
	st := newStore(t)
	ctx := context.Background()

	original := st.path
	st.path = ""
	defer func() { st.path = original }()

	dest := filepath.Join(t.TempDir(), "fallback.db")
	if err := st.Backup(ctx, dest); err != nil {
		t.Fatalf("снимок на общем пуле: %v", err)
	}
	assertFileExists(t, dest)
}

func TestBackupSnapshotIsUsableDatabase(t *testing.T) {
	// Снимок обязан быть рабочей базой, а не просто файлом нужного размера:
	// резервная копия, которую нельзя открыть, бесполезна.
	st := newStore(t)
	ctx := context.Background()

	const url = "http://2222222222222222222222222222222222222222222222222222.onion"
	if err := st.UpsertOnion(ctx, Onion{URL: url, Status: "live", Title: "Сервис"}); err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(t.TempDir(), "snapshot.db")
	if err := st.Backup(ctx, dest); err != nil {
		t.Fatal(err)
	}

	// Открываем копию как обычную базу и читаем из неё.
	copyStore, err := Open(dest)
	if err != nil {
		t.Fatalf("копия не открылась: %v", err)
	}
	defer copyStore.Close()

	got, err := copyStore.GetOnion(ctx, url)
	if err != nil {
		t.Fatalf("чтение из копии: %v", err)
	}
	if got.Status != "live" || got.Title != "Сервис" {
		t.Errorf("в копии %+v, ожидала live и «Сервис»", got)
	}
}

func TestBackupSnapshotConsistentWithConcurrentWrite(t *testing.T) {
	// Снимок, сделанный во время записи, обязан быть консистентным: либо строка
	// в нём есть целиком, либо её нет вовсе. Частично записанная строка означала
	// бы, что копия повреждена.
	st := newStore(t)
	ctx := context.Background()

	dest := filepath.Join(t.TempDir(), "snapshot.db")

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			if err := st.RecordProbe(ctx,
				"http://2222222222222222222222222222222222222222222222222222.onion", true, int64(i)); err != nil {
				t.Errorf("запись %d: %v", i, err)
				return
			}
		}
	}()

	if err := st.Backup(ctx, dest); err != nil {
		t.Fatalf("снимок: %v", err)
	}
	wg.Wait()

	copyStore, err := Open(dest)
	if err != nil {
		t.Fatalf("копия не открылась: %v", err)
	}
	defer copyStore.Close()

	// Целостность копии проверяется штатным запросом: повреждённая база вернула
	// бы ошибку или несогласованные значения.
	got, err := copyStore.GetOnion(ctx, "http://2222222222222222222222222222222222222222222222222222.onion")
	if err != nil && err != ErrNotFound {
		t.Fatalf("чтение из копии: %v", err)
	}
	if err == nil {
		if got.SuccessRate < 0 || got.SuccessRate > 1 {
			t.Errorf("success_rate = %f вне [0,1]: копия несогласована", got.SuccessRate)
		}
		if got.LatencyAvg < 0 {
			t.Errorf("latency_avg = %d отрицательная: копия несогласована", got.LatencyAvg)
		}
	}
}

func TestSQLiteDSNEscapesPath(t *testing.T) {
	// Путь приходит из конфига и из VOIDSEARCH_DATA_DIR, то есть извне. Прежняя
	// версия собирала DSN форматной строкой, поэтому путь с «?», «#» или «%»
	// ломал разбор: «?» начинал список параметров, и часть пути молча становилась
	// прагмой.
	cases := []struct {
		name string
		path string
	}{
		{"обычный", "/tmp/base.db"},
		{"знак вопроса", "/tmp/what?.db"},
		{"решётка", "/tmp/hash#.db"},
		{"процент", "/tmp/pct%20.db"},
		{"пробел", "/tmp/my base.db"},
		{"амперсанд", "/tmp/a&b.db"},
		{"windows", `C:\Users\norw\.voidsearchswag\voidsearchswag.db`},
		{"кириллица", "/tmp/база.db"},
	}
	for _, c := range cases {
		dsn := sqliteDSN(c.path)
		// Прагмы обязаны присутствовать в DSN целиком: если путь «съел» часть
		// строки запроса, одна из прагм пропала бы.
		for _, want := range []string{"busy_timeout(5000)", "journal_mode(WAL)", "foreign_keys(1)"} {
			if !containsStr(dsn, want) {
				t.Errorf("%s: в DSN %q нет прагмы %q", c.name, dsn, want)
			}
		}
		if !containsStr(dsn, "file:") {
			t.Errorf("%s: DSN %q без схемы file:", c.name, dsn)
		}
	}
}

func TestSQLiteDSNOpensRealDatabase(t *testing.T) {
	// DSN обязан не только правильно выглядеть, но и открывать базу: проверка
	// строки без проверки подключения пропустила бы ошибку в схеме или в
	// экранировании, которая всплыла бы только в рантайме.
	for _, name := range []string{"what?.db", "hash#.db", "my base.db", "база.db"} {
		path := filepath.Join(t.TempDir(), name)
		st, err := Open(path)
		if err != nil {
			t.Errorf("Open(%q): %v", name, err)
			continue
		}
		if err := st.Migrate(context.Background()); err != nil {
			t.Errorf("Migrate(%q): %v", name, err)
		}
		if err := st.UpsertOnion(context.Background(), Onion{
			URL: "http://2222222222222222222222222222222222222222222222222222.onion", Status: "live",
		}); err != nil {
			t.Errorf("запись в %q: %v", name, err)
		}
		st.Close()
	}
}

// containsStr - локальная проверка подстроки, чтобы не тянуть strings ради двух
// вспомогательных тестов.
func containsStr(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// assertFileExists проверяет, что файл снимка создан и не пуст.
func assertFileExists(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("файл снимка %s: %v", path, err)
	}
	if info.Size() == 0 {
		t.Errorf("файл снимка %s пустой", path)
	}
}

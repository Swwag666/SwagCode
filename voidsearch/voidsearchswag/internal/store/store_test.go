package store

import (
	"context"
	"path/filepath"
	"testing"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestMigrateCreatesAllTables(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	want := []string{
		"sources", "onion_pool", "selectors", "hunts",
		"cache", "tasks", "file_catalog", "schema_migrations",
	}
	for _, name := range want {
		var got string
		err := st.db.QueryRowContext(ctx,
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&got)
		if err != nil {
			t.Errorf("таблица %s не создана: %v", name, err)
		}
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := st.Migrate(ctx); err != nil {
			t.Fatalf("прогон %d: %v", i+1, err)
		}
	}
	var n int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != len(migrations) {
		t.Errorf("записей миграций %d, надо %d", n, len(migrations))
	}
}

func TestMigrateRecordsVersionsOnce(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err := st.db.QueryContext(ctx, `SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	i := 0
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		i++
		if v != i {
			t.Errorf("версия %d на позиции %d: версии должны идти подряд", v, i)
		}
	}
	if i != len(migrations) {
		t.Errorf("получено %d версий, надо %d", i, len(migrations))
	}
}

func TestCachePurgeExpired(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	ins := `INSERT INTO cache(query_hash, mode, payload, expires_at) VALUES (?,?,?,?)`
	if _, err := st.db.ExecContext(ctx, ins, "h_live", "fast", "{}", "2999-01-01 00:00:00"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, ins, "h_dead1", "fast", "{}", "2000-01-01 00:00:00"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, ins, "h_dead2", "deep", "{}", "2001-06-15 12:00:00"); err != nil {
		t.Fatal(err)
	}
	n, err := st.CachePurgeExpired(ctx)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 2 {
		t.Errorf("вычищено %d, ожидала 2", n)
	}
	var left int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM cache`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 1 {
		t.Errorf("осталось %d записей, ожидала 1", left)
	}
}

func TestCachePurgeExpiredOnEmpty(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	n, err := st.CachePurgeExpired(ctx)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 0 {
		t.Errorf("вычищено %d на пустой таблице, ожидала 0", n)
	}
}

func TestOnionStats(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	total, live, err := st.OnionStats(ctx)
	if err != nil {
		t.Fatalf("пустая статистика: %v", err)
	}
	if total != 0 || live != 0 {
		t.Errorf("пустая база: total=%d live=%d, ожидала 0/0", total, live)
	}
	ins := `INSERT INTO onion_pool(url, status) VALUES (?,?)`
	for _, row := range []struct{ url, status string }{
		{"http://a.onion", "live"},
		{"http://b.onion", "live"},
		{"http://c.onion", "dead"},
		{"http://d.onion", "unknown"},
	} {
		if _, err := st.db.ExecContext(ctx, ins, row.url, row.status); err != nil {
			t.Fatal(err)
		}
	}
	total, live, err = st.OnionStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if total != 4 {
		t.Errorf("total=%d, ожидала 4", total)
	}
	if live != 2 {
		t.Errorf("live=%d, ожидала 2", live)
	}
}

func TestTaskStats(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	total, running, err := st.TaskStats(ctx)
	if err != nil {
		t.Fatalf("пустая статистика: %v", err)
	}
	if total != 0 || running != 0 {
		t.Errorf("пустая база: total=%d running=%d, ожидала 0/0", total, running)
	}
	ins := `INSERT INTO tasks(id, kind, status) VALUES (?,?,?)`
	for _, row := range []struct{ id, kind, status string }{
		{"t1", "search", "running"},
		{"t2", "crawl", "running"},
		{"t3", "discover", "done"},
		{"t4", "parse", "failed"},
	} {
		if _, err := st.db.ExecContext(ctx, ins, row.id, row.kind, row.status); err != nil {
			t.Fatal(err)
		}
	}
	total, running, err = st.TaskStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if total != 4 {
		t.Errorf("total=%d, ожидала 4", total)
	}
	if running != 2 {
		t.Errorf("running=%d, ожидала 2", running)
	}
}

func TestUniqueConstraints(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx,
		`INSERT INTO sources(url) VALUES ('http://x.onion')`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx,
		`INSERT INTO sources(url) VALUES ('http://x.onion')`); err == nil {
		t.Error("дубликат url в sources прошёл, хотя UNIQUE")
	}
	sel := `INSERT INTO selectors(url_pattern, field, selector) VALUES (?,?,?)`
	if _, err := st.db.ExecContext(ctx, sel, "*://shop/*", "price", ".price"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, sel, "*://shop/*", "price", ".cost"); err == nil {
		t.Error("дубликат (url_pattern, field) в selectors прошёл, хотя UNIQUE")
	}
	if _, err := st.db.ExecContext(ctx, sel, "*://shop/*", "title", "h1"); err != nil {
		t.Errorf("другой field должен проходить: %v", err)
	}
}

func TestOpenCreatesMissingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "deeper")
	st, err := Open(filepath.Join(dir, "x.db"))
	if err != nil {
		t.Fatalf("open во вложенном каталоге: %v", err)
	}
	defer st.Close()
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
}

func TestMigrateConcurrentOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.db")
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		st, err := Open(path)
		if err != nil {
			t.Fatalf("открытие %d: %v", i, err)
		}
		if err := st.Migrate(ctx); err != nil {
			st.Close()
			t.Fatalf("миграция %d: %v", i, err)
		}
		st.Close()
	}
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var n int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != len(migrations) {
		t.Errorf("после повторных открытий записей %d, надо %d", n, len(migrations))
	}
}

package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
	// path хранится, потому что Backup открывает отдельный коннект к тому же
	// файлу: VACUUM INTO на общем пуле с SetMaxOpenConns(1) занимал единственное
	// соединение на всё время снимка, и поисковый сервер не отвечал ни на один
	// запрос, пока копия не завершится.
	path string
	// lastRun - итог последнего Migrate для этого Store. Нужен диагностике:
	// команда stats показывает версию схемы и то, что было восстановлено, а
	// вернуть это из Migrate нельзя, не сломав сигнатуру, которой пользуются все
	// команды и тесты.
	lastRun MigrationRun
}

// MigrationRun - чем закончился последний вызов Migrate.
//
// Known и Applied отвечают на вопрос оператора «та ли это схема»; Executed и
// Repaired показывают, что команда сделала прямо сейчас; Foreign перечисляет
// версии журнала, которых бинарь не знает, то есть следы бинарника новее.
type MigrationRun struct {
	Known    int
	Applied  int
	Executed []int
	Repaired []int
	Foreign  []int
}

// Migrations возвращает итог последнего Migrate для этого Store.
func (s *Store) Migrations() MigrationRun { return s.lastRun }

// MigrationCount - сколько миграций знает этот бинарь. Номер версии равен
// позиции оператора в списке, поэтому число и есть последняя известная версия.
func MigrationCount() int { return len(migrations) }

// sqliteDSN собирает строку подключения к файлу базы.
//
// Путь экранируется через url.URL, а не подставляется в шаблон напрямую.
// Прежняя версия собирала DSN форматной строкой, поэтому путь с символами «?»,
// «#» или «%» ломал разбор: «?» начинал список параметров, и часть пути
// молча становилась прагмой. Путь приходит из конфига и из флага
// VOIDSEARCH_DATA_DIR, то есть извне, и на Windows каталог пользователя может
// содержать любые символы.
//
// Прагмы:
//   - busy_timeout(5000) - ждать разблокировки до пяти секунд вместо немедленного
//     SQLITE_BUSY, иначе любой конкурентный запрос падал бы при активной записи;
//   - journal_mode(WAL) - читатели не блокируют писателя, что и позволяет Backup
//     работать на отдельном соединении параллельно с сервером;
//   - foreign_keys(1) - в SQLite выключены по умолчанию для каждого соединения.
func sqliteDSN(path string) string {
	u := url.URL{
		Scheme:   "file",
		Opaque:   path,
		RawQuery: "_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)",
	}
	return u.String()
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("store dir: %w", err)
	}
	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		return nil, fmt.Errorf("store open: %w", err)
	}
	// Одно соединение намеренно: SQLite в WAL допускает одного писателя, и пул
	// из нескольких коннектов дал бы очередь на SQLITE_BUSY вместо предсказуемой
	// сериализации. Обратная сторона - любой долгий запрос блокирует всё,
	// поэтому долгие операции (чистка, снимок) обязаны либо идти порциями, либо
	// открывать собственный коннект.
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("store ping: %w", err)
	}
	return &Store{db: db, path: path}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Path возвращает путь к файлу базы.
//
// Нужен тестам и диагностике: по пути можно открыть базу сторонним инструментом
// и проверить план запроса или содержимое журнала миграций.
func (s *Store) Path() string { return s.path }

// Migrate приводит схему базы к той, которую знает бинарь.
//
// Журнал читается целиком, а не через MAX(version), и это не косметика. Прежняя
// версия сравнивала номер оператора с максимумом журнала и пропускала всё, что не
// выше. Два следствия измерены:
//
//   - база с версией 999 в журнале принималась молча: Migrate возвращал nil при
//     том, что бинарь знает 27 миграций. Это состояние базы от бинарника новее -
//     откат бинаря или чужой снимок. Код продолжал работать со схемой, которой не
//     знает, и первая же ошибка вылезала в произвольном запросе без указания
//     причины;
//   - дырка в журнале оставляла базу без объекта навсегда: MAX рос, пропущенный
//     оператор не выполнялся, и снаружи всё выглядело благополучно. В
//     migrations.go описан ровно такой инцидент с составным индексом tasks.
//
// Теперь версии выше известных дают отказ с номерами, а пропущенные операторы
// выполняются. Пропуск в середине безопасен для выполнения: журнал append-only,
// поэтому потерянный оператор - тот, который база не применяла. Если его текст
// всё же несовместим с уже применёнными (например, колонка добавлена вручную),
// ошибка возвращается с номером версии, а не проглатывается.
func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		applied_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`); err != nil {
		return fmt.Errorf("schema_migrations: %w", err)
	}
	have, maxVersion, err := s.appliedVersions(ctx)
	if err != nil {
		return err
	}

	run := MigrationRun{Known: len(migrations)}
	for v := range have {
		if v > len(migrations) {
			run.Foreign = append(run.Foreign, v)
		}
	}
	sort.Ints(run.Foreign)
	if len(run.Foreign) > 0 {
		s.lastRun = run
		return fmt.Errorf("база мигрирована до версии %d, а этот бинарь знает только %d: нужен бинарник не старше базы или снимок, снятый той версией",
			maxVersion, len(migrations))
	}
	for i, m := range migrations {
		v := i + 1
		if have[v] {
			continue
		}
		if err := s.applyMigration(ctx, v, m); err != nil {
			s.lastRun = run
			return err
		}
		run.Executed = append(run.Executed, v)
		if v < maxVersion {
			// Версия ниже уже применённой максимума означает дырку в журнале, а не
			// обычный догоняющий прогон.
			run.Repaired = append(run.Repaired, v)
		}
	}
	run.Applied = len(have) + len(run.Executed)
	s.lastRun = run
	return nil
}

// appliedVersions читает журнал миграций целиком: набор применённых версий и
// максимальную из них.
func (s *Store) appliedVersions(ctx context.Context) (map[int]bool, int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		return nil, 0, fmt.Errorf("migrations version: %w", err)
	}
	defer rows.Close()
	have := make(map[int]bool)
	var maxVersion int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, 0, fmt.Errorf("migrations version: %w", err)
		}
		have[v] = true
		if v > maxVersion {
			maxVersion = v
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("migrations version: %w", err)
	}
	return have, maxVersion, nil
}

// applyMigration выполняет один оператор журнала в отдельной транзакции: оператор
// и запись о применении обязаны появляться вместе, иначе повторный прогон либо
// пропустит миграцию, либо выполнит её дважды.
func (s *Store) applyMigration(ctx context.Context, v int, m string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, m); err != nil {
		tx.Rollback()
		return fmt.Errorf("миграция %d: %w", v, err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version) VALUES (?)`, v); err != nil {
		tx.Rollback()
		return fmt.Errorf("миграция %d commit: %w", v, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("миграция %d: %w", v, err)
	}
	return nil
}

func (s *Store) OnionStats(ctx context.Context) (total, live int, err error) {
	if err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM onion_pool`).Scan(&total); err != nil {
		return 0, 0, err
	}
	if err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM onion_pool WHERE status='live'`).Scan(&live); err != nil {
		return 0, 0, err
	}
	return total, live, nil
}

// OnionStatusBreakdown считает записи пула по каждому статусу: видно,
// сколько адресов живо, сколько мертво и сколько ещё не проверялось.
func (s *Store) OnionStatusBreakdown(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT status, COUNT(*) FROM onion_pool GROUP BY status ORDER BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]int{}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return nil, err
		}
		out[status] = n
	}
	return out, rows.Err()
}

func (s *Store) TaskStats(ctx context.Context) (total, running int, err error) {
	if err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks`).Scan(&total); err != nil {
		return 0, 0, err
	}
	if err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks WHERE status='running'`).Scan(&running); err != nil {
		return 0, 0, err
	}
	return total, running, nil
}

func (s *Store) CachePurgeExpired(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM cache WHERE expires_at < CURRENT_TIMESTAMP`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

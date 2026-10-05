package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// BackupRotate делает снимок базы в dir и вычищает старые сверх keep.
// Имя файла - voidsearchswag-YYYYMMDD-HHMMSS.mmm.db: сортировка по имени
// совпадает с хронологией, отдельный индекс не нужен.
//
// Доли секунды в имени обязательны. SQLite VACUUM INTO завершается ошибкой,
// если файл назначения уже существует, а проверка существования здесь не
// выполнялась. Два снимка в пределах одной секунды - backup_create, вызванный
// дважды подряд, или ручной запуск backup одновременно с фоновым тиком, -
// падали с невнятной ошибкой SQLite, и PruneBackups после этого не
// выполнялся, поэтому ротация молча останавливалась: второй снимок просто не
// появлялся. Доли секунды делают коллизию практически невозможной, а явная
// проверка даёт внятное сообщение, если она всё же случилась.
//
// PruneBackups сортирует имена лексикографически, и добавленная дробная часть
// этот порядок не ломает: префикс и суффикс маски остаются прежними.
func (s *Store) BackupRotate(ctx context.Context, dir string, keep int) (string, error) {
	if strings.TrimSpace(dir) == "" {
		return "", fmt.Errorf("backup: пустой каталог")
	}
	if keep <= 0 {
		keep = 7
	}
	name := "voidsearchswag-" + time.Now().Format("20060102-150405.000") + ".db"
	dest := filepath.Join(dir, name)
	if _, err := os.Stat(dest); err == nil {
		return "", fmt.Errorf("backup: %s уже существует", dest)
	}
	if err := s.Backup(ctx, dest); err != nil {
		return "", err
	}
	pruned, err := PruneBackups(dir, keep)
	if err != nil {
		return dest, fmt.Errorf("снимок %s создан, ротация не удалась: %w", dest, err)
	}
	// Число удалённых снимков здесь не возвращается: сигнатура занята путём к
	// снимку, а добавлять ради счётчика поле в Store значит заводить общее
	// состояние там, где его быть не должно. Вызывающий, которому это важно,
	// зовёт PruneBackups сам - она экспортирована.
	_ = pruned
	return dest, nil
}

// snapshotNames возвращает имена снимков базы в каталоге, отсортированные по
// имени: префикс, дата и доля секунды делают такой порядок хронологическим.
func snapshotNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var snaps []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if strings.HasPrefix(n, "voidsearchswag-") && strings.HasSuffix(n, ".db") {
			snaps = append(snaps, n)
		}
	}
	sort.Strings(snaps)
	return snaps, nil
}

// PruneBackups удаляет старые снимки сверх keep, новые остаются.
// Чужие файлы в каталоге не трогает: только своя маска имени.
func PruneBackups(dir string, keep int) (int, error) {
	if keep <= 0 {
		keep = 7
	}
	snaps, err := snapshotNames(dir)
	if err != nil {
		return 0, fmt.Errorf("prune: %w", err)
	}
	pruned := 0
	for len(snaps) > keep {
		oldest := snaps[0]
		snaps = snaps[1:]
		if err := os.Remove(filepath.Join(dir, oldest)); err != nil {
			return pruned, fmt.Errorf("prune %s: %w", oldest, err)
		}
		pruned++
	}
	return pruned, nil
}

// PlannedPrune считает, сколько снимков удалит ротация, если сейчас создать один
// новый и оставить keep.
//
// Считать приходится до вызова, а не по факту: BackupRotate создаёт снимок и
// чистит хвост одним действием, и после него спрашивать о подтверждении нечего -
// файлов уже нет. Измерено на каталоге с четырьмя снимками: backup --keep 1
// возвращал rc=0 и ответ {"keep":1,"snapshot":...} без единого слова о том, что
// три прежних снимка удалены. Тот же вызов доступен LLM-агенту через
// MCP-инструмент backup_create, то есть один запрос стирал всю историю резервных
// копий.
//
// Отсутствующий каталог - не ошибка: при первом запуске его ещё нет, а ротации
// там делать нечего.
func PlannedPrune(dir string, keep int) (int, error) {
	if keep <= 0 {
		keep = 7
	}
	// Путь, который существует, но каталогом не является, os.ReadDir в Windows
	// отдаёт как «файл не найден», и молча вернуть ноль значило бы пропустить
	// проверку там, где снимки всё-таки могли лежать. Причина называется сразу.
	if fi, err := os.Stat(dir); err == nil && !fi.IsDir() {
		return 0, fmt.Errorf("prune: %s не каталог", dir)
	}
	snaps, err := snapshotNames(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("prune: %w", err)
	}
	if planned := len(snaps) + 1 - keep; planned > 0 {
		return planned, nil
	}
	return 0, nil
}

// CheckIntegrity открывает файл базы и возвращает результат PRAGMA
// integrity_check: "ok" для целой базы, список ошибок для побитой.
//
// Соединение открывается только на чтение и без WAL-прагм. Оба решения
// принципиальны: проверка снимка не должна создавать рядом файлы -wal и -shm
// (они остались бы лежать в каталоге бэкапов и путали бы как ротацию, так и
// ручное восстановление), и не имеет права ничего менять в файле, который на
// момент проверки может быть единственной копией данных.
//
// Ошибка возвращается, когда файл не читается как база вовсе - например, это
// обрывок загрузки или вообще не SQLite; побитая, но опознаваемая база
// возвращается как результат, отличный от "ok".
func CheckIntegrity(ctx context.Context, path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("integrity: пустой путь")
	}
	u := url.URL{
		Scheme:   "file",
		Opaque:   path,
		RawQuery: "mode=ro&_pragma=busy_timeout(5000)",
	}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return "", fmt.Errorf("integrity: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var res string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&res); err != nil {
		return "", fmt.Errorf("integrity %s: %w", filepath.Base(path), err)
	}
	return strings.TrimSpace(res), nil
}

package store

import (
	"context"
	"fmt"
	"strings"
)

// OnionSync - адрес, который discover хочет провести в пул: мета, снятая
// прогоном, без статистики проб. Статистику (latency, success_rate,
// fail_streak) батч не трогает: у новых адресов её нет, а у известных она
// принадлежит пробам, и рекорд discover'а не имеет права её обнулять.
type OnionSync struct {
	URL         string
	Title       string
	Description string
	Category    string
}

// SyncOnions - батч-запись пула: снимок существующих адресов одним
// проходом, новые записи и мета - в одной транзакции.
//
// Этап 174. Прежде discover писал поадресным циклом GetOnion +
// UpsertOnion/SetOnionMeta: каждый вызов - собственный автокоммит, то есть
// свой fsync журнала, и пул на 14 тысяч адресов превращался в десятки
// секунд хвоста ПОСЛЕ обрезки бюджета (живой замер этапа 173: хвост
// 22.3с/26.6с при бюджетах 30с/90с - запись была вторым по тяжести
// после замера куском). Батч собирает те же решения, но коммит один.
//
// Счётчики совпадают с поадресным циклом дословно: new - адресов, которых
// в пуле не было; updated - существующих, у которых титул или категория
// отличаются от собранных (вызов мета-обновления, а не фактическая
// перепись: пустые поля CASE WHEN не трогает, и это то же поведение, что у
// поадресного пути). При ошибке транзакция откатывается целиком и счётчики
// нулевые: вызывающий обязан пасть на фолбэк-цикле, который посчитает
// отказы по-адресно (контракт этапа 168: save_failed с last_error, а не
// молчание).
//
// Отбор существующих идёт чанками по 400 в WHERE url IN (...): предел
// переменных SQLite - 999, и один список на 14 тысяч адресов упирался бы в
// него. Снимок и запись живут в разных запросах, но транзакция держит их
// согласованными: между SELECT и INSERT конкурентный писатель не пролезет
// - соединение у Store одно.
func (s *Store) SyncOnions(ctx context.Context, items []OnionSync) (newCount, updatedCount int, err error) {
	if len(items) == 0 {
		return 0, 0, nil
	}

	known := make(map[string]struct{ title, category string }, len(items))
	const batch = 400
	for start := 0; start < len(items); start += batch {
		if err := ctx.Err(); err != nil {
			return 0, 0, fmt.Errorf("sync onions: контекст: %w", err)
		}
		end := start + batch
		if end > len(items) {
			end = len(items)
		}
		ph := make([]string, 0, end-start)
		args := make([]any, 0, end-start)
		for _, it := range items[start:end] {
			if it.URL == "" {
				continue
			}
			ph = append(ph, "?")
			args = append(args, it.URL)
		}
		if len(ph) == 0 {
			continue
		}
		rows, err := s.db.QueryContext(ctx,
			`SELECT url, title, category FROM onion_pool WHERE url IN (`+strings.Join(ph, ",")+`)`, args...)
		if err != nil {
			return 0, 0, fmt.Errorf("sync onions: выборка: %w", err)
		}
		for rows.Next() {
			var url, title, category string
			if err := rows.Scan(&url, &title, &category); err != nil {
				rows.Close()
				return 0, 0, fmt.Errorf("sync onions: чтение строки: %w", err)
			}
			known[url] = struct{ title, category string }{title: title, category: category}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return 0, 0, fmt.Errorf("sync onions: обход строк: %w", err)
		}
		rows.Close()
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("sync onions: транзакция: %w", err)
	}
	defer tx.Rollback()

	insert, err := tx.PrepareContext(ctx, `
		INSERT INTO onion_pool(url, status, category, title, description, latency_avg, success_rate, fail_streak, last_probe)
		VALUES (?, 'unknown', ?, ?, ?, 0, 0, 0, CURRENT_TIMESTAMP)
		ON CONFLICT(url) DO UPDATE SET
			category=CASE WHEN excluded.category<>'' THEN excluded.category ELSE onion_pool.category END,
			title=CASE WHEN excluded.title<>'' THEN excluded.title ELSE onion_pool.title END,
			description=CASE WHEN excluded.description<>'' THEN excluded.description ELSE onion_pool.description END`)
	if err != nil {
		return 0, 0, fmt.Errorf("sync onions: подготовка вставки: %w", err)
	}
	defer insert.Close()

	meta, err := tx.PrepareContext(ctx, `
		UPDATE onion_pool SET
			title=CASE WHEN ?<>'' THEN ? ELSE title END,
			description=CASE WHEN ?<>'' THEN ? ELSE description END,
			category=CASE WHEN ?<>'' THEN ? ELSE category END
		WHERE url=?`)
	if err != nil {
		return 0, 0, fmt.Errorf("sync onions: подготовка меты: %w", err)
	}
	defer meta.Close()

	for _, it := range items {
		if it.URL == "" {
			continue
		}
		k, exists := known[it.URL]
		if !exists {
			if _, err := insert.ExecContext(ctx, it.URL, it.Category, it.Title, it.Description); err != nil {
				return 0, 0, fmt.Errorf("sync onions: вставка %s: %w", it.URL, err)
			}
			newCount++
			continue
		}
		if it.Title != k.title || it.Category != k.category {
			if _, err := meta.ExecContext(ctx,
				it.Title, it.Title, it.Description, it.Description,
				it.Category, it.Category, it.URL); err != nil {
				return 0, 0, fmt.Errorf("sync onions: мета %s: %w", it.URL, err)
			}
			updatedCount++
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("sync onions: коммит: %w", err)
	}
	return newCount, updatedCount, nil
}

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// EngineSeed - поисковый движок сверх зашитых сидов: найден автопромоутом
// среди живых сервисов пула.
type EngineSeed struct {
	Name      string
	Base      string
	Path      string
	Selector  string
	Category  string
	Auto      bool
	LastOK    time.Time
	CreatedAt time.Time
}

func (s *Store) UpsertEngineSeed(ctx context.Context, e EngineSeed) error {
	if strings.TrimSpace(e.Name) == "" || strings.TrimSpace(e.Base) == "" {
		return errors.New("engine seed: пустые имя или база")
	}
	if e.Path == "" {
		e.Path = "/search?q={q}"
	}
	if e.Selector == "" {
		e.Selector = "a[href*='.onion']"
	}
	if e.Category == "" {
		e.Category = "general"
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO engines(name, base, path, selector, category, auto, last_ok)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			base=excluded.base, path=excluded.path, selector=excluded.selector,
			category=excluded.category, auto=excluded.auto,
			last_ok=COALESCE(excluded.last_ok, engines.last_ok)`,
		e.Name, e.Base, e.Path, e.Selector, e.Category, boolInt(e.Auto), tsOrEmpty(e.LastOK))
	if err != nil {
		return fmt.Errorf("upsert engine seed: %w", err)
	}
	return nil
}

func (s *Store) ListEngineSeeds(ctx context.Context) ([]EngineSeed, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT name, base, path, selector, category, auto, last_ok, created_at
		FROM engines ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list engine seeds: %w", err)
	}
	defer rows.Close()
	var out []EngineSeed
	for rows.Next() {
		var e EngineSeed
		var auto int
		var lastOK, created sql.NullString
		if err := rows.Scan(&e.Name, &e.Base, &e.Path, &e.Selector, &e.Category, &auto, &lastOK, &created); err != nil {
			return nil, err
		}
		e.Auto = auto != 0
		e.LastOK = parseTS(lastOK)
		e.CreatedAt = parseTS(created)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) TouchEngineSeed(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE engines SET last_ok=CURRENT_TIMESTAMP WHERE name=?`, name)
	if err != nil {
		return fmt.Errorf("touch engine seed: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

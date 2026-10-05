package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"voidsearchswag/internal/config"
	"voidsearchswag/internal/discover"
	"voidsearchswag/internal/search"
	"voidsearchswag/internal/store"
)

// collectLogger копит стартовые строки фона: тест проверяет обещание
// расписание целиком, а не только его вторую половину.
type collectLogger struct {
	mu    sync.Mutex
	lines []string
}

func (l *collectLogger) Infof(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *collectLogger) Warnf(format string, args ...any) {
	l.Infof(format, args...)
}

func (l *collectLogger) has(substr string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, s := range l.lines {
		if strings.Contains(s, substr) {
			return true
		}
	}
	return false
}

// Этап 178: строка старта фона обещала только «каждые N», а первый тик
// приходит раньше (охота - 1м, разведка - 5м, бэкап - 10м, судья - 12ч,
// пиры - 5м). Два слепых смоука этапа 178 читали «фон: разведка каждые
// 24h» и ловили сетевую активность через ~5 минут - «сервер ходит в
// сеть вопреки логу». Контракт: строка называет оба интервала.
func TestStartBackgroundNamesFirstTick(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/bgphrases.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	hunter := openHuntRunner(t, nil)
	pool := &discover.Pool{Store: st}
	engine := &search.Engine{}

	cfg := config.Config{
		HuntBG:             true,
		HuntInterval:       10 * time.Minute,
		DiscoverBG:         true,
		DiscoverBGInterval: 24 * time.Hour,
		BackupBG:           true,
		BackupInterval:     24 * time.Hour,
		BackupKeep:         7,
		Peers:              []string{"http://127.0.0.1:1"},
	}

	log := &collectLogger{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startBackground(ctx, log, cfg, st, engine, hunter, pool, nil, nil, nil)

	// Все первые тики не раньше минуты - отменяем сразу после старта:
	// фразы обязаны выйти до первого тика, ни один цикл не ходит в сеть.
	time.Sleep(50 * time.Millisecond)
	cancel()
	time.Sleep(20 * time.Millisecond)

	wants := []string{
		"фон: охота: первый тик через 1m0s, далее каждые 10m0s",
		"фон: разведка: первый тик через 5m0s, далее каждые 24h0m0s",
		"фон: бэкап: первый тик через 10m0s, далее каждые 24h0m0s (держим 7)",
		"фон: судья: пересчёт весов: первый тик через 12h0m0s, далее каждые 24h0m0s",
		"фон: пиры: синк: первый тик через 5m0s, далее каждые 1h0m0s (1)",
	}
	for _, w := range wants {
		if !log.has(w) {
			t.Errorf("стартовая строка не называет первый тик: %q; собрано: %v", w, log.lines)
		}
	}
}

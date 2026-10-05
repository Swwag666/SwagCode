package store

import (
	"context"
	"testing"
)

func TestOpenEnablesWALAndBusyTimeout(t *testing.T) {
	st, err := Open(t.TempDir() + "/wal.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	var mode string
	if err := st.db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode=%q, ожидала wal", mode)
	}
	var timeout int
	if err := st.db.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&timeout); err != nil {
		t.Fatal(err)
	}
	if timeout < 5000 {
		t.Errorf("busy_timeout=%d, ожидала >=5000", timeout)
	}
	var maxConns int
	_ = maxConns
	if got := st.db.Stats().MaxOpenConnections; got != 1 {
		t.Errorf("MaxOpenConns=%d, ожидала 1 (писатель один, иначе locked)", got)
	}
}

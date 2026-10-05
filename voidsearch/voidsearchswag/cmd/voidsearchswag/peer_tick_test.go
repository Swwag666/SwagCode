package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"voidsearchswag/internal/store"
)

func TestPeerSyncTickMerges(t *testing.T) {
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"onions":[{"url":"http://tickpeer.onion","status":"live"}],
"hunts":[{"query":"tick hunt","mode":"fast","schedule_min":30}]}`))
	}))
	defer peer.Close()

	st, err := store.Open(t.TempDir() + "/peertick.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Пир без токена: экспорт закрыт без серверного токена? Стаб токена не
	// требует - синк идёт, токен шлётся пустым.
	peerSyncTick(context.Background(), stderrLogger{}, st, []string{peer.URL}, "")
	o, err := st.GetOnion(context.Background(), "http://tickpeer.onion")
	if err != nil {
		t.Fatalf("адрес не смержен: %v", err)
	}
	if o.Status != "live" {
		t.Errorf("статус %q", o.Status)
	}
	hunts, err := st.ListHunts(context.Background())
	if err != nil || len(hunts) != 1 || hunts[0].Query != "tick hunt" {
		t.Errorf("охоты %+v %v", hunts, err)
	}
	// Мёртвый пир не роняет тик.
	peerSyncTick(context.Background(), stderrLogger{}, st, []string{"http://127.0.0.1:1"}, "")
	peerSyncTick(context.Background(), stderrLogger{}, nil, []string{peer.URL}, "")
}

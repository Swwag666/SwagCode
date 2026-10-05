package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"voidsearchswag/internal/store"
)

func TestPeerExportGated(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/peer.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertOnion(ctx, store.Onion{URL: "http://a.onion", Status: "live"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateHunt(ctx, store.Hunt{Query: "leak", Mode: "deep"}); err != nil {
		t.Fatal(err)
	}

	srv := New(Deps{Version: "test", Store: st, Started: time.Now()})
	addr := freeAddr(t)
	sctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- ServeWithOptions(sctx, srv, HTTPOptions{Addr: addr, Token: "s3cret", Store: st})
	}()
	base := "http://" + addr
	var ok bool
	for i := 0; i < 50; i++ {
		time.Sleep(100 * time.Millisecond)
		if resp, err := http.Get(base + "/health"); err == nil {
			resp.Body.Close()
			ok = true
			break
		}
	}
	if !ok {
		t.Fatal("сервер не поднялся")
	}
	// Без токена - 401, чужой пул наружу не отдаём.
	if resp, err := http.Get(base + "/peer/export"); err != nil {
		t.Fatal(err)
	} else {
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("export без токена: %d, ожидала 401", resp.StatusCode)
		}
	}
	req, _ := http.NewRequest("GET", base+"/peer/export?limit=10", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var doc peerExportDoc
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Onions) != 1 || doc.Onions[0].URL != "http://a.onion" {
		t.Errorf("пул неверен: %+v", doc.Onions)
	}
	if len(doc.Hunts) != 1 || doc.Hunts[0].Query != "leak" {
		t.Errorf("охоты неверны: %+v", doc.Hunts)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(7 * time.Second):
		t.Error("Serve не остановился")
	}
}

func TestPeerExportForbidsWithoutServerToken(t *testing.T) {
	srv := New(Deps{Version: "test", Started: time.Now()})
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, srv, addr) }()
	base := "http://" + addr
	for i := 0; i < 50; i++ {
		time.Sleep(100 * time.Millisecond)
		if resp, err := http.Get(base + "/health"); err == nil {
			resp.Body.Close()
			break
		}
	}
	resp, err := http.Get(base + "/peer/export")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("export без серверного токена: %d, ожидала 403", resp.StatusCode)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(7 * time.Second):
		t.Error("Serve не остановился")
	}
}

func TestPeerListAndSync(t *testing.T) {
	// Пир - стаб с /health и /peer/export.
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("/peer/export", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer s3cret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"onions":[{"url":"http://peer1.onion","status":"live"}],
"hunts":[{"query":"peer hunt","mode":"deep","schedule_min":60}]}`))
	})
	peerSrv := httptest.NewServer(mux)
	defer peerSrv.Close()
	peer := peerSrv.URL

	st, err := store.Open(t.TempDir() + "/sync.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	srv := New(Deps{Version: "test", Store: st, Peers: []string{peer}, PeerToken: "s3cret", Started: time.Now()})
	c := inProcess(t, srv)
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	res, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "peer_list", Arguments: map[string]any{}},
	})
	if err != nil || res.IsError {
		t.Fatalf("peer_list: %v %+v", err, res)
	}
	var list map[string]any
	if err := json.Unmarshal([]byte(textOf(t, res)), &list); err != nil {
		t.Fatal(err)
	}
	if list["count"] != float64(1) {
		t.Errorf("пиров %+v", list)
	}

	res, err = c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "peer_sync", Arguments: map[string]any{"limit": 100}},
	})
	if err != nil || res.IsError {
		t.Fatalf("peer_sync: %v %+v", err, res)
	}
	var sync map[string]any
	if err := json.Unmarshal([]byte(textOf(t, res)), &sync); err != nil {
		t.Fatal(err)
	}
	if sync["merged_onions"] != float64(1) || sync["merged_hunts"] != float64(1) {
		t.Errorf("слияние неверно: %+v", sync)
	}
	// Повторный синк - дедуп: нули, а не дубликаты.
	res, err = c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "peer_sync", Arguments: map[string]any{"limit": 100}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var sync2 map[string]any
	if err := json.Unmarshal([]byte(textOf(t, res)), &sync2); err != nil {
		t.Fatal(err)
	}
	// Onion upsert идемпотентен (счётчик растёт), охоты дедупятся.
	if sync2["merged_hunts"] != float64(0) {
		t.Errorf("охоты задублились: %+v", sync2)
	}
}

func TestPeerSyncNeedsPeers(t *testing.T) {
	srv := New(Deps{Version: "test", Started: time.Now()})
	c := inProcess(t, srv)
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, name := range []string{"peer_list", "peer_sync"} {
		res, err := c.CallTool(ctx, mcp.CallToolRequest{
			Params: mcp.CallToolParams{Name: name, Arguments: map[string]any{}},
		})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if name == "peer_sync" && !res.IsError {
			t.Error("peer_sync без пиров обязан вернуть ошибку")
		}
	}
}

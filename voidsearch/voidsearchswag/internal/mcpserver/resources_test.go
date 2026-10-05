package mcpserver

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"voidsearchswag/internal/store"
)

func TestResourcesListed(t *testing.T) {
	_, c := newTestServer(t)
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := c.ListResources(ctx, mcp.ListResourcesRequest{})
	if err != nil {
		t.Fatalf("resources/list: %v", err)
	}
	want := map[string]bool{
		"voidsearch://pool/status":   false,
		"voidsearch://hunts/list":    false,
		"voidsearch://stats/summary": false,
	}
	for _, r := range res.Resources {
		if _, ok := want[r.URI]; ok {
			want[r.URI] = true
		}
		if r.MIMEType != "application/json" {
			t.Errorf("ресурс %q без JSON-типа: %q", r.URI, r.MIMEType)
		}
	}
	for uri, found := range want {
		if !found {
			t.Errorf("ресурс %q не зарегистрирован", uri)
		}
	}
}

func TestPoolResourceReflectsDB(t *testing.T) {
	st, c := newTestServer(t)
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := st.UpsertOnion(ctx, store.Onion{URL: "http://a.onion", Status: "live"}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertOnion(ctx, store.Onion{URL: "http://b.onion", Status: "dead"}); err != nil {
		t.Fatal(err)
	}

	res, err := c.ReadResource(ctx, mcp.ReadResourceRequest{
		Params: mcp.ReadResourceParams{URI: "voidsearch://pool/status"},
	})
	if err != nil {
		t.Fatalf("resources/read: %v", err)
	}
	if len(res.Contents) != 1 {
		t.Fatalf("контента %d, ожидала 1", len(res.Contents))
	}
	tc, ok := res.Contents[0].(mcp.TextResourceContents)
	if !ok {
		t.Fatalf("контент не текстовый: %T", res.Contents[0])
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(tc.Text), &out); err != nil {
		t.Fatal(err)
	}
	if out["total"] != float64(2) || out["live"] != float64(1) {
		t.Errorf("сводка неверна: %+v", out)
	}
}

func TestHuntsResourceEmpty(t *testing.T) {
	_, c := newTestServer(t)
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := c.ReadResource(ctx, mcp.ReadResourceRequest{
		Params: mcp.ReadResourceParams{URI: "voidsearch://hunts/list"},
	})
	if err != nil {
		t.Fatalf("resources/read: %v", err)
	}
	tc := res.Contents[0].(mcp.TextResourceContents)
	var out map[string]any
	if err := json.Unmarshal([]byte(tc.Text), &out); err != nil {
		t.Fatal(err)
	}
	if out["count"] != float64(0) {
		t.Errorf("на пустой базе охот 0, получила %+v", out)
	}
}

func TestStatsResourceHasVersion(t *testing.T) {
	_, c := newTestServer(t)
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := c.ReadResource(ctx, mcp.ReadResourceRequest{
		Params: mcp.ReadResourceParams{URI: "voidsearch://stats/summary"},
	})
	if err != nil {
		t.Fatalf("resources/read: %v", err)
	}
	tc := res.Contents[0].(mcp.TextResourceContents)
	var out map[string]any
	if err := json.Unmarshal([]byte(tc.Text), &out); err != nil {
		t.Fatal(err)
	}
	if out["version"] != "test" {
		t.Errorf("version=%v", out["version"])
	}
	if _, ok := out["db"]; !ok {
		t.Error("в сводке нет блока db")
	}
}

func TestResourcesWithoutStore(t *testing.T) {
	srv := New(Deps{Version: "test", Started: time.Now()})
	c := inProcess(t, srv)
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Без базы ресурсы отдают ошибку в теле, а не роняют сервер.
	for _, uri := range []string{"voidsearch://pool/status", "voidsearch://hunts/list"} {
		res, err := c.ReadResource(ctx, mcp.ReadResourceRequest{
			Params: mcp.ReadResourceParams{URI: uri},
		})
		if err != nil {
			t.Fatalf("%s: %v", uri, err)
		}
		tc := res.Contents[0].(mcp.TextResourceContents)
		var out map[string]any
		if err := json.Unmarshal([]byte(tc.Text), &out); err != nil {
			t.Fatal(err)
		}
		if _, ok := out["error"]; !ok {
			t.Errorf("%s без store обязан отдать error: %s", uri, tc.Text)
		}
	}
}

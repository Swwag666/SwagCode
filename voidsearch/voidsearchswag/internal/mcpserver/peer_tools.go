package mcpserver

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

func (d Deps) peerListHandler(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	type peerState struct {
		Addr      string `json:"addr"`
		OK        bool   `json:"ok"`
		LatencyMS int64  `json:"latency_ms"`
		Error     string `json:"error,omitempty"`
	}
	states := make([]peerState, 0, len(d.Peers))
	for _, p := range d.Peers {
		base := strings.TrimRight(strings.TrimSpace(p), "/")
		if base == "" {
			continue
		}
		st := peerState{Addr: base}
		start := time.Now()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/health", nil)
		if err != nil {
			st.Error = err.Error()
			states = append(states, st)
			continue
		}
		cl := &http.Client{Timeout: 10 * time.Second}
		resp, err := cl.Do(req)
		if err != nil {
			st.Error = err.Error()
		} else {
			resp.Body.Close()
			st.LatencyMS = time.Since(start).Milliseconds()
			st.OK = resp.StatusCode == http.StatusOK
			if !st.OK {
				st.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
			}
		}
		states = append(states, st)
	}
	return jsonResult(map[string]any{"peers": states, "count": len(states)})
}

func (d Deps) peerSyncHandler(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if d.Store == nil {
		return mcp.NewToolResultError("база не инициализирована"), nil
	}
	if len(d.Peers) == 0 {
		// Этап 178 (смоук-G): пустой список пиров - состояние, а не ошибка
		// вызова. Прежний isError приучал агента лечить здоровую конфигурацию:
		// сервер без VOIDSEARCH_PEERS - нормальная одиночная нода. Пустой
		// список отдан в той же форме, что и непустой: peers и счётчики.
		return jsonResult(map[string]any{
			"peers": []any{}, "merged_onions": 0, "merged_hunts": 0,
			"note": "пиры не заданы (VOIDSEARCH_PEERS): синк нечего обходить",
		})
	}
	limit := req.GetInt("limit", 500)
	if limit <= 0 {
		limit = 500
	}
	if limit > 2000 {
		limit = 2000
	}
	type peerRep struct {
		Addr   string `json:"addr"`
		OK     bool   `json:"ok"`
		Onions int    `json:"onions"`
		Hunts  int    `json:"hunts"`
		Error  string `json:"error,omitempty"`
	}
	reps := make([]peerRep, 0, len(d.Peers))
	mergedOnions, mergedHunts := 0, 0
	for _, p := range d.Peers {
		base := strings.TrimRight(strings.TrimSpace(p), "/")
		if base == "" {
			continue
		}
		rep := peerRep{Addr: base}
		onions, hunts, err := SyncPeer(ctx, d.Store, base, d.PeerToken, limit)
		if err != nil {
			rep.Error = err.Error()
			reps = append(reps, rep)
			continue
		}
		rep.OK = true
		rep.Onions = onions
		rep.Hunts = hunts
		mergedOnions += onions
		mergedHunts += hunts
		reps = append(reps, rep)
	}
	return jsonResult(map[string]any{
		"peers": reps, "merged_onions": mergedOnions, "merged_hunts": mergedHunts,
	})
}

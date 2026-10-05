package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"voidsearchswag/internal/store"
)

// peerExport отдаёт пул и охоты для синка: limit адресов (дефолт 500,
// потолок 2000), опциональный фильтр status. Вызывающий мержит по url:
// дедупликация уже на его стороне через Upsert.
func peerExport(w http.ResponseWriter, r *http.Request, st *store.Store) {
	if st == nil {
		http.Error(w, "база не инициализирована", http.StatusServiceUnavailable)
		return
	}
	limit := 500
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 2000 {
		limit = 2000
	}
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	onions, err := st.ListOnions(ctx, status, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	type onionOut struct {
		URL      string `json:"url"`
		Status   string `json:"status"`
		Category string `json:"category"`
		Title    string `json:"title"`
	}
	type huntOut struct {
		Query       string `json:"query"`
		Mode        string `json:"mode"`
		ScheduleMin int    `json:"schedule_min"`
	}
	out := map[string]any{"onions": []onionOut{}, "hunts": []huntOut{}}
	for _, o := range onions {
		out["onions"] = append(out["onions"].([]onionOut), onionOut{
			URL: o.URL, Status: o.Status, Category: o.Category, Title: o.Title,
		})
	}
	// Ошибка чтения охот обязана стать отказом выгрузки, а не пустым списком.
	// Документ с hunts: [] неотличим для потребителя от «у пира нет охот»,
	// поэтому SyncPeer принимал выгрузку, возвращал (onions, 0, nil), и обмен
	// переставал переносить охоты навсегда, пока ошибка сохранялась, - обе
	// стороны рапортовали успех.
	//
	// Пул выше обрабатывается отказом с HTTP 500, и два источника одной выгрузки
	// не должны вести себя по-разному: неполная выгрузка опаснее явного отказа,
	// потому что её принимают за полную.
	hunts, err := st.ListHunts(ctx)
	if err != nil {
		http.Error(w, fmt.Sprintf("охоты: %v", err), http.StatusInternalServerError)
		return
	}
	for _, h := range hunts {
		out["hunts"] = append(out["hunts"].([]huntOut), huntOut{
			Query: h.Query, Mode: h.Mode, ScheduleMin: h.ScheduleMin,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// peerExportDoc - контракт экспорта для пиров и тестов.
type peerExportDoc struct {
	Onions []struct {
		URL      string `json:"url"`
		Status   string `json:"status"`
		Category string `json:"category"`
		Title    string `json:"title"`
	} `json:"onions"`
	Hunts []struct {
		Query       string `json:"query"`
		Mode        string `json:"mode"`
		ScheduleMin int    `json:"schedule_min"`
	} `json:"hunts"`
}

func fetchPeerExport(ctx context.Context, base, token string, limit int) (peerExportDoc, error) {
	var doc peerExportDoc
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		return doc, fmt.Errorf("пустой адрес пира")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/peer/export?limit=%d", base, limit), nil)
	if err != nil {
		return doc, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	cl := &http.Client{Timeout: 60 * time.Second}
	resp, err := cl.Do(req)
	if err != nil {
		return doc, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return doc, fmt.Errorf("пир %s: HTTP %d: %s", base, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return doc, fmt.Errorf("пир %s: разбор: %w", base, err)
	}
	return doc, nil
}

// SyncPeer забирает выгрузку пира и мержит в свою базу: onion по url
// (Upsert), охоты по паре query+mode (CreateHunt, дубликат пропускается).
// Общая для peer_sync и фонового тика: одна логика слияния везде.
// SyncPeer принимает экспорт пира: адреса и охоты.
//
// Возвращает число принятых записей и ошибку, если часть строк не записалась.
// Прежняя версия глотала каждую ошибку построчно (`if err == nil { onions++ }`)
// и всегда возвращала nil, поэтому обмен на полном или заблокированном диске
// записывал ноль строк, возвращал (0, 0, nil), а вызывающий рапортовал
// OK: true или вообще молчал. Оператор видел здоровые отношения с пиром, пока
// данные не переставали приходить совсем.
func SyncPeer(ctx context.Context, st *store.Store, base, token string, limit int) (onions, hunts int, err error) {
	doc, err := fetchPeerExport(ctx, base, token, limit)
	if err != nil {
		return 0, 0, err
	}
	var failed int
	var firstErr error
	for _, o := range doc.Onions {
		if strings.TrimSpace(o.URL) == "" {
			continue
		}
		// MergePeerOnion, а не UpsertOnion: экспорт пира не несёт latency_avg,
		// success_rate, fail_streak и last_probe, а UpsertOnion пишет эти
		// колонки безусловно. Почасовой обмен обнулял статистику до 2000 строк
		// пула и проставлял им last_probe = сейчас, из-за чего ранжирование
		// ListOnions вырождались в алфавитное, а NextProbeWave переставал
		// возвращать эти адреса на перепроверку.
		if merr := st.MergePeerOnion(ctx, o.URL, o.Status, o.Category, o.Title); merr != nil {
			failed++
			if firstErr == nil {
				firstErr = merr
			}
			continue
		}
		onions++
	}
	known := map[string]bool{}
	if list, lerr := st.ListHunts(ctx); lerr != nil {
		// Пустой known означал бы, что все охоты пира считаются новыми и
		// будут пересозданы. Ошибка чтения - не то же самое, что пустой список.
		failed++
		if firstErr == nil {
			firstErr = fmt.Errorf("список охот: %w", lerr)
		}
	} else {
		for _, h := range list {
			known[strings.ToLower(strings.TrimSpace(h.Query))+"\x00"+strings.ToLower(h.Mode)] = true
		}
	}
	for _, h := range doc.Hunts {
		q := strings.ToLower(strings.TrimSpace(h.Query))
		if q == "" || known[q+"\x00"+strings.ToLower(h.Mode)] {
			continue
		}
		if _, herr := st.CreateHunt(ctx, store.Hunt{
			Query: strings.TrimSpace(h.Query), Mode: h.Mode, ScheduleMin: h.ScheduleMin,
		}); herr != nil {
			failed++
			if firstErr == nil {
				firstErr = herr
			}
			continue
		} else {
			hunts++
			known[q+"\x00"+strings.ToLower(h.Mode)] = true
		}
	}
	if failed > 0 {
		return onions, hunts, fmt.Errorf("обмен с пиром: принято %d адресов и %d охот, %d записей не удалось, первая ошибка: %w",
			onions, hunts, failed, firstErr)
	}
	return onions, hunts, nil
}

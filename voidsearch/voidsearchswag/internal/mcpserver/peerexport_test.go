package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"voidsearchswag/internal/store"
)

// exportStore открывает базу экспорта по тому же пути, что и dropMCPTable, чтобы
// таблицу можно было удалить отдельным соединением.
func exportStore(t *testing.T, dir string) *store.Store {
	t.Helper()
	st, err := store.Open(dir + "/mcp.db")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return st
}

func seedExport(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := context.Background()
	if err := st.UpsertOnion(ctx, store.Onion{URL: "http://a.onion", Status: "live", Title: "архив"}); err != nil {
		t.Fatalf("onion: %v", err)
	}
	if _, err := st.CreateHunt(ctx, store.Hunt{Query: "leak", Mode: "deep", ScheduleMin: 60}); err != nil {
		t.Fatalf("hunt: %v", err)
	}
}

func TestPeerExportReportsUnreadableHunts(t *testing.T) {
	// Экспорт пира обязан отказаться отдавать данные, если часть их прочитать не
	// удалось. Прежняя версия на ошибке ListHunts оставляла в ответе пустой
	// список, и потребитель получал документ, неотличимый от «у пира нет охот».
	//
	// SyncPeer мержит охоты по паре query+mode и пустой список не превращает в
	// удаление, поэтому данные пира не портились, - но обмен переставал переносить
	// охоты навсегда, пока ошибка сохранялась, и обе стороны рапортовали успех:
	// onions больше нуля, hunts ноль, ошибка nil.
	//
	// Неполная выгрузка опаснее явного отказа ещё и потому, что в этой же функции
	// ошибка чтения пула отдаёт HTTP 500: два источника одной выгрузки вели себя
	// по-разному.
	dir := t.TempDir()
	st := exportStore(t, dir)
	seedExport(t, st)
	dropMCPTable(t, dir, "hunts")

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/peer/export?limit=10", nil)
	peerExport(w, req, st)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("код %d, ожидала %d: выгрузка с непрочитанными охотами отдана как полная", w.Code, http.StatusInternalServerError)
	}
	if body := w.Body.String(); !strings.Contains(body, "hunts") && !strings.Contains(body, "no such table") {
		t.Errorf("в ответе нет причины: %q", body)
	}
}

func TestPeerExportReportsUnreadableOnions(t *testing.T) {
	// Поведение пула уже было правильным; тест фиксирует его, чтобы правка
	// охот не разъехалась с ним.
	dir := t.TempDir()
	st := exportStore(t, dir)
	seedExport(t, st)
	dropMCPTable(t, dir, "onion_pool")

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/peer/export?limit=10", nil)
	peerExport(w, req, st)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("код %d, ожидала %d", w.Code, http.StatusInternalServerError)
	}
}

func TestPeerExportHealthyCarriesBothSources(t *testing.T) {
	// Здоровая база обязана отдавать и адреса, и охоты: правка не должна
	// превратить экспорт в вечно отказывающий.
	dir := t.TempDir()
	st := exportStore(t, dir)
	seedExport(t, st)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/peer/export?limit=10&status=live", nil)
	peerExport(w, req, st)

	if w.Code != http.StatusOK {
		t.Fatalf("код %d, ожидала 200; тело: %s", w.Code, w.Body.String())
	}
	var doc struct {
		Onions []struct {
			URL   string `json:"url"`
			Title string `json:"title"`
		} `json:"onions"`
		Hunts []struct {
			Query string `json:"query"`
			Mode  string `json:"mode"`
		} `json:"hunts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatalf("разбор выгрузки: %v (%s)", err, w.Body.String())
	}
	if len(doc.Onions) != 1 || doc.Onions[0].URL != "http://a.onion" {
		t.Errorf("адреса потеряны: %+v", doc.Onions)
	}
	if len(doc.Hunts) != 1 || doc.Hunts[0].Query != "leak" || doc.Hunts[0].Mode != "deep" {
		t.Errorf("охоты потеряны: %+v", doc.Hunts)
	}
}

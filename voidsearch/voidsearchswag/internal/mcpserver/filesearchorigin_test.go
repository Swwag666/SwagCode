package mcpserver

import (
	"strings"
	"testing"
	"time"
)

// Этап 168. Живой замер ДО (стенд after167b): file_search по метке из
// discover возвращал записи, в которых не было ни task_id, ни id, ни
// даты - происхождение файла оставалось неназванным в единственном
// месте, где юзер его ищет. Пустой результат по метке повтора
// (task_id из свежего discover, файлы уже под первой меткой) тоже шёл
// без объяснения: «files: []» при files=0 в discover, и агент
// пытался «перезаписать» задачу третьим прогоном.

func TestFileSearchRecordsCarryOrigin(t *testing.T) {
	_, c := crawlPool(t)

	dis := callToolLong(t, c, "discover_onions", map[string]any{
		"crawl": true, "depth": 1, "max_hosts": 2, "timeout": 30,
	})
	tid, _ := dis["file_task_id"].(string)
	if tid == "" {
		t.Fatalf("прогон без file_task_id: %v", dis)
	}

	fs := callToolLong(t, c, "file_search", map[string]any{"task_id": tid, "limit": 100})
	ret, _ := fs["returned"].(float64)
	if ret < 1 {
		t.Fatalf("по метке %q пусто: %v", tid, fs)
	}
	files, _ := fs["files"].([]any)
	for i, raw := range files {
		e, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("запись %d не объект: %v", i, raw)
		}
		if id, _ := e["id"].(float64); id <= 0 {
			t.Errorf("запись %d без id: %v", i, e)
		}
		if got, _ := e["task_id"].(string); got != tid {
			t.Errorf("запись %d называет task_id=%q, а метка поиска %q: происхождение не названо", i, got, tid)
		}
		found, _ := e["found_at"].(string)
		if found == "" {
			t.Errorf("запись %d без found_at: когда файл попал в каталог, не видно", i)
			continue
		}
		if _, err := time.Parse(time.RFC3339, found); err != nil {
			t.Errorf("запись %d: found_at %q не RFC3339: %v", i, found, err)
		}
	}
}

func TestFileSearchEmptyTaskIDExplainsWhy(t *testing.T) {
	_, c := crawlPool(t)

	// Метка повтора: файлы под ней не появятся, потому что AddFile держит
	// происхождение первой записи. Пустота обязана быть объяснена, иначе
	// она читается как «файлов нет» и провоцирует лишние прогоны.
	ghost := "crawl-20990101-000000.000"
	fs := callToolLong(t, c, "file_search", map[string]any{"task_id": ghost, "limit": 100})
	ret, _ := fs["returned"].(float64)
	if ret != 0 {
		t.Fatalf("под несуществующей меткой %v записей: %v", ret, fs)
	}
	note, _ := fs["note"].(string)
	if note == "" {
		t.Fatalf("пустой результат по метке без объяснения: %v", fs)
	}
	if !strings.Contains(note, "происхождение") {
		t.Errorf("note не называет причину: %q", note)
	}
}

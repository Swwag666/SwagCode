package search

import (
	"context"
	"strings"
	"testing"

	"voidsearchswag/internal/router"
	"voidsearchswag/internal/searchers"
	"voidsearchswag/internal/store"
)

func stubResults() []searchers.Result {
	return []searchers.Result{{URL: "https://a.example/1", Title: "A"}}
}

func TestSearchRejectsUnknownMode(t *testing.T) {
	// Главная проверка. Движок принимал любой режим: switch в Search имел
	// ветку default, и неизвестное значение молча уходило в clearnet. Режим
	// deep с опечаткой означал запросы в открытый интернет вместо onion -
	// неверный результат и утечка намерения, о которой вызывающий не узнаёт.
	e, _ := newEngine(t, &fixedSearcher{name: "s", res: stubResults()})

	for _, mode := range []router.Mode{"телепорт", "FAST", "fast ", "tor", "onion", "deep!", "-", "quick"} {
		out, err := e.Search(context.Background(), Options{Query: "onion leak dump", Mode: mode, NoCache: true})
		if err == nil {
			t.Errorf("режим %q принят молча: out %+v", mode, out)
			continue
		}
		if out != nil {
			t.Errorf("режим %q: вместе с ошибкой возвращён результат %+v", mode, out)
		}
		if !strings.Contains(err.Error(), "auto|fast|stealth|deep") {
			t.Errorf("режим %q: в ошибке нет списка допустимых значений: %v", mode, err)
		}
	}
}

func TestSearchRejectsUnknownModeBeforeCache(t *testing.T) {
	// Проверка стоит до ключа кэша и до счётчиков: мусорный режим не должен
	// успеть записать себя как полноценный прогон.
	e, st := newEngine(t, &fixedSearcher{name: "s", res: stubResults()})
	ctx := context.Background()

	if _, err := e.Search(ctx, Options{Query: "cache probe", Mode: router.Mode("телепорт")}); err == nil {
		t.Fatal("движок принял мусорный режим")
	}
	payload, ok, err := st.CacheGet(ctx, store.CacheKey("cache probe", "телепорт"))
	if err != nil {
		t.Fatalf("CacheGet: %v", err)
	}
	if ok {
		t.Errorf("мусорный режим записан в кэш: %.120s", payload)
	}
}

func TestSearchAcceptsDocumentedModes(t *testing.T) {
	// Обратная сторона: все режимы, которые возвращает router.Parse, обязаны
	// работать, включая пустое значение и auto - они проходят через роутер.
	e, _ := newEngine(t, &fixedSearcher{name: "s", res: stubResults()})

	for _, mode := range []router.Mode{"", router.ModeAuto, router.ModeFast, router.ModeStealth, router.ModeDeep} {
		out, err := e.Search(context.Background(), Options{Query: "leak database", Mode: mode, NoCache: true})
		if err != nil {
			t.Errorf("режим %q отвергнут: %v", mode, err)
			continue
		}
		if out == nil || out.UsedMode == "" {
			t.Errorf("режим %q: пустой исход поиска %+v", mode, out)
		}
	}
}

func TestSearchUnknownModeErrorNamesValue(t *testing.T) {
	// В ошибке должно быть само значение: вызывающему, который не проходит
	// router.Parse, иначе не понять, что именно он передал.
	e, _ := newEngine(t, &fixedSearcher{name: "s", res: stubResults()})

	_, err := e.Search(context.Background(), Options{Query: "leak", Mode: router.Mode("телепорт"), NoCache: true})
	if err == nil {
		t.Fatal("движок принял мусорный режим")
	}
	if !strings.Contains(err.Error(), "телепорт") {
		t.Errorf("в ошибке нет переданного значения: %v", err)
	}
}

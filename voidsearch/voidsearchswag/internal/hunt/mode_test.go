package hunt

import (
	"context"
	"strings"
	"testing"
)

func TestCreateRejectsUnknownMode(t *testing.T) {
	// Охота с недопустимым режимом принималась и ложилась в базу как есть, а
	// адаптер поиска в cmd при разборе молча подменял значение на auto. Охота
	// «мониторю даркнет» с опечаткой в режиме превращалась в обычный
	// clearnet-поиск: находки не те, отчёт выглядит здоровым, и ни лог, ни
	// код возврата не намекают, что заданный режим проигнорирован.
	//
	// Проверка стоит в Create, а не в вызывающем коде, потому что охоту заводят
	// двое - CLI и MCP-инструмент hunt_create, - и валидация в одном из них
	// оставляет дыру в другом.
	st := openStore(t)
	r := &Runner{Store: st}
	ctx := context.Background()

	for _, bad := range []string{"телепорт", "depp", "DEEP!", "onion-", "quick"} {
		if _, err := r.Create(ctx, "leak "+bad, bad, 60); err == nil {
			t.Errorf("режим %q принят: охота будет молча работать не в нём", bad)
			continue
		} else if !strings.Contains(err.Error(), bad) {
			t.Errorf("режим %q: в ошибке не названо отвергнутое значение: %v", bad, err)
		}
	}
	// Отвергнутые охоты не должны остаться в базе.
	list, err := r.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("в базе %d охот после отказов: %+v", len(list), list)
	}
}

func TestCreateAcceptsDocumentedModes(t *testing.T) {
	// Обратная сторона: строгость не должна отрезать законные значения,
	// включая синонимы и пустой режим, который означает «выберет роутер».
	st := openStore(t)
	r := &Runner{Store: st}
	ctx := context.Background()

	for _, mode := range []string{"", "auto", "fast", "stealth", "deep", "tor", "onion", " DEEP "} {
		if _, err := r.Create(ctx, "leak "+mode, mode, 60); err != nil {
			t.Errorf("режим %q отвергнут: %v", mode, err)
		}
	}
}

func TestCreateNormalizesMode(t *testing.T) {
	// Синонимы приводятся к каноническому значению, чтобы hunt list и отчёты не
	// показывали три разных названия одного режима.
	st := openStore(t)
	r := &Runner{Store: st}
	ctx := context.Background()

	if _, err := r.Create(ctx, "leak tor", "tor", 60); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := r.Create(ctx, "leak empty", "", 60); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := r.Create(ctx, "leak onion", "onion", 60); err != nil {
		t.Fatalf("Create: %v", err)
	}
	list, err := r.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got := map[string]string{}
	for _, h := range list {
		got[h.Query] = h.Mode
	}
	if got["leak tor"] != "deep" {
		t.Errorf("режим tor сохранён как %q, ожидала deep", got["leak tor"])
	}
	if got["leak onion"] != "deep" {
		t.Errorf("режим onion сохранён как %q, ожидала deep", got["leak onion"])
	}
	if got["leak empty"] != "auto" {
		t.Errorf("пустой режим сохранён как %q, ожидала auto", got["leak empty"])
	}
}

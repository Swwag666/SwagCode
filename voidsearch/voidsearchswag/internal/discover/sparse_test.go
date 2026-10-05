package discover

import (
	"context"
	"strings"
	"testing"
	"time"

	"voidsearchswag/internal/httpc"
)

func TestMinYieldDefaults(t *testing.T) {
	f := NewFinder(&flakySource{body: retryBody}, silentLog{})
	if f.minYield() != sourceMinYield {
		t.Errorf("minYield()=%d, ожидала %d", f.minYield(), sourceMinYield)
	}
	f.MinYield = 5
	if f.minYield() != 5 {
		t.Errorf("явный порог проигнорирован: %d", f.minYield())
	}
	f.MinYield = -1
	if f.minYield() != sourceMinYield {
		t.Errorf("отрицательный порог не заменён дефолтом: %d", f.minYield())
	}
}

// oneAddrSource отдаёт страницу с ровно одним адресом: каталог фактически пуст.
// Так в отчёте выглядел ahmia.fi/search - ответ 200, выдача один адрес.
type oneAddrSource struct{ calls int }

func (s *oneAddrSource) Fetch(ctx context.Context, r httpc.Request) (*httpc.Response, error) {
	s.calls++
	body := `<html><body><a href="http://` + v2a + `.onion/">Единственная ссылка в шапке</a></body></html>`
	return &httpc.Response{Status: 200, Body: []byte(body)}, nil
}

func TestSparseSourceNotReportedOK(t *testing.T) {
	// Реальный дефект, найденный вживую: ahmia.fi/search отвечал 200 и отдавал
	// один адрес, а отчёт показывал «ок». Источник мёртв, но выглядел здоровым,
	// поэтому его никто не чинил и не убирал.
	src := &oneAddrSource{}
	f := &Finder{Client: src, Log: silentLog{}, Attempts: 1}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, reports := f.DiscoverClearnet(ctx)

	if len(reports) == 0 {
		t.Fatal("отчётов нет")
	}
	for _, r := range reports {
		if r.OK {
			t.Errorf("%s: пустая выдача отмечена живой (found=%d)", r.Name, r.Found)
		}
		if !r.Sparse {
			t.Errorf("%s: флаг Sparse не выставлен при found=%d", r.Name, r.Found)
		}
		if r.Error == "" {
			t.Errorf("%s: причина не объяснена", r.Name)
		}
		if !strings.Contains(r.Error, "порог") {
			t.Errorf("%s: в причине нет упоминания порога: %q", r.Name, r.Error)
		}
	}
}

func TestSparseFlagSeparatesDeadCatalogFromNetworkError(t *testing.T) {
	// Sparse обязан отличать «ответ получен, выдача пустая» от «запрос не
	// прошёл»: чинить их надо по-разному, а без флага оба случая в отчёте
	// выглядят как OK=false.
	src := &oneAddrSource{}
	f := &Finder{Client: src, Log: silentLog{}, Attempts: 1}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, reports := f.DiscoverClearnet(ctx)

	for _, r := range reports {
		if !r.Sparse {
			t.Fatalf("%s: Sparse=false", r.Name)
		}
		if strings.Contains(r.Error, "HTTP ") || strings.Contains(r.Error, "таймаут") {
			t.Errorf("%s: редкая выдача замаскирована под сетевую ошибку: %q", r.Name, r.Error)
		}
	}
}

func TestHealthySourceStillReportedOK(t *testing.T) {
	// Порог не должен ломать нормальные источники: retryBody даёт два адреса,
	// это ровно дефолтный минимум.
	src := &flakySource{body: retryBody}
	f := &Finder{Client: src, Log: silentLog{}, Attempts: 1}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cands, reports := f.DiscoverClearnet(ctx)

	if len(cands) == 0 {
		t.Fatal("адреса не собраны")
	}
	var ok int
	for _, r := range reports {
		if r.OK {
			ok++
		}
		if r.Sparse {
			t.Errorf("%s: источник с пороговой выдачей помечен редким", r.Name)
		}
	}
	if ok == 0 {
		t.Error("ни один источник не отмечен живым")
	}
}

func TestSparseSourceAddressesStillCollected(t *testing.T) {
	// Редкий источник не здоров, но адрес из него терять нельзя: один
	// рабочий адрес лучше нуля, и он всё равно доедет до пула.
	src := &oneAddrSource{}
	f := &Finder{Client: src, Log: silentLog{}, Attempts: 1}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cands, _ := f.DiscoverClearnet(ctx)

	if len(cands) == 0 {
		t.Error("адрес редкого источника выброшен")
	}
}

func TestExplicitMinYieldRaisesBar(t *testing.T) {
	// Порог настраивается: retryBody даёт два адреса, при MinYield=3 источник
	// обязан стать редким.
	src := &flakySource{body: retryBody}
	f := &Finder{Client: src, Log: silentLog{}, Attempts: 1, MinYield: 3}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, reports := f.DiscoverClearnet(ctx)

	var sparse int
	for _, r := range reports {
		if r.Sparse {
			sparse++
		}
		if r.OK {
			t.Errorf("%s: отмечен живым при пороге 3 и выдаче 2", r.Name)
		}
	}
	if sparse == 0 {
		t.Error("повышенный порог не сработал")
	}
}

package mcpserver

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// Этап 177: четыре семантики, которые жили в коде, но не были названы.
// Смоуки 176-го читали неозвученное как дефект: raw-латентность при
// воскрешении - «аномалия EMA», dead=3 при pool_status=unknown -
// «три провала одного сервиса», порядок pool_status - «последние записи»,
// недомерянный file_ref - «размер потерян». Описание обязано называть
// фактическое поведение, иначе агент тратит ход на разгадку «глюк или
// так задумано».
func TestDescriptionsNameHiddenSemantics(t *testing.T) {
	c := newClient(t, Deps{Version: "test", Started: time.Now()})
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	wants := map[string][]string{
		"probe_pool": {
			// D2: волна - исходы по одному на адрес, а не три провала
			// одного сервиса. Смоук-A 176: dead=3 при pool_status=
			// «unknown» читался как «один сервис трижды отказал».
			"live и dead считают исходы этой волны по одному на адрес",
			"dead=3 значит, что у трёх разных адресов проба не удалась, а не что один сервис провалился трижды",
			// D1: воскрешение из latency_avg=0 - первая мера целиком,
			// не сглаженная (EMA от нуля занизила бы её вчетверо).
			// Смоук-A 176: 3539 и смоук-C: 3110 - raw-числа читались
			// как обход EMA.
			"воскрешённая запись без измеренной латентности (latency_avg=0) получает первую успешную меру целиком",
			"неуспешные пробы латентность не пишут",
		},
		"pool_status": {
			// D4: фактический порядок - живые и быстрые первыми; BEFORE
			// на витрине: записи 2024-01-01 в голове, свежие 16:55 -
			// в хвосте, вопреки обещанию «последние записи».
			"живые и быстрые первыми (тот же порядок выборки, что у onion_search",
			"«последняя проба» - поле last_probe каждой записи, а не порядок списка",
			// D1 в записи: первая латентность целиком.
			"первая успешная проба становится avg целиком - сглаживание включается со второй точки",
		},
		"discover_onions": {
			// D3: фаза замера видна в под-отчёте. BEFORE: ответ с
			// file_refs_found=1 не содержал НИ ОДНОГО ключа о мере.
			"file_refs_measured/file_refs_unmeasured делят найденные ссылки на несущие известный размер",
			"measure_cutoff называет срез окна замера до конца списка",
		},
	}
	for _, tool := range res.Tools {
		want, ok := wants[tool.Name]
		if !ok {
			continue
		}
		for _, w := range want {
			if !strings.Contains(tool.Description, w) {
				t.Errorf("описание %s не содержит %q", tool.Name, w)
			}
		}
		delete(wants, tool.Name)
	}
	for name := range wants {
		t.Errorf("инструмент %s не найден в tools/list", name)
	}
}

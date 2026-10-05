package mcpserver

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// Этап 176: волна probe_pool замораживала проверенные записи. Выборка
// NextUnprobed держала WHERE status='unknown', описание говорило «проверяет
// неизвестные адреса» - и, пока в пуле есть непроверенные (на живой базе
// тысячи), волна не трогала ни одного live или dead: мёртвый сервис
// оставался live навечно, латентность застывала на первой записи, записи с
// легаси-тысячей из до-175 эпохи не перекрашивались. Живой BEFORE-факт на
// витрине (5 live 2024-года, 2 dead, 3 unknown): limit=5 вернул total=3 -
// семь записей пула волна не видит.
//
// Описание обязано называть очередь охвата: иначе агент читает «проверяет
// неизвестные» и трактует освежение live/dead как ошибку выборки.
func TestProbePoolDescriptionNamesWaveCoverage(t *testing.T) {
	c := newClient(t, Deps{Version: "test", Started: time.Now()})
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	for _, tool := range res.Tools {
		if tool.Name != "probe_pool" {
			continue
		}
		for _, want := range []string{
			"сначала ни разу не проверенные",
			"затем unknown с историей",
			"затем самые устаревшие записи пула",
			"live и dead по старшинству last_probe",
			// Устаревший live обновляется, устаревший dead может вернуться:
			// без этих фраз агент не отличает освежение от ошибки выборки.
			"live обновляет латентность",
			"dead при успехе воскрешается в live",
		} {
			if !strings.Contains(tool.Description, want) {
				t.Errorf("описание probe_pool не содержит %q: %q", want, tool.Description)
			}
		}
		// Прежняя формулировка «проверяет неизвестные адреса» объявляла
		// выборку unknown-only - именно её этап 176 и снимает.
		if strings.Contains(tool.Description, "проверяет неизвестные адреса") {
			t.Errorf("описание probe_pool всё ещё объявляет unknown-only выборку")
		}
		return
	}
	t.Fatal("probe_pool не найден")
}

package mcpserver

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// Этап 173. Смоук-агент B этапа 172 поймал хвост ответа: discover отвечал
// на 2с/22.6с/38.5с позже заявленного timeout (5/30/90с), и HTTP-клиент с
// равным таймаутом терял валидные ответы. Причина - замер размеров (сеть)
// под собственным контекстом без отмены родителя. Описание инструмента
// обязано называть это правило явно: замер в бюджете вызова, после
// обрезки сеть для размеров не ходит, недомерянные ссылки - size=0.

func TestDiscoverDescriptionNamesMeasureBudgetRule(t *testing.T) {
	c := newClient(t, Deps{Version: "test", Started: time.Now()})
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	for _, tool := range res.Tools {
		if tool.Name == "discover_onions" {
			for _, want := range []string{
				"за секунды локальной записи",
				"замер размеров файлов (сеть) живёт в бюджете вызова",
				"после обрезки не ходит",
				"size=0",
			} {
				if !strings.Contains(tool.Description, want) {
					t.Errorf("описание discover_onions не содержит %q: %q", want, tool.Description)
				}
			}
			return
		}
	}
	t.Fatal("discover_onions не найден")
}

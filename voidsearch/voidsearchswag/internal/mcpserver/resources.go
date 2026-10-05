package mcpserver

import (
	"context"
	"encoding/json"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// registerResources отдаёт то же, что pool_status/hunt_list/stats, но как
// читаемые MCP-ресурсы: клиент подтягивает состояние без вызова инструментов.
// URI стабильные, содержимое - JSON.
func (d Deps) registerResources(s *server.MCPServer) {
	s.AddResource(
		mcp.NewResource("voidsearch://pool/status", "pool-status",
			mcp.WithResourceDescription("Состояние onion-пула: всего/живых, разбивка по статусам"),
			mcp.WithMIMEType("application/json"),
		),
		func(ctx context.Context, _ mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
			return d.poolResource(ctx)
		},
	)
	s.AddResource(
		mcp.NewResource("voidsearch://hunts/list", "hunts-list",
			mcp.WithResourceDescription("Фоновые мониторинги: запрос, режим, последний прогон"),
			mcp.WithMIMEType("application/json"),
		),
		func(ctx context.Context, _ mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
			return d.huntsResource(ctx)
		},
	)
	s.AddResource(
		mcp.NewResource("voidsearch://stats/summary", "stats-summary",
			mcp.WithResourceDescription("Сводная статистика: база, файлы, задачи, охоты, селекторы, движки"),
			mcp.WithMIMEType("application/json"),
		),
		func(ctx context.Context, _ mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
			return d.statsResource(ctx)
		},
	)
}

func resourceText(uri string, v any) ([]mcp.ResourceContents, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return []mcp.ResourceContents{
		mcp.TextResourceContents{URI: uri, MIMEType: "application/json", Text: string(b)},
	}, nil
}

func (d Deps) poolResource(ctx context.Context) ([]mcp.ResourceContents, error) {
	if d.Store == nil {
		return resourceText("voidsearch://pool/status", map[string]any{"error": "база не инициализирована"})
	}
	// Общий сборщик: список непрочитанных источников попадает в поле errors, и
	// частичная поломка базы перестаёт выглядеть как пустой пул.
	out, _ := poolSummary(ctx, d.Store)
	return resourceText("voidsearch://pool/status", out)
}

func (d Deps) huntsResource(ctx context.Context) ([]mcp.ResourceContents, error) {
	if d.Store == nil {
		return resourceText("voidsearch://hunts/list", map[string]any{"error": "база не инициализирована"})
	}
	hunts, err := d.Store.ListHunts(ctx)
	if err != nil {
		return resourceText("voidsearch://hunts/list", map[string]any{"error": err.Error()})
	}
	return resourceText("voidsearch://hunts/list", map[string]any{"hunts": hunts, "count": len(hunts)})
}

func (d Deps) statsResource(ctx context.Context) ([]mcp.ResourceContents, error) {
	out := map[string]any{"version": d.Version}
	if d.Store != nil {
		// Тот же сборщик, что и у инструмента stats: ресурс и его
		// инструмент-двойник обязаны отдавать одинаковую картину, иначе
		// расхождение между ними выглядит как разные данные, а не как поломка.
		db, _ := dbSummary(ctx, d.Store)
		out["db"] = db
	}
	if d.Search != nil {
		out["onion_engines"] = d.onionEngineNames()
	}
	return resourceText("voidsearch://stats/summary", out)
}

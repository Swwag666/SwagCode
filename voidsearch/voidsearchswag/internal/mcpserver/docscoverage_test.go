package mcpserver

import (
	"context"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// Состав сервера в документации обязан совпадать с фактическим. Единственный
// источник правды - ответы tools/list и prompts/list, а строки и таблицы в README
// и docs/MCP.md устаревали молча, потому что расхождение ничем не проверялось.
//
// Замер до правки на HEAD 7b073ff: сервер отдаёт 27 инструментов и 8 промптов,
// в README написано «доступны 20 инструментов ... и 7 промптов», а hunt_hits не
// упоминается ни в таблице README, ни в таблице docs/MCP.md, хотя инструмент
// зарегистрирован и промпт judge на него не ссылается. Клиент, который читает
// документацию, не узнаёт об истории находок и считает, что половина сервера ему
// недоступна.
const (
	readmePath = "../../README.md"
	mcpDocPath = "../../docs/MCP.md"
)

// docNames возвращает имена в обратных кавычках из раздела документации.
var docNames = regexp.MustCompile("`([a-z][a-z0-9_]*)`")

// docSection вырезает раздел по точному заголовку: таблицы инструментов в обеих
// доках живут в своих разделах, а вне их те же обратные кавычки носят CLI-команды,
// промпты и переменные окружения.
func docSection(t *testing.T, path, heading string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("чтение %s: %v", path, err)
	}
	var out []string
	started := false
	for _, ln := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(ln)
		if strings.HasPrefix(trimmed, "#") {
			if started {
				break
			}
			started = trimmed == heading
			continue
		}
		if started {
			out = append(out, ln)
		}
	}
	if !started {
		t.Fatalf("раздел %q не найден в %s", heading, path)
	}
	return strings.Join(out, "\n")
}

// docTableNames разбирает раздел на множество имён из первой ячейки строк
// таблицы. Только первая ячейка: во второй и третьей те же обратные кавычки
// носят аргументы (`query`, `limit`) и чужие команды, и они не имена инструментов.
func docTableNames(t *testing.T, path, heading string) map[string]bool {
	t.Helper()
	names := make(map[string]bool)
	for _, ln := range strings.Split(docSection(t, path, heading), "\n") {
		trimmed := strings.TrimSpace(ln)
		if !strings.HasPrefix(trimmed, "|") {
			continue
		}
		cells := strings.Split(trimmed, "|")
		if len(cells) < 2 {
			continue
		}
		for _, m := range docNames.FindAllStringSubmatch(cells[1], -1) {
			names[m[1]] = true
		}
	}
	if len(names) == 0 {
		t.Fatalf("в разделе %q файла %s нет ни одного имени в таблице", heading, path)
	}
	return names
}

// serverToolNames возвращает фактические имена инструментов живого сервера.
func serverToolNames(t *testing.T) []string {
	t.Helper()
	_, c := newTestServer(t)
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	res, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	names := make([]string, 0, len(res.Tools))
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	return names
}

// serverPromptNames возвращает фактические имена промптов живого сервера.
func serverPromptNames(t *testing.T) []string {
	t.Helper()
	_, c := newTestServer(t)
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	res, err := c.ListPrompts(ctx, mcp.ListPromptsRequest{})
	if err != nil {
		t.Fatalf("prompts/list: %v", err)
	}
	names := make([]string, 0, len(res.Prompts))
	for _, p := range res.Prompts {
		names = append(names, p.Name)
	}
	sort.Strings(names)
	return names
}

func TestDocsListEveryTool(t *testing.T) {
	tools := serverToolNames(t)
	if len(tools) == 0 {
		t.Fatal("сервер не отдал ни одного инструмента")
	}

	docs := []struct{ path, heading string }{
		{readmePath, "## MCP-инструменты"},
		{mcpDocPath, "## Инструменты"},
	}
	for _, d := range docs {
		names := docTableNames(t, d.path, d.heading)
		for _, tool := range tools {
			if !names[tool] {
				t.Errorf("%s: инструмент %s не документирован в разделе %q", d.path, tool, d.heading)
			}
		}
	}
}

func TestDocsHaveNoUnknownTools(t *testing.T) {
	live := make(map[string]bool)
	for _, tool := range serverToolNames(t) {
		live[tool] = true
	}

	docs := []struct{ path, heading string }{
		{readmePath, "## MCP-инструменты"},
		{mcpDocPath, "## Инструменты"},
	}
	for _, d := range docs {
		for name := range docTableNames(t, d.path, d.heading) {
			if !live[name] {
				t.Errorf("%s: раздел %q описывает %s, которого нет в tools/list", d.path, d.heading, name)
			}
		}
	}
}

// Обе таблицы обязаны описывать один и тот же набор: расхождение между README и
// docs/MCP.md клиент увидит как два разных сервера.
func TestDocsTablesAgreeOnTools(t *testing.T) {
	readme := docTableNames(t, readmePath, "## MCP-инструменты")
	mcpDoc := docTableNames(t, mcpDocPath, "## Инструменты")

	for name := range readme {
		if !mcpDoc[name] {
			t.Errorf("%s есть в README, но нет в docs/MCP.md", name)
		}
	}
	for name := range mcpDoc {
		if !readme[name] {
			t.Errorf("%s есть в docs/MCP.md, но нет в README", name)
		}
	}
}

func TestDocsListEveryPrompt(t *testing.T) {
	prompts := serverPromptNames(t)
	if len(prompts) == 0 {
		t.Fatal("сервер не отдал ни одного промпта")
	}

	names := docTableNames(t, readmePath, "### Промпты")
	for _, prompt := range prompts {
		if !names[prompt] {
			t.Errorf("%s: промпт %s не документирован в разделе «### Промпты»", readmePath, prompt)
		}
	}
	for name := range names {
		found := false
		for _, prompt := range prompts {
			if prompt == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s: раздел «### Промпты» описывает %s, которого нет в prompts/list", readmePath, name)
		}
	}
}

// Числа в вводном абзаце README обязаны совпадать с фактом: именно их читает
// клиент, который ещё не вызывал tools/list.
func TestReadmeCountsMatchServer(t *testing.T) {
	tools := serverToolNames(t)
	prompts := serverPromptNames(t)

	raw, err := os.ReadFile(readmePath)
	if err != nil {
		t.Fatalf("чтение %s: %v", readmePath, err)
	}
	text := string(raw)

	toolRe := regexp.MustCompile(`(\d+) инструментов`)
	promptRe := regexp.MustCompile(`(\d+) промптов`)

	tm := toolRe.FindStringSubmatch(text)
	if tm == nil {
		t.Fatalf("в %s нет строки с числом инструментов", readmePath)
	}
	if got, err := strconv.Atoi(tm[1]); err != nil || got != len(tools) {
		t.Errorf("README обещает %s инструментов, сервер отдаёт %d", tm[1], len(tools))
	}

	pm := promptRe.FindStringSubmatch(text)
	if pm == nil {
		t.Fatalf("в %s нет строки с числом промптов", readmePath)
	}
	if got, err := strconv.Atoi(pm[1]); err != nil || got != len(prompts) {
		t.Errorf("README обещает %s промптов, сервер отдаёт %d", pm[1], len(prompts))
	}
}

// serverToolArgs возвращает фактический состав аргументов: свойства схемы ввода и
// список обязательных по каждому инструменту.
func serverToolArgs(t *testing.T) (props map[string][]string, required map[string][]string) {
	t.Helper()
	_, c := newTestServer(t)
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	res, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	props = make(map[string][]string, len(res.Tools))
	required = make(map[string][]string, len(res.Tools))
	for _, tool := range res.Tools {
		names := make([]string, 0, len(tool.InputSchema.Properties))
		for name := range tool.InputSchema.Properties {
			names = append(names, name)
		}
		sort.Strings(names)
		props[tool.Name] = names
		req := append([]string(nil), tool.InputSchema.Required...)
		sort.Strings(req)
		required[tool.Name] = req
	}
	if len(props) == 0 {
		t.Fatal("сервер не отдал ни одного инструмента")
	}
	return props, required
}

// docArgNames извлекает имя аргумента из второй ячейки таблицы. Имя может идти с
// пояснением внутри тех же кавычек - «`votes: [{url, score 0..1}]`», - поэтому
// берётся только начальный идентификатор.
var docArgNames = regexp.MustCompile("`([a-z_][a-z0-9_]*)")

// docToolArgs разбирает строки таблицы инструментов на множества аргументов.
// Ячейка «-» даёт пустое множество: у инструмента без аргументов их и не
// перечисляют. Пояснения вроде «(дефолт 100, потолок 1000)» живут вне обратных
// кавычек и в набор не попадают.
func docToolArgs(t *testing.T, path, heading string) map[string]map[string]bool {
	t.Helper()
	out := make(map[string]map[string]bool)
	for _, ln := range strings.Split(docSection(t, path, heading), "\n") {
		trimmed := strings.TrimSpace(ln)
		if !strings.HasPrefix(trimmed, "|") {
			continue
		}
		cells := strings.Split(trimmed, "|")
		if len(cells) < 4 {
			continue
		}
		nameM := docNames.FindStringSubmatch(cells[1])
		if nameM == nil {
			continue
		}
		args := make(map[string]bool)
		for _, m := range docArgNames.FindAllStringSubmatch(cells[2], -1) {
			args[m[1]] = true
		}
		out[nameM[1]] = args
	}
	if len(out) == 0 {
		t.Fatalf("в разделе %q файла %s не разобрано ни одной строки с аргументами", heading, path)
	}
	return out
}

// Замер до правки на HEAD 5b58112: состав аргументов в docs/MCP.md не сверялся со
// схемой сервера ничем. Подстановка в строку search выдуманного `foo_arg` вместо
// настоящего `no_cache` оставляла весь набор тестов документации зелёным -
// TestDocsListEveryTool, TestDocsHaveNoUnknownTools, TestDocsTablesAgreeOnTools,
// TestDocsListEveryPrompt и TestReadmeCountsMatchServer прошли, хотя клиент по
// такой документации вызвал бы инструмент с несуществующим аргументом и потерял бы
// настоящий.
func TestDocsListEveryToolArgument(t *testing.T) {
	props, _ := serverToolArgs(t)
	docs := docToolArgs(t, mcpDocPath, "## Инструменты")

	for tool, args := range props {
		listed, ok := docs[tool]
		if !ok {
			t.Errorf("%s: инструмент %s не описан в таблице аргументов", mcpDocPath, tool)
			continue
		}
		for _, arg := range args {
			if !listed[arg] {
				t.Errorf("%s: аргумент %s инструмента %s не перечислен", mcpDocPath, arg, tool)
			}
		}
	}
}

func TestDocsHaveNoUnknownToolArguments(t *testing.T) {
	props, _ := serverToolArgs(t)
	docs := docToolArgs(t, mcpDocPath, "## Инструменты")

	for tool, listed := range docs {
		args, ok := props[tool]
		if !ok {
			// Чужой инструмент в таблице - это проверяет TestDocsHaveNoUnknownTools,
			// здесь сообщение только мешает найти расхождение аргументов.
			continue
		}
		known := make(map[string]bool, len(args))
		for _, arg := range args {
			known[arg] = true
		}
		for arg := range listed {
			if !known[arg] {
				t.Errorf("%s: аргумент %s инструмента %s не существует в схеме", mcpDocPath, arg, tool)
			}
		}
	}
}

// Обязательный аргумент обязан быть перечислен явно: без него вызов не проходит, и
// клиент, который собирает запрос по документации, получит ошибку вместо
// результата.
func TestDocsListEveryRequiredArgument(t *testing.T) {
	_, required := serverToolArgs(t)
	docs := docToolArgs(t, mcpDocPath, "## Инструменты")

	total := 0
	for tool, args := range required {
		total += len(args)
		listed := docs[tool]
		for _, arg := range args {
			if !listed[arg] {
				t.Errorf("%s: обязательный аргумент %s инструмента %s не перечислен", mcpDocPath, arg, tool)
			}
		}
	}
	if total == 0 {
		t.Fatal("сервер не отдал ни одного обязательного аргумента: разбор схемы сломался")
	}
}

// Инструменты без аргументов обязаны быть отмечены прочерком, а не пустой
// ячейкой: пустая ячейка в markdown выглядит как потерянная часть строки.
func TestDocsMarkArgumentlessTools(t *testing.T) {
	props, _ := serverToolArgs(t)
	docs := docToolArgs(t, mcpDocPath, "## Инструменты")

	for tool, args := range props {
		if len(args) != 0 || len(docs[tool]) != 0 {
			continue
		}
		raw, err := os.ReadFile(mcpDocPath)
		if err != nil {
			t.Fatalf("чтение %s: %v", mcpDocPath, err)
		}
		row := "| `" + tool + "`"
		found := false
		for _, ln := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(strings.TrimSpace(ln), row) && strings.Contains(ln, "| - |") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s: у инструмента %s нет аргументов, но прочерк в таблице не стоит", mcpDocPath, tool)
		}
	}
}

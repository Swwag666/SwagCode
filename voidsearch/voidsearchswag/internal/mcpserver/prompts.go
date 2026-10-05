package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// Промпты - это сценарии, а не справка. Каждый описывает порядок вызовов
// под конкретную задачу и подставляет аргументы пользователя. Смысл в
// том, чтобы клиент не собирал цепочку инструментов сам: порядок важен,
// потому что шаги зависят друг от друга (например, обход бессмыслен без
// собранных адресов, а пробы бесполезны до разведки).

func (d Deps) registerPrompts(s promptRegistrar) {
	s.AddPrompt(mcp.NewPrompt("recon",
		mcp.WithPromptDescription("Полная разведка: собрать onion-адреса из каталогов, обойти их и проверить живость"),
		mcp.WithArgument("topic", mcp.ArgumentDescription("тема или запрос для поиска по собранной базе"), mcp.RequiredArgument()),
		mcp.WithArgument("depth", mcp.ArgumentDescription("глубина обхода, по умолчанию 1")),
	), func(ctx context.Context, req mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		topic := strings.TrimSpace(req.Params.Arguments["topic"])
		if topic == "" {
			return nil, fmt.Errorf("аргумент topic обязателен")
		}
		depth := strings.TrimSpace(req.Params.Arguments["depth"])
		if depth == "" {
			depth = "1"
		}
		text := fmt.Sprintf(`Разведка сети по теме: %s

Порядок важен, шаги зависят друг от друга:
1. discover_onions с depth=%s и crawl=true - сбор адресов из каталогов и обход найденных сервисов. Без crawl будут только адреса из каталогов.
2. pool_status - посмотреть, сколько всего и сколько живых.
3. probe_pool с limit=20 - проверить живость свежих адресов. Проба идёт через tor и занимает секунды на адрес, поэтому начинай с малой порции.
4. onion_search с query=%q - искать по собранной базе сервисов.
5. search с query=%q и mode=deep - поиск по clearnet и onion-поисковикам.

На что смотреть в отчётах: доля ok у источников, limit_hit у обхода (значит, часть хостов не тронута и нужен повторный прогон с большим max_hosts), живые из probe_pool.`, topic, depth, topic, topic)
		return promptResult("recon", text), nil
	})

	s.AddPrompt(mcp.NewPrompt("investigate",
		mcp.WithPromptDescription("Разбор конкретного сервиса: что внутри, какие файлы, какие ссылки"),
		mcp.WithArgument("host", mcp.ArgumentDescription("onion-адрес или clearnet-хост"), mcp.RequiredArgument()),
	), func(ctx context.Context, req mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		host := strings.TrimSpace(req.Params.Arguments["host"])
		if host == "" {
			return nil, fmt.Errorf("аргумент host обязателен")
		}
		if !strings.Contains(host, "://") {
			host = "http://" + host
		}
		text := fmt.Sprintf(`Разбор сервиса: %s

1. fetch с url=%q и max_chars=4000 - посмотреть, что отдаёт главная: заголовок, разметка, ссылки.
2. probe_pool с addr - если это onion, сначала убедись, что он живой.
3. collect_files с hosts=[%q] - собрать файлы с сервиса в каталог.
4. file_search - посмотреть, что нашлось; отобрать по расширению, метке категории (verdict), риску (risk_only) или задаче сбора (task_id).

Если fetch вернул защитную страницу (Cloudflare, captcha, «Just a moment»), значит для clearnet-хоста нужен не tor-транспорт: скажи об этом прямо, а не выдавай заглушку за содержимое.`, host, host, host)
		return promptResult("investigate", text), nil
	})

	s.AddPrompt(mcp.NewPrompt("harvest",
		mcp.WithPromptDescription("Сбор файлов из пула живых сервисов с поиском по каталогу"),
		mcp.WithArgument("ext", mcp.ArgumentDescription("расширение для фильтра: zip, pdf, sql, kdbx")),
		mcp.WithArgument("limit", mcp.ArgumentDescription("сколько хостов обойти, по умолчанию 10")),
	), func(ctx context.Context, req mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		ext := strings.TrimSpace(req.Params.Arguments["ext"])
		limit := strings.TrimSpace(req.Params.Arguments["limit"])
		if limit == "" {
			limit = "10"
		}
		step := "3. file_search без фильтра - посмотреть, что вообще нашлось."
		if ext != "" {
			step = fmt.Sprintf("3. file_search с ext=%q - отобрать нужные расширения.", ext)
		}
		text := fmt.Sprintf(`Сбор файлов из живых сервисов пула

1. probe_pool с limit=20 - убедиться, что пул содержит живые адреса. Обход мёртвых тратит время впустую.
2. collect_files с limit=%s - обойти живые хосты и собрать файлы в каталог.
%s
4. Если saved=0 - вывод не «файлов нет», а «сервисы не отдают файловых ссылок»: проверь на одном хосте через fetch, как выглядит разметка.

Учти: сбор идёт через tor, поэтому пауза между запросами к одному хосту обязательна. Частые запросы к одному onion-сервису дают бан вместо данных.`, limit, step)
		return promptResult("harvest", text), nil
	})

	s.AddPrompt(mcp.NewPrompt("compare",
		mcp.WithPromptDescription("Сравнить выдачу по одному запросу в разных режимах поиска"),
		mcp.WithArgument("query", mcp.ArgumentDescription("поисковый запрос"), mcp.RequiredArgument()),
	), func(ctx context.Context, req mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		q := strings.TrimSpace(req.Params.Arguments["query"])
		if q == "" {
			return nil, fmt.Errorf("аргумент query обязателен")
		}
		text := fmt.Sprintf(`Сравнение режимов поиска по запросу: %s

1. route с query=%q - посмотреть, какой режим выберет роутер и почему.
2. search с query=%q и mode=fast - быстрый clearnet-поиск.
3. search с query=%q и mode=deep - поиск с подключением onion-движков.
4. search с query=%q и mode=stealth - режим против антибота.

Сравни не только результаты, но и отчёты по движкам: какой движок дал сколько, где была ошибка, где сработал кэш. Режим fast хорош для фактов, deep - когда нужен Tor, stealth - когда обычные движки отдают challenge.`, q, q, q, q, q)
		return promptResult("compare", text), nil
	})

	s.AddPrompt(mcp.NewPrompt("extract",
		mcp.WithPromptDescription("Извлечь структурированные данные с неизвестного сайта через самосборный парсер"),
		mcp.WithArgument("url", mcp.ArgumentDescription("URL страницы"), mcp.RequiredArgument()),
		mcp.WithArgument("fields", mcp.ArgumentDescription("поля через запятую: title,price,email")),
	), func(ctx context.Context, req mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		rawURL := strings.TrimSpace(req.Params.Arguments["url"])
		if rawURL == "" {
			return nil, fmt.Errorf("аргумент url обязателен")
		}
		fields := strings.TrimSpace(req.Params.Arguments["fields"])
		if fields == "" {
			fields = "title"
		}
		text := fmt.Sprintf(`Извлечение данных со страницы: %s (поля: %s)

1. parse с url=%q и fields=[%s] - сначала пробуем готовые селекторы хоста.
2. Если healed=true или ошибка "ни одно поле не извлечено" - build_parser с url=%q: получишь pattern и DOM-скелет.
3. По скелету подбери CSS-селекторы под каждое поле и сохрани их save_selector с url_pattern из шага 2 (по одному вызову на поле).
4. Повтори parse: теперь confidence обязан быть 1.0 и healed=false.
5. Если parse снова healed - верстка меняется между запросами: скажи об этом прямо и отдай значения с пометкой стратегий.`, rawURL, fields, rawURL, fields, rawURL)
		return promptResult("extract", text), nil
	})

	s.AddPrompt(mcp.NewPrompt("judge",
		mcp.WithPromptDescription("Оценить выдачу search и научить реранк: оценки уходят в judge_submit"),
		mcp.WithArgument("query", mcp.ArgumentDescription("запрос из search"), mcp.RequiredArgument()),
		mcp.WithArgument("mode", mcp.ArgumentDescription("режим из search (used_mode), по умолчанию fast")),
	), func(ctx context.Context, req mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		q := strings.TrimSpace(req.Params.Arguments["query"])
		if q == "" {
			return nil, fmt.Errorf("аргумент query обязателен")
		}
		mode := strings.TrimSpace(req.Params.Arguments["mode"])
		if mode == "" {
			mode = "fast"
		}
		text := fmt.Sprintf(`Оценка выдачи по запросу: %s (режим %s)

1. search с query=%q и mode=%q, limit=10 - свежая выдача (no_cache=false, чтобы судьи разных дней сравнивали одно и то же).
2. Прочитай топ-10 и поставь каждому URL score 0..1: 1 - точно в запрос, 0.5 - рядом, 0 - мусор/накрутка SEO. Пустые заголовки и дорвеи - 0 без жалости.
3. judge_submit с query=%q и votes=[{url, score}...] - оценки уходят в базу, хосты с высокими оценками всплывают в будущих выдачах после ночного пересчёта.
4. Не суди то, что не открывал хотя бы сниппетом: оценка вслепую хуже её отсутствия.`, q, mode, q, mode, q)
		return promptResult("judge", text), nil
	})
	s.AddPrompt(mcp.NewPrompt("watch",
		mcp.WithPromptDescription("Мониторинг запроса: завести охоту и проверить находки"),
		mcp.WithArgument("query", mcp.ArgumentDescription("запрос мониторинга"), mcp.RequiredArgument()),
		mcp.WithArgument("mode", mcp.ArgumentDescription("режим поиска, по умолчанию auto")),
	), func(ctx context.Context, req mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		q := strings.TrimSpace(req.Params.Arguments["query"])
		if q == "" {
			return nil, fmt.Errorf("аргумент query обязателен")
		}
		mode := strings.TrimSpace(req.Params.Arguments["mode"])
		if mode == "" {
			mode = "auto"
		}
		text := fmt.Sprintf(`Мониторинг запроса: %s (режим %s)

1. hunt_create с query=%q и mode=%q - завести охоту (первый прогон зафиксирует базу, находкой не считается).
2. hunt_run без id - прогнать все с вышедшим расписанием; смотри hits: changed=true значит выдача сменилась, saved - сколько url из неё новые. Находки приходят и push-уведомлением notifications/hunt_update.
3. hunt_watch с timeout_s=120 - ждать находок блокирующим вызовом вместо поллинга hunt_run.
4. hunt_hits с id - история находок охоты: url, запрос и режим на момент находки, дата и times_seen. В отличие от hits текущего прогона переживает перезапуск.
5. hunt_list - проверить, когда каждая охота бегала в последний раз.
6. Новыми считай только URL из hits текущего прогона либо строки hunt_hits, а не всю выдачу search.`, q, mode, q, mode)
		return promptResult("watch", text), nil
	})
	s.AddPrompt(mcp.NewPrompt("health",
		mcp.WithPromptDescription("Проверить состояние системы: движки, пул, каталог"),
	), func(ctx context.Context, req mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		text := `Проверка состояния VoidSearchSwag

1. status - версия, аптайм, транспорт, статистика базы и движков.
2. onion_health с probe=true - живы ли onion-поисковики. Без probe берутся прошлые замеры, это быстро.
3. pool_status - сколько адресов собрано и сколько живо.
4. file_search без аргументов - размер каталога файлов и разбивка по расширениям.

Что считать проблемой: движок с растущей серией провалов (health-пул отключает его после трёх подряд), пул без живых записей, каталог с большим объёмом, но без файлов нужного типа. Отчёт веди по фактам из вывода, а не по догадкам.`
		return promptResult("health", text), nil
	})
}

func promptResult(name, text string) *mcp.GetPromptResult {
	return &mcp.GetPromptResult{
		Description: name,
		Messages: []mcp.PromptMessage{
			{
				Role:    mcp.RoleUser,
				Content: mcp.NewTextContent(text),
			},
		},
	}
}

// promptRegistrar - часть сервера, нужная для регистрации промптов.
// Интерфейс вместо конкретного типа оставляет New читаемым.
type promptRegistrar interface {
	AddPrompt(prompt mcp.Prompt, handler server.PromptHandlerFunc)
}

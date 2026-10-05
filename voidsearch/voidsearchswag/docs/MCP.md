# MCP-инструменты VoidSearchSwag

Транспорт: `stdio` по умолчанию, `streamable-http` через `run --http 127.0.0.1:8800`
(адрес для чужих хостов - только вместе с токеном).
Endpoint: `POST http://host:8800/mcp`, проверка живости: `GET /health`.

## Auth

Без токена сервер поднимется только на петлевом адресе: локальный агент и его
MCP-клиент живут на одной машине, и токен там лишний. Не-петлевой адрес
(`0.0.0.0:8800`, `:8800`, адрес сети) без токена отклоняется при запуске, ещё
до открытия базы. Три выхода: задать токен, привязать адрес к `127.0.0.1` либо
включить `--http-allow-open` (`VOIDSEARCH_HTTP_ALLOW_OPEN=1`) для изолированного
стенда - тогда сервер пишет в лог предупреждение и отдаёт все инструменты любому
хосту сети.

```bash
go run ./cmd/voidsearchswag run --http :8800 --token s3cret
# или
VOIDSEARCH_HTTP_TOKEN=s3cret go run ./cmd/voidsearchswag run --http :8800
```

Клиент шлёт один из заголовков:

```
Authorization: Bearer s3cret
X-API-Token: s3cret
```

Без него — `401 unauthorized`. `/health` токена не требует.

## Инструменты

| Инструмент | Аргументы | Что отдаёт |
|---|---|---|
| `status` | - | версия, аптайм, транспорт, статистика базы и движков |
| `search` | `query`, `mode`, `limit` (max 200), `no_cache`, `validate` (проверить первые 8 адресов живыми запросами, выкинуть мёртвые, до 90 сек) | результаты + решение роутера + отчёт по движкам; результаты без единого токена запроса отсекаются с пометкой в `note` |
| `route` | `query` | режим роутера без поиска |
| `onion_health` | `probe` | живость onion-поисковиков |
| `fetch` | `url`, `max_chars` | страница целиком через антидетект-транспорт |
| `discover_onions` | `depth`, `max_hosts`, `crawl`, `seeds`, `timeout` (сек, 5..600, дефолт 120), `max_addresses` (дефолт 200, потолок 2000), `offset` | разведка + запись в пул; `seeds` задаёт стартовые хосты обхода и применяется вместе с `crawl`; `timeout` обрезает прогон (`timeout_hit` в ответе), адреса в ответе - окно `offset`/`max_addresses` с `addresses_total`, полный список - `pool_status`; при `crawl` файлы пишутся в каталог под `file_task_id` из ответа (`file_search task_id=<она>`), повторные находки считаются `revisited` и остаются под прежней меткой |
| `pool_status` | `status_filter` (live/unknown/dead, пусто = все; прежнее имя status тоже принимается как алиас), `limit` (max 500) | пул: всего/живых, разбивка, записи |
| `probe_pool` | `limit`, `addr`, `timeout_ms` (мс на пробу, дефолт 20000), `delay_ms` (пауза между пробами, дефолт 2000), `concurrency` (дефолт 16) | пробы живости через tor |
| `onion_search` | `query`, `status`, `include_dead`, `limit` | поиск по базе сервисов |
| `file_search` | `query`, `ext`, `verdict`, `risk_only`, `task_id`, `min_size`, `max_size`, `unknown_size`, `limit` | каталог файлов: имя, адрес или страница-источник, метка категории, риск, задача сбора + разбивка по расширениям; `min_size`/`max_size` не показывают файлы неизвестного размера - их отбирает `unknown_size=true` |
| `collect_files` | `hosts`, `limit`, `max_files`, `timeout` (сек, 5..900, дефолт 300) | обход и сбор файлов в каталог; при обрезке по `timeout` ответ несёт `timeout_hit`, собранное до обрезки доезжает до базы, прогресс по `task_id` виден в `file_search` |
| `build_parser` | `url`, `max_chars` | DOM-скелет под сайт: pattern + skel, mapping возвращается через save_selector |
| `save_selector` | `url_pattern`, `field`, `selector`, `strategy` | сохранить селектор поля в реестр |
| `parse` | `url`, `fields` | извлечь поля: точный CSS, при поломке self-healing (fuzzy/regex/structural/generic), healed=true = перегенерируй; warning = база селекторов недоступна, разбор шёл без них |
| `classify_source` | `url` | тип public/private/paid/scam + качество и популярность по правилам |
| `hunt_create` | `query`, `mode`, `schedule_min` | завести мониторинг запроса |
| `hunt_list` | - | список охот: запрос, режим, последний прогон |
| `hunt_run` | `id` (пусто = все due) | прогнать охоту, diff по hash; первый прогон = база, не находка. Находки дублируются push-уведомлением `notifications/hunt_update` |
| `hunt_watch` | `id`, `timeout_s` (дефолт 120, потолок 600), `interval_s` (дефолт 30) | ждать находок блокирующим вызовом вместо поллинга; расписание игнорируется |
| `hunt_hits` | `id` (пусто = все охоты), `limit` (дефолт 100, потолок 1000) | история находок охоты: url, запрос и режим на момент находки, дата и times_seen. Переживает перезапуск, в отличие от hits текущего прогона |
| `promote_engines` | `limit` (дефолт 10, потолок 50) | проверить живые сервисы на поисковую форму, поднять прошедшие в каталог |
| `metrics` | - | счётчики сервера JSON-ом (то же, что `GET /metrics`) |
| `backup_create` | `keep` (дефолт 7, потолок 30), `confirm_prune` | онлайн-снимок базы с ротацией; когда ротация стирает больше одного снимка, без `confirm_prune=true` возвращает ошибку, а не удаляет |
| `judge_submit` | `query`, `votes: [{url, score 0..1}]`, `author` | оценки судьи: учат бонусы хостов по этому же запросу (ночной пересчёт) |
| `peer_list` | - | живость пиров кластера |
| `peer_sync` | `limit` (дефолт 500, потолок 2000) | синк пула и охот с пиров с дедупом |
| `stats` | - | сводка: пул, файлы, задачи, охоты, селекторы, движки |

## HTTP: auth, TLS, метрики

```bash
voidsearchswag run --http 127.0.0.1:8800 --token-file /run/secrets/vss-token --tls-cert /etc/vss/cert.pem --tls-key /etc/vss/key.pem
```

- Токен: `--token` (светится в `ps`), `VOIDSEARCH_HTTP_TOKEN`, `--token-file` / `VOIDSEARCH_HTTP_TOKEN_FILE` (лучше всего). Заголовки `Authorization: Bearer` или `X-API-Token`.
- TLS: `--tls-cert` + `--tls-key` только парой, иначе ошибка конфигурации.
- `GET /health` открыт, `GET /metrics` за Bearer (на сервере без токена открыт).
- Rate limit: `VOIDSEARCH_RATE_LIMIT` (запросов в минуту с одного IP, дефолт 120; `0` - выкл). Сверх квоты - `429 Too Many Requests` с `Retry-After`. `X-Forwarded-For` не доверяем: за реверс-прокси все клиенты делят квоту адреса прокси - ослабляйте лимит или выключайте, если прокси троттлит сам.

## Ресурсы

То же состояние чтением без инструментов (`resources/list` + `resources/read`):

| URI | Что отдаёт |
|---|---|
| `voidsearch://pool/status` | пул: всего/живых, разбивка по статусам |
| `voidsearch://hunts/list` | охоты: запрос, режим, последний прогон |
| `voidsearch://stats/summary` | сводка: база, файлы, задачи, охоты, селекторы, движки |

## Уведомления

Находки охоты (фоновые и по `hunt_run`) дублируются push-уведомлением
`notifications/hunt_update` с полями `hits` и `count`. Best-effort: без живой
сессии отправка молча пропускается, находки уже отданы в ответе tool.

Ошибки отдаются как MCP `IsError` с текстом на русском.

## Промпты

`recon(topic,depth)`, `investigate(host)`, `harvest(ext,limit)`,
`compare(query)`, `health()`, `extract(url,fields)`, `watch(query,mode)`,
`judge(query,mode)`: `judge` ведёт по цепочке search → оценка → judge_submit.

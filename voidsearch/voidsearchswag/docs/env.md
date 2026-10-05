# Справочник переменных окружения

Полный реестр `VOIDSEARCH_*` проекта. Инвариант покрытия держит тест
`internal/config/envref_test.go`: переменная, добавленная в код без
строки здесь, роняет сьют; строка, вымытая из кода, роняет его тоже.

Соглашения: пустое значение равно «не задано»; пустой дефолт - функция
выключена или путь вычисляется сам; булевы читают `true`/`false`.

## Общие

| Переменная | Дефолт | Смысл |
|---|---|---|
| `VOIDSEARCH_DATA_DIR` | пусто | Каталог данных (БД и прочее). Пусто - платформенный `~/.voidsearchswag`. |
| `VOIDSEARCH_CONFIG` | пусто | Путь к JSON-конфигу. Пусто - `DATA_DIR/config.json`, если существует. Опечатка в явном пути - фатальная ошибка, не молчание. |
| `VOIDSEARCH_LOG_LEVEL` | `info` | Порог логов: `debug`, `info`, `warn`, `error`. |
| `VOIDSEARCH_VERBOSE` | пусто | Многословный режим CLI. Эквивалент флага `--verbose`. |
| `VOIDSEARCH_REQUEST_TIMEOUT` | `45s` | Таймаут одного сетевого запроса. |
| `VOIDSEARCH_ALLOW_PRIVATE` | `false` | Разрешить запросы к приватным и loopback-целям. По умолчанию SSRF-гвард отклоняет их. |
| `VOIDSEARCH_RESULT_LIMIT` | `20` | Максимум результатов одного поиска. |
| `VOIDSEARCH_CACHE_TTL` | `1h` | Срок жизни кэша ответов. |
| `VOIDSEARCH_HEADLESS` | `false` | Безголовый режим браузера. |
| `VOIDSEARCH_CHROME_PATH` | пусто | Путь к Chrome/Chromium. Пусто - поиск в стандартных местах. |
| `VOIDSEARCH_ONION_ENGINES` | пусто | Список движков только для onion-поиска. Пусто - все доступные. |
| `VOIDSEARCH_SEARXNG_URL` | пусто | Базовый URL SearXNG-инстанса для соответствующего движка. |
| `VOIDSEARCH_PROMOTE_LIMIT` | `10` | Лимит повышения (promote) за цикл. |

## HTTP-сервер

| Переменная | Дефолт | Смысл |
|---|---|---|
| `VOIDSEARCH_HTTP_ADDR` | пусто | Адрес вида `127.0.0.1:3333`. Пусто - сервер MCP не поднимается. |
| `VOIDSEARCH_HTTP_TOKEN` | пусто | Bearer-токен сервера. Открытый адрес без токена - фатальная ошибка. |
| `VOIDSEARCH_HTTP_TOKEN_FILE` | пусто | Файл с токеном: читается при старте, токен не светится в argv. Приоритетнее `HTTP_TOKEN`. |
| `VOIDSEARCH_HTTP_ALLOW_OPEN` | `false` | Разрешить открытый (без токена) сервер на loopback-адресе. На публичном адресе не действует. |
| `VOIDSEARCH_RATE_LIMIT` | `120` | Запросов в минуту с одного IP. `0` - без лимита. За балансировщиком считай лимит на нём: клиентский IP определяется по RemoteAddr, `X-Forwarded-For` не доверяется. |
| `VOIDSEARCH_TLS_CERT` | пусто | Сертификат TLS. С парой `TLS_KEY` включает HTTPS. |
| `VOIDSEARCH_TLS_KEY` | пусто | Приватный ключ TLS. |

## Tor

| Переменная | Дефолт | Смысл |
|---|---|---|
| `VOIDSEARCH_TOR` | `on` | `on`/`off`/`auto`: использовать ли Tor. Пустое значение - как `auto`. |
| `VOIDSEARCH_TOR_BINARY` | пусто | Путь к бинарнику tor. Пусто - свой в `DATA_DIR/vendor/tor`, иначе поиск в системе. |
| `VOIDSEARCH_TOR_DIST` | пусто | Корень каталога бандлов tor для `setup`: официальный dist.torproject.org регулярно отдаёт 503 и не всегда доступен из-за гео-ограничений, переменная позволяет брать бандл со своего зеркала. Пусто - `https://dist.torproject.org/torbrowser/`. |
| `VOIDSEARCH_TOR_DATA_DIR` | пусто | Каталог данных tor. Пусто - `DATA_DIR/tor-data`. |
| `VOIDSEARCH_TOR_OWN_PROCESS` | `true` | Самому запускать и останавливать процесс tor. |
| `VOIDSEARCH_TOR_SOCKS` | пусто | Адрес SOCKS-прокси tor: подключиться к уже запущенному tor вместо поднятия своего. Пусто - свой tor на свободном порту loopback. |
| `VOIDSEARCH_TOR_CONTROL` | пусто | Адрес control-порта tor. Пусто - как SOCKS, порт рядом. |
| `VOIDSEARCH_NEWNYM_INTERVAL` | `11s` | Минимум между сменами цепочки (NEWNYM). |

## Транспорт и пул прокси

| Переменная | Дефолт | Смысл |
|---|---|---|
| `VOIDSEARCH_TRANSPORT` | `tor` | `tor`, `proxy`, `direct` - чем ходить. |
| `VOIDSEARCH_PROXIES` | пусто | Статический список прокси (в Transport=proxy). |
| `VOIDSEARCH_PROXY_PROVIDER` | пусто | Имя провайдера пула (например, `proxyscrape`). |
| `VOIDSEARCH_PROXY_PROVIDER_URL` | пусто | Переопределение endpoint провайдера. По умолчанию - публичный api.proxyscrape.com; тесты подменяют на локальный сервер. |
| `VOIDSEARCH_PROXYSCRAPE_PROTOCOL` | `http` | Протокол прокси в запросе к провайдеру. |
| `VOIDSEARCH_PROXYSCRAPE_COUNTRY` | `all` | Фильтр страны. |
| `VOIDSEARCH_PROXYSCRAPE_ANONYMITY` | `all` | Фильтр анонимности. |
| `VOIDSEARCH_PROXYSCRAPE_SSL` | `all` | Фильтр SSL. |
| `VOIDSEARCH_PROXYSCRAPE_TIMEOUT_MS` | `8000` | Таймаут запроса списка (мс). |
| `VOIDSEARCH_PROXY_POOL_SIZE` | `40` | Целевой размер пула. |
| `VOIDSEARCH_PROXY_MIN_LIVE` | `3` | Минимум живых прокси, ниже которого пул догревается. |
| `VOIDSEARCH_PROXY_FETCH_LIMIT` | `400` | Потолок адресов в одной выборке. |
| `VOIDSEARCH_PROXY_PROBE_TIMEOUT` | `7s` | Таймаут пробы прокси. |
| `VOIDSEARCH_PROXY_PROBE_CONCURRENCY` | `48` | Параллельность проб прокси. |
| `VOIDSEARCH_PROXY_SKIP_VERIFY` | `false` | Не проверять живость прокси при сборе. |
| `VOIDSEARCH_PROXY_STATE_PATH` | пусто | Путь к состоянию пула. Пусто - `DATA_DIR/proxy-state.json`. |

## Discover, пробы, охоты

| Переменная | Дефолт | Смысл |
|---|---|---|
| `VOIDSEARCH_DISCOVER_DEPTH` | `2` | Глубина прохода discover. |
| `VOIDSEARCH_DISCOVER_CONCURRENCY` | `4` | Параллельность discover. |
| `VOIDSEARCH_DISCOVER_DELAY` | `2s` | Пауза между шагами discover. |
| `VOIDSEARCH_DISCOVER_MAX_HOSTS` | `50` | Потолок хостов за проход. |
| `VOIDSEARCH_DISCOVER_BG` | `true` | Фоновый режим discover в сервере. |
| `VOIDSEARCH_DISCOVER_BG_INTERVAL` | `24h` | Период фонового discover. |
| `VOIDSEARCH_DISCOVER_BG_PROBE` | `100` | Сколько целей пробовать за фоновый цикл. |
| `VOIDSEARCH_PROBE_ON_DISCOVER` | `true` | Пробить найденное сразу. |
| `VOIDSEARCH_PROBE_TIMEOUT` | `20s` | Таймаут пробы onion-цели. |
| `VOIDSEARCH_PROBE_CONCURRENCY` | `16` | Параллельность проб onion-целей. |
| `VOIDSEARCH_HUNT_BG` | `true` | Фоновые охоты в сервере. |
| `VOIDSEARCH_HUNT_INTERVAL` | `10m` | Период планировщика охот. |
| `VOIDSEARCH_PEERS` | пусто | Адреса пиров для обмена (`/peer/export`): свои же инстансы через запятую, общий Bearer-токен. |

## Бекапы

| Переменная | Дефолт | Смысл |
|---|---|---|
| `VOIDSEARCH_BACKUP_DIR` | пусто | Каталог бекапов. Пусто - бекапы выключены. |
| `VOIDSEARCH_BACKUP_INTERVAL` | `24h` | Период бекапов. |
| `VOIDSEARCH_BACKUP_KEEP` | `7` | Сколько последних бекапов хранить. |
| `VOIDSEARCH_BACKUP_BG` | `true` | Фоновый бекап в сервере. |

## Зарезервировано

| Переменная | Дефолт | Смысл |
|---|---|---|
| `VOIDSEARCH_OFFLINE_TESTS` | пусто | Читается сетевыми тестами, когда они появятся (см. `docs/testing.md`). Кодом пока не используется. |

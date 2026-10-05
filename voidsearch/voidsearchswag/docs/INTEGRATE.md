# INTEGRATE — подключение VoidSearchSwag к агенту

## Вариант A: stdio (локально, без сети)

`claude_desktop_config.json`:

```json
{
  "mcpServers": {
    "voidsearchswag": {
      "command": "C:/path/to/voidsearchswag.exe",
      "args": ["run"]
    }
  }
}
```

Linux/macOS: путь к бинарю без `.exe`. Tor поднимется сам (~25-30с bootstrap),
для чистого clearnet добавь `"args": ["run", "--no-tor"]`.

## Вариант B: streamable-http (удалённый / docker / второй хост)

Сервер:

```bash
voidsearchswag run --http 0.0.0.0:8800 --token s3cret
```

Не-петлевой адрес требует токен: без него запуск отклоняется до открытия базы
(ошибка `открытый адрес без токена` с подсказкой). Локальному клиенту хватает
`--http 127.0.0.1:8800` без токена, а для изолированного стенда без токена есть
явный переключатель `--http-allow-open` (`VOIDSEARCH_HTTP_ALLOW_OPEN=1`): он
пишет в лог предупреждение и открывает все инструменты любому хосту сети.

Клиент MCP (streamable-http):

```
url: http://host:8800/mcp
headers:
  Authorization: Bearer s3cret
```

Проверка без MCP-клиента:

```bash
curl -H "Authorization: Bearer s3cret" http://host:8800/health
# ok
```

`/health` отвечает и без токена: заголовок `Authorization` в примере лишний,
он оставлен только чтобы показать форму заголовка для клиента MCP.

### Маршруты HTTP-сервера

| Путь | Что отдаёт | Токен |
| --- | --- | --- |
| `/mcp` | протокол MCP (POST JSON-RPC, GET - SSE-поток, DELETE - завершение сессии) | нужен, если задан |
| `/` | то же, что `/mcp`: точный корень оставлен для клиентов с url без пути | нужен, если задан |
| `/health`, `/healthz` | `ok`, проверка живости | нет |
| `/metrics` | JSON-снимок счётчиков | нужен, если задан |
| `/peer/export` | выгрузка пула и охот для синка кластера | нужен всегда |
| всё остальное | `404 нет такого маршрута` | - |

MCP-обработчик зарегистрирован только на своих путях, а не на всём дереве,
поэтому реверс-прокси может раздавать соседние пути другому приложению, а
сканер не получает на каждый мусорный запрос висящее SSE-соединение.

## Импорт как Go-пакет

```go
import (
    "voidsearchswag/internal/search"
    "voidsearchswag/internal/searchers"
    "voidsearchswag/internal/store"
)

st, _ := store.Open("./voidsearchswag.db")
engines := searchers.DefaultOnionEngines() // или EnginesFromSeeds
engine, _, _ := search.Build(search.DeepConfig{
    Transport: "direct",
    OnionEngines: engines,
})
defer engine.Close()
```

Модуль `voidsearchswag`, Go 1.25. Зависимости rod/stealth уже прямые —
браузерная эскалация собирается без `go get`.

## Окружение для интеграции

```bash
VOIDSEARCH_DATA_DIR=/data/vss
VOIDSEARCH_TOR=off            # вырубить tor в CI/тестах
VOIDSEARCH_TRANSPORT=direct
VOIDSEARCH_HTTP_TOKEN=s3cret
VOIDSEARCH_TOR_DIST=https://mirror.example/torbrowser/
VOIDSEARCH_HEADLESS=true          # cloaked-браузер в stealth-режиме (нужен chromium)
VOIDSEARCH_CHROME_PATH=/usr/bin/chromium
VOIDSEARCH_PROXY_PROVIDER=proxyscrape
```

`setup --offline` — только миграции базы, без скачивания tor.
`setup --skip-check` — без bootstrap-проверки (быстро в CI).

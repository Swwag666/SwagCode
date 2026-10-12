# SwagCod Phone (F-9)

Телефонный клиент SwagCod — это отдельный проект `dsh-phone` (PWA + python-гейтвей
на stdlib), который живёт **вне этого репозитория**: `C:\Users\atiun\Desktop\qwe\dsh-phone`.
Он умеет два бэкенда: родной мост DSH (JSON-RPC по TCP) и loopback HTTP API
SwagCod. Здесь лежат артефакты стороны SwagCod и каноническая копия адаптера,
чтобы работа не потерялась вместе с папкой телефона.

| Файл | Что это |
|---|---|
| `swagcod_backend.py` | копия адаптера бэкенда (канонически — `dsh-phone/swagcod_backend.py`) |
| `test_swagcod_backend.py` | копия его тестов: фейковый SwagCod API на stdlib + 34 проверки |
| `phone-gateway-check.py` | ручная проверка всей цепочки: гейтвей → HTTP API → ход → стрим |
| `../phone-check.ps1` | ручная проверка самой стороны SwagCod: 15 проверок HTTP API |

## Как это устроено

Гейтвей телефона знает один интерфейс бэкенда (`.connected`, `.on_notify`,
`connect()`, `request(method, params, timeout)`, `_close()`). Адаптер реализует
его поверх HTTP API SwagCod и делает две работы: переводит RPC-методы телефона
в маршруты API и переводит события шины (SSE `/v1/events`) в события гейтвея
(`items`, `state`, `turnEnded`, `sessions`), чтобы телефон получал живой стрим,
а не опрос раз в `pollSeconds`.

| Метод телефона | Маршрут SwagCod |
|---|---|
| `session.list` | `GET /v1/sessions` |
| `session.getState` | `GET /v1/sessions` (кэш 0.75 с) + локальный статус |
| `session.getSnapshot` | `POST /v1/sessions/transcript` |
| `session.getNotices` | локально: из `approval_required` в SSE |
| `session.startTurn` | `POST /v1/turns` |
| `session.createAndStart` | `POST /v1/sessions` + `POST /v1/turns` |
| `session.interrupt` | `POST /v1/turns/stop` |
| `session.respondInteraction` | `POST /v1/approvals` |
| `session.updateSelections` | модель — локально (уедет следующим ходом); политика подтверждений — отказ |
| `catalog.listModels` | `GET /v1/models` (фолбэк — модели из списка сессий) |
| `catalog.listPermissions` | локально: три значения `ApprovalPolicy` |
| `workspace.list` | локально: `cwd` известных сессий |
| живой стрим | `GET /v1/events` (SSE) |

Ключевое соглашение — id элементов ленты. PWA хранит элементы по `item.id` и
перерисовывает только при изменении `revision`, поэтому id обязаны совпадать у
истории (транскрипт) и у живого потока (SSE), иначе каждое сообщение
продублировалось бы после обновления. Id выводятся из id самого SwagCod:
`sw-<turn>-user`, `sw-<turn>-reasoning`, `sw-<turn>-content`, `sw-<call_id>`,
`sw-<turn>-end`. Тест `test_snapshot_and_stream_agree_on_ids` это фиксирует.

## Подключение

1. Включи HTTP API в SwagCod: prefs-ключ `http_api_port` = `47479` (или
   `SWAGCOD_HTTP_PORT=47479`) и перезапусти приложение. Слушает только
   `127.0.0.1`.
2. Скопируй токен: SwagCod → Настройки → Безопасность → «Копировать токен».
3. В `dsh-phone/config.json` выставь:

   ```json
   {
     "backend": "swagcod",
     "swagcodBaseUrl": "http://127.0.0.1:47479",
     "swagcodToken": "<hex-токен из шага 2>",
     "swagcodSse": true
   }
   ```

   `config.json` после этого содержит секрет — в репозиторий его нельзя.
4. Запусти гейтвей (`python gateway.py` или `run.bat`) и открой на телефоне
   `http://<адрес машины в Tailscale>:8460/`; токен гейтвея (`gatewayToken`)
   вводится в самом клиенте или приходит QR-ом.

Если адаптер правился здесь, скопируй его обратно:

```powershell
Copy-Item tools\phone\swagcod_backend.py C:\Users\atiun\Desktop\qwe\dsh-phone\ -Force
```

## Проверка

```powershell
# сторона SwagCod: 15 проверок HTTP API (нужен токен, приложение запущено)
powershell -NoProfile -ExecutionPolicy Bypass -File tools\phone-check.ps1 -Token <hex>

# то же + настоящий ход и стрим (тратит токены провайдера)
powershell -NoProfile -ExecutionPolicy Bypass -File tools\phone-check.ps1 -Token <hex> -Turn

# вся цепочка через гейтвей: 17 проверок, read-only
python tools\phone\phone-gateway-check.py --config C:\Users\atiun\Desktop\qwe\dsh-phone\config.json

# + «новый чат» с телефона: создание сессии, ход, стрим, история (24 проверки)
python tools\phone\phone-gateway-check.py --config <путь>\config.json --create <пустая папка> --model <id модели>
```

Оба скрипта печатают таблицу «ожидание / факт» и возвращают код 0 только если
провалов нет. Тесты адаптера: `python tools\phone\test_swagcod_backend.py`
(34 теста, фейковый API поднимается сам, сеть и десктоп не нужны).

## Чего телефон не умеет против SwagCod

Честно, а не «почти всё»: вложения (API принимает только текст), смена политики
подтверждений (это настройка десктопа, и API намеренно не даёт её менять
удалённо — телефон не должен расширять себе права), откат хода к git-снимку
(маршрут `/v1/turns/revert` есть, UI в телефоне нет), черновики ввода живут в
гейтвее, а не в SwagCod. `runtime.getCapabilities` сообщает это клиенту.

# BACKEND_PHASE2.md — SwagCod, фаза 2 (rev38+): зрелость агента

Продолжение `BACKEND_EXTENSIONS.md` (фаза 1, E-1…E-9, закрыта в rev35).
Запрос владельца: палитра команд уровня DSH-референса + обязательная
тройка (git-чекпоинты, diff-review, веб-поиск) + память, hooks, phone,
tasks_cancel, allow-list, гигиена фулл, тесты и CI всё.

## Конвенции (те же, что в фазе 1)

Каждая стадия = атомарный коммит: код + тесты стадии. Ворота перед
коммитом: `cargo test --workspace`, `cargo clippy --workspace
--all-targets -- -D warnings`, `cargo fmt --check`, `svelte-check`
0 ошибок, `vitest` зелёный, JS-бандл < 356 000 Б. После коммита:
деплой портативного exe + `tools/bench-startup.ps1` (старт < 400 мс,
our private < 40 МБ — D-013), запись в `DECISIONS.md` (ревизия =
стадия), push в Swwag666/SwagCode. Стадии идут по порядку: поздние
могут опираться на ранние, ранние не знают о поздних.

| Стадия | Ревизия | Содержание | Статус |
|---|---|---|---|
| F-1 | 38 | tasks_cancel: кнопка отмены в телеметрии | готово |
| F-2 | 39 | палитра команд v2 (slash-команды с описаниями) | |
| F-3 | 40 | allow/deny-list команд в подтверждениях | |
| F-4 | 41 | инструмент web_search | |
| F-5 | 42 | git-чекпоинты ходов + откат | |
| F-6 | 43 | diff-review для edit/write в подтверждениях | |
| F-7 | 44 | персистентная память (таблица + инструменты + UI) | |
| F-8 | 45 | hooks (pre/post turn, pre_tool) | |
| F-9 | 46 | SwagCod Phone: подключить qwe/dsh-phone к HTTP API | |
| F-10 | 47-48 | гигиена фулл: компоненты App.svelte, модули lib.rs, FTS5 | |
| F-11 | 49 | тесты и CI всё: E2E-smoke, компонентные тесты | |

## F-1 · tasks_cancel UI (rev38)

Бэкенд УЖЕ был в E-5 (`task_cancel`: отменяет только queued,
running честно отклоняет — обработчик владеет процессом). Дырка была
в UI: список задач показывался, отменить нельзя. Строка таблицы
задач (Настройки → Телеметрия) получает кнопку «отменить» для
queued-строк; ошибка команды — во flash-статус. Приёмка: queued →
cancelled в таблице; running → текст ошибки, состояние не меняется.

## F-2 · Палитра команд v2 (rev39)

Референс — палитра DSH (скриншот владельца): compact / export /
feedback / goal / permission / plan / model, у каждой описание.
Наша палитра (Ctrl+P) существует, но бедная. Апгрейд: slash-команды
с описаниями и группами, fuzzy-поиск, подсветка совпадения,
клавиатура (стрелки/Enter/Esc уже есть):

- `/compact` — принудительная компакция истории (B-3);
- `/export` — сохранить журнал сессии (save-диалог);
- `/permission` — цикл политики подтверждений;
- `/model` — открыть поповер модели;
- `/memory` — вкладка памяти (F-7);
- `/tasks` — телеметрия задач;
- `/search <q>` — FTS5-поиск по истории (F-10);
- `/theme` — цикл тем;
- `/update` — проверка обновлений (rev37).

Приёмка: палитра по `/` в пустом вводе и по Ctrl+P; команда
исполняется; описания локализованы ru/en.

## F-3 · Allow/deny-list команд (rev40)

Движок подтверждений получает два regex-списка из prefs:
`cmd_allow` (авто-одобрение без диалога) и `cmd_deny` (всегда
отклонение, инструмент возвращает ошибку). Порядок: deny > allow >
политика. Дефолты allow: `git (status|log|diff|branch)`, `ls`, `dir`,
`cargo (test|clippy|fmt)`, `node --version`… Дефолты deny:
`rm -rf /`, `format `, `del /s c:\`, `shutdown`, pipe в powershell…
UI: Настройки → Безопасность — редактируемые списки (textarea по
строке-regex). Тесты: юнит движка (core), компонент списка.

## F-4 · Инструмент web_search (rev41)

Инструмент `web_search {query, max_results?}`: бэкенд — env
`SWAGCOD_SEARCH_URL` (SearXNG с `format=json`) либо дефолт
DuckDuckGo html без ключа; парсинг: title/url/snippet, кап 8,
таймаут 10 с; SSRF-ограничения как у fetch_url. Результат — tool
output в траектории. Тесты: mock HTTP-сервер (паттерн есть в
provider), парсер на фикстурах.

## F-5 · Git-чекпоинты ходов (rev42)

Перед ходом: если cwd — git-репа и worktree грязный →
`git stash create` (снимок НЕ трогает refs/index/worktree) → sha в
`turns.checkpoint_sha` (миграция). Команда `turn_revert(turn_id)`:
восстановление worktree из снимка через
`git restore --source=<sha> --worktree -- .` (HEAD/index целы).
UI: кнопка «откатить» на строке хода в траектории + диалог
подтверждения. Тесты: core-функции на temp-репе; UI-смоук.

## F-6 · Diff-review для edit/write (rev43)

Payload подтверждения для edit/write получает `preview`: unified
diff old→new (diff-крейт fsx, обрезка > 64 КБ). Диалог подтверждения
рисует diff с подсветкой +/- (моно), кнопки принять/отклонить.
Авто-политика применяет без диалога (политика выше удобства).
Тесты: генерация preview, компонент диалога.

## F-7 · Персистентная память (rev44)

Стор: таблица `memory` (id, scope global|session, text, created_ms,
session_id) + миграция. Инструменты `memory_add` / `memory_list` /
`memory_forget`. Контекст: глобальная память вшивается в системный
промпт (кап 4 КБ). UI: Настройки → «Память» (список, добавить,
удалить). Тесты: roundtrip стора, вшивка в контекст, e2e через
fake-провайдер.

## F-8 · Hooks (rev45)

prefs.hooks: `{pre_turn: [cmd], post_turn: [cmd],
pre_tool: [{match: regex, cmd}]}`. Исполнение через silent_cmd
(CREATE_NO_WINDOW), env: SWAGCOD_SESSION / SWAGCOD_EVENT /
SWAGCOD_TOOL / SWAGCOD_CWD; таймаут 15 с; pre_tool с exit != 0 →
инструмент отклонён со stderr хука в ошибке; метрика hook_ms +
запись в журнал. UI: Настройки → Безопасность — редактор hooks
(JSON). Тесты: юнит движка, e2e pre_tool-отклонение.

## F-9 · SwagCod Phone (rev46)

`C:\Users\atiun\Desktop\qwe\dsh-phone` (web PWA + гейтвей)
подключается к HTTP API SwagCod (E-7): конфиг гейтвея на
localhost:47479 + токен из prefs (DPAPI), сверка имён SSE-событий и
эндпоинтов сессий/ходов; найденные нехватки API чиним минимально.
Приёмка: phone web открывает сессии, шлёт ход, принимает стрим.
Артефакт: конфиг + секция README + скрипт ручной проверки.

## F-10 · Гигиена фулл (rev47-48)

App.svelte (~7330 строк) разбирается на компоненты:
`components/SettingsTabs.svelte` (general/security/telemetry/
providers/memory), `components/ChatPane.svelte`,
`components/Sidebar.svelte`, `components/Palette.svelte`,
`components/ApprovalDialog.svelte`; App.svelte остаётся
оркестратором состояния. lib.rs (~5330) — модуль `turn_driver.rs`
(start_turn_core + стрим-цикл + хуки). FTS5: виртуальная таблица
`messages_fts` + команда `search_messages` + `/search` в палитре +
UI результатов. Ворота и бандл — те же.

## F-11 · Тесты и CI всё (rev49)

CI job `e2e-smoke`: релизная сборка, запуск с env-флагом
встроенного echo-провайдера (паттерн fake-mcp), прогон хода через
HTTP API: создать сессию → ход → ассерт SSE-событий и токенов.
Компонентные тесты (testing-library уже в зависимостях): Palette,
ApprovalDialog (diff), MemoryTab, TasksTable (кнопка отмены).
Coverage-отчёт артефактом CI (не ворота). README + DECISIONS.

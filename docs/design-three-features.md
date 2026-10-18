# Дизайн-документ: Hot-reload, Folder-scoped rules, Уведомления

Дата: 2025-07-18
Статус: Draft
Автор: Abyss

---

## Фича 1: Hot-reload MCP серверов, плагинов и скиллов

### 1.1 Архитектура

**Затронутые крейты:**
- `crates/app` - новые модули `hot_reload.rs`, изменения в `mcp.rs`, `jsplugins.rs`, `lib.rs` (AppState)
- `crates/core` - новый модуль `skills.rs` (если скиллы ещё не выделены), расширение `bus::EventKind`
- `sidecar/plugin-server.js` - поддержка инкрементальной перезагрузки по имени файла

**Новые модули:**
- `crates/app/src/hot_reload.rs` - единый координатор файловых вотчеров и reload-логики
- `crates/core/src/skills.rs` - домен скиллов (загрузка шаблонов, интерполяция переменных)

**Изменения schema:**
- Таблица `prefs`: новые ключи `mcp_servers` (уже существует), `skills_dir`, `hot_reload_enabled`
- Никаких миграций БД: конфигурация хранится в prefs как JSON-строки

**Ключевое архитектурное решение:** файловый вотчер (`notify` crate) работает в отдельной tokio-задаче, дебаунсит события (300 мс) и отправляет reload-команды через канал. Reload никогда не блокирует активные ходы: старые соединения/инструменты живут до завершения текущего turn, новые подхватываются на следующем.

### 1.2 Структуры данных

```rust
// crates/app/src/hot_reload.rs

use notify::{RecommendedWatcher, RecursiveMode, Watcher};
use std::path::PathBuf;
use tokio::sync::mpsc;

/// Событие от файлового вотчера после дебаунса.
#[derive(Debug, Clone)]
pub enum HotReloadEvent {
    /// Конфиг MCP-серверов изменился (prefs или внешний файл).
    McpConfigChanged,
    /// JS-плагин добавлен/изменён/удалён.
    PluginChanged { path: PathBuf },
    /// Шаблон скилла изменён.
    SkillChanged { name: String, path: PathBuf },
}

/// Координатор hot-reload: держит вотчеры и рассылает события.
pub struct HotReloadCoordinator {
    watcher: RecommendedWatcher,
    tx: mpsc::Sender<HotReloadEvent>,
    rx: mpsc::Receiver<HotReloadEvent>,
    /// Debounce-таймеры по путям: несколько событий за 300 мс схлопываются.
    pending: std::collections::HashMap<PathBuf, tokio::time::Instant>,
}

/// Результат reload для UI: что обновилось, какие ошибки.
#[derive(Debug, Clone, serde::Serialize)]
pub struct ReloadReport {
    pub mcp_reconnected: Vec<String>,
    pub mcp_errors: Vec<(String, String)>,
    pub plugins_reloaded: usize,
    pub plugin_errors: Vec<String>,
    pub skills_updated: Vec<String>,
    pub ts_ms: u64,
}
```

```rust
// crates/core/src/skills.rs (новый модуль)

use std::collections::HashMap;
use std::path::{Path, PathBuf};

/// Скилл = именованный промпт-шаблон с переменными.
#[derive(Debug, Clone, serde::Serialize, serde::Deserialize)]
pub struct Skill {
    pub name: String,
    pub description: String,
    /// Шаблон с плейсхолдерами {{variable}}.
    pub template: String,
    /// Переменные по умолчанию.
    pub defaults: HashMap<String, String>,
    /// Путь файла-источника (для hot-reload).
    #[serde(skip)]
    pub source_path: Option<PathBuf>,
}

/// Реестр скиллов: загрузка из директории, интерполяция, горячее обновление.
pub struct SkillRegistry {
    skills: HashMap<String, Skill>,
    dir: PathBuf,
}

impl SkillRegistry {
    /// Загрузить все *.md / *.json из директории скиллов.
    pub fn load(dir: &Path) -> Result<Self, String>;
    
    /// Перезагрузить один скилл по пути (hot-reload).
    pub fn reload_file(&mut self, path: &Path) -> Result<Option<String>, String>;
    
    /// Интерполировать шаблон с заданными переменными.
    pub fn render(&self, name: &str, vars: &HashMap<String, String>) -> Result<String, String>;
    
    /// Список доступных скиллов для модели/UI.
    pub fn list(&self) -> Vec<&Skill>;
}
```

```typescript
// apps/desktop/src/lib/types.ts (дополнения)

interface ReloadReport {
  mcp_reconnected: string[];
  mcp_errors: [string, string][];
  plugins_reloaded: number;
  plugin_errors: string[];
  skills_updated: string[];
  ts_ms: number;
}

// Новое событие шины
interface HotReloadedEvent {
  kind: 'hot_reloaded';
  data: ReloadReport;
}
```

### 1.3 Алгоритм работы

**MCP hot-reload:**
1. Вотчер следит за источником конфига (prefs-ключ `mcp_servers` меняется через HTTP API / Tauri IPC → событие генерируется программно, не через FS)
2. При изменении конфига: diff старого и нового списка серверов
3. Удалённые серверы → `McpRegistry::disconnect()`
4. Добавленные/изменённые → `McpRegistry::connect_server()` с новым конфигом
5. Неизменённые серверы не трогаются
6. Активные turn'ы продолжают работать со старыми Arc<Conn> (clone в начале хода); новые turn'ы получают обновлённый реестр
7. По завершении: publish `EventKind::Status { message: "MCP reloaded: ..." }`

**JS Plugins hot-reload:**
1. Вотчер следит за `plugins_dir()` (рекурсивно, маска `*.js`)
2. Дебаунс 300 мс: несколько сохранений за короткое время → один reload
3. Вызов `JsHost::reload()` (уже существует!) - sidecar перечитывает каталог
4. Diff инструментов: удалённые убираются из `js_tools`, новые добавляются
5. Активные вызовы в sidecar не прерываются (vm-контекст изолирован)
6. Publish `EventKind::Status`

**Skills hot-reload:**
1. Вотчер следит за `skills_dir` (по умолчанию `~/.swagcod/skills/`, настраивается)
2. При изменении `*.md` / `*.json` → `SkillRegistry::reload_file(path)`
3. Парсинг: frontmatter (YAML/TOML) + тело шаблона
4. Ошибка парсинга → Status-событие с ошибкой, старый скилл остаётся
5. Успех → замена в HashMap, publish Status

**Общий цикл координатора:**
```
spawn(hot_reload_loop):
  loop:
    select:
      event = rx.recv() =>
        match event:
          McpConfigChanged => reload_mcp(registry, configs).await
          PluginChanged => js_host.reload().await
          SkillChanged => skill_registry.reload_file(path)
        publish ReloadReport via Bus
      _ = shutdown_signal => break
```

### 1.4 Тестовая стратегия

| Уровень | Что тестируем | Как |
|---------|--------------|-----|
| Unit | `SkillRegistry::load/reload_file/render` | Tempdir с .md файлами, изменение содержимого, проверка парсинга |
| Unit | Дебаунс-логика координатора | Mock-события, проверка что N событий за 300 мс → 1 reload |
| Integration | MCP reconnect при смене конфига | Фейковый MCP-сервер (уже есть `FAKE_SERVER_JS`), смена args → новый connect |
| Integration | JS plugin reload | Уже покрыто тестом `js_host_loads_calls_prefs_and_enforces_deadline` (строка 376-379) |
| E2E | Полный цикл: изменить файл → увидеть Status в bus | Test app с подписчиком шины |

### 1.5 Оценка сложности: **L**

- MCP reconnect: ~2 дня (diff + graceful swap + тесты)
- JS plugin reload: ~0.5 дня (метод `reload()` уже есть, нужен только вотчер + дебаунс)
- Skills registry: ~3 дня (новый модуль, парсинг, интерполяция, вотчер)
- Координатор + дебаунс: ~1 день
- UI для статуса reload: ~1 день
- Тесты: ~2 дня

**Итого: ~9.5 рабочих дней**

### 1.6 Зависимости от существующего кода

- `McpRegistry::connect_server/disconnect` - используется напрямую, без изменений
- `JsHost::reload()` - уже реализован, используется как есть
- `Bus::publish(EventKind::Status)` - для уведомлений о reload
- `AppState::mcp`, `AppState::js_host`, `AppState::js_tools` - доступ через Mutex/Arc
- `swagcod_core::node_store::sidecar_file()` - путь к plugin-server.js
- Новый зависимость: `notify` crate (file watcher, cross-platform)

---

## Фича 2: Folder-scoped rules + auto-cleanup

### 2.1 Архитектура

**Затронутые крейты:**
- `crates/core` - расширение `rules.rs`, новый модуль `folder_rules.rs`, расширение `store.rs` (TTL/cleanup)
- `crates/app` - интеграция в `turn_driver.rs` (применение правил при старте хода), `lib.rs` (AppState)
- `apps/desktop` - UI для просмотра/редактирования folder rules, настройки TTL

**Новые модули:**
- `crates/core/src/folder_rules.rs` - загрузка, наследование, merge правил по дереву папок

**Изменения schema SQLite:**
```sql
-- Новая таблица для folder-scoped правил
CREATE TABLE IF NOT EXISTS folder_rules (
  path TEXT PRIMARY KEY,           -- нормализованный абсолютный путь
  rules_json TEXT NOT NULL,        -- JSON: FolderRulesConfig
  updated_ms INTEGER NOT NULL
);

-- Расширение таблицы sessions: привязка к рабочей папке + метка исключения
ALTER TABLE sessions ADD COLUMN work_dir TEXT;
ALTER TABLE sessions ADD COLUMN exempt_from_cleanup INTEGER DEFAULT 0;

-- Индекс для TTL-запросов
CREATE INDEX IF NOT EXISTS idx_sessions_activity ON sessions(last_activity_ms);
CREATE INDEX IF NOT EXISTS idx_sessions_work_dir ON sessions(work_dir);
```

### 2.2 Структуры данных

```rust
// crates/core/src/folder_rules.rs

use std::collections::HashMap;
use std::path::{Path, PathBuf};

/// Конфигурация правил для одной папки (.swagcod/rules.json или rules.md).
#[derive(Debug, Clone, Default, serde::Serialize, serde::Deserialize)]
pub struct FolderRulesConfig {
    /// Дополнительные allow-паттерны команд (к глобальным).
    #[serde(default)]
    pub allow: Vec<String>,
    
    /// Дополнительные deny-паттерны команд.
    #[serde(default)]
    pub deny: Vec<String>,
    
    /// Отключённые MCP-серверы (по имени).
    #[serde(default)]
    pub disabled_mcp: Vec<String>,
    
    /// Отключённые JS-плагины (по short-имени).
    #[serde(default)]
    pub disabled_plugins: Vec<String>,
    
    /// Отключённые скиллы (по имени).
    #[serde(default)]
    pub disabled_skills: Vec<String>,
    
    /// Override системного промпта (добавляется, не заменяет).
    #[serde(default)]
    pub system_prompt_append: Option<String>,
    
    /// TTL автоудаления сессий в этой папке (секунды). None = глобальный TTL.
    #[serde(default)]
    pub session_ttl_secs: Option<u64>,
    
    /// Исключить эту папку и все вложенные из автоудаления.
    #[serde(default)]
    pub exempt_from_cleanup: bool,
}

/// Разрешённые правила после merge по дереву папок.
#[derive(Debug, Clone)]
pub struct ResolvedFolderRules {
    pub allow: Vec<String>,
    pub deny: Vec<String>,
    pub disabled_mcp: Vec<String>,
    pub disabled_plugins: Vec<String>,
    pub disabled_skills: Vec<String>,
    pub system_prompt_append: String,
    pub session_ttl_secs: Option<u64>,
    pub exempt_from_cleanup: bool,
    /// Цепочка папок, откуда пришли правила (для отладки).
    pub sources: Vec<PathBuf>,
}

/// Кэш загруженных folder rules: путь → конфиг.
/// Инвалидируется при hot-reload (Фича 1).
pub struct FolderRulesCache {
    cache: HashMap<PathBuf, FolderRulesConfig>,
}

impl FolderRulesCache {
    /// Загрузить правила для папки (читает .swagcod/rules.json или rules.md).
    pub fn load_for_dir(dir: &Path) -> Option<FolderRulesConfig>;
    
    /// Resolve: пройти от work_dir вверх до корня, собрать и смержить правила.
    /// Ближние правила имеют приоритет (deny/allow объединяются, disabled_* объединяются).
    pub fn resolve(&mut self, work_dir: &Path) -> ResolvedFolderRules;
    
    /// Инвалидировать кэш для пути (при hot-reload).
    pub fn invalidate(&mut self, path: &Path);
}
```

```rust
// crates/core/src/store.rs (расширение трейта Store)

/// Параметры автоудаления.
#[derive(Debug, Clone, serde::Serialize, serde::Deserialize)]
pub struct CleanupConfig {
    /// Глобальный TTL сессий в секундах. 0 = выключено.
    pub session_ttl_secs: u64,
    /// Удалять ли пустые рабочие папки после удаления всех сессий.
    pub cleanup_empty_dirs: bool,
    /// Паттерны исключений (glob).
    pub exempt_patterns: Vec<String>,
}

// Новые методы в trait Store:
// fn save_folder_rules(&self, path: &str, config: &FolderRulesConfig) -> StoreResult<()>;
// fn load_folder_rules(&self, path: &str) -> StoreResult<Option<FolderRulesConfig>>;
// fn list_folder_rules(&self) -> StoreResult<Vec<(String, FolderRulesConfig)>>;
// fn delete_folder_rules(&self, path: &str) -> StoreResult<()>;
// fn set_session_work_dir(&self, id: &str, work_dir: &str) -> StoreResult<()>;
// fn set_session_exempt(&self, id: &str, exempt: bool) -> StoreResult<()>;
// fn sessions_for_cleanup(&self, ttl_secs: u64, now_ms: u64) -> StoreResult<Vec<String>>;
// fn get_cleanup_config(&self) -> StoreResult<CleanupConfig>;
// fn set_cleanup_config(&self, cfg: &CleanupConfig) -> StoreResult<()>;
```

```typescript
// apps/desktop/src/lib/types.ts

interface FolderRulesConfig {
  allow: string[];
  deny: string[];
  disabled_mcp: string[];
  disabled_plugins: string[];
  disabled_skills: string[];
  system_prompt_append?: string;
  session_ttl_secs?: number;
  exempt_from_cleanup: boolean;
}

interface CleanupConfig {
  session_ttl_secs: number;
  cleanup_empty_dirs: boolean;
  exempt_patterns: string[];
}
```

### 2.3 Алгоритм работы

**Загрузка и применение folder rules:**
1. При создании сессии: определить `work_dir` (cwd или выбранный в UI)
2. Сохранить `work_dir` в сессию (`set_session_work_dir`)
3. Перед каждым ходом: `FolderRulesCache::resolve(work_dir)`
4. Merge: обход от work_dir вверх до filesystem root
   - На каждом уровне: проверить `.swagcod/rules.json` и `.swagcod/rules.md`
   - JSON приоритетнее MD (если оба есть)
   - Deny-списки: объединение (union)
   - Allow-списки: объединение (union)
   - Disabled-*: объединение (union)
   - `system_prompt_append`: конкатенация (от корня к листу)
   - `session_ttl_secs`: берётся ближайший непустой
   - `exempt_from_cleanup`: true если ANY уровень = true
5. Результирующие `ResolvedFolderRules` передаются в `TurnConfig`
6. В `turn_driver`: deny/allow фильтруют инструменты, disabled_mcp/plugins/skills исключаются из списка доступных

**Auto-cleanup:**
1. Фоновая задача (tokio::spawn) с интервалом 1 час
2. Запрос `sessions_for_cleanup(ttl, now_ms)`:
   ```sql
   SELECT id FROM sessions 
   WHERE last_activity_ms < ? AND exempt_from_cleanup = 0
   ```
3. Для каждой сессии: проверить glob-исключения из CleanupConfig
4. Удалить сессию (`delete_session`)
5. Если `cleanup_empty_dirs`: проверить, остались ли сессии в work_dir; если нет - удалить `.swagcod/` и саму папку (только если пустая)
6. Логирование: сколько удалено, какие ошибки

**Наследование - пример:**
```
/home/user/projects/          → rules.json: { deny: ["rm -rf"] }
/home/user/projects/myapp/    → rules.json: { disabled_mcp: ["danger-tool"] }
/home/user/projects/myapp/src/ → (нет правил)
```
Для сессии в `src/`: deny=["rm -rf"], disabled_mcp=["danger-tool"], sources=[projects/, myapp/]

### 2.4 Тестовая стратегия

| Уровень | Что тестируем | Как |
|---------|--------------|-----|
| Unit | Парсинг rules.json и rules.md | Фикстуры, проверка десериализации |
| Unit | Merge по дереву | Вложенные tempdir, проверка приоритетов |
| Unit | Glob-исключения из cleanup | Паттерны vs пути |
| Integration | SQL: sessions_for_cleanup | In-memory SQLite, seed сессий с разным last_activity_ms |
| Integration | Применение folder rules в turn_driver | Mock provider, проверка что disabled_mcp не попал в tools/list |
| E2E | Создать .swagcod/rules.json → запустить ход → убедиться что deny сработал | Test app |

### 2.5 Оценка сложности: **XL**

- FolderRulesConfig + парсинг JSON/MD: ~2 дня
- FolderRulesCache + resolve + merge: ~3 дня
- Миграция БД + новые методы Store (обе реализации): ~3 дня
- Интеграция в turn_driver (фильтрация инструментов): ~2 дня
- Auto-cleanup background task: ~2 дня
- UI: редактор folder rules, настройки TTL, список исключений: ~4 дня
- Тесты: ~3 дня

**Итого: ~19 рабочих дней**

### 2.6 Зависимости от существующего кода

- `CommandRules` (rules.rs) - расширяется folder-scoped правилами, порядок deny > allow > политика сохраняется
- `Store` trait - новые методы, обе реализации (SqliteStore + NodeStore)
- `Session` struct - новое поле `work_dir`
- `TurnConfig` - новое поле `folder_rules: Option<ResolvedFolderRules>`
- `turn_driver.rs` - фильтрация tools по disabled_*, применение allow/deny
- `context.rs` - вшивание `system_prompt_append` в системный промпт
- `tasks.rs` - фоновая задача cleanup (через task_enqueue с периодическим сидом)
- Hot-reload (Фича 1) - инвалидация FolderRulesCache при изменении .swagcod/

---

## Фича 3: Звуковые уведомления + системные нотификации

### 3.1 Архитектура

**Затронутые крейты:**
- `crates/core` - расширение `bus::EventKind` (новые события для триггеров уведомлений)
- `crates/app` - новый модуль `notifications.rs`, изменения в `lib.rs` (AppState), `httpapi.rs` (API настроек)
- `apps/desktop` - SettingsTabs: секция уведомлений, выбор звуков, тестовые кнопки

**Новые модули:**
- `crates/app/src/notifications.rs` - менеджер уведомлений: звуки + toast + конфигурация

**Изменения schema:**
- Таблица `prefs`: ключ `notification_config` (JSON NotificationConfig)
- Никаких новых таблиц

**Ключевое решение:** звуки воспроизводятся на стороне Rust (crate `rodio` или Windows WASAPI напрямую), не через WebView. Toast-уведомления - через Windows Runtime API (`windows_sys::Win32::UI::Notifications`). Это гарантирует работу даже когда WebView не в фокусе.

### 3.2 Структуры данных

```rust
// crates/app/src/notifications.rs

use serde::{Deserialize, Serialize};
use std::collections::HashMap;
use std::path::PathBuf;

/// Триггер уведомления.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum NotifyTrigger {
    /// Ход завершён успешно.
    TurnComplete,
    /// Ход завершён с ошибкой.
    TurnError,
    /// Требуется подтверждение пользователя.
    ApprovalRequired,
    /// MCP-сервер переподключился.
    McpReconnected,
    /// Hot-reload завершён.
    HotReloadDone,
    /// Фоновая задача завершена.
    TaskComplete,
}

/// Действие уведомления.
#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(tag = "type", rename_all = "snake_case")]
pub enum NotifyAction {
    /// Воспроизвести звук.
    Sound {
        /// Имя встроенного звука или путь к пользовательскому файлу.
        source: SoundSource,
        /// Громкость 0.0..1.0.
        volume: f32,
    },
    /// Системное toast-уведомление.
    Toast {
        title: String,
        body: String,
        /// Иконка (опционально).
        icon: Option<PathBuf>,
    },
    /// Оба действия.
    Both {
        sound: SoundSource,
        volume: f32,
        title: String,
        body: String,
        icon: Option<PathBuf>,
    },
}

/// Источник звука.
#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(tag = "type", rename_all = "snake_case")]
pub enum SoundSource {
    /// Встроенный звук (bundled в resources/).
    Builtin { name: String },
    /// Пользовательский WAV/MP3/OGG файл.
    Custom { path: PathBuf },
    /// Системный звук Windows.
    System { alias: String },
}

/// Полная конфигурация уведомлений.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct NotificationConfig {
    /// Маппинг триггер → действие. Отсутствие триггера = тихо.
    pub triggers: HashMap<NotifyTrigger, NotifyAction>,
    /// Глобальная громкость (множитель).
    pub master_volume: f32,
    /// Включены ли уведомления вообще.
    pub enabled: bool,
    /// Не уведомлять когда окно в фокусе.
    pub suppress_when_focused: bool,
    /// Директория пользовательских звуков.
    pub custom_sounds_dir: Option<PathBuf>,
}

impl Default for NotificationConfig {
    fn default() -> Self {
        let mut triggers = HashMap::new();
        triggers.insert(NotifyTrigger::ApprovalRequired, NotifyAction::Both {
            sound: SoundSource::Builtin { name: "chime".into() },
            volume: 0.8,
            title: "SwagCod".into(),
            body: "Требуется подтверждение".into(),
            icon: None,
        });
        triggers.insert(NotifyTrigger::TurnComplete, NotifyAction::Sound {
            source: SoundSource::Builtin { name: "done".into() },
            volume: 0.5,
        });
        triggers.insert(NotifyTrigger::TurnError, NotifyAction::Both {
            sound: SoundSource::Builtin { name: "error".into() },
            volume: 0.7,
            title: "SwagCod".into(),
            body: "Ход завершился с ошибкой".into(),
            icon: None,
        });
        Self {
            triggers,
            master_volume: 0.8,
            enabled: true,
            suppress_when_focused: true,
            custom_sounds_dir: None,
        }
    }
}

/// Менеджер уведомлений: держит конфиг, воспроизводит звуки, показывает toast.
pub struct NotificationManager {
    config: NotificationConfig,
    /// Кэш декодированных звуков (避免 повторное чтение файла).
    sound_cache: HashMap<String, Vec<u8>>,
    /// Was focused в последний раз (для suppress_when_focused).
    was_focused: bool,
}

impl NotificationManager {
    pub fn new(config: NotificationConfig) -> Self;
    
    /// Обработать событие шины: решить, нужно ли уведомление, и выполнить.
    pub async fn handle_event(&mut self, event: &swagcod_core::bus::Event, window_focused: bool);
    
    /// Воспроизвести звук (async, не блокирует).
    pub async fn play_sound(&self, source: &SoundSource, volume: f32);
    
    /// Показать Windows toast.
    pub fn show_toast(&self, title: &str, body: &str, icon: Option<&std::path::Path>);
    
    /// Обновить конфиг (из prefs / UI).
    pub fn update_config(&mut self, config: NotificationConfig);
    
    /// Протестировать триггер (кнопка «Test» в UI).
    pub async fn test_trigger(&mut self, trigger: NotifyTrigger);
    
    /// Список доступных встроенных звуков.
    pub fn builtin_sounds() -> Vec<&'static str>;
    
    /// Список пользовательских звуков из custom_sounds_dir.
    pub fn custom_sounds(&self) -> Vec<PathBuf>;
}
```

```rust
// crates/core/src/bus.rs (расширение EventKind)

// Новые варианты (добавить в enum):
// /// Уведомление для менеджера уведомлений (не для UI напрямую).
// Notify { trigger: NotifyTrigger, message: Option<String> },
```

```typescript
// apps/desktop/src/lib/types.ts

type NotifyTrigger = 
  | 'turn_complete' 
  | 'turn_error' 
  | 'approval_required' 
  | 'mcp_reconnected' 
  | 'hot_reload_done' 
  | 'task_complete';

interface NotificationConfig {
  triggers: Record<NotifyTrigger, NotifyAction>;
  master_volume: number;
  enabled: boolean;
  suppress_when_focused: boolean;
  custom_sounds_dir?: string;
}

// Tauri commands:
// get_notification_config() -> NotificationConfig
// set_notification_config(cfg: NotificationConfig) -> void
// test_notification(trigger: NotifyTrigger) -> void
// list_builtin_sounds() -> string[]
// list_custom_sounds() -> string[]
```

### 3.3 Алгоритм работы

**Подписка на события:**
1. `NotificationManager` создаётся при старте приложения, конфиг загружается из prefs
2. Подписка на `Bus::subscribe()` в отдельной tokio-задаче
3. Фильтрация событий:
   - `TurnEnded { ok: true }` → `NotifyTrigger::TurnComplete`
   - `TurnEnded { ok: false }` → `NotifyTrigger::TurnError`
   - `ApprovalRequired` → `NotifyTrigger::ApprovalRequired`
   - `Status { message }` с маркером reload/mcp → соответствующий триггер
4. Проверка `suppress_when_focused`: если окно в фокусе и настройка включена → skip
5. Lookup триггера в `config.triggers`: нет → skip
6. Выполнение действия:
   - `Sound` → `play_sound()` (spawn на отдельный поток, rodio sink)
   - `Toast` → `show_toast()` (Windows Runtime API)
   - `Both` → параллельно оба

**Воспроизведение звука:**
1. Resolve source: Builtin → resources/sounds/{name}.wav, Custom → path, System → Win32 PlaySound
2. Проверка кэша: если декодирован → использовать
3. Иначе: прочитать файл, декодировать (rodio Decoder), закэшировать
4. Создать Sink, установить volume = source.volume * master_volume
5. Append source, sleep until done (или detach для fire-and-forget)

**Windows Toast:**
1. Использовать `windows_sys::Win32::UI::Notifications::ToastNotificationManager`
2. XML-шаблон toast (toastGeneric)
3. Показать через `ToastNotifier::Show`
4. Fallback: если WinRT недоступен → `MessageBoxW` или silently skip

**Сохранение конфига:**
1. UI меняет конфиг → Tauri command `set_notification_config`
2. Сериализация в JSON → `store.set_pref("notification_config", &json)`
3. `NotificationManager::update_config()` in-memory

### 3.4 Тестовая стратегия

| Уровень | Что тестируем | Как |
|---------|--------------|-----|
| Unit | Сериализация/десериализация NotificationConfig | Roundtrip JSON |
| Unit | Маппинг EventKind → NotifyTrigger | Табличный тест |
| Unit | suppress_when_focused логика | Mock focused state |
| Integration | Загрузка/сохранение конфига в prefs | In-memory store |
| Integration | Воспроизведение встроенного звука | Проверка что rodio sink создан (без реального аудио в CI) |
| Manual | Toast на реальной Windows | Ручной тест с кнопкой Test |
| E2E | Запустить ход → дождаться TurnEnded → услышать звук | Manual |

### 3.5 Оценка сложности: **M**

- NotificationConfig + сериализация: ~1 день
- NotificationManager (sound + toast): ~3 дня
- Подписка на Bus + маппинг событий: ~1 день
- Windows Toast integration: ~2 дня (WinRT API капризный)
- Tauri commands + prefs persistence: ~1 день
- UI: SettingsTabs секция, выбор звуков, тестовые кнопки: ~3 дня
- Встроенные звуки (лицензия + запись/покупка): ~1 день
- Тесты: ~1 день

**Итого: ~13 рабочих дней**

### 3.6 Зависимости от существующего кода

- `Bus::subscribe()` - подписка на события
- `EventKind` - маппинг на триггеры (новые варианты не обязательны, можно фильтровать существующие)
- `Store::get_pref/set_pref` - персистентность конфига
- `AppState` - новый field `notifications: Mutex<NotificationManager>`
- Tauri `AppHandle` - для проверки фокуса окна (`WebviewWindow::is_focused()`)
- Новые зависимости: `rodio` (audio playback), возможно `windows-sys` (toast, уже используется для DPAPI)

---

## Сводная таблица

| Фича | Сложность | Дни | Риски |
|------|-----------|-----|-------|
| Hot-reload | L | ~9.5 | notify crate на Windows может требовать special handling; race condition при reload во время активного turn |
| Folder-scoped rules | XL | ~19 | Миграция БД (ALTER TABLE); merge-логика может быть неочевидной; NodeStore + SqliteStore дублирование |
| Уведомления | M | ~13 | WinRT Toast API нестабилен в headless/CI; rodio может конфликтовать с exclusive audio devices |

**Рекомендуемый порядок реализации:**
1. **Фича 3 (Уведомления)** - наименьшая связность, можно делать параллельно с остальными, быстрый win для UX
2. **Фича 1 (Hot-reload)** - средняя сложность, но критична для DX; JS plugin reload почти бесплатен
3. **Фича 2 (Folder-scoped rules)** - самая большая, зависит от Фичи 1 (invalidation cache), делать последней

**Общая оценка: ~41.5 рабочих дней (~8 недель)**

---

## Cross-cutting concerns

### Взаимодействие фич

- Фича 2 зависит от Фичи 1: `FolderRulesCache` должен инвалидироваться при изменении `.swagcod/rules.*` → нужен `HotReloadEvent::FolderRulesChanged`
- Фича 3 реагирует на события Фичи 1: `HotReloadDone` → уведомление
- Фича 3 реагирует на Фичу 2: cleanup удалил сессии → опциональное уведомление

### OPSEC / Безопасность

- Folder rules: deny-список всегда побеждает allow (сохраняется существующая семантика `rules.rs`)
- Custom sounds: валидация пути (не выходить за пределы custom_sounds_dir, no path traversal)
- Toast: не показывать чувствительные данные (summary approval - ок, содержимое чата - нет)
- Hot-reload: проверять целостность конфигов перед применением (битый JSON → ошибка, не crash)

### Производительность

- File watcher: debounce 300 мс, recursive watch только на нужные директории
- Folder rules cache: LRU с лимитом 256 записей, инвалидация по событию
- Sound cache: ограничить 32 MB суммарно, LRU eviction
- Cleanup task: batch delete (транзакция), не по одной сессии

...

Документ готов. Три фичи разобраны до структур данных, алгоритмов и тестов. Можно брать любую и начинать кодить 🖤

...

<tool_call>
<function=send_message>
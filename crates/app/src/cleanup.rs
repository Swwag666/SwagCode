/*! Auto-cleanup: гигиена сессий, логов и пустых рабочих папок.

Два входа в одну логику:
1. **При старте** — synchronous прогон в setup Tauri (быстрый, на SQLite).
2. **Фоново** — [`cleanup_worker`] раз в [`DEFAULT_PERIOD_SECS`]: долгое
   приложение не должно копить мёртвые сессии до следующего перезапуска.

Учитываются folder-rules (Фича 2): `.swagcod/rules.json` рабочей папки сессии
может задать свой `session_ttl_secs` или вовсе вывести папку из очистки через
`exempt_from_cleanup`. Это единственная причина, по которой очистка знает про
дерево папок: владелец проекта решает, что в нём не удаляется автоматически.

Конфигурация живёт в prefs (ключ [`CLEANUP_CONFIG_PREF`], JSON) — правка в UI
действует со следующего прогона без перезапуска.
*/

use serde::{Deserialize, Serialize};
use swagcod_core::folder_rules::FolderRulesCache;
use swagcod_core::store::Store;

/// Период фонового прогона по умолчанию — час.
pub const DEFAULT_PERIOD_SECS: u64 = 3_600;
/// Переменная окружения для периода (тесты и нетерпеливые владельцы).
pub const PERIOD_ENV: &str = "SWAGCOD_CLEANUP_PERIOD_SECS";
/// Оценка объёма одной сессии для `space_freed_bytes`. Точный размер потребовал
/// бы запроса к базе на каждую сессию; честная грубая оценка лучше нуля и
/// помечена как оценка в UI.
const EST_BYTES_PER_SESSION: u64 = 10 * 1024;
/// Ключи prefs, способные раздуться в мегабайты текста.
const LOG_PREF_KEYS: [&str; 3] = ["session_logs", "debug_log", "app_log"];

/// Конфигурация авто-очистки.
///
/// Новые поля помечены `#[serde(default)]`: конфиг, сохранённый прежней
/// версией приложения, читается целиком и не теряет смысл.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct CleanupConfig {
    /// Максимальное количество сессий. При превышении удаляются самые старые.
    #[serde(default = "default_max_sessions")]
    pub max_sessions: usize,
    /// Удалять сессии старше этого количества дней.
    #[serde(default = "default_max_age_days")]
    pub max_age_days: u64,
    /// Обрезать логи (prefs) больше этого размера в МБ.
    #[serde(default = "default_max_log_size_mb")]
    pub max_log_size_mb: u64,
    /// Включена ли авто-очистка.
    #[serde(default = "default_enabled")]
    pub enabled: bool,
    /// Glob-паттерны исключений по рабочей папке сессии.
    #[serde(default)]
    pub exempt_patterns: Vec<String>,
    /// Учитывать `session_ttl_secs` и `exempt_from_cleanup` из folder-rules.
    #[serde(default = "default_enabled")]
    pub honor_folder_rules: bool,
    /// Удалять опустевшие рабочие папки (только если каталог пуст).
    #[serde(default)]
    pub cleanup_empty_dirs: bool,
}

fn default_max_sessions() -> usize {
    100
}
fn default_max_age_days() -> u64 {
    90
}
fn default_max_log_size_mb() -> u64 {
    50
}
fn default_enabled() -> bool {
    true
}

impl Default for CleanupConfig {
    fn default() -> Self {
        Self {
            max_sessions: default_max_sessions(),
            max_age_days: default_max_age_days(),
            max_log_size_mb: default_max_log_size_mb(),
            enabled: default_enabled(),
            exempt_patterns: Vec::new(),
            honor_folder_rules: default_enabled(),
            cleanup_empty_dirs: false,
        }
    }
}

/// Отчёт о проведённой очистке.
#[derive(Debug, Clone, Default, Serialize, Deserialize, PartialEq)]
pub struct CleanupReport {
    /// Сколько сессий удалено.
    pub sessions_deleted: usize,
    /// Сколько сессий пощажено правилами папки или glob-исключением.
    pub sessions_exempted: usize,
    /// Сколько логов обрезано.
    pub logs_truncated: usize,
    /// Сколько пустых рабочих папок удалено.
    pub dirs_removed: usize,
    /// Сколько байт освобождено (сессии — оценка, логи — точно).
    pub space_freed_bytes: u64,
    /// Рабочие папки, в которых не осталось сессий (кандидаты на удаление).
    pub emptied_dirs: Vec<String>,
}

/// Prefs-ключ конфигурации очистки.
pub const CLEANUP_CONFIG_PREF: &str = "cleanup_config";

/// Загрузить конфигурацию из store. Отсутствующая или битая — дефолт:
/// очистка не должна падать из-за опечатки в сохранённом JSON.
pub fn load_config(store: &dyn Store) -> CleanupConfig {
    store
        .get_pref(CLEANUP_CONFIG_PREF)
        .ok()
        .flatten()
        .and_then(|v| serde_json::from_str(&v).ok())
        .unwrap_or_default()
}

/// Сохранить конфигурацию в store.
pub fn save_config(store: &dyn Store, config: &CleanupConfig) -> Result<(), String> {
    let json = serde_json::to_string(config).map_err(|e| e.to_string())?;
    store
        .set_pref(CLEANUP_CONFIG_PREF, &json)
        .map_err(|e| e.to_string())
}

/// Период фонового прогона: env-override, иначе [`DEFAULT_PERIOD_SECS`].
/// Значения вне 5..86400 сек игнорируются — защита от опечатки, которая
/// превратила бы очистку в долбёжку базы каждую миллисекунду.
pub fn period_secs() -> u64 {
    std::env::var(PERIOD_ENV)
        .ok()
        .and_then(|v| v.parse::<u64>().ok())
        .filter(|v| (5..=86_400).contains(v))
        .unwrap_or(DEFAULT_PERIOD_SECS)
}

/// Сессия исключена из очистки glob-паттерном по её рабочей папке.
/// Битый паттерн не исключает ничего: лучше удалить по честному правилу,
/// чем молча хранить сессии из-за опечатки владельца.
pub fn is_exempt_by_pattern(patterns: &[String], cwd: &str) -> bool {
    let norm = swagcod_core::folder_rules::normalize(std::path::Path::new(cwd));
    patterns.iter().any(|p| match glob::Pattern::new(p) {
        Ok(pat) => pat.matches(&norm) || pat.matches(cwd),
        Err(e) => {
            eprintln!("cleanup: битый exempt-паттерн {p:?}: {e}");
            false
        }
    })
}

/// Запустить очистку. Возвращает отчёт о проделанной работе.
///
/// `rules` — кэш folder-rules; нужен, чтобы спросить у дерева папок TTL и
/// исключения. Кэш мутируется (перечитывает конфиги по mtime), поэтому
/// передаётся как `&mut`.
pub fn run_cleanup(
    config: &CleanupConfig,
    store: &dyn Store,
    rules: &mut FolderRulesCache,
) -> CleanupReport {
    let mut report = CleanupReport::default();

    if !config.enabled {
        return report;
    }

    let now_ms = swagcod_core::bus::now_ms();
    let global_cutoff = now_ms.saturating_sub(config.max_age_days * 24 * 3_600_000);

    /* Список сессий берём лёгким снимком (`session_stubs`), а не `load_all`:
    последний через fill_session поднимает весь текст ходов и сообщений каждой
    сессии. На живой базе владельца (46 МБ, 71 сессия, 6337 сообщений — замер
    D-193) уборка на старте прочитала бы всю историю в память и съела бюджет
    запуска 400 мс, и платили бы за это на КАЖДОМ старте и КАЖДЫЙ час. Для
    решения «удалить или нет» нужны ровно id, cwd и created_ms. */
    if let Ok(all) = store.session_stubs() {
        /* 1. Возраст и исключения. TTL берётся из folder-rules папки сессии,
        если владелец его задал; иначе — глобальный max_age_days. */
        let mut to_delete: Vec<String> = Vec::new();
        for s in &all {
            let exempt_by_glob = is_exempt_by_pattern(&config.exempt_patterns, &s.cwd);
            let (exempt_by_rules, ttl_cutoff) = if config.honor_folder_rules {
                let res = rules.resolve(std::path::Path::new(&s.cwd));
                let ttl = res
                    .session_ttl_secs
                    .map(|secs| now_ms.saturating_sub(secs.saturating_mul(1_000)));
                (res.exempt_from_cleanup, ttl)
            } else {
                (false, None)
            };

            if exempt_by_glob || exempt_by_rules {
                report.sessions_exempted += 1;
                continue;
            }
            let cutoff = ttl_cutoff.unwrap_or(global_cutoff);
            if s.created_ms < cutoff {
                to_delete.push(s.id.clone());
            }
        }

        /* 2. Лимит количества: если после удаления старых всё ещё больше
        `max_sessions`, режем самые старые из оставшихся. `session_stubs`
        приходит отсортированным тем же `ORDER BY created_ms, id`, что и
        load_all, поэтому «самые старые» здесь однозначны. */
        let remaining_after_age = all.len().saturating_sub(to_delete.len());
        if remaining_after_age > config.max_sessions {
            let excess = remaining_after_age - config.max_sessions;
            let mut extra: Vec<(u64, String)> = all
                .iter()
                .filter(|s| !to_delete.contains(&s.id))
                .map(|s| (s.created_ms, s.id.clone()))
                .collect();
            extra.sort_by_key(|(ms, _)| *ms);
            for (_, id) in extra.into_iter().take(excess) {
                to_delete.push(id);
            }
        }

        /* 3. Само удаление. Запомнили рабочие папки: после удаления в них
        могло не остаться сессий — это кандидаты на снос каталога. */
        let mut touched_dirs: Vec<String> = Vec::new();
        for id in &to_delete {
            if let Some(cwd) = all.iter().find(|s| &s.id == id).map(|s| &s.cwd) {
                if !touched_dirs.contains(cwd) {
                    touched_dirs.push(cwd.clone());
                }
            }
            match store.delete_session(id) {
                Ok(()) => {
                    report.sessions_deleted += 1;
                    report.space_freed_bytes += EST_BYTES_PER_SESSION;
                }
                Err(e) => eprintln!("cleanup: не удалил сессию {id}: {e}"),
            }
        }

        /* 4. Опустевшие папки. Считаем по базе, а не по кэшу в памяти, и тем
        же лёгким снимком: вторая полная загрузка истории стоила бы вдвое. */
        if !touched_dirs.is_empty() {
            let alive: Vec<String> = store
                .session_stubs()
                .unwrap_or_default()
                .into_iter()
                .map(|s| s.cwd)
                .collect();
            for dir in &touched_dirs {
                if !alive.iter().any(|c| c == dir) {
                    report.emptied_dirs.push(dir.clone());
                }
            }
            if config.cleanup_empty_dirs {
                for dir in report.emptied_dirs.clone() {
                    if remove_empty_workdir(&dir) {
                        report.dirs_removed += 1;
                    }
                }
            }
        }
    }

    /* 5. Обрезка раздувшихся prefs-логов. */
    let max_bytes = config.max_log_size_mb.saturating_mul(1024 * 1024);
    for key in LOG_PREF_KEYS {
        if let Ok(Some(val)) = store.get_pref(key) {
            if val.len() as u64 > max_bytes {
                let truncated: String = val.chars().take(max_bytes as usize).collect();
                if store.set_pref(key, &truncated).is_ok() {
                    report.logs_truncated += 1;
                    report.space_freed_bytes += val.len().saturating_sub(truncated.len()) as u64;
                }
            }
        }
    }

    report
}

/// Снести рабочую папку, если она пуста. Три предохранителя:
/// 1. каталог обязан существовать;
/// 2. `.swagcod/` внутри удаляется только если в нём нет ничего, кроме наших
///    служебных файлов — чужое содержимое не трогаем;
/// 3. сама папка удаляется только ставшей пустой (`remove_dir` не снесёт
///    непустой каталог, так что это проверка, а не намерение).
///
/// Возвращает true, если папка действительно удалена.
fn remove_empty_workdir(dir: &str) -> bool {
    let p = std::path::Path::new(dir);
    if !p.is_dir() {
        return false;
    }
    let swag = p.join(swagcod_core::folder_rules::SWAGCOD_DIR);
    if swag.is_dir() {
        let foreign = std::fs::read_dir(&swag)
            .map(|rd| {
                rd.filter_map(|e| e.ok())
                    .any(|e| !is_swagcod_service_file(e.file_name().to_string_lossy().as_ref()))
            })
            .unwrap_or(true);
        if foreign {
            return false;
        }
        if std::fs::remove_dir_all(&swag).is_err() {
            return false;
        }
    }
    std::fs::remove_dir(p).is_ok()
}

/// Служебные файлы `.swagcod/`, которые приложение создало само и имеет право
/// убрать. Всё остальное (MEMORY.md владельца, его заметки) — чужое.
fn is_swagcod_service_file(name: &str) -> bool {
    matches!(
        name,
        "rules.json" | "rules.md" | "folder_rules.json" | "state.json" | "index.sqlite"
    )
}

/// Фоновый воркер очистки: тик раз в [`period_secs`], каждый тик — полный
/// прогон. Первый тик сразу (interval стартует немедленно), чтобы долгое
/// приложение не ждало час до первой уборки.
pub async fn cleanup_worker(state: std::sync::Arc<crate::AppState>) {
    let period = std::time::Duration::from_secs(period_secs());
    let mut tick = tokio::time::interval(period);
    loop {
        tick.tick().await;
        let report = {
            let Ok(store) = state.store.lock() else {
                eprintln!("cleanup-worker: store заблокирован, тик пропущен");
                continue;
            };
            let config = load_config(store.as_ref());
            if !config.enabled {
                continue;
            }
            /* Кэш folder-rules берём вторым локом и держим оба только на
            время прогона: очистка быстрая (чтение prefs + обход дерева),
            а порядок захвата здесь единственный в приложении — store, затем
            folder_rules. */
            let mut rules = match state.folder_rules.lock() {
                Ok(cache) => cache,
                Err(e) => {
                    eprintln!("cleanup-worker: кэш правил заблокирован ({e})");
                    continue;
                }
            };
            run_cleanup(&config, store.as_ref(), &mut rules)
        };
        let touched =
            report.sessions_deleted > 0 || report.logs_truncated > 0 || report.dirs_removed > 0;
        if !touched {
            continue;
        }
        eprintln!(
            "cleanup-worker: удалено сессий {}, обрезано логов {}, папок {}, освобождено {} байт",
            report.sessions_deleted,
            report.logs_truncated,
            report.dirs_removed,
            report.space_freed_bytes
        );
        state.bus.publish(swagcod_core::bus::EventKind::Status {
            message: format!(
                "автоочистка: удалено сессий {}, обрезано логов {}, снесено пустых папок {}",
                report.sessions_deleted, report.logs_truncated, report.dirs_removed
            ),
        });
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use swagcod_core::session::{Session, SessionId};

    fn make_store() -> Box<dyn Store> {
        swagcod_core::store::open_memory()
    }

    fn make_session(id: &str, created_ms: u64) -> Session {
        let mut s = Session::new(
            SessionId::new(id),
            String::from("/tmp"),
            String::from("test"),
        );
        s.created_ms = created_ms;
        s
    }

    fn make_session_in(id: &str, created_ms: u64, cwd: &str) -> Session {
        let mut s = Session::new(SessionId::new(id), String::from(cwd), String::from("test"));
        s.created_ms = created_ms;
        s
    }

    fn empty_rules() -> FolderRulesCache {
        FolderRulesCache::from_rules(Vec::new(), std::path::PathBuf::from("/dev/null"))
    }

    fn cfg(max_sessions: usize, max_age_days: u64) -> CleanupConfig {
        CleanupConfig {
            max_sessions,
            max_age_days,
            max_log_size_mb: 50,
            enabled: true,
            ..Default::default()
        }
    }

    #[test]
    fn test_cleanup_deletes_old_sessions() {
        let store = make_store();
        let now = swagcod_core::bus::now_ms();
        let old = now.saturating_sub(100 * 24 * 3_600_000);
        let recent = now.saturating_sub(10 * 24 * 3_600_000);

        store.create_session(&make_session("old-1", old)).unwrap();
        store.create_session(&make_session("old-2", old)).unwrap();
        store
            .create_session(&make_session("recent-1", recent))
            .unwrap();

        let report = run_cleanup(&cfg(100, 90), store.as_ref(), &mut empty_rules());
        assert_eq!(report.sessions_deleted, 2);
        assert!(report.space_freed_bytes > 0);

        let remaining = store.load_all().unwrap();
        assert_eq!(remaining.len(), 1);
        assert_eq!(remaining[0].id.to_string(), "recent-1");
    }

    #[test]
    fn test_cleanup_respects_max_sessions() {
        let store = make_store();
        let now = swagcod_core::bus::now_ms();

        for i in 0..5 {
            let ms = now.saturating_sub((i as u64) * 1_000_000);
            store
                .create_session(&make_session(&format!("s-{i}"), ms))
                .unwrap();
        }

        let report = run_cleanup(&cfg(3, 90), store.as_ref(), &mut empty_rules());
        assert_eq!(report.sessions_deleted, 2);
        assert_eq!(store.load_all().unwrap().len(), 3);
    }

    #[test]
    fn test_disabled_skips() {
        let store = make_store();
        let now = swagcod_core::bus::now_ms();
        store
            .create_session(&make_session(
                "old-1",
                now.saturating_sub(200 * 24 * 3_600_000),
            ))
            .unwrap();

        let config = CleanupConfig {
            enabled: false,
            ..Default::default()
        };
        let report = run_cleanup(&config, store.as_ref(), &mut empty_rules());
        assert_eq!(report.sessions_deleted, 0);
        assert_eq!(report.space_freed_bytes, 0);
        assert_eq!(store.load_all().unwrap().len(), 1);
    }

    #[test]
    fn test_default_config_values() {
        let config = CleanupConfig::default();
        assert_eq!(config.max_sessions, 100);
        assert_eq!(config.max_age_days, 90);
        assert_eq!(config.max_log_size_mb, 50);
        assert!(config.enabled);
        assert!(config.honor_folder_rules);
        assert!(!config.cleanup_empty_dirs);
        assert!(config.exempt_patterns.is_empty());
    }

    #[test]
    fn test_config_roundtrip_through_store() {
        let store = make_store();
        let config = CleanupConfig {
            max_sessions: 50,
            max_age_days: 30,
            max_log_size_mb: 10,
            enabled: true,
            exempt_patterns: vec!["**/keep/**".into()],
            honor_folder_rules: false,
            cleanup_empty_dirs: true,
        };
        save_config(store.as_ref(), &config).unwrap();
        assert_eq!(load_config(store.as_ref()), config);
    }

    /// Конфиг прежней версии (4 поля) обязан читаться целиком: новые поля
    /// получают дефолты, а не роняют десериализацию.
    #[test]
    fn test_legacy_config_without_new_fields_loads() {
        let store = make_store();
        store
            .set_pref(
                CLEANUP_CONFIG_PREF,
                r#"{"max_sessions":7,"max_age_days":3,"max_log_size_mb":1,"enabled":true}"#,
            )
            .unwrap();
        let loaded = load_config(store.as_ref());
        assert_eq!(loaded.max_sessions, 7);
        assert!(loaded.exempt_patterns.is_empty());
        assert!(loaded.honor_folder_rules);
        assert!(!loaded.cleanup_empty_dirs);
    }

    #[test]
    fn test_broken_config_falls_back_to_default() {
        let store = make_store();
        store.set_pref(CLEANUP_CONFIG_PREF, "{ не json").unwrap();
        assert_eq!(load_config(store.as_ref()), CleanupConfig::default());
    }

    #[test]
    fn test_exempt_pattern_skips_sessions() {
        let store = make_store();
        let now = swagcod_core::bus::now_ms();
        let old = now.saturating_sub(200 * 24 * 3_600_000);
        store
            .create_session(&make_session_in("keep", old, "D:/work/important/proj"))
            .unwrap();
        store
            .create_session(&make_session_in("drop", old, "D:/work/scratch/proj"))
            .unwrap();

        let config = CleanupConfig {
            exempt_patterns: vec!["**/important/**".into()],
            ..cfg(100, 90)
        };
        let report = run_cleanup(&config, store.as_ref(), &mut empty_rules());
        assert_eq!(report.sessions_deleted, 1);
        assert_eq!(report.sessions_exempted, 1);
        let left = store.load_all().unwrap();
        assert_eq!(left.len(), 1);
        assert_eq!(left[0].id.to_string(), "keep");
    }

    #[test]
    fn test_broken_exempt_pattern_exempts_nothing() {
        assert!(!is_exempt_by_pattern(&["[bad".into()], "D:/x/y"));
        assert!(is_exempt_by_pattern(&["**/y".into()], "D:\\x\\y"));
        assert!(is_exempt_by_pattern(&["D:/x/**".into()], "D:\\x\\y"));
        assert!(!is_exempt_by_pattern(&[], "D:\\x\\y"));
    }

    /// Folder-rules папки выводят сессию из очистки даже когда она древняя.
    #[test]
    fn test_folder_rules_exempt_wins() {
        let root = tempfile::tempdir().unwrap();
        let work = root.path().join("proj");
        let swag = work.join(".swagcod");
        std::fs::create_dir_all(&swag).unwrap();
        std::fs::write(
            swag.join("rules.json"),
            serde_json::to_string(&serde_json::json!({"exempt_from_cleanup": true})).unwrap(),
        )
        .unwrap();

        let store = make_store();
        let old = swagcod_core::bus::now_ms().saturating_sub(400 * 24 * 3_600_000);
        store
            .create_session(&make_session_in("protected", old, work.to_str().unwrap()))
            .unwrap();

        let report = run_cleanup(&cfg(100, 90), store.as_ref(), &mut empty_rules());
        assert_eq!(report.sessions_deleted, 0);
        assert_eq!(report.sessions_exempted, 1);
        assert_eq!(store.load_all().unwrap().len(), 1);
    }

    /// `honor_folder_rules = false` отключает учёт правил папки.
    #[test]
    fn test_folder_rules_can_be_ignored() {
        let root = tempfile::tempdir().unwrap();
        let work = root.path().join("proj");
        let swag = work.join(".swagcod");
        std::fs::create_dir_all(&swag).unwrap();
        std::fs::write(
            swag.join("rules.json"),
            serde_json::to_string(&serde_json::json!({"exempt_from_cleanup": true})).unwrap(),
        )
        .unwrap();

        let store = make_store();
        let old = swagcod_core::bus::now_ms().saturating_sub(400 * 24 * 3_600_000);
        store
            .create_session(&make_session_in("doomed", old, work.to_str().unwrap()))
            .unwrap();

        let config = CleanupConfig {
            honor_folder_rules: false,
            ..cfg(100, 90)
        };
        let report = run_cleanup(&config, store.as_ref(), &mut empty_rules());
        assert_eq!(report.sessions_deleted, 1);
        assert_eq!(report.sessions_exempted, 0);
    }

    /// TTL папки короче глобального — сессия умирает раньше.
    #[test]
    fn test_folder_ttl_overrides_global() {
        let root = tempfile::tempdir().unwrap();
        let work = root.path().join("proj");
        let swag = work.join(".swagcod");
        std::fs::create_dir_all(&swag).unwrap();
        std::fs::write(
            swag.join("rules.json"),
            serde_json::to_string(&serde_json::json!({"session_ttl_secs": 3600})).unwrap(),
        )
        .unwrap();

        let store = make_store();
        // Сессия berusia 2 часа: глобально (90 дней) жива, по TTL папки — нет.
        let two_hours_ago = swagcod_core::bus::now_ms().saturating_sub(2 * 3_600_000);
        store
            .create_session(&make_session_in(
                "short-lived",
                two_hours_ago,
                work.to_str().unwrap(),
            ))
            .unwrap();

        let report = run_cleanup(&cfg(100, 90), store.as_ref(), &mut empty_rules());
        assert_eq!(report.sessions_deleted, 1);
    }

    /// Опустевшая папка попадает в отчёт, но не удаляется без флага.
    #[test]
    fn test_emptied_dirs_reported_not_removed_by_default() {
        let root = tempfile::tempdir().unwrap();
        let work = root.path().join("proj");
        std::fs::create_dir_all(&work).unwrap();

        let store = make_store();
        let old = swagcod_core::bus::now_ms().saturating_sub(400 * 24 * 3_600_000);
        store
            .create_session(&make_session_in("s1", old, work.to_str().unwrap()))
            .unwrap();

        let report = run_cleanup(&cfg(100, 90), store.as_ref(), &mut empty_rules());
        assert_eq!(report.sessions_deleted, 1);
        assert_eq!(
            report.emptied_dirs,
            vec![work.to_str().unwrap().to_string()]
        );
        assert_eq!(report.dirs_removed, 0);
        assert!(work.exists(), "папка не должна удаляться по умолчанию");
    }

    #[test]
    fn test_empty_dir_removed_when_enabled() {
        let root = tempfile::tempdir().unwrap();
        let work = root.path().join("proj");
        std::fs::create_dir_all(&work).unwrap();

        let store = make_store();
        let old = swagcod_core::bus::now_ms().saturating_sub(400 * 24 * 3_600_000);
        store
            .create_session(&make_session_in("s1", old, work.to_str().unwrap()))
            .unwrap();

        let config = CleanupConfig {
            cleanup_empty_dirs: true,
            ..cfg(100, 90)
        };
        let report = run_cleanup(&config, store.as_ref(), &mut empty_rules());
        assert_eq!(report.dirs_removed, 1);
        assert!(!work.exists());
    }

    /// Непустая папка не сносится: удаление каталога — только для пустых.
    #[test]
    fn test_non_empty_dir_is_never_removed() {
        let root = tempfile::tempdir().unwrap();
        let work = root.path().join("proj");
        std::fs::create_dir_all(&work).unwrap();
        std::fs::write(work.join("важный-файл.txt"), "данные").unwrap();

        let store = make_store();
        let old = swagcod_core::bus::now_ms().saturating_sub(400 * 24 * 3_600_000);
        store
            .create_session(&make_session_in("s1", old, work.to_str().unwrap()))
            .unwrap();

        let config = CleanupConfig {
            cleanup_empty_dirs: true,
            ..cfg(100, 90)
        };
        let report = run_cleanup(&config, store.as_ref(), &mut empty_rules());
        assert_eq!(report.sessions_deleted, 1);
        assert_eq!(report.dirs_removed, 0);
        assert!(work.join("важный-файл.txt").exists());
    }

    /// Чужие файлы в `.swagcod/` (MEMORY.md владельца) защищают папку от сноса.
    #[test]
    fn test_foreign_swagcod_files_protect_dir() {
        let root = tempfile::tempdir().unwrap();
        let work = root.path().join("proj");
        let swag = work.join(".swagcod");
        std::fs::create_dir_all(&swag).unwrap();
        std::fs::write(swag.join("rules.json"), "{}").unwrap();
        std::fs::write(swag.join("MEMORY.md"), "заметки владельца").unwrap();

        assert!(!remove_empty_workdir(work.to_str().unwrap()));
        assert!(work.exists());
        assert!(swag.join("MEMORY.md").exists());
    }

    /// Служебные файлы приложение имеет право убрать вместе с папкой.
    #[test]
    fn test_service_only_swagcod_dir_is_removable() {
        let root = tempfile::tempdir().unwrap();
        let work = root.path().join("proj");
        let swag = work.join(".swagcod");
        std::fs::create_dir_all(&swag).unwrap();
        std::fs::write(swag.join("rules.json"), "{}").unwrap();

        assert!(remove_empty_workdir(work.to_str().unwrap()));
        assert!(!work.exists());
    }

    #[test]
    fn test_remove_empty_workdir_missing_dir_is_false() {
        assert!(!remove_empty_workdir("Z:/точно/нет/такой/папки"));
    }

    #[test]
    fn test_is_swagcod_service_file() {
        assert!(is_swagcod_service_file("rules.json"));
        assert!(is_swagcod_service_file("rules.md"));
        assert!(!is_swagcod_service_file("MEMORY.md"));
        assert!(!is_swagcod_service_file("notes.txt"));
    }

    #[test]
    fn test_logs_truncated_over_limit() {
        let store = make_store();
        let config = CleanupConfig {
            max_log_size_mb: 0, // любой непустой лог больше нуля байт
            ..Default::default()
        };
        store.set_pref("debug_log", &"x".repeat(4096)).unwrap();
        let report = run_cleanup(&config, store.as_ref(), &mut empty_rules());
        assert_eq!(report.logs_truncated, 1);
        assert_eq!(report.space_freed_bytes, 4096);
        assert_eq!(store.get_pref("debug_log").unwrap(), Some(String::new()));
    }

    #[test]
    fn test_logs_under_limit_untouched() {
        let store = make_store();
        store.set_pref("debug_log", "короткий лог").unwrap();
        let report = run_cleanup(
            &CleanupConfig::default(),
            store.as_ref(),
            &mut empty_rules(),
        );
        assert_eq!(report.logs_truncated, 0);
        assert_eq!(
            store.get_pref("debug_log").unwrap(),
            Some("короткий лог".to_string())
        );
    }

    #[test]
    fn test_period_secs_bounds() {
        // Без env — дефолт. Границы проверяем на явных значениях.
        std::env::remove_var(PERIOD_ENV);
        assert_eq!(period_secs(), DEFAULT_PERIOD_SECS);

        std::env::set_var(PERIOD_ENV, "10");
        assert_eq!(period_secs(), 10);

        std::env::set_var(PERIOD_ENV, "1"); // слишком мало → дефолт
        assert_eq!(period_secs(), DEFAULT_PERIOD_SECS);

        std::env::set_var(PERIOD_ENV, "мусор");
        assert_eq!(period_secs(), DEFAULT_PERIOD_SECS);

        std::env::remove_var(PERIOD_ENV);
    }
}

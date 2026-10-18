//! Folder-scoped rules: два механизма, один результат.
//!
//! 1. **Глобальные glob-правила** из `~/.swagcod/folder_rules.json` — паттерн →
//!    строки для инъекции в системный промпт. Удобны, когда правило привязано
//!    к форме пути (`crates/**`, `tests/**`, `*.md`).
//! 2. **Наследование по дереву** — `.swagcod/rules.json` (или `rules.md`) в
//!    рабочей папке сессии и во всех предках до корня ФС. Ближние уровни
//!    дополняют дальние: списки объединяются, `system_prompt_append`
//!    конкатенируется от корня к листу, `session_ttl_secs` берётся с ближайшего
//!    непустого уровня, `exempt_from_cleanup` — true, если true на любом уровне.
//!
//! Оба механизма сходятся в [`ResolvedFolderRules`], который ход применяет
//! целиком: строки и append уходят в системный промпт, allow/deny — в
//! [`crate::rules::CommandRules`], disabled_* — в фильтрацию списка
//! инструментов, ttl/exempt — в автоочистку.
//!
//! Правила перечитываются при изменении файлов (hot-reload из Фичи 1 дёргает
//! [`FolderRulesCache::reload`]); кэш конфигов папок дополнительно сверяет
//! mtime, так что правка `.swagcod/rules.json` в любой папке подхватывается без
//! перезапуска и без явного сигнала.

use serde::{Deserialize, Serialize};
use std::collections::HashMap;
use std::path::{Path, PathBuf};
use std::time::SystemTime;

/// Предел объёма правил, уходящих в системный промпт. Держит контекст
/// предсказуемым: сто папок с простынями текста не должны съедать окно модели.
pub const FOLDER_RULES_MAX_CHARS: usize = 8_000;

/// Имя файла правил папки (JSON приоритетнее MD).
pub const RULES_JSON: &str = "rules.json";
/// Запасной формат: markdown целиком уходит в `system_prompt_append`.
pub const RULES_MD: &str = "rules.md";
/// Имя служебного каталога проекта.
pub const SWAGCOD_DIR: &str = ".swagcod";

/// Одно глобальное glob-правило: паттерн + строки для инъекции + приоритет.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct FolderRule {
    /// Glob-паттерн (например `src/**/*.rs`, `tests/**`, `*.md`).
    pub pattern: String,
    /// Строки правил для инъекции в контекст модели при совпадении.
    pub rules: Vec<String>,
    /// Приоритет: выше число = выше приоритет. При равном — порядок в файле.
    #[serde(default)]
    pub priority: u32,
}

/// Конфигурация правил одной папки: `.swagcod/rules.json`.
///
/// Все поля опциональны — отсутствующий файл или пустой объект валидны и
/// означают «папка ничего не добавляет».
#[derive(Debug, Clone, Default, Serialize, Deserialize, PartialEq)]
pub struct FolderRulesConfig {
    /// Дополнительные allow-паттерны команд (к глобальным из prefs).
    #[serde(default)]
    pub allow: Vec<String>,
    /// Дополнительные deny-паттерны команд. Deny всегда сильнее allow.
    #[serde(default)]
    pub deny: Vec<String>,
    /// Отключённые MCP-серверы (по имени сервера, не инструмента).
    #[serde(default)]
    pub disabled_mcp: Vec<String>,
    /// Отключённые JS-плагины (по short-имени, без префикса `js:`).
    #[serde(default)]
    pub disabled_plugins: Vec<String>,
    /// Отключённые скиллы (по имени).
    #[serde(default)]
    pub disabled_skills: Vec<String>,
    /// Дополнение системного промпта (добавляется, не заменяет базовый).
    #[serde(default)]
    pub system_prompt_append: Option<String>,
    /// TTL автоудаления сессий этой папки, сек. `None` — глобальный TTL.
    #[serde(default)]
    pub session_ttl_secs: Option<u64>,
    /// Исключить папку и вложенные из автоудаления.
    #[serde(default)]
    pub exempt_from_cleanup: bool,
}

impl FolderRulesConfig {
    /// Пустой конфиг: ничего не добавляет и ничего не отключает.
    pub fn is_noop(&self) -> bool {
        self.allow.is_empty()
            && self.deny.is_empty()
            && self.disabled_mcp.is_empty()
            && self.disabled_plugins.is_empty()
            && self.disabled_skills.is_empty()
            && self.system_prompt_append.is_none()
            && self.session_ttl_secs.is_none()
            && !self.exempt_from_cleanup
    }
}

/// Правила папки после слияния всего дерева — то, что применяет ход.
#[derive(Debug, Clone, Default, PartialEq)]
pub struct ResolvedFolderRules {
    /// Строки глобальных glob-правил, совпавших с путём (по приоритету desc).
    pub prompt_lines: Vec<String>,
    /// Объединённые allow-паттерны команд.
    pub allow: Vec<String>,
    /// Объединённые deny-паттерны команд.
    pub deny: Vec<String>,
    /// Отключённые MCP-серверы.
    pub disabled_mcp: Vec<String>,
    /// Отключённые JS-плагины (short-имена).
    pub disabled_plugins: Vec<String>,
    /// Отключённые скиллы.
    pub disabled_skills: Vec<String>,
    /// Склеенный `system_prompt_append` от корня к листу.
    pub system_prompt_append: String,
    /// TTL с ближайшего уровня, где он задан.
    pub session_ttl_secs: Option<u64>,
    /// true, если исключение выставлено на любом уровне.
    pub exempt_from_cleanup: bool,
    /// Цепочка файлов, откуда пришли правила (для отладки и UI).
    pub sources: Vec<PathBuf>,
}

impl ResolvedFolderRules {
    /// Ничего не нашлось: ход идёт ровно как до появления folder-rules.
    pub fn is_noop(&self) -> bool {
        self.prompt_lines.is_empty()
            && self.allow.is_empty()
            && self.deny.is_empty()
            && self.disabled_mcp.is_empty()
            && self.disabled_plugins.is_empty()
            && self.disabled_skills.is_empty()
            && self.system_prompt_append.is_empty()
            && self.session_ttl_secs.is_none()
            && !self.exempt_from_cleanup
    }

    /// Текст для системного промпта: glob-строки + append по дереву.
    /// Пустая строка означает «нечего вшивать».
    pub fn prompt_text(&self) -> String {
        let mut out = String::new();
        for line in &self.prompt_lines {
            let t = line.trim();
            if !t.is_empty() {
                out.push_str("- ");
                out.push_str(t);
                out.push('\n');
            }
        }
        let app = self.system_prompt_append.trim();
        if !app.is_empty() {
            if !out.is_empty() {
                out.push('\n');
            }
            out.push_str(app);
            out.push('\n');
        }
        crate::context::truncate_chars(out.trim_end(), FOLDER_RULES_MAX_CHARS).to_string()
    }
}

/// Кэш folder-rules: глобальные glob-правила + конфиги папок по mtime.
#[derive(Debug)]
pub struct FolderRulesCache {
    /// Исходные глобальные правила (для сериализации обратно в JSON).
    pub rules: Vec<FolderRule>,
    /// Скомпилированные паттерны с правилами, отсортированные по приоритету
    /// (высший первым). Компиляция дорогая — делаем один раз при загрузке.
    compiled: Vec<(glob::Pattern, Vec<String>, u32)>,
    /// Путь к файлу глобальных правил (для reload).
    path: PathBuf,
    /// Конфиги папок: путь к файлу → (mtime, конфиг). mtime хранится, чтобы
    /// правка файла подхватывалась без сигнала от watcher.
    dir_cache: HashMap<PathBuf, (Option<SystemTime>, FolderRulesConfig)>,
}

impl FolderRulesCache {
    /// Загрузить глобальные правила из JSON-файла. Отсутствующий файл —
    /// пустой кэш (не ошибка: правила опциональны).
    pub fn load(rules_path: &Path) -> Self {
        let rules = match std::fs::read_to_string(rules_path) {
            Ok(text) => match serde_json::from_str::<Vec<FolderRule>>(&text) {
                Ok(r) => r,
                Err(e) => {
                    eprintln!("folder_rules: не распарсил {rules_path:?}: {e}");
                    Vec::new()
                }
            },
            Err(_) => Vec::new(),
        };
        Self::from_rules(rules, rules_path.to_path_buf())
    }

    /// Создать кэш из списка глобальных правил (без чтения файла).
    pub fn from_rules(rules: Vec<FolderRule>, path: PathBuf) -> Self {
        let mut compiled: Vec<(glob::Pattern, Vec<String>, u32)> = rules
            .iter()
            .filter_map(|r| match glob::Pattern::new(&r.pattern) {
                Ok(p) => Some((p, r.rules.clone(), r.priority)),
                Err(e) => {
                    eprintln!("folder_rules: битый паттерн {:?}: {e}", r.pattern);
                    None
                }
            })
            .collect();
        // Сортировка: высший приоритет первым. При равном — сохраняем
        // порядок из файла (stable sort).
        compiled.sort_by_key(|a| std::cmp::Reverse(a.2));
        Self {
            rules,
            compiled,
            path,
            dir_cache: HashMap::new(),
        }
    }

    /// Перечитать глобальные правила и сбросить кэш конфигов папок.
    pub fn reload(&mut self) {
        let path = self.path.clone();
        *self = Self::load(&path);
    }

    /// Найти все глобальные glob-правила, совпадающие с путём. Возвращает
    /// объединённый список строк, отсортированный по приоритету (высший
    /// первым), при равном приоритете — в порядке следования в файле.
    pub fn match_path(&self, path: &str) -> Vec<String> {
        let mut out: Vec<(u32, usize, String)> = Vec::new();
        for (idx, (pat, rules, prio)) in self.compiled.iter().enumerate() {
            if pat.matches(path) {
                for rule in rules {
                    out.push((*prio, idx, rule.clone()));
                }
            }
        }
        out.sort_by(|a, b| b.0.cmp(&a.0).then_with(|| a.1.cmp(&b.1)));
        out.into_iter().map(|(_, _, r)| r).collect()
    }

    /// Путь к файлу глобальных правил.
    pub fn path(&self) -> &Path {
        &self.path
    }

    /// Количество загруженных глобальных правил.
    pub fn len(&self) -> usize {
        self.rules.len()
    }

    /// Пуст ли кэш глобальных правил.
    pub fn is_empty(&self) -> bool {
        self.rules.is_empty()
    }

    /// Прочитать конфиг папки с кэшем по mtime. `None` — файла нет.
    fn dir_config(&mut self, file: &Path) -> Option<FolderRulesConfig> {
        let mtime = std::fs::metadata(file).and_then(|m| m.modified()).ok();
        if let Some((cached_mt, cfg)) = self.dir_cache.get(file) {
            if *cached_mt == mtime {
                return Some(cfg.clone());
            }
        }
        let cfg = load_dir_config(file)?;
        self.dir_cache
            .insert(file.to_path_buf(), (mtime, cfg.clone()));
        Some(cfg)
    }

    /// Обойти дерево от `work_dir` вверх до корня ФС, собрать и слить правила.
    ///
    /// Порядок обхода — от листа к корню; `system_prompt_append` при этом
    /// конкатенируется в обратном порядке (от корня к листу), чтобы более
    /// конкретные инструкции шли последними и читались моделью как уточнение.
    pub fn resolve(&mut self, work_dir: &Path) -> ResolvedFolderRules {
        let mut res = ResolvedFolderRules::default();

        // 1. Глобальные glob-правила: матчим абсолютный путь и путь
        //    относительно корня проекта (ближайший предок с .git/.swagcod).
        let abs = normalize(work_dir);
        let mut seen: Vec<String> = Vec::new();
        for candidate in glob_candidates(work_dir, &abs) {
            for line in self.match_path(&candidate) {
                if !seen.contains(&line) {
                    seen.push(line.clone());
                    res.prompt_lines.push(line);
                }
            }
        }

        // 2. Дерево папок: собираем уровни от листа к корню.
        let mut levels: Vec<FolderRulesConfig> = Vec::new();
        let mut dir = Some(work_dir.to_path_buf());
        while let Some(d) = dir {
            let json = d.join(SWAGCOD_DIR).join(RULES_JSON);
            let md = d.join(SWAGCOD_DIR).join(RULES_MD);
            /* JSON приоритетнее MD — тот же порядок, что в `dir_rules_file` у
            команд UI. В `sources` кладём ИМЕННО применённый файл: владелец
            читает этот список, чтобы понять, откуда прилетел запрет, и путь к
            несуществующему rules.json вместо настоящего rules.md увёл бы его
            править не тот файл. */
            let loaded = match self.dir_config(&json) {
                Some(cfg) => Some((json, cfg)),
                None => self.dir_config(&md).map(|cfg| (md, cfg)),
            };
            if let Some((file, cfg)) = loaded {
                if !cfg.is_noop() {
                    res.sources.push(file);
                    levels.push(cfg);
                }
            }
            dir = d.parent().map(|p| p.to_path_buf());
        }

        // 3. Слияние. Списки — union от корня к листу (дальние правила
        //    перечисляются первыми, ближе к концу — более конкретные).
        for cfg in levels.iter().rev() {
            merge_into(&mut res, cfg);
        }
        // session_ttl_secs: ближайший непустой уровень побеждает.
        res.session_ttl_secs = levels.iter().find_map(|c| c.session_ttl_secs);
        // exempt_from_cleanup: true, если выставлен на любом уровне.
        res.exempt_from_cleanup = levels.iter().any(|c| c.exempt_from_cleanup);

        res
    }
}

/// Слить один конфиг в результат: списки объединяются без дублей,
/// append конкатенируется.
fn merge_into(res: &mut ResolvedFolderRules, cfg: &FolderRulesConfig) {
    /* allow/deny — regex-строки, у которых значим любой пробел, включая
    хвостовой: `^rm ` матчит команду rm с аргументом и НЕ матчит rmdir.
    Трим здесь молча расширил бы запрет на чужие команды, поэтому паттерны
    уходят дословно, а отбрасываются только целиком пробельные строки. */
    extend_raw(&mut res.allow, &cfg.allow);
    extend_raw(&mut res.deny, &cfg.deny);
    // disabled_* — имена, а не паттерны: их trim только приводит ввод в UI.
    extend_unique(&mut res.disabled_mcp, &cfg.disabled_mcp);
    extend_unique(&mut res.disabled_plugins, &cfg.disabled_plugins);
    extend_unique(&mut res.disabled_skills, &cfg.disabled_skills);
    if let Some(txt) = cfg.system_prompt_append.as_ref() {
        let t = txt.trim();
        if !t.is_empty() {
            if !res.system_prompt_append.is_empty() {
                res.system_prompt_append.push('\n');
            }
            res.system_prompt_append.push_str(t);
        }
    }
}

/// Дописать строки дословно (порядок источника сохраняется), пропуская
/// пустые и целиком пробельные.
fn extend_raw(dst: &mut Vec<String>, src: &[String]) {
    for s in src {
        if !s.trim().is_empty() && !dst.iter().any(|d| d == s) {
            dst.push(s.clone());
        }
    }
}

/// Дописать имена, которых ещё нет: trim + дедупликация без учёта порядка.
fn extend_unique(dst: &mut Vec<String>, src: &[String]) {
    for s in src {
        let t = s.trim();
        if !t.is_empty() && !dst.iter().any(|d| d == t) {
            dst.push(t.to_string());
        }
    }
}

/// Прочитать конфиг папки: JSON приоритетнее MD. Битый JSON — не ошибка хода,
/// а честный лог и пустой конфиг: одна опечатка в файле не должна оставлять
/// сессию без правил вовсе.
fn load_dir_config(file: &Path) -> Option<FolderRulesConfig> {
    let text = std::fs::read_to_string(file).ok()?;
    if file.extension().and_then(|e| e.to_str()) == Some("md") {
        let t = text.trim();
        if t.is_empty() {
            return None;
        }
        return Some(FolderRulesConfig {
            system_prompt_append: Some(t.to_string()),
            ..Default::default()
        });
    }
    match serde_json::from_str::<FolderRulesConfig>(&text) {
        Ok(c) => Some(c),
        Err(e) => {
            eprintln!("folder_rules: не распарсил {file:?}: {e}");
            None
        }
    }
}

/// Нормализовать путь для glob: единый разделитель `/`, без trailing slash.
pub fn normalize(p: &Path) -> String {
    let s = p.to_string_lossy().replace('\\', "/");
    let trimmed = s.trim_end_matches('/');
    if trimmed.is_empty() {
        s.clone()
    } else {
        trimmed.to_string()
    }
}

/// Варианты пути для матчинга глобальных glob-правил: абсолютный
/// нормализованный и относительный от корня проекта (ближайший предок с
/// `.git` или `.swagcod`). Относительная форма позволяет писать естественные
/// паттерны вроде `crates/**` вместо `**/SwagCod/crates/**`.
fn glob_candidates(work_dir: &Path, abs: &str) -> Vec<String> {
    let mut out = vec![abs.to_string()];
    if let Some(root) = project_root(work_dir) {
        if let Ok(rel) = work_dir.strip_prefix(&root) {
            let r = normalize(rel);
            if !r.is_empty() && !r.ends_with('.') && !out.contains(&r) {
                out.push(r);
            }
        }
    }
    out
}

/// Корень проекта: ближайший предок (включая сам путь) с `.git` или `.swagcod`.
pub fn project_root(start: &Path) -> Option<PathBuf> {
    let mut d = Some(start.to_path_buf());
    while let Some(cur) = d {
        if cur.join(".git").exists() || cur.join(SWAGCOD_DIR).exists() {
            return Some(cur);
        }
        d = cur.parent().map(|p| p.to_path_buf());
    }
    None
}

/// Дефолтный путь к файлу глобальных правил: `~/.swagcod/folder_rules.json`.
pub fn default_rules_path() -> PathBuf {
    let base = std::env::var("USERPROFILE")
        .or_else(|_| std::env::var("HOME"))
        .unwrap_or_else(|_| ".".to_string());
    PathBuf::from(base)
        .join(SWAGCOD_DIR)
        .join("folder_rules.json")
}

#[cfg(test)]
mod tests {
    use super::*;

    fn make_cache(rules: Vec<FolderRule>) -> FolderRulesCache {
        FolderRulesCache::from_rules(rules, PathBuf::from("/dev/null"))
    }

    fn rule(pattern: &str, lines: &[&str], priority: u32) -> FolderRule {
        FolderRule {
            pattern: pattern.into(),
            rules: lines.iter().map(|s| s.to_string()).collect(),
            priority,
        }
    }

    #[test]
    fn test_load_from_json() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("rules.json");
        let json = serde_json::json!([
            {"pattern": "src/**/*.rs", "rules": ["no unsafe", "use clippy"], "priority": 10},
            {"pattern": "*.md", "rules": ["check spelling"], "priority": 5}
        ]);
        std::fs::write(&path, serde_json::to_string_pretty(&json).unwrap()).unwrap();
        let cache = FolderRulesCache::load(&path);
        assert_eq!(cache.len(), 2);
    }

    #[test]
    fn test_glob_matching() {
        let cache = make_cache(vec![
            rule("src/**/*.rs", &["rust rule"], 1),
            rule("*.md", &["md rule"], 1),
        ]);
        assert_eq!(cache.match_path("src/main.rs"), vec!["rust rule"]);
        assert_eq!(cache.match_path("README.md"), vec!["md rule"]);
        assert_eq!(cache.match_path("src/lib/foo.rs"), vec!["rust rule"]);
        assert!(cache.match_path("Cargo.toml").is_empty());
    }

    #[test]
    fn test_priority_ordering() {
        let cache = make_cache(vec![
            rule("**/*.rs", &["low"], 1),
            rule("src/**/*.rs", &["high"], 10),
            rule("src/lib/**", &["mid"], 5),
        ]);
        assert_eq!(
            cache.match_path("src/lib/foo.rs"),
            vec!["high", "mid", "low"]
        );
    }

    #[test]
    fn test_equal_priority_preserves_file_order() {
        let cache = make_cache(vec![
            rule("**/*.rs", &["first"], 5),
            rule("**/*.rs", &["second"], 5),
        ]);
        assert_eq!(cache.match_path("main.rs"), vec!["first", "second"]);
    }

    #[test]
    fn test_empty_rules() {
        let cache = make_cache(vec![]);
        assert!(cache.is_empty());
        assert!(cache.match_path("anything.rs").is_empty());
    }

    #[test]
    fn test_missing_file_gives_empty_cache() {
        let cache = FolderRulesCache::load(Path::new("/nonexistent/path/rules.json"));
        assert!(cache.is_empty());
    }

    #[test]
    fn test_reload() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("rules.json");
        std::fs::write(
            &path,
            serde_json::to_string(&serde_json::json!([
                {"pattern": "*.rs", "rules": ["v1"], "priority": 1}
            ]))
            .unwrap(),
        )
        .unwrap();

        let mut cache = FolderRulesCache::load(&path);
        assert_eq!(cache.match_path("foo.rs"), vec!["v1"]);

        std::fs::write(
            &path,
            serde_json::to_string(&serde_json::json!([
                {"pattern": "*.rs", "rules": ["v2"], "priority": 1}
            ]))
            .unwrap(),
        )
        .unwrap();
        cache.reload();
        assert_eq!(cache.match_path("foo.rs"), vec!["v2"]);
    }

    #[test]
    fn test_bad_pattern_skipped() {
        let cache = make_cache(vec![
            rule("[invalid", &["bad"], 1),
            rule("*.rs", &["good"], 1),
        ]);
        assert_eq!(cache.len(), 2); // исходных два
        assert_eq!(cache.match_path("foo.rs"), vec!["good"]);
    }

    // ── Наследование по дереву папок ────────────────────────────────────────

    /// Записать `.swagcod/rules.json` в папку.
    fn put_json(dir: &Path, cfg: serde_json::Value) {
        let d = dir.join(SWAGCOD_DIR);
        std::fs::create_dir_all(&d).unwrap();
        std::fs::write(
            d.join(RULES_JSON),
            serde_json::to_string_pretty(&cfg).unwrap(),
        )
        .unwrap();
    }

    #[test]
    fn test_resolve_empty_tree_is_noop() {
        let dir = tempfile::tempdir().unwrap();
        let mut cache = make_cache(vec![]);
        let res = cache.resolve(dir.path());
        assert!(res.is_noop());
        assert!(res.prompt_text().is_empty());
    }

    #[test]
    fn test_resolve_single_level() {
        let dir = tempfile::tempdir().unwrap();
        put_json(
            dir.path(),
            serde_json::json!({
                "deny": ["rm -rf .*"],
                "disabled_mcp": ["danger"],
                "system_prompt_append": "Пиши тесты.",
                "exempt_from_cleanup": true
            }),
        );
        let mut cache = make_cache(vec![]);
        let res = cache.resolve(dir.path());
        assert_eq!(res.deny, vec!["rm -rf .*"]);
        assert_eq!(res.disabled_mcp, vec!["danger"]);
        assert_eq!(res.system_prompt_append, "Пиши тесты.");
        assert!(res.exempt_from_cleanup);
        assert_eq!(res.sources.len(), 1);
    }

    #[test]
    fn test_resolve_merges_tree_and_unions_lists() {
        let root = tempfile::tempdir().unwrap();
        let mid = root.path().join("myapp");
        let leaf = mid.join("src");
        std::fs::create_dir_all(&leaf).unwrap();

        put_json(
            root.path(),
            serde_json::json!({ "deny": ["root-deny"], "allow": ["root-allow"] }),
        );
        put_json(
            &mid,
            serde_json::json!({
                "deny": ["mid-deny"],
                "disabled_plugins": ["shout"],
                "system_prompt_append": "Уровень проекта."
            }),
        );
        // leaf без правил — не ломает обход.

        let mut cache = make_cache(vec![]);
        let res = cache.resolve(&leaf);

        // Union: корневые первыми (обход rev от листа к корню → корень раньше).
        assert_eq!(res.deny, vec!["root-deny", "mid-deny"]);
        assert_eq!(res.allow, vec!["root-allow"]);
        assert_eq!(res.disabled_plugins, vec!["shout"]);
        // append: от корня к листу.
        assert_eq!(res.system_prompt_append, "Уровень проекта.");
        assert_eq!(res.sources.len(), 2);
    }

    #[test]
    fn test_prompt_append_concatenates_root_to_leaf() {
        let root = tempfile::tempdir().unwrap();
        let leaf = root.path().join("sub");
        std::fs::create_dir_all(&leaf).unwrap();
        put_json(
            root.path(),
            serde_json::json!({ "system_prompt_append": "КОРЕНЬ" }),
        );
        put_json(&leaf, serde_json::json!({ "system_prompt_append": "ЛИСТ" }));
        let mut cache = make_cache(vec![]);
        let res = cache.resolve(&leaf);
        assert_eq!(res.system_prompt_append, "КОРЕНЬ\nЛИСТ");
    }

    #[test]
    fn test_ttl_takes_nearest_non_empty() {
        let root = tempfile::tempdir().unwrap();
        let leaf = root.path().join("sub");
        std::fs::create_dir_all(&leaf).unwrap();
        put_json(root.path(), serde_json::json!({ "session_ttl_secs": 100 }));
        put_json(&leaf, serde_json::json!({ "session_ttl_secs": 7 }));
        let mut cache = make_cache(vec![]);
        assert_eq!(cache.resolve(&leaf).session_ttl_secs, Some(7));
        assert_eq!(cache.resolve(root.path()).session_ttl_secs, Some(100));
    }

    #[test]
    fn test_exempt_is_true_if_any_level() {
        let root = tempfile::tempdir().unwrap();
        let leaf = root.path().join("sub");
        std::fs::create_dir_all(&leaf).unwrap();
        put_json(
            root.path(),
            serde_json::json!({ "exempt_from_cleanup": true }),
        );
        put_json(&leaf, serde_json::json!({ "deny": ["x"] }));
        let mut cache = make_cache(vec![]);
        assert!(cache.resolve(&leaf).exempt_from_cleanup);
    }

    #[test]
    fn test_rules_md_becomes_prompt_append() {
        let dir = tempfile::tempdir().unwrap();
        let d = dir.path().join(SWAGCOD_DIR);
        std::fs::create_dir_all(&d).unwrap();
        std::fs::write(d.join(RULES_MD), "# Правила\nВсегда гоняй fmt.\n").unwrap();
        let mut cache = make_cache(vec![]);
        let res = cache.resolve(dir.path());
        assert!(res.system_prompt_append.contains("Всегда гоняй fmt."));
        // sources указывает на ПРИМЕНЁННЫЙ файл: rules.json здесь не существует,
        // и путь к нему увёл бы владельца править не тот файл.
        assert_eq!(res.sources.len(), 1);
        assert_eq!(res.sources[0].file_name().unwrap(), RULES_MD);
    }

    #[test]
    fn test_json_wins_over_md() {
        let dir = tempfile::tempdir().unwrap();
        let d = dir.path().join(SWAGCOD_DIR);
        std::fs::create_dir_all(&d).unwrap();
        std::fs::write(d.join(RULES_MD), "из MD").unwrap();
        std::fs::write(
            d.join(RULES_JSON),
            serde_json::to_string(&serde_json::json!({
                "system_prompt_append": "из JSON"
            }))
            .unwrap(),
        )
        .unwrap();
        let mut cache = make_cache(vec![]);
        assert_eq!(cache.resolve(dir.path()).system_prompt_append, "из JSON");
    }

    #[test]
    fn test_broken_json_does_not_break_resolve() {
        let dir = tempfile::tempdir().unwrap();
        let d = dir.path().join(SWAGCOD_DIR);
        std::fs::create_dir_all(&d).unwrap();
        std::fs::write(d.join(RULES_JSON), "{ это не json").unwrap();
        let mut cache = make_cache(vec![]);
        let res = cache.resolve(dir.path());
        assert!(res.is_noop());
    }

    #[test]
    fn test_empty_config_is_not_a_source() {
        let dir = tempfile::tempdir().unwrap();
        put_json(dir.path(), serde_json::json!({}));
        let mut cache = make_cache(vec![]);
        let res = cache.resolve(dir.path());
        assert!(res.is_noop());
        assert!(res.sources.is_empty());
    }

    #[test]
    fn test_dir_config_repicks_up_on_mtime_change() {
        let dir = tempfile::tempdir().unwrap();
        put_json(dir.path(), serde_json::json!({ "deny": ["v1"] }));
        let mut cache = make_cache(vec![]);
        assert_eq!(cache.resolve(dir.path()).deny, vec!["v1"]);

        // Перезапись с явным сдвигом mtime: на некоторых ФС гранулярность
        // секундная, поэтому время выставляем руками.
        let file = dir.path().join(SWAGCOD_DIR).join(RULES_JSON);
        std::fs::write(
            &file,
            serde_json::to_string(&serde_json::json!({ "deny": ["v2"] })).unwrap(),
        )
        .unwrap();
        let fut = std::time::SystemTime::now() + std::time::Duration::from_secs(5);
        let f = std::fs::OpenOptions::new().write(true).open(&file).unwrap();
        f.set_modified(fut).unwrap();

        assert_eq!(cache.resolve(dir.path()).deny, vec!["v2"]);
    }

    // ── Glob-правила против рабочей папки ───────────────────────────────────

    #[test]
    fn test_glob_rule_matches_relative_to_project_root() {
        let root = tempfile::tempdir().unwrap();
        // .git делает tempdir корнем проекта.
        std::fs::create_dir_all(root.path().join(".git")).unwrap();
        let sub = root.path().join("crates").join("core");
        std::fs::create_dir_all(&sub).unwrap();

        let mut cache = make_cache(vec![rule("crates/**", &["core rule"], 1)]);
        let res = cache.resolve(&sub);
        assert_eq!(res.prompt_lines, vec!["core rule"]);
        assert!(res.prompt_text().contains("core rule"));
    }

    #[test]
    fn test_prompt_text_format_and_truncation() {
        let res = ResolvedFolderRules {
            prompt_lines: vec!["первое".into(), "  ".into(), "второе".into()],
            system_prompt_append: "хвост".into(),
            ..Default::default()
        };
        let txt = res.prompt_text();
        assert!(txt.starts_with("- первое\n"));
        assert!(txt.contains("- второе"));
        assert!(txt.ends_with("хвост"));
        assert!(!txt.contains("-  \n"));

        let big = ResolvedFolderRules {
            system_prompt_append: "а".repeat(FOLDER_RULES_MAX_CHARS * 2),
            ..Default::default()
        };
        assert!(big.prompt_text().chars().count() <= FOLDER_RULES_MAX_CHARS + 32);
        assert!(big.prompt_text().contains("[... обрезано ...]"));
    }

    #[test]
    fn test_normalize_unifies_separators() {
        assert_eq!(normalize(Path::new("a\\b\\c")), "a/b/c");
        assert_eq!(normalize(Path::new("a/b/")), "a/b");
        assert_eq!(normalize(Path::new("/")), "/");
    }

    #[test]
    fn test_config_is_noop() {
        assert!(FolderRulesConfig::default().is_noop());
        let exempt = FolderRulesConfig {
            exempt_from_cleanup: true,
            ..Default::default()
        };
        assert!(!exempt.is_noop());
        let appended = FolderRulesConfig {
            system_prompt_append: Some("x".into()),
            ..Default::default()
        };
        assert!(!appended.is_noop());
        // Каждое поле по отдельности делает конфиг значимым.
        let with_ttl = FolderRulesConfig {
            session_ttl_secs: Some(60),
            ..Default::default()
        };
        assert!(!with_ttl.is_noop());
        let with_deny = FolderRulesConfig {
            deny: vec!["x".into()],
            ..Default::default()
        };
        assert!(!with_deny.is_noop());
    }

    #[test]
    fn test_extend_unique_dedups_and_trims() {
        let mut v = vec!["a".to_string()];
        extend_unique(&mut v, &["a".into(), " b ".into(), "".into()]);
        assert_eq!(v, vec!["a", "b"]);
    }

    /// allow/deny — regex, где хвостовой пробел значим: `^rm ` не должен
    /// превратиться в `^rm` (то matчит и rmdir). Текст владельца сохраняется
    /// дословно; отбрасываются только целиком пробельные строки.
    #[test]
    fn test_allow_deny_preserve_whitespace_verbatim() {
        let mut v: Vec<String> = vec!["^ls ".into()];
        extend_raw(
            &mut v,
            &["^ls ".into(), "^rm ".into(), "   ".into(), "".into()],
        );
        assert_eq!(v, vec!["^ls ", "^rm "]);
    }

    /// Имена (disabled_*) тримятся: это идентификаторы, а не паттерны.
    #[test]
    fn test_disabled_names_are_trimmed() {
        let dir = tempfile::tempdir().unwrap();
        let swag = dir.path().join(SWAGCOD_DIR);
        std::fs::create_dir_all(&swag).unwrap();
        std::fs::write(
            swag.join(RULES_JSON),
            serde_json::to_string(&serde_json::json!({
                "deny": ["^rm "],
                "disabled_mcp": ["  fs-server  "],
                "disabled_plugins": ["shout", "shout", ""]
            }))
            .unwrap(),
        )
        .unwrap();
        let mut cache = make_cache(vec![]);
        let res = cache.resolve(dir.path());
        assert_eq!(res.deny, vec!["^rm "], "regex сохранён дословно");
        assert_eq!(res.disabled_mcp, vec!["fs-server"], "имя оттримлено");
        assert_eq!(res.disabled_plugins, vec!["shout"], "дубли и пустые убраны");
    }

    /// Совпадение паттерна без учёта регистра — для имён, а не для regex.
    #[test]
    fn test_resolve_is_deterministic_across_calls() {
        let dir = tempfile::tempdir().unwrap();
        put_json(
            dir.path(),
            serde_json::json!({"deny": ["^x"], "allow": ["^y"], "disabled_skills": ["deploy"]}),
        );
        let mut cache = make_cache(vec![]);
        let a = cache.resolve(dir.path());
        let b = cache.resolve(dir.path());
        assert_eq!(a, b, "повторный резолв обязан дать тот же результат");
        assert_eq!(a.deny, vec!["^x"]);
        assert_eq!(a.allow, vec!["^y"]);
        assert_eq!(a.disabled_skills, vec!["deploy"]);
    }
}

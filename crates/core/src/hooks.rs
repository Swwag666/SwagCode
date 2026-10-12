/*! Hooks: команды владельца на границах хода и инструмента (F-8).

Политика подтверждений ([`crate::turn::ApprovalPolicy`]) отвечает на вопрос
«спрашивать ли человека», а списки команд ([`crate::rules`]) — «разрешён ли
вызов». Оба про правила, которые знает ядро. Хуки закрывают третий случай:
правило знает только владелец, и выражается оно внешней командой —
«перед ходом проверь, что ветка чистая», «не трогать `dist/`», «после хода
прогони линтер».

Ядро трактует хук как решение по коду возврата:

- `pre_turn` — до первого запроса модели. Ненулевой код означает «ход не
  начинается»: охранник, который не может остановить ход, бесполезен.
- `pre_tool` — до исполнения конкретного инструмента (отбор по regex имени).
  Ненулевой код означает «вызов отклонён», а stderr хука уходит модели как
  причина: ход продолжается, модель может сделать иначе.
- `post_turn` — после записи хода. Код возврата здесь ни на что не влияет
  (ход уже случился), поэтому он только логируется: ругаться на владельца
  за упавший линтер, запретив ему видеть результат, было бы самонадеянно.

Конфигурация живёт в prefs (ключ [`HOOKS_PREF`], JSON) и читается на каждый
ход — правка в настройках действует без перезапуска приложения.

## Контракт вызова

Хук исполняется оболочкой (`cmd /C` на Windows) с рабочей директорией сессии
и переменными окружения:

| Переменная        | Содержимое                                        |
|-------------------|---------------------------------------------------|
| `SWAGCOD_SESSION` | id сессии                                          |
| `SWAGCOD_EVENT`   | `pre_turn` \| `post_turn` \| `pre_tool`            |
| `SWAGCOD_TOOL`    | имя инструмента (пусто для событий хода)           |
| `SWAGCOD_CWD`     | рабочая директория сессии                          |
| `SWAGCOD_OK`      | `true`/`false` — успешен ли ход; задана всегда, вне `post_turn` равна `na` |

stdin не передаётся: хук, ждущий ввода, повис бы на все 15 секунд таймаута.

Исполнение (процессы, таймауты, убийство дерева) живёт в `crates/app`,
потому что ядро не зависит от платформы (D-011); здесь — только разбор,
проверка и отбор, то есть чистая логика, которую можно прогнать headless.
!*/

use regex::Regex;
use serde::{Deserialize, Serialize};

/// Prefs-ключ конфигурации хуков (JSON [`HooksConfig`]).
pub const HOOKS_PREF: &str = "hooks";

/// Таймаут одного хука в секундах. Хук стоит в критическом пути хода:
/// 15 секунд достаточно для `git status` или линтера одного файла и
/// мало для «собрать проект» — такие вещи делают фоном.
pub const HOOK_TIMEOUT_SECS: u64 = 15;

/// Предел числа хуков на одно событие. Каждый может съесть до
/// [`HOOK_TIMEOUT_SECS`], поэтому очередь хуков не должна иметь возможность
/// остановить ход на минуты.
pub const HOOKS_PER_EVENT: usize = 8;

/// Предел длины одной команды: конфигурация — не место для скрипта на 100 КБ,
/// а длинную команду всё равно удобнее положить в файл и вызвать его.
pub const HOOK_CMD_MAX_CHARS: usize = 2000;

/// Имена событий (они же значения `SWAGCOD_EVENT`).
pub const EVENT_PRE_TURN: &str = "pre_turn";
pub const EVENT_POST_TURN: &str = "post_turn";
pub const EVENT_PRE_TOOL: &str = "pre_tool";

/// Имена переменных окружения хука — контракт с командами владельца,
/// поэтому вынесен в константы: переименование молча сломало бы его скрипты.
pub const ENV_SESSION: &str = "SWAGCOD_SESSION";
pub const ENV_EVENT: &str = "SWAGCOD_EVENT";
pub const ENV_TOOL: &str = "SWAGCOD_TOOL";
pub const ENV_CWD: &str = "SWAGCOD_CWD";
pub const ENV_OK: &str = "SWAGCOD_OK";

/// Хук перед инструментом: regex по имени + команда.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PreToolHook {
    /// Regex по имени инструмента, регистр не учитывается, совпадение по
    /// подстроке (якоря `^…$` пишет владелец): `^bash$`, `write|patch`.
    #[serde(rename = "match")]
    pub match_re: String,
    /// Команда оболочки. Код возврата 0 — пропустить вызов, иначе отклонить.
    pub cmd: String,
}

/// Конфигурация хуков сессии приложения.
///
/// `deny_unknown_fields` намеренно: опечатка `"pretool"` не должна молча
/// превращать охранник в отсутствующий.
#[derive(Debug, Clone, PartialEq, Eq, Default, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct HooksConfig {
    #[serde(default)]
    pub pre_turn: Vec<String>,
    #[serde(default)]
    pub post_turn: Vec<String>,
    #[serde(default)]
    pub pre_tool: Vec<PreToolHook>,
}

impl HooksConfig {
    /// Пустая конфигурация: хуков нет, ничего не исполняется.
    pub fn is_empty(&self) -> bool {
        self.pre_turn.is_empty() && self.post_turn.is_empty() && self.pre_tool.is_empty()
    }

    /// Хуки `pre_tool`, относящиеся к этому инструменту.
    ///
    /// Битый regex пропускается (не матчит ничего): после [`parse_hooks`]
    /// таких в конфигурации быть не может, а падать ходом из-за испорченного
    /// prefs нельзя. Молчаливость здесь безопаснее обратной: «совпало»
    /// отклонило бы вызов, которого владелец не ограничивал.
    pub fn matching_pre_tool(&self, tool: &str) -> Vec<&PreToolHook> {
        self.pre_tool
            .iter()
            .filter(|h| {
                Regex::new(&format!("(?i){}", h.match_re))
                    .map(|re| re.is_match(tool))
                    .unwrap_or(false)
            })
            .collect()
    }

    /// Проверить конфигурацию и вернуть человекочитаемые ошибки.
    ///
    /// Отдельный метод, а не часть [`parse_hooks`], потому что его же зовёт
    /// UI-валидация: редактор показывает ошибки до сохранения.
    pub fn validate(&self) -> Vec<String> {
        let mut errs = Vec::new();
        check_cmds(&self.pre_turn, EVENT_PRE_TURN, &mut errs);
        check_cmds(&self.post_turn, EVENT_POST_TURN, &mut errs);
        if self.pre_tool.len() > HOOKS_PER_EVENT {
            errs.push(format!(
                "{EVENT_PRE_TOOL}: {} хуков при пределе {HOOKS_PER_EVENT}",
                self.pre_tool.len()
            ));
        }
        for (i, h) in self.pre_tool.iter().enumerate() {
            if h.match_re.trim().is_empty() {
                errs.push(format!("{EVENT_PRE_TOOL}[{i}]: пустой match"));
            } else if let Err(e) = Regex::new(&format!("(?i){}", h.match_re)) {
                errs.push(format!("{EVENT_PRE_TOOL}[{i}]: битый regex: {e}"));
            }
            if let Err(e) = check_cmd(&h.cmd) {
                errs.push(format!("{EVENT_PRE_TOOL}[{i}]: {e}"));
            }
        }
        errs
    }
}

fn check_cmds(cmds: &[String], event: &str, errs: &mut Vec<String>) {
    if cmds.len() > HOOKS_PER_EVENT {
        errs.push(format!(
            "{event}: {} хуков при пределе {HOOKS_PER_EVENT}",
            cmds.len()
        ));
    }
    for (i, c) in cmds.iter().enumerate() {
        if let Err(e) = check_cmd(c) {
            errs.push(format!("{event}[{i}]: {e}"));
        }
    }
}

fn check_cmd(cmd: &str) -> Result<(), String> {
    if cmd.trim().is_empty() {
        return Err("пустая команда".into());
    }
    if cmd.chars().count() > HOOK_CMD_MAX_CHARS {
        return Err(format!(
            "команда длиннее {HOOK_CMD_MAX_CHARS} символов: вынесите её в файл"
        ));
    }
    Ok(())
}

/// Разобрать JSON конфигурации из prefs.
///
/// Пустая строка — валидная «хуков нет». Ошибка возвращается текстом для
/// человека: её показывает редактор настроек при сохранении, поэтому
/// сохранение битой конфигурации невозможно (тот же принцип, что у списков
/// команд F-3: битое правило не должно молча применяться).
pub fn parse_hooks(raw: &str) -> Result<HooksConfig, String> {
    if raw.trim().is_empty() {
        return Ok(HooksConfig::default());
    }
    let cfg: HooksConfig = serde_json::from_str(raw).map_err(|e| format!("hooks: json: {e}"))?;
    let errs = cfg.validate();
    if !errs.is_empty() {
        return Err(errs.join("; "));
    }
    Ok(cfg)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn empty_pref_means_no_hooks() {
        let cfg = parse_hooks("").unwrap();
        assert!(cfg.is_empty());
        assert!(cfg.pre_turn.is_empty() && cfg.pre_tool.is_empty());
        // Пробелы и переводы строк — тоже «нет хуков».
        assert!(parse_hooks("  \n ").unwrap().is_empty());
    }

    #[test]
    fn parses_full_config_from_json() {
        let raw = r#"{
            "pre_turn": ["git status --porcelain"],
            "post_turn": ["cargo fmt --check", "pwsh -File tools/report.ps1"],
            "pre_tool": [
                {"match": "^bash$", "cmd": "tools\\guard-bash.cmd"},
                {"match": "write|patch", "cmd": "tools\\guard-write.cmd"}
            ]
        }"#;
        let cfg = parse_hooks(raw).unwrap();
        assert_eq!(cfg.pre_turn, vec!["git status --porcelain"]);
        assert_eq!(cfg.post_turn.len(), 2);
        assert_eq!(cfg.pre_tool.len(), 2);
        assert_eq!(cfg.pre_tool[0].match_re, "^bash$");
        assert!(!cfg.is_empty());
    }

    #[test]
    fn partial_config_fills_missing_events_with_defaults() {
        // Владелец настроил только pre_tool: остальные события пустые, а не
        // ошибка — требовать все три ключа значило бы мешать настройке.
        let cfg = parse_hooks(r#"{"pre_tool":[{"match":"^write$","cmd":"g.cmd"}]}"#).unwrap();
        assert!(cfg.pre_turn.is_empty());
        assert!(cfg.post_turn.is_empty());
        assert_eq!(cfg.pre_tool.len(), 1);
        assert_eq!(cfg.validate(), Vec::<String>::new());
    }

    /// Опечатка в имени события не должна молча убирать охранник: неизвестные
    /// поля отвергаются, и человек видит причину.
    #[test]
    fn unknown_field_is_rejected() {
        let e = parse_hooks(r#"{"pretool":[{"match":"^write$","cmd":"g.cmd"}]}"#).unwrap_err();
        assert!(e.contains("unknown field"), "{e}");
        let e = parse_hooks(r#"{"pre_turn":["x"],"extra":1}"#).unwrap_err();
        assert!(e.contains("unknown field"), "{e}");
    }

    #[test]
    fn broken_json_is_rejected_with_reason() {
        let e = parse_hooks("{ pre_turn: [").unwrap_err();
        assert!(e.contains("json"), "{e}");
    }

    #[test]
    fn validation_rejects_empty_and_overlong_commands() {
        let e = parse_hooks(r#"{"pre_turn":["  "]}"#).unwrap_err();
        assert!(e.contains("пустая команда"), "{e}");
        let e = parse_hooks(r#"{"post_turn":[""]}"#).unwrap_err();
        assert!(e.contains("пустая команда"), "{e}");
        let long = "x".repeat(HOOK_CMD_MAX_CHARS + 1);
        let e = parse_hooks(&format!(r#"{{"pre_turn":["{long}"]}}"#)).unwrap_err();
        assert!(e.contains("длиннее"), "{e}");
    }

    #[test]
    fn validation_rejects_broken_regex_and_empty_match() {
        let e = parse_hooks(r#"{"pre_tool":[{"match":"([","cmd":"g.cmd"}]}"#).unwrap_err();
        assert!(e.contains("битый regex"), "{e}");
        let e = parse_hooks(r#"{"pre_tool":[{"match":"","cmd":"g.cmd"}]}"#).unwrap_err();
        assert!(e.contains("пустой match"), "{e}");
        let e = parse_hooks(r#"{"pre_tool":[{"match":"^write$","cmd":" "}]}"#).unwrap_err();
        assert!(e.contains("пустая команда"), "{e}");
    }

    #[test]
    fn validation_caps_number_of_hooks_per_event() {
        let cmds: Vec<String> = (0..HOOKS_PER_EVENT + 1)
            .map(|i| format!("echo {i}"))
            .collect();
        let cfg = HooksConfig {
            pre_turn: cmds.clone(),
            post_turn: Vec::new(),
            pre_tool: Vec::new(),
        };
        let errs = cfg.validate();
        assert!(
            errs.iter().any(|e| e.contains("пределе")),
            "предел числа хуков не сработал: {errs:?}"
        );
        // Ровно предел — ещё можно.
        let ok = HooksConfig {
            pre_turn: cmds[..HOOKS_PER_EVENT].to_vec(),
            ..Default::default()
        };
        assert_eq!(ok.validate(), Vec::<String>::new());
    }

    #[test]
    fn matching_is_case_insensitive_substring() {
        let cfg = parse_hooks(
            r#"{"pre_tool":[
                {"match":"^bash$","cmd":"a.cmd"},
                {"match":"write|patch","cmd":"b.cmd"},
                {"match":"^memory_","cmd":"c.cmd"}
            ]}"#,
        )
        .unwrap();
        let names = |tool: &str| -> Vec<String> {
            cfg.matching_pre_tool(tool)
                .iter()
                .map(|h| h.cmd.to_string())
                .collect()
        };
        assert_eq!(names("bash"), vec!["a.cmd"]);
        // Регистр не учитывается: имена инструментов приходят от провайдера.
        assert_eq!(names("BASH"), vec!["a.cmd"]);
        assert_eq!(names("write"), vec!["b.cmd"]);
        assert_eq!(names("patch"), vec!["b.cmd"]);
        assert_eq!(names("memory_add"), vec!["c.cmd"]);
        assert!(names("read").is_empty(), "read не ограничен");
        // ^bash$ не должен ловить «my_bash_tool»: якоря владельца работают.
        assert!(names("my_bash_tool").is_empty());
    }

    #[test]
    fn broken_regex_in_stored_config_matches_nothing() {
        // parse_hooks такое не пропускает; проверяем защиту на случай ручной
        // правки prefs: испорченный regex не отклоняет всё подряд и не роняет ход.
        let cfg = HooksConfig {
            pre_tool: vec![
                PreToolHook {
                    match_re: "([".into(),
                    cmd: "bad.cmd".into(),
                },
                PreToolHook {
                    match_re: "^read$".into(),
                    cmd: "ok.cmd".into(),
                },
            ],
            ..Default::default()
        };
        let matched: Vec<&str> = cfg
            .matching_pre_tool("read")
            .iter()
            .map(|h| h.cmd.as_str())
            .collect();
        assert_eq!(matched, vec!["ok.cmd"], "{matched:?}");
    }
}

/*! Allow/deny-списки команд для движка подтверждений (F-3).

Политика подтверждений ([`crate::turn::ApprovalPolicy`]) отвечает на вопрос
«спрашивать ли человека», но она грубая: `OnDangerous` спрашивает на любом
`bash`, даже на `git status`. Списки команд уточняют её без снижения
безопасности:

- `deny` — вызов не исполняется никогда, инструмент возвращает ошибку
  (диалога нет: человек уже принял решение, записав правило);
- `allow` — вызов исполняется без диалога;
- иначе — решает политика.

Порядок строгий: **deny > allow > политика**. Deny побеждает, потому что
список запретов — это граница, которую пользователь поставил осознанно;
allow-список, перекрывающий deny, превратил бы запрет в рекомендацию.

Решение живёт в ядре, а не в UI (тот же принцип, что у политики — D-002):
обойти списки подменой обработчика во фронтенде нельзя.

## Что именно матчится

[`rule_subject`] выбирает предмет правила предсказуемо:

- у инструментов-оболочек (`bash`, `pwsh`, `shell`, …) — текст команды из
  аргумента `command`, потому что правила вида `^git (status|log)` должны
  читаться как команды, а не как JSON;
- у остальных инструментов — имя инструмента, так что правило `^write$`
  запрещает запись целиком.

Регистр не учитывается (Windows-команды регистронезависимы), совпадение
ищется по подстрокам — якоря `^…$` пишет сам пользователь. Битый regex
правилом не становится: он возвращается в [`CommandRules::errors`] и
пропускается, молчаливое «считаем, что совпало» здесь недопустимо.
!*/

use regex::Regex;
use swagcod_provider::types::ToolCall;

/// Дефолтный allow-список: read-only команды, которые гоняют на каждом ходу.
///
/// Осознанно узкий: сюда входят только команды, не меняющие ни репозиторий,
/// ни систему. `git commit` или `cargo run` сюда не попадают — у них есть
/// побочные эффекты, и авто-одобрение было бы неожиданностью.
pub const DEFAULT_ALLOW: &[&str] = &[
    // Read-only git. Деструктивные формы git закрыты deny-списком ниже:
    // deny проверяется раньше, поэтому `git branch -D` не пройдёт молча.
    r"^git (status|log|diff|show|rev-parse|branch)\b",
    r"^(ls|dir|pwd|echo|type|cat|head|tail|wc|where|which)\b",
    // cargo test/clippy/fmt/check — то, что агент гоняет на каждом ходу.
    // `cargo run`/`build` сюда не входят осознанно: они исполняют код.
    r"^cargo (test|clippy|fmt|check)\b",
    r"^(node|rustc|cargo|git|python|py|rustup) (--version|-V|show)\b",
];

/// Дефолтный deny-список: необратимое разрушение системы и обход оболочки.
pub const DEFAULT_DENY: &[&str] = &[
    // Зачистка корня или текущего каталога: `rm` с любым набором флагов,
    // цель которого `/`, `/*` или `.` (но не `./build`).
    r"\brm\s+(-\S+\s+)*/(\s|$|\*)",
    r"\brm\s+(-\S+\s+)*\.(\s|$)",
    r"\b(rd|rmdir)\s+/s\b",
    r"\b(del|erase)\s+/[sq]\b",
    // Форматирование и низкоуровневая запись на диск.
    r"\bformat(\.com)?\s+[a-z]:",
    r"\bmkfs(\.[a-z0-9]+)?\s",
    r"\bdd\s+if=",
    r">\s*/dev/sd[a-z]",
    // Выключение/перезапуск машины из-под агента.
    r"\bshutdown\b",
    r"\brestart-computer\b",
    r"\binit\s+0\b",
    // Pipe в интерпретатор: что именно исполнится, по команде не видно.
    r"\|\s*(powershell|pwsh|cmd|sh|bash|zsh)\b",
    // Fork-бомба.
    r":\(\)\s*\{\s*:\|:&\s*\}",
    // Деструктивный git: перекрывает allow-список (deny проверяется первым).
    r"\bgit\s+push\b[^|;&]*(--force\b|-f\b)",
    r"\bgit\s+(reset\s+--hard|clean\s+-\S*f|branch\s+-[dD]\b|update-ref\s+-d\b)",
    r"\bgit\s+checkout\s+\.(\s|$)",
];

/// Одно правило: исходная строка (для журнала и отладки) + собранный regex.
#[derive(Debug, Clone)]
pub struct Rule {
    /// Текст правила, как его ввёл пользователь.
    pub source: String,
    re: Regex,
}

impl Rule {
    /// Собрать правило из строки. Регистр игнорируется.
    pub fn new(source: &str) -> Result<Self, regex::Error> {
        // (?i) префиксом, а не опцией RegexBuilder: пользователь может сам
        // написать флаг в строке, и два (?i) не конфликтуют.
        let re = Regex::new(&format!("(?i){source}"))?;
        Ok(Self {
            source: source.to_string(),
            re,
        })
    }

    fn is_match(&self, subject: &str) -> bool {
        self.re.is_match(subject)
    }
}

/// Вердикт списков по вызову инструмента.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum RuleVerdict {
    /// Совпало правило deny: вызов запрещён, `rule` — текст правила.
    Deny { rule: String },
    /// Совпало правило allow: вызов исполняется без подтверждения.
    Allow { rule: String },
    /// Списки молчат: решает политика подтверждений.
    Pass,
}

/// Allow/deny-списки в собранном виде.
///
/// Пустые списки валидны и означают «правил нет» — всё решает политика.
#[derive(Debug, Clone, Default)]
pub struct CommandRules {
    allow: Vec<Rule>,
    deny: Vec<Rule>,
    errors: Vec<String>,
}

impl CommandRules {
    /// Дефолты из [`DEFAULT_ALLOW`] / [`DEFAULT_DENY`].
    pub fn defaults() -> Self {
        Self::from_lines(DEFAULT_ALLOW, DEFAULT_DENY)
    }

    /// Собрать из строк. Битые regex не роняют сборку: строка попадает в
    /// [`CommandRules::errors`], правилом не становится.
    pub fn from_lines<I, J, A, D>(allow: I, deny: J) -> Self
    where
        I: IntoIterator<Item = A>,
        J: IntoIterator<Item = D>,
        A: AsRef<str>,
        D: AsRef<str>,
    {
        let mut errors = Vec::new();
        let allow = compile(allow, "allow", &mut errors);
        let deny = compile(deny, "deny", &mut errors);
        Self {
            allow,
            deny,
            errors,
        }
    }

    /// Строки, которые не собрались в regex: UI показывает их честно,
    /// вместо того чтобы делать вид, что правило работает.
    pub fn errors(&self) -> &[String] {
        &self.errors
    }

    /// Сколько правил собрано (allow + deny).
    pub fn len(&self) -> usize {
        self.allow.len() + self.deny.len()
    }

    /// Правил нет вовсе.
    pub fn is_empty(&self) -> bool {
        self.len() == 0
    }

    /// Вердикт по предмету правила (см. [`rule_subject`]).
    pub fn verdict(&self, subject: &str) -> RuleVerdict {
        // Deny первым: запрет важнее удобства (см. шапку модуля).
        if let Some(r) = self.deny.iter().find(|r| r.is_match(subject)) {
            return RuleVerdict::Deny {
                rule: r.source.clone(),
            };
        }
        if let Some(r) = self.allow.iter().find(|r| r.is_match(subject)) {
            return RuleVerdict::Allow {
                rule: r.source.clone(),
            };
        }
        RuleVerdict::Pass
    }

    /// Вердикт по вызову инструмента — то, что зовёт машина хода.
    pub fn verdict_for(&self, call: &ToolCall) -> RuleVerdict {
        self.verdict(&rule_subject(call))
    }
}

fn compile<I, S>(lines: I, kind: &str, errors: &mut Vec<String>) -> Vec<Rule>
where
    I: IntoIterator<Item = S>,
    S: AsRef<str>,
{
    let mut out = Vec::new();
    for raw in lines {
        // Пустые строки и комментарии `#` — не правила: список удобно
        // комментировать, и случайный пустой textarea не должен давать
        // правило «совпадает со всем».
        let line = raw.as_ref().trim();
        if line.is_empty() || line.starts_with('#') {
            continue;
        }
        match Rule::new(line) {
            Ok(r) => out.push(r),
            Err(e) => errors.push(format!("{kind}: {line} — {e}")),
        }
    }
    out
}

/// Предмет правила для вызова: текст команды у оболочек, иначе имя тулза.
pub fn rule_subject(call: &ToolCall) -> String {
    if let Some(cmd) = call
        .arguments
        .get("command")
        .and_then(|v| v.as_str())
        .map(str::trim)
        .filter(|s| !s.is_empty())
    {
        return cmd.to_string();
    }
    call.name.clone()
}

/// Разобрать значение prefs (строка на список) в строки правил.
///
/// Формат хранения — по правилу на строку: тот же текст, что видит
/// пользователь в textarea, поэтому «сохранил» и «прочитал» симметричны.
pub fn lines_from_pref(raw: &str) -> Vec<String> {
    raw.lines()
        .map(|l| l.trim().to_string())
        .filter(|l| !l.is_empty())
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::{json, Map};

    fn call(name: &str, args: serde_json::Value) -> ToolCall {
        let arguments = match args {
            serde_json::Value::Object(m) => m,
            other => {
                let mut m = Map::new();
                m.insert("value".to_string(), other);
                m
            }
        };
        ToolCall {
            id: "c1".into(),
            name: name.into(),
            arguments,
        }
    }

    fn bash(cmd: &str) -> ToolCall {
        call("bash", json!({ "command": cmd }))
    }

    #[test]
    fn defaults_deny_destructive_commands() {
        let r = CommandRules::defaults();
        for cmd in [
            "rm -rf /",
            "rm -rf --no-preserve-root /",
            "sudo rm -rf .",
            "format c:",
            "FORMAT D:",
            "del /s c:\\temp",
            "rd /s /q c:\\temp",
            "shutdown -s -t 0",
            "cat x | powershell -",
            "mkfs.ext4 /dev/sda1",
            "dd if=/dev/zero of=/dev/sda",
        ] {
            assert!(
                matches!(r.verdict_for(&bash(cmd)), RuleVerdict::Deny { .. }),
                "ожидал deny для {cmd:?}"
            );
        }
    }

    #[test]
    fn defaults_allow_readonly_commands() {
        let r = CommandRules::defaults();
        for cmd in [
            "git status",
            "git log --oneline -5",
            "GIT DIFF HEAD",
            "ls -la",
            "dir",
            "cargo test --workspace",
            "cargo clippy -- -D warnings",
            "node --version",
        ] {
            assert!(
                matches!(r.verdict_for(&bash(cmd)), RuleVerdict::Allow { .. }),
                "ожидал allow для {cmd:?}"
            );
        }
    }

    #[test]
    fn defaults_pass_on_mutating_commands() {
        let r = CommandRules::defaults();
        // Не запрещены, но и не одобрены молча: решает политика.
        for cmd in [
            "git push origin main",
            "git commit -m x",
            "cargo run",
            "rm -rf build",
            "npm install",
            "echo hi",
        ] {
            let v = r.verdict_for(&bash(cmd));
            if cmd == "echo hi" {
                assert!(matches!(v, RuleVerdict::Allow { .. }), "{cmd:?} → {v:?}");
            } else {
                assert_eq!(v, RuleVerdict::Pass, "ожидал pass для {cmd:?}");
            }
        }
    }

    #[test]
    fn deny_wins_over_allow() {
        let r = CommandRules::from_lines(
            vec![r"^git\b".to_string()],
            vec![r"git push --force".to_string()],
        );
        assert!(matches!(
            r.verdict_for(&bash("git status")),
            RuleVerdict::Allow { .. }
        ));
        assert!(matches!(
            r.verdict_for(&bash("git push --force origin main")),
            RuleVerdict::Deny { .. }
        ));
    }

    #[test]
    fn non_shell_tools_match_by_name() {
        let r = CommandRules::from_lines(Vec::<&str>::new(), vec![r"^write$".to_string()]);
        assert!(matches!(
            r.verdict_for(&call("write", json!({ "path": "a.txt" }))),
            RuleVerdict::Deny { .. }
        ));
        assert_eq!(
            r.verdict_for(&call("read", json!({ "path": "a.txt" }))),
            RuleVerdict::Pass
        );
    }

    #[test]
    fn empty_command_falls_back_to_tool_name() {
        // Пустой `command` не должен становиться предметом правила: иначе
        // `^` совпал бы с любой строкой и запретил всё.
        let r = CommandRules::from_lines(Vec::<&str>::new(), vec![r"^bash$".to_string()]);
        assert!(matches!(
            r.verdict_for(&call("bash", json!({ "command": "  " }))),
            RuleVerdict::Deny { .. }
        ));
    }

    #[test]
    fn invalid_regex_is_reported_not_applied() {
        let r = CommandRules::from_lines(vec!["(unclosed".to_string()], vec!["*".to_string()]);
        assert_eq!(r.errors().len(), 2);
        assert!(r.is_empty());
        // Битое правило не совпало ни с чем: вердикт отдаётся политике.
        assert_eq!(r.verdict("git status"), RuleVerdict::Pass);
        assert!(r.errors()[0].starts_with("allow: (unclosed"));
        assert!(r.errors()[1].starts_with("deny: *"));
    }

    #[test]
    fn comments_and_blank_lines_are_not_rules() {
        let r = CommandRules::from_lines(
            vec![
                "".to_string(),
                "  ".to_string(),
                "# комментарий".to_string(),
            ],
            vec!["# только комментарий".to_string()],
        );
        assert!(r.is_empty());
        assert!(r.errors().is_empty());
        assert_eq!(r.verdict("что угодно"), RuleVerdict::Pass);
    }

    #[test]
    fn verdict_carries_rule_source_for_journal() {
        let r = CommandRules::from_lines(Vec::<&str>::new(), vec![r"\bshutdown\b".to_string()]);
        match r.verdict_for(&bash("shutdown /s")) {
            RuleVerdict::Deny { rule } => assert_eq!(rule, r"\bshutdown\b"),
            other => panic!("ожидал Deny, получил {other:?}"),
        }
    }

    #[test]
    fn pref_lines_split_and_trim() {
        let lines = lines_from_pref("  ^git status  \n\n# x\n^cargo test\n");
        assert_eq!(lines, vec!["^git status", "# x", "^cargo test"]);
    }

    #[test]
    fn matching_is_case_insensitive() {
        let r = CommandRules::from_lines(vec![r"^cargo test".to_string()], Vec::<&str>::new());
        assert!(matches!(
            r.verdict_for(&bash("CARGO TEST --workspace")),
            RuleVerdict::Allow { .. }
        ));
    }

    #[test]
    fn empty_rules_pass_everything() {
        let r = CommandRules::default();
        assert!(r.is_empty());
        assert_eq!(r.verdict_for(&bash("rm -rf /")), RuleVerdict::Pass);
    }
}

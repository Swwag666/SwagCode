//! Скиллы: markdown-шаблоны с YAML-frontmatter для быстрого заполнения
//! промптов и системных инструкций. Файлы живут в `~/.swagcod/skills/`,
//! перечитываются на лету (hot-reload) без перезапуска приложения.
//!
//! Формат файла:
//! ```text
//! ---
//! name: code-review
//! description: Обзор кода с фокусом на безопасность
//! defaults:
//!   language: rust
//! ---
//! Проведи обзор кода на {{language}}. Фокус: {{focus}}.
//! ```
//!
//! Если frontmatter отсутствует, имя скилла = stem файла (без `.md`),
//! описание пусто, шаблон = всё содержимое файла.

use std::collections::HashMap;
use std::path::{Path, PathBuf};

use serde::{Deserialize, Serialize};

/// Один скилл: метаданные + шаблон + путь источника.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Skill {
    pub name: String,
    pub description: String,
    /// Шаблон с `{{placeholder}}`-маркерами.
    pub template: String,
    /// Значения по умолчанию для плейсхолдеров.
    #[serde(default)]
    pub defaults: HashMap<String, String>,
    /// Откуда загружен (для диагностики и hot-reload).
    #[serde(skip)]
    pub source_path: PathBuf,
}

/// Реестр скиллов: загрузка из директории, рендеринг, горячая перезагрузка.
#[derive(Debug)]
pub struct SkillRegistry {
    pub skills: HashMap<String, Skill>,
    pub dir: PathBuf,
}

/// Дефолтная директория скиллов: `~/.swagcod/skills/`.
pub fn default_skills_dir() -> PathBuf {
    if let Ok(d) = std::env::var("SWAGCOD_SKILLS_DIR") {
        if !d.trim().is_empty() {
            return PathBuf::from(d);
        }
    }
    let home = std::env::var("USERPROFILE")
        .or_else(|_| std::env::var("HOME"))
        .unwrap_or_else(|_| ".".into());
    PathBuf::from(home).join(".swagcod").join("skills")
}

/// Парсинг YAML-frontmatter из markdown-текста.
/// Возвращает `(Option<yaml_block>, body)`.
fn split_frontmatter(text: &str) -> (Option<&str>, &str) {
    let trimmed = text.trim_start();
    if !trimmed.starts_with("---") {
        return (None, text);
    }
    // Ищем закрывающий --- после открывающего.
    let after_open = &trimmed[3..];
    // Пропускаем остаток первой строки (после ---).
    let after_first_line = match after_open.find('\n') {
        Some(pos) => &after_open[pos + 1..],
        None => return (None, text),
    };
    match after_first_line.find("\n---") {
        Some(pos) => {
            let yaml = &after_first_line[..pos];
            let rest_start = pos + 4; // "\n---" = 4 байта
            let body = if rest_start < after_first_line.len() {
                after_first_line[rest_start..].trim_start_matches('\n')
            } else {
                ""
            };
            (Some(yaml), body)
        }
        None => (None, text),
    }
}

/// Метаданные из YAML-frontmatter.
#[derive(Debug, Default, Deserialize)]
struct Frontmatter {
    name: Option<String>,
    description: Option<String>,
    #[serde(default)]
    defaults: HashMap<String, String>,
}

impl SkillRegistry {
    /// Создать пустой реестр с указанной директорией.
    pub fn new(dir: PathBuf) -> Self {
        Self {
            skills: HashMap::new(),
            dir,
        }
    }

    /// Загрузить все `*.md` файлы из директории. Создаёт директорию если нет.
    pub fn load(dir: &Path) -> Result<Self, String> {
        let mut reg = Self::new(dir.to_path_buf());
        if !dir.exists() {
            std::fs::create_dir_all(dir)
                .map_err(|e| format!("не создал директорию скиллов: {e}"))?;
            return Ok(reg);
        }
        let entries = std::fs::read_dir(dir).map_err(|e| format!("read_dir skills: {e}"))?;
        for entry in entries.flatten() {
            let path = entry.path();
            if path.extension().and_then(|e| e.to_str()) == Some("md") {
                match Self::parse_file(&path) {
                    Ok(skill) => {
                        reg.skills.insert(skill.name.clone(), skill);
                    }
                    Err(e) => {
                        eprintln!("skills: ошибка загрузки {:?}: {e}", path);
                    }
                }
            }
        }
        Ok(reg)
    }

    /// Перезагрузить один файл (hot-reload). Удаляет старую версию если
    /// имя изменилось.
    pub fn reload_file(&mut self, path: &Path) -> Result<(), String> {
        // Убираем старую запись по этому пути.
        self.skills.retain(|_, s| s.source_path != path);
        if !path.exists() {
            // Файл удалён — уже убрали выше.
            return Ok(());
        }
        if path.extension().and_then(|e| e.to_str()) != Some("md") {
            return Ok(());
        }
        let skill = Self::parse_file(path)?;
        self.skills.insert(skill.name.clone(), skill);
        Ok(())
    }

    /// Рендеринг шаблона: замена `{{key}}` на значения из vars, фолбэк
    /// на defaults, отсутствие — оставляет маркер как есть.
    pub fn render(&self, name: &str, vars: &HashMap<String, String>) -> Result<String, String> {
        let skill = self
            .skills
            .get(name)
            .ok_or_else(|| format!("скилл не найден: {name}"))?;
        Ok(render_template(&skill.template, &skill.defaults, vars))
    }

    /// Список скиллов для UI/API.
    pub fn list(&self) -> Vec<SkillSummary> {
        let mut out: Vec<SkillSummary> = self
            .skills
            .values()
            .map(|s| SkillSummary {
                name: s.name.clone(),
                description: s.description.clone(),
                placeholders: extract_placeholders(&s.template),
                defaults: s.defaults.clone(),
            })
            .collect();
        out.sort_by(|a, b| a.name.cmp(&b.name));
        out
    }

    fn parse_file(path: &Path) -> Result<Skill, String> {
        let text =
            std::fs::read_to_string(path).map_err(|e| format!("чтение {}: {e}", path.display()))?;
        let stem = path
            .file_stem()
            .and_then(|s| s.to_str())
            .unwrap_or("unnamed")
            .to_string();

        let (yaml_opt, body) = split_frontmatter(&text);
        let fm = match yaml_opt {
            Some(yaml) => serde_yaml::from_str::<Frontmatter>(yaml)
                .map_err(|e| format!("frontmatter {}: {e}", path.display()))?,
            None => Frontmatter::default(),
        };

        Ok(Skill {
            name: fm.name.unwrap_or(stem),
            description: fm.description.unwrap_or_default(),
            template: body.to_string(),
            defaults: fm.defaults,
            source_path: path.to_path_buf(),
        })
    }
}

/// Краткая карточка скилла для списка.
#[derive(Debug, Clone, Serialize)]
pub struct SkillSummary {
    pub name: String,
    pub description: String,
    pub placeholders: Vec<String>,
    pub defaults: HashMap<String, String>,
}

/// Замена `{{key}}` в шаблоне. Приоритет: vars > defaults > оставить как есть.
fn render_template(
    template: &str,
    defaults: &HashMap<String, String>,
    vars: &HashMap<String, String>,
) -> String {
    let mut result = String::with_capacity(template.len());
    let mut chars = template.chars().peekable();
    while let Some(c) = chars.next() {
        if c == '{' && chars.peek() == Some(&'{') {
            chars.next(); // consume second {
            let mut key = String::new();
            loop {
                match chars.next() {
                    Some('}') if chars.peek() == Some(&'}') => {
                        chars.next();
                        break;
                    }
                    Some(ch) => key.push(ch),
                    None => {
                        // Незакрытый маркер — выводим как есть.
                        result.push_str("{{");
                        result.push_str(&key);
                        break;
                    }
                }
            }
            let key = key.trim().to_string();
            if let Some(val) = vars.get(&key) {
                result.push_str(val);
            } else if let Some(val) = defaults.get(&key) {
                result.push_str(val);
            } else {
                result.push_str("{{");
                result.push_str(&key);
                result.push_str("}}");
            }
        } else {
            result.push(c);
        }
    }
    result
}

/// Извлечь имена плейсхолдеров `{{...}}` из шаблона.
fn extract_placeholders(template: &str) -> Vec<String> {
    let mut out = Vec::new();
    let mut chars = template.chars().peekable();
    while let Some(c) = chars.next() {
        if c == '{' && chars.peek() == Some(&'{') {
            chars.next();
            let mut key = String::new();
            loop {
                match chars.next() {
                    Some('}') if chars.peek() == Some(&'}') => {
                        chars.next();
                        break;
                    }
                    Some(ch) => key.push(ch),
                    None => break,
                }
            }
            let key = key.trim().to_string();
            if !key.is_empty() && !out.contains(&key) {
                out.push(key);
            }
        }
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    fn tmp_skills_dir(tag: &str) -> PathBuf {
        let dir = std::env::temp_dir().join(format!("swagcod-skills-{tag}-{}", std::process::id()));
        let _ = std::fs::create_dir_all(&dir);
        dir
    }

    #[test]
    fn load_from_dir_parses_frontmatter_and_plain() {
        let dir = tmp_skills_dir("load");
        // С frontmatter.
        std::fs::write(
            dir.join("review.md"),
            "---\nname: code-review\ndescription: Обзор кода\ndefaults:\n  language: rust\n---\nОбзор на {{language}}: {{focus}}.\n",
        )
        .unwrap();
        // Без frontmatter.
        std::fs::write(dir.join("simple.md"), "Просто текст {{x}}.").unwrap();
        // Не-md файл игнорируется.
        std::fs::write(dir.join("notes.txt"), "не скилл").unwrap();

        let reg = SkillRegistry::load(&dir).unwrap();
        assert_eq!(reg.skills.len(), 2);

        let review = reg.skills.get("code-review").expect("code-review");
        assert_eq!(review.description, "Обзор кода");
        assert_eq!(
            review.defaults.get("language").map(|s| s.as_str()),
            Some("rust")
        );
        assert!(review.template.contains("{{focus}}"));

        let simple = reg.skills.get("simple").expect("simple");
        assert_eq!(simple.template, "Просто текст {{x}}.");
        assert!(simple.description.is_empty());

        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn reload_file_updates_and_removes() {
        let dir = tmp_skills_dir("reload");
        std::fs::write(dir.join("a.md"), "---\nname: alpha\n---\nВерсия 1.\n").unwrap();

        let mut reg = SkillRegistry::load(&dir).unwrap();
        assert_eq!(reg.skills.len(), 1);
        assert!(reg.skills.contains_key("alpha"));

        // Обновляем содержимое.
        std::fs::write(
            dir.join("a.md"),
            "---\nname: alpha\ndescription: обновлён\n---\nВерсия 2.\n",
        )
        .unwrap();
        reg.reload_file(&dir.join("a.md")).unwrap();
        assert_eq!(reg.skills["alpha"].description, "обновлён");
        assert!(reg.skills["alpha"].template.contains("Версия 2"));

        // Удаляем файл.
        std::fs::remove_file(dir.join("a.md")).unwrap();
        reg.reload_file(&dir.join("a.md")).unwrap();
        assert!(reg.skills.is_empty());

        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn render_with_vars_and_defaults() {
        let dir = tmp_skills_dir("render");
        std::fs::write(
            dir.join("t.md"),
            "---\nname: tmpl\ndefaults:\n  lang: python\n  style: pep8\n---\nПиши на {{lang}}, стиль {{style}}, тема {{topic}}.\n",
        )
        .unwrap();

        let reg = SkillRegistry::load(&dir).unwrap();
        let mut vars = HashMap::new();
        vars.insert("topic".into(), "парсинг".into());
        let out = reg.render("tmpl", &vars).unwrap();
        assert_eq!(out, "Пиши на python, стиль pep8, тема парсинг.\n");

        // Override default.
        vars.insert("lang".into(), "go".into());
        let out2 = reg.render("tmpl", &vars).unwrap();
        assert_eq!(out2, "Пиши на go, стиль pep8, тема парсинг.\n");

        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn unknown_skill_returns_error() {
        let dir = tmp_skills_dir("unknown");
        let reg = SkillRegistry::load(&dir).unwrap();
        let err = reg.render("nonexistent", &HashMap::new()).unwrap_err();
        assert!(err.contains("не найден"), "{err}");
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn extract_placeholders_finds_all_unique() {
        let ph = extract_placeholders("Hello {{name}}, your {{role}} is {{name}}.");
        assert_eq!(ph, vec!["name".to_string(), "role".to_string()]);
    }

    #[test]
    fn split_frontmatter_handles_missing_and_malformed() {
        let (yaml, body) = split_frontmatter("нет фронтматтера");
        assert!(yaml.is_none());
        assert_eq!(body, "нет фронтматтера");

        let (yaml, body) = split_frontmatter("---\nname: x\n---\nТело.");
        assert_eq!(yaml, Some("name: x"));
        assert_eq!(body, "Тело.");

        // Незакрытый frontmatter.
        let (yaml, body) = split_frontmatter("---\nname: x\nнет закрытия");
        assert!(yaml.is_none());
        assert!(body.contains("name: x"));
    }

    #[test]
    fn load_creates_dir_if_missing() {
        let dir = tmp_skills_dir("create");
        let _ = std::fs::remove_dir_all(&dir);
        let reg = SkillRegistry::load(&dir).unwrap();
        assert!(reg.skills.is_empty());
        assert!(dir.exists());
        let _ = std::fs::remove_dir_all(&dir);
    }
}

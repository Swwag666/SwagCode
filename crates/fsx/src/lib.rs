/*! Файловые операции: обход дерева, поиск, watch, diff. Этап 2.

Сейчас — заготовка и чистая логика diff, которую можно мерить бенчем
(цель: diff на 5000-строчном патче < 50 мс, DECISIONS.md §2).
*/

use serde::{Deserialize, Serialize};

/// Одна изменённая строка в diff.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum LineChange {
    Same,
    Added,
    Removed,
}

/// Строка diff с номерами в обоих файлах.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct DiffLine {
    pub change: LineChange,
    /// Номер в старом файле (None для добавленных).
    pub old_no: Option<u32>,
    /// Номер в новом файле (None для удалённых).
    pub new_no: Option<u32>,
    pub text: String,
}

/// Результат сравнения двух текстов.
#[derive(Debug, Clone, Default, PartialEq)]
pub struct Diff {
    pub lines: Vec<DiffLine>,
    pub added: u32,
    pub removed: u32,
}

impl Diff {
    pub fn is_empty(&self) -> bool {
        self.added == 0 && self.removed == 0
    }

    /// Статистика в формате, привычном по git: `+12 -3`.
    pub fn stat(&self) -> String {
        format!("+{} -{}", self.added, self.removed)
    }
}

/// Сравнить два текста построчно.
///
/// Делегирует LCS в `similar`, но нормализует результат в наш компактный
/// формат: UI рендерит его на canvas и не должен тянуть тяжёлую структуру.
pub fn diff_text(old: &str, new: &str) -> Diff {
    use similar::{ChangeTag, TextDiff};

    let d = TextDiff::from_lines(old, new);
    let mut out = Diff::default();
    let mut old_no = 0u32;
    let mut new_no = 0u32;

    for change in d.iter_all_changes() {
        // similar отдаёт строки вместе с переводом; для рендера он не нужен.
        let text = change.value().trim_end_matches(['\n', '\r']).to_string();
        match change.tag() {
            ChangeTag::Equal => {
                old_no += 1;
                new_no += 1;
                out.lines.push(DiffLine {
                    change: LineChange::Same,
                    old_no: Some(old_no),
                    new_no: Some(new_no),
                    text,
                });
            }
            ChangeTag::Delete => {
                old_no += 1;
                out.removed += 1;
                out.lines.push(DiffLine {
                    change: LineChange::Removed,
                    old_no: Some(old_no),
                    new_no: None,
                    text,
                });
            }
            ChangeTag::Insert => {
                new_no += 1;
                out.added += 1;
                out.lines.push(DiffLine {
                    change: LineChange::Added,
                    old_no: None,
                    new_no: Some(new_no),
                    text,
                });
            }
        }
    }
    out
}

/// Проверить, что путь не выходит за границу рабочей директории.
///
/// Это барьер песочницы для файловых тулзов: модель может попросить
/// `../../etc/passwd`, и резолвить такое нельзя. Проверка по нормализованному
/// пути, а не по строке — иначе `a/../b` обойдёт её.
///
/// `candidate` может не существовать (тулз `write` создаёт файл), поэтому
/// нормализуем ближайшего существующего предка и дописываем остаток.
/// `canonicalize` обязателен с обеих сторон: на Windows он возвращает префикс
/// `\\?\`, и сравнение канонического root с сырым candidate дало бы ложный
/// отказ на каждом новом файле.
pub fn is_within(root: &std::path::Path, candidate: &std::path::Path) -> bool {
    let abs_root = match root.canonicalize() {
        Ok(p) => p,
        // Недоступный root — не «внутри». Молча разрешать нельзя.
        Err(_) => return false,
    };
    let abs = match normalize_existing(candidate) {
        Some(p) => p,
        None => return false,
    };
    abs.starts_with(&abs_root)
}

/// Канонизировать путь, допустив что его хвост ещё не существует.
///
/// Поднимаемся вверх до первого существующего предка, канонизируем его и
/// дописываем отброшенный хвост. Отдельно отбрасываем `.` и `..` вручную:
/// `..` в несуществующем хвосте — это попытка выхода, и canonicalize его не
/// схлопнет, потому что схлопывать нечего.
fn normalize_existing(path: &std::path::Path) -> Option<std::path::PathBuf> {
    // Ищем существующего предка, запоминая отброшенный хвост.
    let mut tail: Vec<std::ffi::OsString> = Vec::new();
    let mut cur = path;
    let base = loop {
        if cur.exists() {
            break cur.to_path_buf();
        }
        match (cur.file_name(), cur.parent()) {
            (Some(name), Some(parent)) => {
                tail.push(name.to_os_string());
                cur = parent;
            }
            // Дошли до корня и ничего не существует — сравнивать не с чем.
            _ => return None,
        }
    };
    let mut out = base.canonicalize().ok()?;
    // Хвост восстанавливаем в исходном порядке, отбрасывая `.`.
    // `..` не схлопываем: его наличие в несуществующем хвосте уже само по себе
    // подозрительно, и схлопывание могло бы замаскировать выход за границу.
    for name in tail.iter().rev() {
        let n = std::path::Path::new(name);
        if n == std::path::Path::new(".") {
            continue;
        }
        if n == std::path::Path::new("..") {
            if !out.pop() {
                return None;
            }
            continue;
        }
        out.push(name);
    }
    Some(out)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn identical_texts_produce_no_changes() {
        let d = diff_text("a\nb\n", "a\nb\n");
        assert!(d.is_empty());
        assert_eq!(d.stat(), "+0 -0");
        assert_eq!(d.lines.len(), 2);
        assert!(d.lines.iter().all(|l| l.change == LineChange::Same));
    }

    #[test]
    fn added_line_is_counted_and_numbered() {
        let d = diff_text("a\n", "a\nb\n");
        assert_eq!(d.added, 1);
        assert_eq!(d.removed, 0);
        let added = d
            .lines
            .iter()
            .find(|l| l.change == LineChange::Added)
            .unwrap();
        assert_eq!(added.text, "b");
        assert_eq!(added.new_no, Some(2));
        assert_eq!(added.old_no, None);
    }

    #[test]
    fn removed_line_keeps_old_number() {
        let d = diff_text("a\nb\n", "a\n");
        assert_eq!(d.removed, 1);
        let rem = d
            .lines
            .iter()
            .find(|l| l.change == LineChange::Removed)
            .unwrap();
        assert_eq!(rem.text, "b");
        assert_eq!(rem.old_no, Some(2));
        assert_eq!(rem.new_no, None);
    }

    #[test]
    fn modification_shows_as_remove_plus_add() {
        let d = diff_text("line\n", "LINE\n");
        assert_eq!(d.added, 1);
        assert_eq!(d.removed, 1);
        assert!(!d.is_empty());
    }

    #[test]
    fn empty_to_content_and_back() {
        assert_eq!(diff_text("", "a\nb\n").added, 2);
        assert_eq!(diff_text("a\nb\n", "").removed, 2);
        assert!(diff_text("", "").is_empty());
    }

    #[test]
    fn trailing_newlines_are_stripped_for_rendering() {
        let d = diff_text("a\r\nb\r\n", "a\r\nb\r\n");
        assert!(d.lines.iter().all(|l| !l.text.ends_with('\r')));
        assert!(d.lines.iter().all(|l| !l.text.ends_with('\n')));
    }

    #[test]
    fn unicode_content_survives_diff() {
        let d = diff_text("привет\nмир\n", "привет\nМИР\n");
        assert_eq!(d.added, 1);
        assert_eq!(d.removed, 1);
        let added = d
            .lines
            .iter()
            .find(|l| l.change == LineChange::Added)
            .unwrap();
        assert_eq!(added.text, "МИР");
    }

    #[test]
    fn stat_format_matches_git_convention() {
        let d = diff_text("a\nb\nc\n", "a\nX\nY\nc\n");
        assert_eq!(d.stat(), format!("+{} -{}", d.added, d.removed));
        assert!(d.stat().starts_with('+'));
    }

    #[test]
    fn diff_serializes_for_ipc() {
        let d = diff_text("a\n", "b\n");
        let j = serde_json::to_string(&d.lines).unwrap();
        let back: Vec<DiffLine> = serde_json::from_str(&j).unwrap();
        assert_eq!(back, d.lines);
    }

    #[test]
    fn path_escape_is_rejected() {
        let root = std::path::Path::new(env!("CARGO_MANIFEST_DIR"));
        assert!(is_within(root, &root.join("src")));
        assert!(is_within(root, &root.join("src/lib.rs")));
        assert!(
            !is_within(root, &root.join("../../etc/passwd")),
            "выход за границу воркспейса должен отклоняться"
        );
    }

    #[test]
    fn path_with_dotdot_resolving_inside_is_allowed() {
        // a/../b нормализуется внутрь — отклонять такое было бы ложным срабатыванием.
        let root = std::path::Path::new(env!("CARGO_MANIFEST_DIR"));
        assert!(is_within(root, &root.join("src/../src")));
    }

    #[test]
    fn nonexistent_path_inside_root_is_allowed() {
        // Тулз write создаёт новый файл: проверять существование нельзя.
        let root = std::path::Path::new(env!("CARGO_MANIFEST_DIR"));
        assert!(is_within(root, &root.join("src/new_file.rs")));
    }

    #[test]
    fn deeply_nested_nonexistent_path_inside_root_is_allowed() {
        // Хвост из нескольких несуществующих уровней — типичный случай mkdir -p.
        let root = std::path::Path::new(env!("CARGO_MANIFEST_DIR"));
        assert!(is_within(root, &root.join("a/b/c/d/file.txt")));
    }

    #[test]
    fn nonexistent_path_with_dotdot_escaping_root_is_rejected() {
        // Опасный случай: .. внутри несуществующего хвоста. canonicalize его не
        // схлопнет, поэтому обрабатываем вручную.
        let root = std::path::Path::new(env!("CARGO_MANIFEST_DIR"));
        assert!(!is_within(root, &root.join("src/../../../etc/passwd")));
        assert!(!is_within(root, &root.join("newdir/../../outside.txt")));
    }

    #[test]
    fn nonexistent_path_with_dotdot_staying_inside_is_allowed() {
        let root = std::path::Path::new(env!("CARGO_MANIFEST_DIR"));
        // src/../otherfile — остаётся внутри root.
        assert!(is_within(root, &root.join("src/../otherfile.rs")));
    }

    #[test]
    fn dot_components_do_not_change_the_verdict() {
        let root = std::path::Path::new(env!("CARGO_MANIFEST_DIR"));
        assert!(is_within(root, &root.join("./src/./lib.rs")));
        assert!(is_within(root, &root.join("a/./b/./c.txt")));
    }

    #[test]
    fn inaccessible_root_is_rejected_not_allowed() {
        // Молчаливое разрешение при недоступном root — дыра в песочнице.
        let bogus = std::path::Path::new("Z:/swagcod-definitely-not-here-8f3a1c");
        let inside = bogus.join("file.txt");
        assert!(!is_within(bogus, &inside));
    }

    #[test]
    fn sibling_directory_is_not_within_root() {
        let root = std::path::Path::new(env!("CARGO_MANIFEST_DIR"));
        // ../provider — соседний крейт, не внутри fsx.
        assert!(!is_within(root, &root.join("../provider/src/lib.rs")));
    }

    #[test]
    fn root_itself_is_within_root() {
        let root = std::path::Path::new(env!("CARGO_MANIFEST_DIR"));
        assert!(is_within(root, root));
    }

    /// Бенч-заготовка: цель — 5000 строк < 50 мс (DECISIONS.md §2).
    #[test]
    fn diff_5000_line_patch_is_fast() {
        let old: String = (0..5000).map(|i| format!("line {i}\n")).collect();
        let new: String = (0..5000)
            .map(|i| {
                if i % 7 == 0 {
                    format!("changed {i}\n")
                } else {
                    format!("line {i}\n")
                }
            })
            .collect();
        let start = std::time::Instant::now();
        let d = diff_text(&old, &new);
        let elapsed = start.elapsed();
        assert!(d.added > 100, "diff должен найти изменения");
        assert!(
            elapsed.as_millis() < 500,
            "слишком медленно: {} мс (цель < 50 мс в release)",
            elapsed.as_millis()
        );
    }
}

/*! Индекс файлов рабочей директории и fuzzy-поиск (этап B-4).

Обход через `ignore::WalkBuilder`: .gitignore уважается бесплатно и так же,
как его понимает git (и ripgrep). Индекс — плоский список относительных
путей: на нём fuzzy-поиск `nucleo-matcher` летает без аллокаций на файл.
*/

use std::path::{Path, PathBuf};

use nucleo_matcher::{Config, Matcher, Utf32Str};

use crate::FsxError;

/// Плоский индекс файлов: относительные пути с '/'-разделителем.
#[derive(Debug, Default, Clone)]
pub struct FileIndex {
    pub root: PathBuf,
    pub files: Vec<String>,
}

/// Потолок числа файлов в индексе: защита от случайного обхода гигантских
/// деревьев (монтированные диски, кэши пакетов без .gitignore).
pub const MAX_INDEX_FILES: usize = 200_000;

impl FileIndex {
    /// Построить индекс каталога, уважая .gitignore и пропуская служебное.
    pub fn build(root: &Path) -> Result<Self, FsxError> {
        if !root.is_dir() {
            return Err(FsxError::NotFound(root.to_path_buf()));
        }
        let mut files = Vec::new();
        let walker = ignore::WalkBuilder::new(root)
            .hidden(false) // dot-файлы видны: .swagcod/MEMORY.md — рабочий файл
            .git_ignore(true)
            .git_global(false) // глобальный ignore пользователя не наш каприз
            .git_exclude(false)
            .require_git(false) // .gitignore работает и без git-репозитория
            .build();
        for entry in walker {
            let entry = match entry {
                Ok(e) => e,
                Err(_) => continue, // битая symlink и прочее — не причина ронять индекс
            };
            if !entry.file_type().map(|t| t.is_file()).unwrap_or(false) {
                continue;
            }
            let rel = match entry.path().strip_prefix(root) {
                Ok(r) => r,
                Err(_) => continue,
            };
            // Служебные каталоги не индексируем даже без .gitignore.
            if rel
                .components()
                .any(|c| matches!(c.as_os_str().to_str(), Some(".git") | Some("node_modules") | Some("target")))
            {
                continue;
            }
            let rel_str = rel.to_string_lossy().replace('\\', "/");
            files.push(rel_str);
            if files.len() >= MAX_INDEX_FILES {
                break;
            }
        }
        files.sort_unstable();
        Ok(Self { root: root.to_path_buf(), files })
    }

    /// Fuzzy-поиск: топ `limit` путей по убыванию очка nucleo.
    ///
    /// Пустой запрос возвращает первые `limit` файлов индекса: UI показывает
    /// «просто список», пока человек не начал печатать.
    pub fn search(&self, query: &str, limit: usize) -> Vec<String> {
        let limit = limit.clamp(1, 200);
        if query.trim().is_empty() {
            return self.files.iter().take(limit).cloned().collect();
        }
        let mut matcher = Matcher::new(Config::DEFAULT.match_paths());
        let mut buf = Vec::new();
        let needle = Utf32Str::new(query, &mut buf);
        let mut scored: Vec<(u16, &String)> = self
            .files
            .iter()
            .filter_map(|f| {
                let mut hay_buf = Vec::new();
                let hay = Utf32Str::new(f, &mut hay_buf);
                matcher.fuzzy_match(hay, needle).map(|s| (s, f))
            })
            .collect();
        scored.sort_by(|a, b| b.0.cmp(&a.0).then_with(|| a.1.cmp(b.1)));
        scored.into_iter().take(limit).map(|(_, f)| f.clone()).collect()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn temp_root(tag: &str) -> PathBuf {
        let dir = std::env::temp_dir().join(format!(
            "swagcod-fsx-{tag}-{}-{}",
            std::process::id(),
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_nanos()
        ));
        std::fs::create_dir_all(&dir).unwrap();
        dir
    }

    fn write(root: &Path, rel: &str, content: &str) {
        let p = root.join(rel);
        std::fs::create_dir_all(p.parent().unwrap()).unwrap();
        std::fs::write(p, content).unwrap();
    }

    #[test]
    fn index_respects_gitignore() {
        let root = temp_root("gitignore");
        write(&root, ".gitignore", "secret/\n*.log\n");
        write(&root, "src/main.rs", "fn main() {}");
        write(&root, "secret/key.txt", "hidden");
        write(&root, "debug.log", "noise");
        write(&root, "notes.md", "# notes");
        let idx = FileIndex::build(&root).unwrap();
        assert!(idx.files.contains(&"src/main.rs".to_string()));
        assert!(idx.files.contains(&"notes.md".to_string()));
        assert!(!idx.files.iter().any(|f| f.starts_with("secret/")));
        assert!(!idx.files.iter().any(|f| f.ends_with(".log")));
        let _ = std::fs::remove_dir_all(&root);
    }

    #[test]
    fn index_skips_service_dirs_without_gitignore() {
        let root = temp_root("service");
        write(&root, ".git/file", "x");
        write(&root, "node_modules/pkg/index.js", "x");
        write(&root, "target/debug/app.exe", "x");
        write(&root, "real.rs", "x");
        let idx = FileIndex::build(&root).unwrap();
        assert_eq!(idx.files, vec!["real.rs".to_string()]);
        let _ = std::fs::remove_dir_all(&root);
    }

    #[test]
    fn fuzzy_search_ranks_relevant_first() {
        let root = temp_root("fuzzy");
        write(&root, "src/components/Terminal.svelte", "x");
        write(&root, "src/components/SessionList.svelte", "x");
        write(&root, "src/lib/bus.ts", "x");
        let idx = FileIndex::build(&root).unwrap();
        let hits = idx.search("term", 10);
        assert_eq!(hits[0], "src/components/Terminal.svelte");
        let hits = idx.search("slist", 10);
        assert!(hits.iter().any(|h| h.contains("SessionList")), "fuzzy должен найти {hits:?}");
        let _ = std::fs::remove_dir_all(&root);
    }

    #[test]
    fn empty_query_returns_head_of_index() {
        let root = temp_root("empty");
        write(&root, "a.txt", "1");
        write(&root, "b.txt", "2");
        let idx = FileIndex::build(&root).unwrap();
        assert_eq!(idx.search("", 5).len(), 2);
        assert_eq!(idx.search("   ", 5).len(), 2);
        let _ = std::fs::remove_dir_all(&root);
    }

    #[test]
    fn limit_is_clamped() {
        let root = temp_root("limit");
        for i in 0..10 {
            write(&root, &format!("f{i}.txt"), "x");
        }
        let idx = FileIndex::build(&root).unwrap();
        assert_eq!(idx.search("f", 3).len(), 3);
        assert_eq!(idx.search("f", 0).len(), 1);
        let _ = std::fs::remove_dir_all(&root);
    }

    #[test]
    fn missing_root_is_error_not_panic() {
        let bogus = std::env::temp_dir().join("swagcod-fsx-nope-8f3a1c");
        assert!(FileIndex::build(&bogus).is_err());
    }
}

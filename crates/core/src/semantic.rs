/*! Семантический индекс кодовой базы (этап E-2 расширений).

Эмбеддинги — **перестраиваемый кэш**, а не доменные данные, поэтому живут
в отдельной базе `embeddings.db` (rusqlite, WAL) рядом с основной, а не в
better-sqlite3: десятки тысяч векторов по stdio-JSON-RPC гонять накладно,
а потеря кэша не стоит ни одной пользовательской строчки.

Поиск — brute-force косинус по всем чанкам (план: до ~50 тыс. чанков этого
хватает; HNSW — отдельный этап, если упрёмся). Вектора хранятся BLOB'ом
f32 little-endian: компактно и без JSON-потерь.

Модуль не знает про сеть: эмбеддинги приносит вызывающий (Router из B-6),
здесь только детерминированная нарезка, хранение и ранжирование.
*/

use std::collections::HashMap;
use std::hash::{Hash, Hasher};
use std::path::Path;

use rusqlite::{params, Connection};

/// Чанк файла: полуинтервал строк [start, end] (1-based, end включительно).
#[derive(Debug, Clone, PartialEq)]
pub struct Chunk {
    pub start: u32,
    pub end: u32,
    pub text: String,
}

/// Результат поиска: чанк + косинусная близость к запросу.
#[derive(Debug, Clone, PartialEq)]
pub struct Hit {
    pub path: String,
    pub start: u32,
    pub end: u32,
    pub score: f32,
}

/// Нарезка: ~40 строк на чанк с перекрытием 5 строк (план E-2):
/// перекрытие не рвёт контекст на границах.
pub const CHUNK_LINES: usize = 40;
pub const CHUNK_OVERLAP: usize = 5;

/// Режем текст на чанки по строкам. Пустой текст — пустой список.
/// Хвост короче чанка сливается с предыдущим (огрызки в 2 строки
/// эмбеддить бессмысленно).
pub fn chunk_text(text: &str) -> Vec<Chunk> {
    let lines: Vec<&str> = text.lines().collect();
    if lines.is_empty() {
        return Vec::new();
    }
    let step = CHUNK_LINES.saturating_sub(CHUNK_OVERLAP).max(1);
    let mut out: Vec<Chunk> = Vec::new();
    let mut start = 0usize;
    while start < lines.len() {
        let end = (start + CHUNK_LINES).min(lines.len());
        out.push(Chunk {
            start: (start + 1) as u32,
            end: end as u32,
            text: lines[start..end].join("\n"),
        });
        if end == lines.len() {
            break;
        }
        start += step;
    }
    // Хвостовой огрызок пришиваем к предыдущему чанку. При шаге 35 и
    // чанке 40 отдельный хвост не бывает короче overlap+1 строк — поэтому
    // порог слияния ровно такой: хвост-минимум вливается, осмысленный
    // хвост (7+ строк) остаётся своим чанком.
    if out.len() >= 2 {
        let last = out.len() - 1;
        if (out[last].end - out[last].start + 1) as usize <= CHUNK_OVERLAP + 1 {
            let tail = out.pop().unwrap();
            let prev = out.last_mut().unwrap();
            prev.end = tail.end;
            prev.text = lines[(prev.start as usize - 1)..tail.end as usize].join("\n");
        }
    }
    out
}

/// Косинусная близость. Нулевые вектора даём 0.0, не NaN.
pub fn cosine(a: &[f32], b: &[f32]) -> f32 {
    if a.is_empty() || a.len() != b.len() {
        return 0.0;
    }
    let mut dot = 0f64;
    let mut na = 0f64;
    let mut nb = 0f64;
    for (x, y) in a.iter().zip(b.iter()) {
        dot += (*x as f64) * (*y as f64);
        na += (*x as f64) * (*x as f64);
        nb += (*y as f64) * (*y as f64);
    }
    if na == 0.0 || nb == 0.0 {
        return 0.0;
    }
    (dot / (na.sqrt() * nb.sqrt())) as f32
}

/// Хэш содержимого для кэша: SipHash (std) достаточен — это не защита,
/// а детектор «файл не изменился».
pub fn content_hash(text: &str) -> String {
    let mut h = std::collections::hash_map::DefaultHasher::new();
    text.hash(&mut h);
    format!("{:016x}", h.finish())
}

/// Размерность локального лексического вектора.
pub const LOCAL_EMBED_DIM: usize = 512;

/// E-2: локальный эмбеддер — feature hashing мешка токенов. Работает без
/// сети и без ключа; это НЕ настоящая семантика (синонимы не найдёт),
/// а честный лексический уровень: «approval policy journal» найдёт
/// идентификаторы, русское «оплата» не найдёт `payment`. Поэтому маркер
/// пространства в базе обязателен, а облачные эмбеддинги при доступном
/// `/v1/embeddings` всегда предпочтительнее.
///
/// Токены: алфавитно-цифровые последовательности + camelCase-сplits,
/// lowercase; вес — сублинейный tf (1 + ln count); нормировка L2, так что
/// косинус вырождается в скалярное произведение.
pub fn local_embedding(text: &str) -> Vec<f32> {
    let tokens = tokenize(text);
    let mut counts: HashMap<String, u32> = HashMap::new();
    for t in tokens {
        *counts.entry(t).or_insert(0) += 1;
    }
    let mut vec = vec![0f32; LOCAL_EMBED_DIM];
    for (tok, n) in &counts {
        let mut h = std::collections::hash_map::DefaultHasher::new();
        tok.hash(&mut h);
        let idx = (h.finish() % LOCAL_EMBED_DIM as u64) as usize;
        vec[idx] += 1.0 + (*n as f64).ln() as f32;
    }
    let norm = vec
        .iter()
        .map(|x| (*x as f64) * (*x as f64))
        .sum::<f64>()
        .sqrt();
    if norm > 0.0 {
        for x in vec.iter_mut() {
            *x = (*x as f64 / norm) as f32;
        }
    }
    vec
}

/// Токенизация для локального эмбеддера: не-буквенно-цифровые разделители,
/// camelCase дробится (approveUser -> approve, user, approveuser).
fn tokenize(text: &str) -> Vec<String> {
    let mut out = Vec::new();
    for raw in text.split(|c: char| !c.is_alphanumeric() && c != '_') {
        for word in raw.split('_') {
            if word.is_empty() {
                continue;
            }
            let lower = word.to_lowercase();
            out.push(lower.clone());
            // camelCase / PascalCase дроби
            let mut cur = String::new();
            let chars: Vec<char> = word.chars().collect();
            for (i, ch) in chars.iter().enumerate() {
                if i > 0 && ch.is_uppercase() && !chars[i - 1].is_uppercase() && !cur.is_empty() {
                    out.push(cur.to_lowercase());
                    cur.clear();
                }
                cur.push(*ch);
            }
            if !cur.is_empty() && cur.to_lowercase() != lower {
                out.push(cur.to_lowercase());
            }
        }
    }
    out
}

fn vec_to_blob(v: &[f32]) -> Vec<u8> {
    let mut out = Vec::with_capacity(v.len() * 4);
    for x in v {
        out.extend_from_slice(&x.to_le_bytes());
    }
    out
}

fn blob_to_vec(b: &[u8]) -> Vec<f32> {
    b.chunks_exact(4)
        .map(|c| f32::from_le_bytes([c[0], c[1], c[2], c[3]]))
        .collect()
}

/// Кэш эмбеддингов: файлы + их чанки с векторами. Одна база на приложение
/// (ключ `files.root` не нужен — пути храним относительными от cwd сессии,
/// разные рабочие директории различаются префиксом пути).
pub struct SemanticIndex {
    conn: Connection,
}

impl SemanticIndex {
    /// Открыть (или создать) базу индекса. WAL: читатель (поиск) не
    /// блокирует писателя (фоновая индексация).
    pub fn open(path: &Path) -> Result<Self, rusqlite::Error> {
        if let Some(parent) = path.parent() {
            std::fs::create_dir_all(parent).ok();
        }
        let conn = Connection::open(path)?;
        Self::from_conn(conn)
    }

    /// Индекс в памяти (тесты и одноразовые прогоны).
    pub fn in_memory() -> Result<Self, rusqlite::Error> {
        Self::from_conn(Connection::open_in_memory()?)
    }

    fn from_conn(conn: Connection) -> Result<Self, rusqlite::Error> {
        conn.execute_batch(
            "PRAGMA journal_mode = WAL;
             PRAGMA synchronous = NORMAL;
             CREATE TABLE IF NOT EXISTS files(
                 path  TEXT PRIMARY KEY,
                 hash  TEXT NOT NULL,
                 chunks INTEGER NOT NULL DEFAULT 0
             );
             CREATE TABLE IF NOT EXISTS chunks(
                 path       TEXT NOT NULL,
                 start_line INTEGER NOT NULL,
                 end_line   INTEGER NOT NULL,
                 vec        BLOB NOT NULL,
                 PRIMARY KEY(path, start_line)
             );
             CREATE TABLE IF NOT EXISTS meta(
                 key   TEXT PRIMARY KEY,
                 value TEXT NOT NULL
             );",
        )?;
        Ok(Self { conn })
    }

    /// E-2: маркер пространства эмбеддингов ("cloud:<model>" или "local:v1").
    /// Вектора разных эмбеддеров несравнимы: при смене маркера индекс
    /// обязан быть перестроен с нуля (см. [`SemanticIndex::clear_all`]).
    pub fn embedder(&self) -> Result<Option<String>, rusqlite::Error> {
        let mut stmt = self
            .conn
            .prepare("SELECT value FROM meta WHERE key = 'embedder'")?;
        Ok(stmt.query_row([], |r| r.get::<_, String>(0)).ok())
    }

    /// Записать маркер пространства эмбеддингов.
    pub fn set_embedder(&self, kind: &str) -> Result<(), rusqlite::Error> {
        self.conn.execute(
            "INSERT OR REPLACE INTO meta(key, value) VALUES ('embedder', ?1)",
            params![kind],
        )?;
        Ok(())
    }

    /// Полная очистка кэша (смена эмбеддера — вектора несовместимы).
    pub fn clear_all(&self) -> Result<(), rusqlite::Error> {
        self.conn
            .execute_batch("DELETE FROM chunks; DELETE FROM files;")
    }

    /// Хэши всех проиндексированных файлов: snapshot для дешёвого
    /// «что изменилось» в фоновой индексации.
    pub fn file_hashes(&self) -> Result<HashMap<String, String>, rusqlite::Error> {
        let mut stmt = self.conn.prepare("SELECT path, hash FROM files")?;
        let rows = stmt.query_map([], |r| Ok((r.get::<_, String>(0)?, r.get::<_, String>(1)?)))?;
        let mut map = HashMap::new();
        for row in rows {
            let (p, h) = row?;
            map.insert(p, h);
        }
        Ok(map)
    }

    /// Перезаписать чанки файла одной транзакцией.
    pub fn upsert_file(
        &self,
        path: &str,
        hash: &str,
        items: &[(Chunk, Vec<f32>)],
    ) -> Result<(), rusqlite::Error> {
        let tx = self.conn.unchecked_transaction()?;
        tx.execute("DELETE FROM chunks WHERE path = ?1", params![path])?;
        {
            let mut stmt = tx.prepare(
                "INSERT INTO chunks(path, start_line, end_line, vec) VALUES (?1, ?2, ?3, ?4)",
            )?;
            for (chunk, vec) in items {
                stmt.execute(params![path, chunk.start, chunk.end, vec_to_blob(vec)])?;
            }
        }
        tx.execute(
            "INSERT OR REPLACE INTO files(path, hash, chunks) VALUES (?1, ?2, ?3)",
            params![path, hash, items.len() as i64],
        )?;
        tx.commit()
    }

    /// Удалить файл из индекса.
    pub fn remove_file(&self, path: &str) -> Result<(), rusqlite::Error> {
        self.conn
            .execute("DELETE FROM chunks WHERE path = ?1", params![path])?;
        self.conn
            .execute("DELETE FROM files WHERE path = ?1", params![path])?;
        Ok(())
    }

    /// Вычистить файлы, которых больше нет в рабочей директории.
    /// Возвращает число удалённых записей.
    pub fn prune_missing(
        &self,
        present: &std::collections::HashSet<String>,
    ) -> Result<usize, rusqlite::Error> {
        let stale: Vec<String> = {
            let mut stmt = self.conn.prepare("SELECT path FROM files")?;
            let rows = stmt.query_map([], |r| r.get::<_, String>(0))?;
            rows.filter_map(|p| p.ok())
                .filter(|p| !present.contains(p))
                .collect()
        };
        for p in &stale {
            self.remove_file(p)?;
        }
        Ok(stale.len())
    }

    /// (файлов, чанков) в индексе — для статуса «индекс пуст/готов».
    pub fn stats(&self) -> Result<(usize, usize), rusqlite::Error> {
        let files: i64 = self
            .conn
            .query_row("SELECT COUNT(*) FROM files", [], |r| r.get(0))
            .unwrap_or(0);
        let chunks: i64 = self
            .conn
            .query_row("SELECT COUNT(*) FROM chunks", [], |r| r.get(0))
            .unwrap_or(0);
        Ok((files as usize, chunks as usize))
    }

    /// Brute-force косинус-поиск: топ `top_k` чанков по убыванию близости.
    /// Вектора читаются потоково из базы — в памяти только топ-k.
    pub fn search(&self, query: &[f32], top_k: usize) -> Result<Vec<Hit>, rusqlite::Error> {
        let top_k = top_k.clamp(1, 100);
        let mut stmt = self
            .conn
            .prepare("SELECT path, start_line, end_line, vec FROM chunks")?;
        let rows = stmt.query_map([], |r| {
            Ok((
                r.get::<_, String>(0)?,
                r.get::<_, i64>(1)?,
                r.get::<_, i64>(2)?,
                r.get::<_, Vec<u8>>(3)?,
            ))
        })?;
        let mut hits: Vec<Hit> = Vec::new();
        for row in rows {
            let (path, start, end, blob) = row?;
            let vec = blob_to_vec(&blob);
            let score = cosine(query, &vec);
            if score <= 0.0 {
                continue;
            }
            hits.push(Hit {
                path,
                start: start as u32,
                end: end as u32,
                score,
            });
            if hits.len() > top_k * 4 {
                // Держим список коротким: частичная сортировка вместо полного склада.
                hits.sort_by(|a, b| b.score.total_cmp(&a.score));
                hits.truncate(top_k);
            }
        }
        hits.sort_by(|a, b| b.score.total_cmp(&a.score));
        hits.truncate(top_k);
        Ok(hits)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn sample_vec(x: f32) -> Vec<f32> {
        vec![x, 1.0 - x, 0.5]
    }

    #[test]
    fn chunker_splits_with_overlap() {
        let text: String = (1..=100).map(|i| format!("строка {i}\n")).collect();
        let chunks = chunk_text(&text);
        assert_eq!(chunks[0].start, 1);
        assert_eq!(chunks[0].end, 40);
        assert_eq!(chunks[1].start, 36); // 40 - 5 перекрытие + 1
        assert_eq!(chunks[1].end, 75);
        let last = chunks.last().unwrap();
        assert_eq!(last.end, 100);
        assert!(last.text.contains("строка 100"));
    }

    #[test]
    fn chunker_short_and_empty() {
        assert!(chunk_text("").is_empty());
        let chunks = chunk_text("одна строка");
        assert_eq!(chunks.len(), 1);
        assert_eq!(chunks[0].start, 1);
        assert_eq!(chunks[0].end, 1);
    }

    #[test]
    fn chunker_tail_merges_into_previous() {
        // 41 строка: второй чанк — огрызок в 1 строку, обязан приклеиться к первому.
        let text: String = (1..=41).map(|i| format!("l{i}\n")).collect();
        let chunks = chunk_text(&text);
        assert_eq!(chunks.len(), 1, "огрызок должен слиться: {chunks:?}");
        assert_eq!(chunks[0].end, 41);
    }

    #[test]
    fn cosine_basics() {
        let a = vec![1.0, 0.0, 0.0];
        assert!((cosine(&a, &a) - 1.0).abs() < 1e-6);
        assert!(cosine(&a, &[0.0, 1.0, 0.0]).abs() < 1e-6);
        assert_eq!(cosine(&a, &[0.0, 0.0, 0.0]), 0.0);
        assert_eq!(cosine(&a, &[]), 0.0);
        assert_eq!(cosine(&a, &[1.0, 2.0]), 0.0); // разные размерности
    }

    #[test]
    fn content_hash_is_stable() {
        assert_eq!(content_hash("abc"), content_hash("abc"));
        assert_ne!(content_hash("abc"), content_hash("abd"));
    }

    #[test]
    fn local_embedding_is_deterministic_and_normalized() {
        let a = local_embedding("fn approval_policy_journal() { /* x */ }");
        let b = local_embedding("fn approval_policy_journal() { /* x */ }");
        assert_eq!(a, b);
        assert_eq!(a.len(), LOCAL_EMBED_DIM);
        let norm: f64 = a
            .iter()
            .map(|x| (*x as f64) * (*x as f64))
            .sum::<f64>()
            .sqrt();
        assert!((norm - 1.0).abs() < 1e-3, "L2-нормировка: {norm}");
    }

    #[test]
    fn local_embedding_ranks_overlap_above_disjoint() {
        let q = local_embedding("approval policy journal");
        let rel = local_embedding("pub fn log_approval(policy: ApprovalPolicy) -> Journal");
        let unrel = local_embedding("vec![1.0, 2.0].iter().map(|x| x * 3).sum::<i32>()");
        assert!(cosine(&q, &rel) > cosine(&q, &unrel));
        assert!(cosine(&q, &rel) > 0.1, "лексический отклик слишком слаб");
    }

    #[test]
    fn local_embedding_camel_case_splits() {
        let q = local_embedding("approve user");
        let hit = local_embedding("function approveUser() {}");
        let miss = local_embedding("function deleteSession() {}");
        assert!(cosine(&q, &hit) > cosine(&q, &miss));
    }

    #[test]
    fn local_embedding_empty_text_is_zero_vector() {
        let v = local_embedding("");
        assert_eq!(v.len(), LOCAL_EMBED_DIM);
        assert!(v.iter().all(|x| *x == 0.0));
    }

    #[test]
    fn embedder_marker_roundtrip_and_clear() {
        let ix = SemanticIndex::in_memory().unwrap();
        assert_eq!(ix.embedder().unwrap(), None);
        ix.set_embedder("local:v1").unwrap();
        assert_eq!(ix.embedder().unwrap().as_deref(), Some("local:v1"));
        let c = Chunk {
            start: 1,
            end: 2,
            text: "x".into(),
        };
        ix.upsert_file("a.rs", "h", &[(c, sample_vec(0.5))])
            .unwrap();
        assert_eq!(ix.stats().unwrap(), (1, 1));
        ix.clear_all().unwrap();
        assert_eq!(ix.stats().unwrap(), (0, 0));
        // Маркер переживает очистку чанков: это история о пространстве, не о данных.
        assert_eq!(ix.embedder().unwrap().as_deref(), Some("local:v1"));
    }

    #[test]
    fn blob_roundtrip() {
        let v = vec![1.5, -2.25, 0.0, f32::MAX];
        assert_eq!(blob_to_vec(&vec_to_blob(&v)), v);
    }

    #[test]
    fn index_roundtrip_search_and_prune() {
        let ix = SemanticIndex::in_memory().unwrap();
        let c1 = Chunk {
            start: 1,
            end: 40,
            text: "a".into(),
        };
        let c2 = Chunk {
            start: 36,
            end: 75,
            text: "b".into(),
        };
        ix.upsert_file(
            "src/a.rs",
            "h1",
            &[(c1.clone(), sample_vec(0.9)), (c2.clone(), sample_vec(0.1))],
        )
        .unwrap();
        ix.upsert_file("src/b.rs", "h2", &[(c1.clone(), sample_vec(0.5))])
            .unwrap();
        assert_eq!(ix.stats().unwrap(), (2, 3));

        let hits = ix.search(&sample_vec(0.9), 2).unwrap();
        assert_eq!(hits.len(), 2);
        assert_eq!(hits[0].path, "src/a.rs");
        assert_eq!(hits[0].start, 1);
        assert!(hits[0].score > hits[1].score);

        let hashes = ix.file_hashes().unwrap();
        assert_eq!(hashes.get("src/a.rs").map(|s| s.as_str()), Some("h1"));

        let mut present = std::collections::HashSet::new();
        present.insert("src/a.rs".to_string());
        assert_eq!(ix.prune_missing(&present).unwrap(), 1);
        assert_eq!(ix.stats().unwrap(), (1, 2));

        ix.remove_file("src/a.rs").unwrap();
        assert_eq!(ix.stats().unwrap(), (0, 0));
        assert!(ix.search(&sample_vec(0.9), 5).unwrap().is_empty());
    }

    #[test]
    fn upsert_replaces_old_chunks() {
        let ix = SemanticIndex::in_memory().unwrap();
        let c = Chunk {
            start: 1,
            end: 40,
            text: "a".into(),
        };
        ix.upsert_file(
            "f.rs",
            "h1",
            &[
                (c.clone(), sample_vec(0.9)),
                (
                    Chunk {
                        start: 36,
                        end: 70,
                        text: "b".into(),
                    },
                    sample_vec(0.4),
                ),
            ],
        )
        .unwrap();
        ix.upsert_file("f.rs", "h2", &[(c, sample_vec(0.2))])
            .unwrap();
        assert_eq!(ix.stats().unwrap(), (1, 1));
        assert_eq!(ix.file_hashes().unwrap()["f.rs"], "h2");
    }
}

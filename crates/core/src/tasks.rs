//! E-5: фоновые задачи — таблица `tasks` и домен воркера.
//!
//! Задача: id (ключ дедупликации), kind (обработчик в app-слое), payload
//! (JSON-аргументы), состояние, попытки и срок следующего старта.
//! Ретраи — механика B-6: экспоненциальный backoff 30 с → 30 мин;
//! после исчерпания попыток задача встаёт в `failed` (журнал исполнения
//! — колонка last_error и метки времени). Периодическая задача несёт
//! `every_ms` и после успеха перевооружается воркером.
//!
//! Состояния — строки (wire-формат без enum-миграций):
//! queued → running → done | failed | cancelled.

use serde::{Deserialize, Serialize};
use serde_json::Value;

pub const TASK_QUEUED: &str = "queued";
pub const TASK_RUNNING: &str = "running";
pub const TASK_DONE: &str = "done";
pub const TASK_FAILED: &str = "failed";
pub const TASK_CANCELLED: &str = "cancelled";

/// База backoff: 30 с, рост x2, потолок 30 мин (механика B-6).
pub const TASK_BACKOFF_BASE_MS: u64 = 30_000;
pub const TASK_BACKOFF_CAP_MS: u64 = 1_800_000;

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct Task {
    pub id: String,
    pub kind: String,
    #[serde(default = "empty_object")]
    pub payload: Value,
    #[serde(default = "queued_state")]
    pub state: String,
    #[serde(default)]
    pub attempts: u32,
    #[serde(default)]
    pub next_try_ms: u64,
    /// Журнал исполнения: текст последнего результата/ошибки.
    #[serde(default)]
    pub last_error: String,
    /// Some — периодическая задача (после успеха снова в очередь).
    #[serde(default)]
    pub every_ms: Option<u64>,
    #[serde(default)]
    pub created_ms: u64,
    #[serde(default)]
    pub updated_ms: u64,
}

fn empty_object() -> Value {
    serde_json::json!({})
}

fn queued_state() -> String {
    TASK_QUEUED.to_string()
}

impl Task {
    /// Разовая задача, готовая к немедленному старту (next_try = now).
    pub fn new(
        id: impl Into<String>,
        kind: impl Into<String>,
        payload: Value,
        now_ms: u64,
    ) -> Task {
        Task {
            id: id.into(),
            kind: kind.into(),
            payload,
            state: TASK_QUEUED.to_string(),
            attempts: 0,
            next_try_ms: now_ms,
            last_error: String::new(),
            every_ms: None,
            created_ms: now_ms,
            updated_ms: now_ms,
        }
    }

    /// Периодическая задача: первый старт через `every_ms`, далее по
    /// завершению воркер перевооружает next_try = now + every_ms.
    pub fn periodic(
        id: impl Into<String>,
        kind: impl Into<String>,
        payload: Value,
        every_ms: u64,
        now_ms: u64,
    ) -> Task {
        Task {
            every_ms: Some(every_ms),
            next_try_ms: now_ms + every_ms,
            ..Task::new(id, kind, payload, now_ms)
        }
    }
}

/// Задержка ретрая по числу попыток (attempts >= 1): 30 с, 60 с, 2 мин,
/// 4 мин... потолок 30 мин. Чистая функция — тестируется без часов.
pub fn task_backoff_ms(attempts: u32) -> u64 {
    let exp = attempts.saturating_sub(1).min(10);
    TASK_BACKOFF_BASE_MS
        .saturating_mul(1u64 << exp)
        .min(TASK_BACKOFF_CAP_MS)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::store::{SqliteStore, Store};
    use serde_json::json;

    #[test]
    fn backoff_is_exponential_and_capped() {
        assert_eq!(task_backoff_ms(0), TASK_BACKOFF_BASE_MS);
        assert_eq!(task_backoff_ms(1), 30_000);
        assert_eq!(task_backoff_ms(2), 60_000);
        assert_eq!(task_backoff_ms(3), 120_000);
        assert_eq!(task_backoff_ms(4), 240_000);
        assert_eq!(task_backoff_ms(5), 480_000);
        assert_eq!(task_backoff_ms(6), 960_000);
        assert_eq!(task_backoff_ms(7), TASK_BACKOFF_CAP_MS);
        assert_eq!(task_backoff_ms(500), TASK_BACKOFF_CAP_MS, "потолок держит");
        let mut prev = 0;
        for a in 1..=12 {
            let v = task_backoff_ms(a);
            assert!(v >= prev, "монотонно");
            prev = v;
        }
    }

    #[test]
    fn task_constructors_and_serde() {
        let one = Task::new("a", "git_fetch", json!({"cwd": "x"}), 1000);
        assert_eq!(one.state, TASK_QUEUED);
        assert_eq!(one.next_try_ms, 1000);
        assert_eq!(one.every_ms, None);
        let per = Task::periodic("p", "semantic_reindex", json!({}), 86_400_000, 1000);
        assert_eq!(per.next_try_ms, 1000 + 86_400_000);
        assert_eq!(per.every_ms, Some(86_400_000));
        // Wire-формат: задачи уходят в UI/команды serde'ом.
        let wire = serde_json::to_string(&per).unwrap();
        let back: Task = serde_json::from_str(&wire).unwrap();
        assert_eq!(back, per);
        // Дефолты: payload/state не обязательны в JSON.
        let bare: Task = serde_json::from_str(r#"{"id":"x","kind":"k"}"#).unwrap();
        assert_eq!(bare.state, TASK_QUEUED);
        assert_eq!(bare.payload, json!({}));
    }

    #[test]
    fn store_tasks_roundtrip_enqueue_claim_update() {
        let st = SqliteStore::in_memory().expect("in-memory база");
        let t = Task::new("a", "semantic_reindex", json!({"cwd": "x"}), 1000);
        st.task_enqueue(&t).unwrap();
        st.task_enqueue(&t).unwrap(); // повтор игнорируется: id — ключ дедупликации
        assert_eq!(st.tasks_list(10).unwrap().len(), 1);

        // Срок не наступил — claim пуст.
        assert!(st.task_claim_due(999).unwrap().is_empty());

        let due = st.task_claim_due(2000).unwrap();
        assert_eq!(due.len(), 1);
        assert_eq!(due[0].id, "a");
        assert_eq!(due[0].state, TASK_RUNNING);
        assert_eq!(due[0].attempts, 1, "claim считает попытку");
        assert_eq!(due[0].payload, json!({"cwd": "x"}));

        // Второй claim не берёт running повторно.
        assert!(st.task_claim_due(2000).unwrap().is_empty());

        // Краш-восстановление: running → queued.
        st.tasks_recover_running().unwrap();
        let t2 = st.task_get("a").unwrap().unwrap();
        assert_eq!(t2.state, TASK_QUEUED);
        assert_eq!(t2.next_try_ms, 0);

        // Успех: done, attempts сброшены, журнал в last_error.
        st.task_update("a", TASK_DONE, "ok: 42", 0, 0).unwrap();
        let t3 = st.task_get("a").unwrap().unwrap();
        assert_eq!(t3.state, TASK_DONE);
        assert_eq!(t3.attempts, 0);
        assert_eq!(t3.last_error, "ok: 42");
        assert!(t3.updated_ms > 0);

        // Гигиена: старые done удаляются.
        st.tasks_prune(u64::MAX).unwrap();
        assert!(st.task_get("a").unwrap().is_none());
        assert!(st.tasks_list(10).unwrap().is_empty());
    }

    #[test]
    fn store_tasks_cancel_sticks_and_periodic_rearms() {
        let st = SqliteStore::in_memory().expect("in-memory база");
        let t = Task::new("c", "git_fetch", json!({}), 0);
        st.task_enqueue(&t).unwrap();
        st.task_update("c", TASK_CANCELLED, "отменена", 0, 0)
            .unwrap();
        // Повторный сид не воскрешает отменённую задачу (INSERT OR IGNORE).
        st.task_enqueue(&t).unwrap();
        assert_eq!(st.task_get("c").unwrap().unwrap().state, TASK_CANCELLED);

        let p = Task::periodic("p", "semantic_reindex", json!({}), 86_400_000, 0);
        st.task_enqueue(&p).unwrap();
        assert_eq!(
            st.task_get("p").unwrap().unwrap().every_ms,
            Some(86_400_000)
        );
        let due = st.task_claim_due(86_400_000).unwrap();
        assert_eq!(due.len(), 1);
        // Перевооружение после успеха: снова queued со следующим сроком.
        st.task_update("p", TASK_QUEUED, "ok", 0, 200_000_000)
            .unwrap();
        let p2 = st.task_get("p").unwrap().unwrap();
        assert_eq!(p2.state, TASK_QUEUED);
        assert_eq!(p2.next_try_ms, 200_000_000);
        assert_eq!(p2.attempts, 0);
        assert_eq!(p2.every_ms, Some(86_400_000), "every_ms переживает цикл");
    }

    #[test]
    fn store_tasks_list_respects_limit_and_order() {
        let st = SqliteStore::in_memory().expect("in-memory база");
        for i in 0..5 {
            st.task_enqueue(&Task::new(
                format!("t{i}"),
                "git_fetch",
                json!({}),
                i * 1000,
            ))
            .unwrap();
        }
        let all = st.tasks_list(3).unwrap();
        assert_eq!(all.len(), 3);
        assert_eq!(all[0].id, "t4", "свежие первыми");
        assert_eq!(st.tasks_list(0).unwrap().len(), 1, "лимит зажат");
    }
}

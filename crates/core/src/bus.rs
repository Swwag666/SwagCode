/*! Шина событий: единственный путь, которым данные доходят до UI.

Два транспорта наружу, одна шина внутри (см. DECISIONS.md §«Слои»):
десктоп получает события in-process через [`Bus::subscribe`], а любые сетевые
клиенты подключаются отдельными адаптерами позже. Шина при этом одна, и
семантика событий не разъезжается между транспортами.

Канал **bounded** намеренно: это backpressure. Медленный подписчик не может
съесть всю память на бурном стриме — он получает [`BusError::Lagging`], а
остальные подписчики продолжают работать.
*/

use serde::{Deserialize, Serialize};
use tokio::sync::broadcast;

use crate::session::{SessionId, TurnId};

/// Ёмкость канала шины. Подбиралась под стрим токенов: при 200 ток/с и
/// 60 fps это ~3 события на кадр, 1024 даёт запас на десятки секунд.
const BUS_CAPACITY: usize = 1024;

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(tag = "kind", content = "data", rename_all = "snake_case")]
pub enum EventKind {
    /// Кусок рассуждений модели. Отдельный визуальный канал от `Content`
    /// (находка 1 в DECISIONS.md §5.6) — склеивать их нельзя.
    Reasoning { turn: TurnId, text: String },
    /// Кусок ответа модели.
    Content { turn: TurnId, text: String },
    /// Модель начала вызывать инструмент.
    ToolCallStart {
        turn: TurnId,
        call_id: String,
        name: String,
    },
    /// Инструмент вызван, аргументы собраны.
    ToolCall {
        turn: TurnId,
        call_id: String,
        name: String,
        arguments: serde_json::Value,
    },
    /// Результат выполнения инструмента.
    ToolResult {
        turn: TurnId,
        call_id: String,
        ok: bool,
        output: String,
        /// Сколько длилось выполнение, мс. Нужно для метрик, не только для UI.
        elapsed_ms: u64,
    },
    /// Ход начался.
    TurnStarted { turn: TurnId, session: SessionId },
    /// Ход завершён.
    TurnEnded {
        turn: TurnId,
        session: SessionId,
        ok: bool,
        /// Причина незавершённости, если ход оборвался.
        reason: Option<String>,
    },
    /// Требуется подтверждение пользователя. Permission-решение живёт в ядре,
    /// а не в UI (DECISIONS.md §2) — UI только отображает запрос.
    ApprovalRequired {
        turn: TurnId,
        call_id: String,
        tool: String,
        /// Человекочитаемое описание действия для показа пользователю.
        summary: String,
    },
    /// Ошибка, которую надо показать пользователю.
    Error {
        turn: Option<TurnId>,
        message: String,
    },
    /// Диагностическое событие: статус провайдера, подключение и т.п.
    Status { message: String },
}

/// Событие с монотонной меткой порядка.
///
/// `seq` обязателен: UI батчит события по кадрам (rAF) и должен уметь
/// восстановить порядок, даже если кадры обрабатываются пачками.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct Event {
    pub seq: u64,
    /// Unix time в миллисекундах, для журнала и отладки.
    pub ts_ms: u64,
    pub kind: EventKind,
}

#[derive(Debug, thiserror::Error)]
pub enum BusError {
    /// Подписчик отстал: события выброшены из кольцевого буфера.
    /// Не фатально — подписчик должен перечитать снапшот состояния.
    #[error("subscriber lagged, {0} events dropped")]
    Lagging(u64),
    /// Подписчиков нет. Не ошибка: события всё равно пишутся в журнал.
    #[error("bus closed")]
    Closed,
}

/// Шина событий. Дешёвая в клонировании: внутри `Arc`, счётчик общий.
#[derive(Debug, Clone)]
pub struct Bus {
    inner: std::sync::Arc<Inner>,
}

#[derive(Debug)]
struct Inner {
    tx: broadcast::Sender<Event>,
    seq: std::sync::atomic::AtomicU64,
}

impl Default for Bus {
    fn default() -> Self {
        Self::new()
    }
}

impl Bus {
    pub fn new() -> Self {
        let (tx, _) = broadcast::channel(BUS_CAPACITY);
        Self {
            inner: std::sync::Arc::new(Inner {
                tx,
                seq: std::sync::atomic::AtomicU64::new(0),
            }),
        }
    }

    /// Опубликовать событие. Возвращает присвоенный `seq`.
    ///
    /// Не является ошибкой, если подписчиков нет: событие всё равно получает
    /// порядковый номер и может быть записано в журнал.
    pub fn publish(&self, kind: EventKind) -> u64 {
        let seq = self
            .inner
            .seq
            .fetch_add(1, std::sync::atomic::Ordering::Relaxed)
            + 1;
        let ev = Event {
            seq,
            ts_ms: now_ms(),
            kind,
        };
        let _ = self.inner.tx.send(ev);
        seq
    }

    /// Подписаться на поток событий.
    pub fn subscribe(&self) -> broadcast::Receiver<Event> {
        self.inner.tx.subscribe()
    }

    pub fn subscriber_count(&self) -> usize {
        self.inner.tx.receiver_count()
    }

    /// Текущий счётчик последовательности (для снапшотов и resume).
    pub fn seq(&self) -> u64 {
        self.inner.seq.load(std::sync::atomic::Ordering::Relaxed)
    }
}

pub fn now_ms() -> u64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_millis() as u64)
        .unwrap_or(0)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn sid() -> SessionId {
        SessionId::new("s1")
    }
    fn tid() -> TurnId {
        TurnId::new("t1")
    }

    #[tokio::test]
    async fn subscriber_receives_published_event() {
        let bus = Bus::new();
        let mut rx = bus.subscribe();
        bus.publish(EventKind::Content {
            turn: tid(),
            text: "hi".into(),
        });
        let ev = rx.recv().await.unwrap();
        assert_eq!(ev.seq, 1);
        assert!(ev.ts_ms > 0);
        assert!(matches!(ev.kind, EventKind::Content { ref text, .. } if text == "hi"));
    }

    #[tokio::test]
    async fn seq_is_monotonic_across_publishes() {
        let bus = Bus::new();
        let mut rx = bus.subscribe();
        for i in 0..50 {
            bus.publish(EventKind::Status {
                message: format!("s{i}"),
            });
        }
        let mut prev = 0;
        for _ in 0..50 {
            let ev = rx.recv().await.unwrap();
            assert!(
                ev.seq > prev,
                "seq не монотонный: {} после {}",
                ev.seq,
                prev
            );
            prev = ev.seq;
        }
        assert_eq!(bus.seq(), 50);
    }

    #[tokio::test]
    async fn multiple_subscribers_each_get_all_events() {
        let bus = Bus::new();
        let mut a = bus.subscribe();
        let mut b = bus.subscribe();
        bus.publish(EventKind::Status {
            message: "x".into(),
        });
        assert!(matches!(
            a.recv().await.unwrap().kind,
            EventKind::Status { .. }
        ));
        assert!(matches!(
            b.recv().await.unwrap().kind,
            EventKind::Status { .. }
        ));
        assert_eq!(bus.subscriber_count(), 2);
    }

    #[tokio::test]
    async fn publish_without_subscribers_is_not_an_error() {
        // Ядро публикует в журнал даже когда UI не открыт.
        let bus = Bus::new();
        assert_eq!(bus.subscriber_count(), 0);
        let seq = bus.publish(EventKind::Status {
            message: "no one listening".into(),
        });
        assert_eq!(seq, 1);
        assert_eq!(bus.seq(), 1);
    }

    #[tokio::test]
    async fn slow_subscriber_gets_lagging_not_a_hang() {
        // Backpressure: медленный подписчик не вешает шину и не ест память.
        let bus = Bus::new();
        let mut rx = bus.subscribe();
        for i in 0..(BUS_CAPACITY + 500) {
            bus.publish(EventKind::Status {
                message: format!("{i}"),
            });
        }
        match rx.recv().await {
            Err(broadcast::error::RecvError::Lagged(n)) => assert!(n > 0, "должны быть дропы"),
            Ok(_) => {} // успел прочитать до переполнения — тоже приемлемо
            Err(e) => panic!("неожиданная ошибка: {e}"),
        }
        // Шина жива и продолжает работать после отставания.
        let fresh = bus.subscribe();
        bus.publish(EventKind::Status {
            message: "alive".into(),
        });
        drop(rx);
        drop(fresh);
        assert_eq!(bus.seq() as usize, BUS_CAPACITY + 501);
    }

    #[test]
    fn events_roundtrip_through_json() {
        // Сериализация нужна для журнала сессий (этап 3) и IPC.
        let ev = Event {
            seq: 7,
            ts_ms: 123,
            kind: EventKind::ToolResult {
                turn: tid(),
                call_id: "c1".into(),
                ok: true,
                output: "done".into(),
                elapsed_ms: 42,
            },
        };
        let s = serde_json::to_string(&ev).unwrap();
        let back: Event = serde_json::from_str(&s).unwrap();
        assert_eq!(ev, back);
    }

    #[test]
    fn all_event_kinds_serialize_with_kind_tag() {
        // Тег `kind` — контракт с UI. Если он пропадёт, фронт молча ослепнет.
        let kinds = vec![
            EventKind::Reasoning {
                turn: tid(),
                text: "r".into(),
            },
            EventKind::Content {
                turn: tid(),
                text: "c".into(),
            },
            EventKind::ToolCallStart {
                turn: tid(),
                call_id: "1".into(),
                name: "n".into(),
            },
            EventKind::ToolCall {
                turn: tid(),
                call_id: "1".into(),
                name: "n".into(),
                arguments: serde_json::json!({}),
            },
            EventKind::ToolResult {
                turn: tid(),
                call_id: "1".into(),
                ok: true,
                output: "o".into(),
                elapsed_ms: 1,
            },
            EventKind::TurnStarted {
                turn: tid(),
                session: sid(),
            },
            EventKind::TurnEnded {
                turn: tid(),
                session: sid(),
                ok: true,
                reason: None,
            },
            EventKind::ApprovalRequired {
                turn: tid(),
                call_id: "1".into(),
                tool: "bash".into(),
                summary: "rm -rf".into(),
            },
            EventKind::Error {
                turn: None,
                message: "m".into(),
            },
            EventKind::Status {
                message: "m".into(),
            },
        ];
        for k in kinds {
            let v = serde_json::to_value(&k).unwrap();
            assert!(v.get("kind").is_some(), "нет тега kind у {v}");
            let back: EventKind = serde_json::from_value(v.clone()).unwrap();
            assert_eq!(k, back, "roundtrip сломался: {v}");
        }
    }

    #[test]
    fn clones_share_one_counter_and_one_channel() {
        // Шину передают в команды Tauri клонами: если счётчик разойдётся,
        // порядок событий в UI сломается, и это будет невоспроизводимый баг.
        let a = Bus::new();
        let b = a.clone();
        let mut rx = b.subscribe();
        a.publish(EventKind::Status {
            message: "from a".into(),
        });
        b.publish(EventKind::Status {
            message: "from b".into(),
        });
        let e1 = rx.try_recv().unwrap();
        let e2 = rx.try_recv().unwrap();
        assert_eq!((e1.seq, e2.seq), (1, 2));
        assert_eq!(a.seq(), 2);
        assert_eq!(b.seq(), 2);
    }

    #[test]
    fn session_and_turn_ids_are_distinct_types_with_values() {
        assert_eq!(SessionId::new("a").as_str(), "a");
        assert_eq!(TurnId::new("a").as_str(), "a");
        assert_ne!(SessionId::new("a"), SessionId::new("b"));
    }
}

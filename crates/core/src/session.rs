/*! Домен-типы сессии и хода.

Здесь только данные и чистая логика. Никакого I/O: сессию можно
восстановить из журнала, передать в тест или сериализовать, не трогая сеть
и файловую систему.
*/

use serde::{Deserialize, Serialize};

use swagcod_provider::types::{ChatMessage, ToolCall};

/// Идентификатор сессии. Отдельный тип от [`TurnId`], чтобы перепутать их
/// было ошибкой компиляции, а не багом в рантайме.
#[derive(Debug, Clone, PartialEq, Eq, Hash, Serialize, Deserialize)]
pub struct SessionId(String);

/// Идентификатор хода (одна итерация «запрос модели → исполнение тулзов»).
#[derive(Debug, Clone, PartialEq, Eq, Hash, Serialize, Deserialize)]
pub struct TurnId(String);

macro_rules! id_impl {
    ($t:ty) => {
        impl $t {
            pub fn new(s: impl Into<String>) -> Self {
                Self(s.into())
            }
            pub fn as_str(&self) -> &str {
                &self.0
            }
            pub fn is_empty(&self) -> bool {
                self.0.is_empty()
            }
        }
        impl std::fmt::Display for $t {
            fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
                f.write_str(&self.0)
            }
        }
        impl AsRef<str> for $t {
            fn as_ref(&self) -> &str {
                &self.0
            }
        }
    };
}

id_impl!(SessionId);
id_impl!(TurnId);

/// Состояние сессии.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum SessionStatus {
    /// Создана, ходов не было.
    Idle,
    /// Идёт ход: модель генерирует или тулзы исполняются.
    Running,
    /// Ждём подтверждения пользователя (permission-гейт).
    WaitingApproval,
    /// Ход завершён успешно, можно начинать новый.
    Ready,
    /// Ход оборвался ошибкой.
    Failed,
    /// Остановлен пользователем.
    Cancelled,
}

impl SessionStatus {
    /// Можно ли начать новый ход из этого состояния.
    ///
    /// Занятая сессия не принимает ход: сообщение уходит в очередь, а не
    /// теряется и не отправляется вторым параллельным ходом.
    pub fn can_start_turn(self) -> bool {
        matches!(
            self,
            Self::Idle | Self::Ready | Self::Failed | Self::Cancelled
        )
    }

    /// Сессия сейчас занята (нельзя дёргать параллельно).
    pub fn is_busy(self) -> bool {
        matches!(self, Self::Running | Self::WaitingApproval)
    }
}

/// Один завершённый ход — запись журнала.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct TurnRecord {
    pub id: TurnId,
    pub started_ms: u64,
    pub ended_ms: Option<u64>,
    /// Ответ модели (без reasoning).
    pub content: String,
    /// Поток рассуждений, если был.
    pub reasoning: String,
    pub tool_calls: Vec<ToolCall>,
    /// Суммарно токенов, по нашей оценке: провайдер в стриме их не отдаёт
    /// (`usage` приходит `null` — DECISIONS.md §5.6), считаем сами.
    pub est_input_tokens: u32,
    pub est_output_tokens: u32,
    pub ok: bool,
    pub failure: Option<String>,
    /// E-6: дерево суб-агентов — родительский ход (None у ходов верхнего
    /// уровня). serde-default: старые журналы без поля читаются как раньше.
    #[serde(default)]
    pub parent_turn_id: Option<TurnId>,
}

/// Сессия: рабочее пространство, история, текущее состояние.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Session {
    pub id: SessionId,
    pub title: String,
    /// Рабочая директория. Все файловые тулзы резолвятся относительно неё —
    /// это же граница песочницы.
    pub cwd: String,
    pub model: String,
    pub status: SessionStatus,
    pub current_turn: Option<TurnId>,
    /// История в wire-формате провайдера: именно её отправляем на следующий ход.
    pub history: Vec<ChatMessage>,
    /// B-3: свёртка старых ходов суммаризатором. Не хранится в history:
    /// драйвер подставляет её в system-промпт каждого запроса свежей.
    #[serde(default)]
    pub summary: String,
    /// B-7: политика подтверждений этой сессии. None — наследуется от
    /// глобальной настройки приложения.
    #[serde(default)]
    pub approval_policy: Option<crate::turn::ApprovalPolicy>,
    /// Завершённые ходы — журнал.
    pub turns: Vec<TurnRecord>,
    pub created_ms: u64,
}

impl Session {
    pub fn new(id: SessionId, cwd: impl Into<String>, model: impl Into<String>) -> Self {
        Self {
            id,
            title: String::new(),
            cwd: cwd.into(),
            model: model.into(),
            status: SessionStatus::Idle,
            current_turn: None,
            history: Vec::new(),
            summary: String::new(),
            approval_policy: None,
            turns: Vec::new(),
            created_ms: crate::bus::now_ms(),
        }
    }

    /// Добавить пользовательское сообщение. Возвращает `false`, если сессия
    /// занята — тогда вызывающий обязан поставить сообщение в очередь.
    pub fn push_user_message(&mut self, text: impl Into<String>) -> bool {
        if !self.status.can_start_turn() {
            return false;
        }
        self.history.push(ChatMessage::user(text));
        true
    }

    /// Зафиксировать ответ модели в истории.
    pub fn push_assistant_message(&mut self, msg: ChatMessage) {
        self.history.push(msg);
    }

    /// Зафиксировать результат инструмента в истории.
    pub fn push_tool_result(&mut self, call_id: impl Into<String>, output: impl Into<String>) {
        self.history.push(ChatMessage {
            role: swagcod_provider::types::Role::Tool,
            content: output.into(),
            reasoning: String::new(),
            tool_calls: Vec::new(),
            tool_call_id: Some(call_id.into()),
        });
    }

    /// Оценка числа токенов. Грубая, но воспроизводимая: ~4 символа на токен
    /// для английского и ~2 для CJK. Точность не критична — это метрика для
    /// UI и порога компакции, а не для биллинга.
    pub fn estimate_tokens(text: &str) -> u32 {
        if text.is_empty() {
            return 0;
        }
        // char-счёт, а не байты: иначе CJK завышает оценку втрое.
        let chars = text.chars().count() as u64;
        chars.div_ceil(4).max(1) as u32
    }

    /// Оценка размера контекста всей истории в токенах.
    pub fn estimate_context_tokens(&self) -> u32 {
        self.history
            .iter()
            .map(|m| Self::estimate_tokens(&m.content) + Self::estimate_tokens(&m.reasoning))
            .sum()
    }

    /// Завершённые ходы, от новых к старым — для списка в UI.
    pub fn turns_desc(&self) -> impl Iterator<Item = &TurnRecord> {
        self.turns.iter().rev()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn s() -> Session {
        Session::new(SessionId::new("s1"), "D:/proj", "test-model")
    }

    #[test]
    fn ids_display_and_compare() {
        let a = SessionId::new("x");
        assert_eq!(a.to_string(), "x");
        assert_eq!(a.as_str(), "x");
        assert_eq!(a, SessionId::new("x"));
        assert_ne!(a, SessionId::new("y"));
    }

    #[test]
    fn new_session_is_idle_and_accepts_messages() {
        let mut ses = s();
        assert_eq!(ses.status, SessionStatus::Idle);
        assert!(ses.status.can_start_turn());
        assert!(ses.push_user_message("привет"));
        assert_eq!(ses.history.len(), 1);
    }

    #[test]
    fn running_session_rejects_new_message() {
        // Занятая сессия не должна принимать ход: иначе два хода пойдут
        // параллельно в одну историю и перемешаются.
        let mut ses = s();
        ses.status = SessionStatus::Running;
        assert!(ses.status.is_busy());
        assert!(!ses.status.can_start_turn());
        assert!(!ses.push_user_message("пока занят"));
        assert!(
            ses.history.is_empty(),
            "сообщение не должно попасть в историю"
        );
    }

    #[test]
    fn waiting_approval_is_busy() {
        assert!(SessionStatus::WaitingApproval.is_busy());
        assert!(!SessionStatus::WaitingApproval.can_start_turn());
    }

    #[test]
    fn failed_and_cancelled_can_restart() {
        assert!(SessionStatus::Failed.can_start_turn());
        assert!(SessionStatus::Cancelled.can_start_turn());
        assert!(SessionStatus::Ready.can_start_turn());
    }

    #[test]
    fn history_keeps_role_order_for_wire_format() {
        let mut ses = s();
        ses.push_user_message("вопрос");
        ses.push_assistant_message(ChatMessage::assistant("ответ"));
        ses.push_tool_result("call_1", "результат");
        let roles: Vec<_> = ses.history.iter().map(|m| m.role).collect();
        assert_eq!(
            roles,
            vec![
                swagcod_provider::types::Role::User,
                swagcod_provider::types::Role::Assistant,
                swagcod_provider::types::Role::Tool
            ]
        );
        assert_eq!(ses.history[2].tool_call_id.as_deref(), Some("call_1"));
    }

    #[test]
    fn token_estimate_is_nonzero_for_any_nonempty_text() {
        assert_eq!(Session::estimate_tokens(""), 0);
        assert_eq!(Session::estimate_tokens("a"), 1);
        assert_eq!(Session::estimate_tokens("abcd"), 1);
        assert_eq!(Session::estimate_tokens("abcde"), 2);
    }

    #[test]
    fn token_estimate_counts_chars_not_bytes() {
        // CJK: 3 символа = 9 байт. Считаем по char, иначе оценка втрое выше.
        let cjk = "日本語";
        assert_eq!(Session::estimate_tokens(cjk), 1);
        let ascii = "abc";
        assert_eq!(Session::estimate_tokens(ascii), 1);
    }

    #[test]
    fn context_estimate_sums_history_including_reasoning() {
        let mut ses = s();
        ses.push_user_message("aaaaaaaa"); // 8 chars -> 2
        ses.push_assistant_message(ChatMessage {
            role: swagcod_provider::types::Role::Assistant,
            content: "bbbbbbbb".into(),   // 2
            reasoning: "cccccccc".into(), // 2
            tool_calls: vec![],
            tool_call_id: None,
        });
        assert_eq!(ses.estimate_context_tokens(), 6);
    }

    #[test]
    fn turns_desc_returns_newest_first() {
        let mut ses = s();
        for i in 0..3 {
            ses.turns.push(TurnRecord {
                id: TurnId::new(format!("t{i}")),
                started_ms: i,
                ended_ms: Some(i + 1),
                content: String::new(),
                reasoning: String::new(),
                tool_calls: vec![],
                est_input_tokens: 0,
                est_output_tokens: 0,
                ok: true,
                failure: None,
                parent_turn_id: None,
            });
        }
        let ids: Vec<_> = ses.turns_desc().map(|t| t.id.as_str()).collect();
        assert_eq!(ids, vec!["t2", "t1", "t0"]);
    }

    #[test]
    fn turn_record_parent_wire_compat() {
        /* E-6: старые журналы без parent_turn_id читаются как ходы верхнего
           уровня (serde-default), а дочерние сериализуются с полем. */
        let old = r#"{"id":"t1","started_ms":1,"ended_ms":null,"content":"","reasoning":"","tool_calls":[],"est_input_tokens":0,"est_output_tokens":0,"ok":true,"failure":null}"#;
        let rec: TurnRecord = serde_json::from_str(old).unwrap();
        assert_eq!(rec.parent_turn_id, None);

        let mut child = rec.clone();
        child.parent_turn_id = Some(TurnId::new("t0"));
        let json = serde_json::to_string(&child).unwrap();
        assert!(json.contains("parent_turn_id"));
        let back: TurnRecord = serde_json::from_str(&json).unwrap();
        assert_eq!(back, child);
        assert_eq!(back.parent_turn_id.as_ref().map(|p| p.as_str()), Some("t0"));
    }

    #[test]
    fn session_roundtrips_through_json() {
        // Нужно для журнала и resume после крэша (этап 3).
        let mut ses = s();
        ses.push_user_message("hi");
        let j = serde_json::to_string(&ses).unwrap();
        let back: Session = serde_json::from_str(&j).unwrap();
        assert_eq!(back.id, ses.id);
        assert_eq!(back.cwd, "D:/proj");
        assert_eq!(back.history.len(), 1);
    }
}

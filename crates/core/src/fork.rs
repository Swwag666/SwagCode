/*! Ветвление сессии (rev72): ответвление от текущего места разговора.

Владельцу нужен способ попробовать другой путь, не теряя того, что уже есть: не
«откатить и забыть», а «оставить как было и пойти иначе». История при этом -
единственное, что реально держит контекст: именно её отправляем модели на
следующий ход, поэтому ветка забирает историю целиком.

## Почему журнал ходов пуст, а история полна

`turns.id` - первичный ключ в базе. Копия тех же ходов с теми же id не вставится,
а с новыми id потеряет связь с чекпоинтами, по которым работает откат. Поэтому
журнал ветки начинается с нуля: первый её ход снимает собственный чекпоинт, и
откат в ветке работает независимо от родителя. Это не упрощение, а сохранение
инварианта «ход принадлежит своей сессии».

## Что наследуется

`cwd`, модель, свёртка старых ходов, политика подтверждений - то, что делает ветку
продолжением той же работы. Статус сбрасывается в [`SessionStatus::Idle`]:
ветка не занята, даже если родитель был посреди хода.
!*/

use crate::session::{Session, SessionId, SessionStatus};

/// Добавка к заголовку ветки.
pub const FORK_TITLE_SUFFIX: &str = " (ветка)";

/// Ответвить сессию: полная история, пустой журнал, своё имя.
///
/// `now_ms` передаётся снаружи, чтобы тесты не зависели от часов, а вызывающий
/// сам решал, чьё время писать.
pub fn fork_session(source: &Session, new_id: SessionId, now_ms: u64) -> Session {
    let title = if source.title.trim().is_empty() {
        // Пустой заголовок остаётся пустым: ветка безымянной сессии тоже
        // безымянна, иначе в списке появилось бы название из ничего.
        String::new()
    } else {
        format!("{}{FORK_TITLE_SUFFIX}", source.title)
    };
    Session {
        id: new_id,
        title,
        cwd: source.cwd.clone(),
        model: source.model.clone(),
        /* Ветка не занята, даже если родитель был посреди хода: два хода на одну
        историю - это гонка за файлы, которую никто не просил. */
        status: SessionStatus::Idle,
        current_turn: None,
        history: source.history.clone(),
        summary: source.summary.clone(),
        approval_policy: source.approval_policy,
        turns: Vec::new(),
        created_ms: now_ms,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use swagcod_provider::types::ChatMessage;

    fn source() -> Session {
        let mut s = Session::new(SessionId::new("s-1"), "D:/work", "claude-sonnet-4");
        s.title = "разбор парсера".to_string();
        s.summary = "кратко о старом".to_string();
        s.history = vec![
            ChatMessage::user("привет"),
            ChatMessage::assistant("да, слушаю"),
        ];
        s.status = SessionStatus::Running;
        s
    }

    #[test]
    fn fork_keeps_history_and_starts_a_clean_journal() {
        let src = source();
        let fork = fork_session(&src, SessionId::new("s-2"), 1_000);

        assert_eq!(fork.history.len(), 2, "контекст разговора сохранён");
        assert_eq!(fork.history[1].content, "да, слушаю");
        assert!(
            fork.turns.is_empty(),
            "журнал ветки пуст: id ходов - первичный ключ, копия не вставится"
        );
        assert_eq!(fork.cwd, "D:/work");
        assert_eq!(fork.model, "claude-sonnet-4");
        assert_eq!(fork.summary, "кратко о старом");
    }

    #[test]
    fn fork_is_not_busy_even_if_the_parent_was() {
        /* Родитель был посреди хода - ветка всё равно свободна: иначе два хода
        писали бы в одну директорию. */
        let src = source();
        assert_eq!(src.status, SessionStatus::Running);
        let fork = fork_session(&src, SessionId::new("s-2"), 1_000);
        assert_eq!(fork.status, SessionStatus::Idle);
        assert!(fork.current_turn.is_none());
    }

    #[test]
    fn fork_gets_its_own_id_and_time() {
        let src = source();
        let before = src.created_ms;
        let fork = fork_session(&src, SessionId::new("s-2"), 42_000);
        assert_eq!(fork.id.to_string(), "s-2");
        assert_ne!(fork.id.to_string(), src.id.to_string());
        assert_eq!(fork.created_ms, 42_000);
        assert_eq!(src.created_ms, before, "исходная сессия не тронута");
    }

    #[test]
    fn fork_title_gets_a_suffix_and_empty_title_stays_empty() {
        let src = source();
        let fork = fork_session(&src, SessionId::new("s-2"), 1);
        assert_eq!(fork.title, format!("разбор парсера{FORK_TITLE_SUFFIX}"));

        let mut blank = source();
        blank.title = "   ".to_string();
        let fork = fork_session(&blank, SessionId::new("s-3"), 1);
        assert!(
            fork.title.is_empty(),
            "из пустого заголовка не делаем название"
        );
    }

    #[test]
    fn fork_history_is_a_copy_not_a_handle() {
        /* Правка ветки не должна менять родителя: иначе «попробовать иначе»
        стирало бы исходный разговор. */
        let src = source();
        let mut fork = fork_session(&src, SessionId::new("s-2"), 1);
        fork.history.push(ChatMessage::user("только в ветке"));
        assert_eq!(fork.history.len(), 3);
        assert_eq!(src.history.len(), 2, "родитель не изменился");
    }
}

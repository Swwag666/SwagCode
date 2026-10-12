/*! F-8: исполнение хуков владельца.

Разбор, проверка и отбор хуков живут в [`swagcod_core::hooks`] (чистая
логика, headless-тесты); здесь — то, что требует платформы и состояния
приложения: запуск процесса без консольного окна, таймаут с убийством дерева,
метрика `hook_ms` и сообщение в шину.

Хук — внешняя команда владельца, поэтому к нему относятся те же требования,
что и к инструменту `bash`: никакого всплывающего окна, обязательный таймаут
(зависший скрипт не имеет права вешать ход) и честный текст ошибки, из
которого видно, какая именно команда и с каким кодом отказала.
!*/

use std::path::Path;
use std::process::Stdio;
use std::sync::Arc;
use std::time::{Duration, Instant};

use swagcod_core::bus::EventKind;
use swagcod_core::hooks::{
    HooksConfig, ENV_CWD, ENV_EVENT, ENV_OK, ENV_SESSION, ENV_TOOL, HOOKS_PREF, HOOK_TIMEOUT_SECS,
};

use crate::{silent_cmd, AppState};

/// Потолок текста хука, который уходит в шину и модели: stderr скрипта может
/// быть бесконечным, а история хода — нет.
pub const HOOK_OUTPUT_MAX_CHARS: usize = 2000;

/// Контекст одного прогона: что подставляется в переменные окружения хука.
pub struct HookCtx<'a> {
    pub session: &'a str,
    /// `pre_turn` | `post_turn` | `pre_tool` (см. `core::hooks::EVENT_*`).
    pub event: &'a str,
    /// Имя инструмента; пусто для событий хода.
    pub tool: &'a str,
    pub cwd: &'a Path,
    /// Только для `post_turn`: успешен ли ход.
    pub ok: Option<bool>,
}

/// Исход прогона очереди хуков одного события.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct HookRun {
    /// Все команды завершились с кодом 0 (или команд не было).
    pub ok: bool,
    /// Сколько команд исполнилось; упавшая включена, остальные не запускались.
    pub ran: usize,
    pub elapsed_ms: u64,
    /// Причина отказа для человека и модели (пустая при успехе).
    pub message: String,
}

impl HookRun {
    fn passed() -> Self {
        Self {
            ok: true,
            ran: 0,
            elapsed_ms: 0,
            message: String::new(),
        }
    }
}

/// Прочитать конфигурацию хуков из prefs.
///
/// Битый JSON даёт пустую конфигурацию и сообщение в шину: сохранить битую
/// конфигуляцию через настройки нельзя (`core::hooks::parse_hooks` её
/// отвергает), значит речь о ручной правке базы, и ход из-за этого падать не
/// должен. Молчать нельзя по той же причине — владелец обязан узнать, что его
/// охранники не работают.
pub fn load_hooks(state: &AppState) -> HooksConfig {
    let raw = state
        .store
        .lock()
        .ok()
        .and_then(|s| s.get_pref(HOOKS_PREF).ok().flatten())
        .unwrap_or_default();
    match swagcod_core::hooks::parse_hooks(&raw) {
        Ok(cfg) => cfg,
        Err(e) => {
            state.bus.publish(EventKind::Status {
                message: format!("хуки не применены: {e}"),
            });
            eprintln!("hooks: конфигурация не разобралась, хуки отключены: {e}");
            HooksConfig::default()
        }
    }
}

/// Прогнать очередь хуков события: метрика `hook_ms` на команду, при отказе —
/// сообщение в шину.
///
/// Команды исполняются последовательно и останавливаются на первом nonzero:
/// если охранник сказал «нет», следующие за ним команды не должны делать вид,
/// что всё в порядке.
pub async fn run_event_hooks(state: &Arc<AppState>, cmds: &[String], ctx: &HookCtx<'_>) -> HookRun {
    if cmds.is_empty() {
        return HookRun::passed();
    }
    let started = Instant::now();
    let mut ran = 0usize;
    for cmd in cmds {
        let one = Instant::now();
        let res = run_one(cmd, ctx).await;
        let ms = one.elapsed().as_millis() as u64;
        ran += 1;
        record_metric(state, ms);
        match res {
            Ok(()) => {}
            Err(message) => {
                let message = format!("hook {} отклонил: {message}", ctx.event);
                publish_status(state, &message);
                return HookRun {
                    ok: false,
                    ran,
                    elapsed_ms: started.elapsed().as_millis() as u64,
                    message,
                };
            }
        }
    }
    HookRun {
        ok: true,
        ran,
        elapsed_ms: started.elapsed().as_millis() as u64,
        message: String::new(),
    }
}

/// Хуки `pre_tool`, относящиеся к вызову: `None`, если все пропустили вызов
/// (или хуков нет), иначе — причина отказа для модели.
pub async fn run_pre_tool_hooks(
    state: &Arc<AppState>,
    cfg: &HooksConfig,
    session: &str,
    tool: &str,
    cwd: &Path,
) -> Option<String> {
    let matched = cfg.matching_pre_tool(tool);
    if matched.is_empty() {
        return None;
    }
    let cmds: Vec<String> = matched.iter().map(|h| h.cmd.clone()).collect();
    let ctx = HookCtx {
        session,
        event: swagcod_core::hooks::EVENT_PRE_TOOL,
        tool,
        cwd,
        ok: None,
    };
    let run = run_event_hooks(state, &cmds, &ctx).await;
    if run.ok {
        None
    } else {
        Some(run.message)
    }
}

/// Одна команда хука. `Ok(())` — код возврата 0, `Err(текст)` — причина.
async fn run_one(cmd: &str, ctx: &HookCtx<'_>) -> Result<(), String> {
    let timeout = Duration::from_secs(HOOK_TIMEOUT_SECS);
    let mut child_cmd = silent_cmd("cmd");
    child_cmd
        .arg("/C")
        .arg(cmd)
        .current_dir(ctx.cwd)
        /* stdin закрыт намеренно: скрипт, ждущий ввода, иначе съел бы весь
        таймаут, а владелец увидел бы «хук не сработал» без причины. */
        .stdin(Stdio::null())
        .env(ENV_SESSION, ctx.session)
        .env(ENV_EVENT, ctx.event)
        .env(ENV_TOOL, ctx.tool)
        .env(ENV_CWD, ctx.cwd.to_string_lossy().to_string())
        /* SWAGCOD_OK задана всегда: в cmd необъявленная переменная не
        раскрывается, и скрипт получил бы литерал `%SWAGCOD_OK%` вместо
        значения — классическая ловушка пакетных файлов. Пустая строка не
        годится: в блоке окружения Windows пустое значение означает
        «переменной нет». Поэтому вне post_turn — явное `na`. */
        .env(
            ENV_OK,
            match ctx.ok {
                Some(true) => "true",
                Some(false) => "false",
                None => "na",
            },
        );
    let child = child_cmd
        .spawn()
        .map_err(|e| format!("запуск {cmd:?}: {e}"))?;
    // pid берём до wait_with_output: future уносит child, а убивать по
    // таймауту нужно дерево процессов (taskkill /T).
    let pid = child.id();
    match tokio::time::timeout(timeout, child.wait_with_output()).await {
        Ok(Ok(out)) => {
            if out.status.success() {
                return Ok(());
            }
            /* Причина отказа — stderr: именно туда охранник пишет, почему он
            не пустил вызов. Код возврата оставляем в тексте: «exit 1» без
            вывода тоже должен быть виден. */
            let stderr = String::from_utf8_lossy(&out.stderr).trim().to_string();
            let stdout = String::from_utf8_lossy(&out.stdout).trim().to_string();
            let detail = if stderr.is_empty() { stdout } else { stderr };
            let code = out.status.code().unwrap_or(-1);
            Err(format!(
                "{cmd:?} → exit {code}{}",
                if detail.is_empty() {
                    String::new()
                } else {
                    format!(": {}", cap(&detail))
                }
            ))
        }
        Ok(Err(e)) => Err(format!("запуск {cmd:?}: {e}")),
        Err(_) => {
            if let Some(pid) = pid {
                let _ = silent_cmd("taskkill")
                    .args(["/PID", &pid.to_string(), "/T", "/F"])
                    .output()
                    .await;
            }
            Err(format!(
                "{cmd:?} не завершилась за {} с и остановлена",
                timeout.as_secs()
            ))
        }
    }
}

fn cap(text: &str) -> String {
    let n = text.chars().count();
    if n <= HOOK_OUTPUT_MAX_CHARS {
        return text.to_string();
    }
    let cut: String = text.chars().take(HOOK_OUTPUT_MAX_CHARS).collect();
    format!("{cut}\n[... обрезано ...]")
}

fn record_metric(state: &AppState, ms: u64) {
    if let Ok(store) = state.store.lock() {
        let _ = store.metrics_insert(swagcod_core::bus::now_ms(), "hook_ms", ms as f64);
    }
}

fn publish_status(state: &AppState, message: &str) {
    state.bus.publish(EventKind::Status {
        message: message.to_string(),
    });
}

#[cfg(test)]
mod tests {
    use super::*;
    use swagcod_core::hooks::{parse_hooks, EVENT_POST_TURN};

    fn state() -> Arc<AppState> {
        Arc::new(AppState {
            store: std::sync::Mutex::new(swagcod_core::store::open_memory()),
            ..Default::default()
        })
    }

    fn temp_dir(tag: &str) -> std::path::PathBuf {
        let dir = std::env::temp_dir().join(format!(
            "swagcod-hook-{tag}-{}-{}",
            std::process::id(),
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .map(|d| d.subsec_nanos())
                .unwrap_or(0)
        ));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        dir
    }

    /// Собрать тексты Status-событий из шины.
    fn drain_status(rx: &mut tokio::sync::broadcast::Receiver<swagcod_core::Event>) -> Vec<String> {
        let mut out = Vec::new();
        while let Ok(ev) = rx.try_recv() {
            if let EventKind::Status { message } = &ev.kind {
                out.push(message.clone());
            }
        }
        out
    }

    /* Контракт окружения — главное в хуках: команда владельца должна видеть
    контекст, иначе она не может принять решение. Проверяем записью в файл
    внутри cwd сессии: заодно подтверждается и рабочая директория. */
    #[tokio::test]
    #[cfg(windows)]
    async fn hook_sees_env_contract_and_runs_in_session_cwd() {
        let st = state();
        let dir = temp_dir("env");
        let ctx = HookCtx {
            session: "s-42",
            event: swagcod_core::hooks::EVENT_PRE_TOOL,
            tool: "write",
            cwd: &dir,
            ok: None,
        };
        // `#` в cmd не специальный, а `|` пришлось бы экранировать.
        let cmd =
            "echo %SWAGCOD_SESSION%#%SWAGCOD_EVENT%#%SWAGCOD_TOOL%#%SWAGCOD_OK%>hook-probe.txt"
                .to_string();
        let run = run_event_hooks(&st, std::slice::from_ref(&cmd), &ctx).await;
        assert!(run.ok, "{}", run.message);
        assert_eq!(run.ran, 1);
        assert!(run.elapsed_ms < HOOK_TIMEOUT_SECS * 1000);

        let probe = std::fs::read_to_string(dir.join("hook-probe.txt"))
            .expect("хук отработал в cwd сессии");
        // SWAGCOD_OK наполнена только у post_turn — здесь явное «неприменимо».
        assert_eq!(probe.trim(), "s-42#pre_tool#write#na", "{probe}");

        // Метрика hook_ms записана: телеметрия видит стоимость хуков.
        let m = st
            .store
            .lock()
            .unwrap()
            .metrics_query("hook_ms", 0, 10)
            .unwrap();
        assert_eq!(m.len(), 1, "{m:?}");
        std::fs::remove_dir_all(&dir).ok();
    }

    #[tokio::test]
    #[cfg(windows)]
    async fn post_turn_hook_reports_turn_success() {
        let st = state();
        let dir = temp_dir("ok");
        let ctx = HookCtx {
            session: "s-1",
            event: EVENT_POST_TURN,
            tool: "",
            cwd: &dir,
            ok: Some(true),
        };
        let cmd = "echo %SWAGCOD_EVENT%=%SWAGCOD_OK%>ok-probe.txt".to_string();
        let run = run_event_hooks(&st, std::slice::from_ref(&cmd), &ctx).await;
        assert!(run.ok, "{}", run.message);
        let probe = std::fs::read_to_string(dir.join("ok-probe.txt")).unwrap();
        assert_eq!(probe.trim(), "post_turn=true", "{probe}");
        std::fs::remove_dir_all(&dir).ok();
    }

    #[tokio::test]
    #[cfg(windows)]
    async fn failing_hook_refuses_with_stderr_and_exit_code() {
        let st = state();
        let dir = temp_dir("refuse");
        let mut rx = st.bus.subscribe();
        let ctx = HookCtx {
            session: "s-1",
            event: swagcod_core::hooks::EVENT_PRE_TOOL,
            tool: "bash",
            cwd: &dir,
            ok: None,
        };
        let cmd = "echo dist закрыт для правок 1>&2 & exit /b 3".to_string();
        let run = run_event_hooks(&st, std::slice::from_ref(&cmd), &ctx).await;
        assert!(!run.ok);
        assert_eq!(run.ran, 1);
        assert!(run.message.contains("exit 3"), "{}", run.message);
        assert!(run.message.contains("dist закрыт"), "{}", run.message);
        assert!(run.message.contains("pre_tool"), "{}", run.message);
        // Отказ виден в шине: человек понимает, почему вызов не исполнился.
        let statuses = drain_status(&mut rx);
        assert!(
            statuses.iter().any(|m| m.contains("отклонил")),
            "{statuses:?}"
        );
        std::fs::remove_dir_all(&dir).ok();
    }

    /// Очередь останавливается на первом отказе: следующий за охранником хук
    /// не должен делать вид, что всё в порядке (и тем более что-то менять).
    #[tokio::test]
    #[cfg(windows)]
    async fn hook_queue_stops_at_first_failure() {
        let st = state();
        let dir = temp_dir("queue");
        let ctx = HookCtx {
            session: "s-1",
            event: swagcod_core::hooks::EVENT_PRE_TURN,
            tool: "",
            cwd: &dir,
            ok: None,
        };
        let cmds = vec!["exit /b 1".to_string(), "echo x>second.txt".to_string()];
        let run = run_event_hooks(&st, &cmds, &ctx).await;
        assert!(!run.ok);
        assert_eq!(run.ran, 1, "вторая команда не должна исполняться");
        assert!(
            !dir.join("second.txt").exists(),
            "после отказа очередь остановилась"
        );
        // Успешная очередь исполняется целиком.
        let ok_cmds = vec!["echo a>one.txt".to_string(), "echo b>two.txt".to_string()];
        let run = run_event_hooks(&st, &ok_cmds, &ctx).await;
        assert!(run.ok, "{}", run.message);
        assert_eq!(run.ran, 2);
        assert!(dir.join("one.txt").exists() && dir.join("two.txt").exists());
        std::fs::remove_dir_all(&dir).ok();
    }

    #[tokio::test]
    #[cfg(windows)]
    async fn pre_tool_hooks_run_only_for_matching_tools() {
        let st = state();
        let dir = temp_dir("match");
        let cfg = parse_hooks(
            r#"{"pre_tool":[{"match":"^write$","cmd":"echo x>ran.txt & echo нельзя 1>&2 & exit /b 1"}]}"#,
        )
        .unwrap();

        // Чужой инструмент: хук не исполняется вовсе.
        let refusal = run_pre_tool_hooks(&st, &cfg, "s-1", "read", &dir).await;
        assert_eq!(refusal, None);
        assert!(
            !dir.join("ran.txt").exists(),
            "хук сработал на чужом инструменте"
        );

        let refusal = run_pre_tool_hooks(&st, &cfg, "s-1", "write", &dir)
            .await
            .expect("write ограничен хуком");
        assert!(refusal.contains("нельзя"), "{refusal}");
        assert!(dir.join("ran.txt").exists());
        std::fs::remove_dir_all(&dir).ok();
    }

    /// Пустая очередь ничего не запускает и не пишет метрику: иначе каждый ход
    /// без хуков платил бы точкой телеметрии.
    #[tokio::test]
    async fn empty_queue_is_a_noop() {
        let st = state();
        let dir = temp_dir("noop");
        let ctx = HookCtx {
            session: "s-1",
            event: swagcod_core::hooks::EVENT_PRE_TURN,
            tool: "",
            cwd: &dir,
            ok: None,
        };
        let run = run_event_hooks(&st, &[], &ctx).await;
        assert!(run.ok && run.ran == 0 && run.elapsed_ms == 0);
        assert!(st
            .store
            .lock()
            .unwrap()
            .metrics_query("hook_ms", 0, 10)
            .unwrap()
            .is_empty());
        std::fs::remove_dir_all(&dir).ok();
    }

    #[test]
    fn broken_pref_disables_hooks_loudly() {
        let st = state();
        st.store
            .lock()
            .unwrap()
            .set_pref(HOOKS_PREF, "{ pre_turn: [")
            .unwrap();
        let mut rx = st.bus.subscribe();
        let cfg = load_hooks(&st);
        assert!(cfg.is_empty(), "битая конфигурация не применяется");
        let statuses = drain_status(&mut rx);
        assert!(
            statuses.iter().any(|m| m.contains("хуки не применены")),
            "владелец должен узнать, что охранники не работают: {statuses:?}"
        );
    }

    #[test]
    fn valid_pref_loads_as_config() {
        let st = state();
        st.store
            .lock()
            .unwrap()
            .set_pref(
                HOOKS_PREF,
                r#"{"pre_turn":["git status"],"pre_tool":[{"match":"^bash$","cmd":"guard.cmd"}]}"#,
            )
            .unwrap();
        let cfg = load_hooks(&st);
        assert_eq!(cfg.pre_turn, vec!["git status"]);
        assert_eq!(cfg.pre_tool.len(), 1);
        // Хуки читаются на каждый ход: правка в настройках действует сразу.
        st.store
            .lock()
            .unwrap()
            .set_pref(HOOKS_PREF, r#"{"post_turn":["lint.cmd"]}"#)
            .unwrap();
        let cfg = load_hooks(&st);
        assert!(cfg.pre_turn.is_empty());
        assert_eq!(cfg.post_turn, vec!["lint.cmd"]);
    }

    #[test]
    fn dto_reports_counts_and_parse_errors() {
        let st = state();
        let dto = crate::hooks_dto(st.store.lock().unwrap().as_ref());
        assert_eq!((dto.pre_turn, dto.post_turn, dto.pre_tool), (0, 0, 0));
        assert!(dto.errors.is_empty());
        assert!(dto.raw.is_empty());
        assert_eq!(dto.timeout_secs, HOOK_TIMEOUT_SECS);
        assert_eq!(dto.max_per_event, swagcod_core::hooks::HOOKS_PER_EVENT);

        st.store
            .lock()
            .unwrap()
            .set_pref(
                HOOKS_PREF,
                r#"{"pre_turn":["a","b"],"pre_tool":[{"match":"^write$","cmd":"g.cmd"}]}"#,
            )
            .unwrap();
        let dto = crate::hooks_dto(st.store.lock().unwrap().as_ref());
        assert_eq!((dto.pre_turn, dto.post_turn, dto.pre_tool), (2, 0, 1));
        assert!(dto.errors.is_empty());

        // Битый текст (ручная правка базы) — ошибка видна в редакторе.
        st.store
            .lock()
            .unwrap()
            .set_pref(HOOKS_PREF, "{oops")
            .unwrap();
        let dto = crate::hooks_dto(st.store.lock().unwrap().as_ref());
        assert_eq!(dto.errors.len(), 1, "{:?}", dto.errors);
        assert_eq!((dto.pre_turn, dto.post_turn, dto.pre_tool), (0, 0, 0));
    }
}

/*! F-10: драйвер хода — ядро старта и остановки.

Стрим-цикл жил в lib.rs и был её самой большой частью, хотя правило крейта —
«здесь не живёт бизнес-логика» (D-011). Теперь он здесь: [`TurnMachine`] из
swagcod-core принимает решения, а этот модуль их исполняет — стримит модель,
ждёт человека на подтверждении, гоняет инструменты и хуки владельца,
публикует события в шину и пишет журнал хода.

Поведение прежнее: тот же один вход для IPC-команды и loopback REST API (E-7),
та же машина хода, та же шина. Tauri-обёртки `#[tauri::command]` остались в
lib.rs — команде положено жить в шелле, который её регистрирует.
!*/

use std::sync::Arc;

use swagcod_core::bus::EventKind;
use swagcod_core::turn::{
    builtin_tool_specs, describe_call, truncate_output, ApprovalDecision, TurnMachine, TurnOutcome,
    TurnStep,
};
use swagcod_provider::types::{ToolCall, ToolSpec};
use swagcod_provider::{ChatRequest, StreamEvent};
use tokio::sync::oneshot;

use crate::{
    arg_str, execute_tool, hooks, is_memory_tool, jsplugins, mcp, memory_facts_for_prompt, meta_of,
    providers, rehydrate_session, run_compaction, run_js_call, run_mcp_call, run_memory_tool,
    run_semantic_search, run_subagent, sandbox_path, short_id, spawn_semantic_index, touch_turn,
    AppState, PluginTool, TurnActivity, TurnHandle,
};

/// Вложения пользователя (скрепка в UI): текстовые файлы подшиваются к
/// сообщению блоками с содержимым. Провайдер текстовый — картинки и
/// бинарщина честно помечаются неприкреплёнными, а не молча теряются.
/// Лимит 200 КБ на файл бережёт контекст (B-3) от одного жирного лога.
pub(crate) const ATTACH_MAX_BYTES: u64 = 200 * 1024;

/* ── rev62: предел автономии главного хода ──────────────────────────────────
 * Суб-агент ограничен с самого начала (SUBAGENT_MAX_ROUNDS 6 и бюджет 100k), а
 * главный цикл не был ограничен ничем: с политикой «always» ход крутил
 * инструменты один за другим, пока владелец не заметит. При 32k вывода на
 * раунд и повторах роутера (до трёх таймаутов по 60 с на эндпоинт) ночной
 * прогон без этого капа - способ спалить бюджет за одну ночь.
 * Остановка честная: в журнал уезжает причина с числами, а история хода
 * сохраняется целиком, поэтому сделанное не теряется. */

/// Предел раундов (запросов модели) главного хода по умолчанию.
///
/// Нарочно щедрый: 40 раундов - это большая законная работа (правка десятка
/// файлов с проверками), поэтому порог ловит бесконечный цикл, а не усердие.
pub const TURN_MAX_ROUNDS: u32 = 40;

/// Бюджет токенов главного хода (вход+выход всех раундов) по умолчанию.
///
/// Второй пояс к первому: раундов может быть мало, но один раунд бывает
/// огромным (контекст в сотни тысяч токенов), и тогда расход режет бюджет.
pub const TURN_BUDGET_TOKENS: u64 = 2_000_000;

/// Лимиты главного хода: env, затем дефолты.
///
/// Клампы защищают и от нуля, и от абсурда: ноль остановил бы ход на первом же
/// раунде, то есть выглядел бы как поломка, а не как настройка.
pub fn turn_limits() -> (u32, u64) {
    let env_rounds = std::env::var("SWAGCOD_TURN_MAX_ROUNDS")
        .ok()
        .and_then(|v| v.parse::<u32>().ok());
    let env_budget = std::env::var("SWAGCOD_TURN_BUDGET_TOKENS")
        .ok()
        .and_then(|v| v.parse::<u64>().ok());
    let rounds = env_rounds.unwrap_or(TURN_MAX_ROUNDS).clamp(1, 1000);
    let budget = env_budget
        .unwrap_or(TURN_BUDGET_TOKENS)
        .clamp(1_000, 100_000_000);
    (rounds, budget)
}

/// Пора ли остановить ход: пусто - идём дальше, иначе причина для журнала.
///
/// Чистая функция без шины и провайдера - поэтому границы проверяются тестом,
/// а не прогоном хода. Вызывается ПЕРЕД очередным запросом модели, значит
/// `rounds_used` - число уже сделанных раундов: при `rounds_used == max_rounds`
/// следующий запрос не уходит, и в журнале стоит ровно сделанное число.
pub fn turn_guard_reason(
    rounds_used: u32,
    tokens_used: u64,
    max_rounds: u32,
    budget_tokens: u64,
) -> Option<String> {
    if rounds_used >= max_rounds {
        return Some(format!(
            "ход остановлен: сделан предел раундов {rounds_used} из {max_rounds}. \
             Сделанное сохранено в истории хода. Предел поднимается переменной \
             SWAGCOD_TURN_MAX_ROUNDS."
        ));
    }
    if tokens_used >= budget_tokens {
        return Some(format!(
            "ход остановлен: израсходован бюджет токенов {tokens_used} из {budget_tokens}. \
             Сделанное сохранено в истории хода. Бюджет поднимается переменной \
             SWAGCOD_TURN_BUDGET_TOKENS."
        ));
    }
    None
}

pub(crate) fn render_attachments(paths: &[String]) -> String {
    let mut out = String::new();
    for p in paths {
        let name = std::path::Path::new(p)
            .file_name()
            .map(|n| n.to_string_lossy().to_string())
            .unwrap_or_else(|| p.clone());
        let ext = std::path::Path::new(p)
            .extension()
            .map(|e| e.to_string_lossy().to_lowercase())
            .unwrap_or_default();
        if matches!(
            ext.as_str(),
            "png" | "jpg" | "jpeg" | "gif" | "webp" | "bmp" | "ico"
        ) {
            out.push_str(&format!(
                "\n\n---\nВложение {name}: не прикреплено — картинки пока не поддерживаются (текстовый провайдер)."
            ));
            continue;
        }
        match std::fs::metadata(p) {
            Ok(md) if md.len() > ATTACH_MAX_BYTES => {
                out.push_str(&format!(
                    "\n\n---\nВложение {name}: не прикреплено — файл больше {} КБ.",
                    ATTACH_MAX_BYTES / 1024
                ));
            }
            Ok(_) => match std::fs::read_to_string(p) {
                Ok(text) => {
                    out.push_str(&format!(
                        "\n\n---\nВложение: {name} ({p})\n```\n{text}\n```"
                    ));
                }
                Err(_) => {
                    out.push_str(&format!(
                        "\n\n---\nВложение {name}: не прикреплено — бинарный файл."
                    ));
                }
            },
            Err(e) => {
                out.push_str(&format!("\n\n---\nВложение {name}: не прикреплено — {e}."));
            }
        }
    }
    out
}

/* ── F-6: diff-review перед применением правки ─────────────────────────────
 * Диалог подтверждения показывал «write {"path":…,"content":…}» — по такому
 * тексту человек одобряет не глядя, а именно это подтверждение и должно
 * предотвращать. Поэтому вместе со сводкой уходит unified-diff того, что
 * произойдёт с файлом: посчитанный ДО записи, из текущего содержимого и
 * предложенного. */

/// Инструменты, у которых есть что показать до применения.
const PREVIEWABLE_TOOLS: [&str; 2] = ["write", "patch"];

/// Файлы больше этого не диффим: LCS на мегабайтах съел бы секунды прямо в
/// ходе, а диалог всё равно обрезан 64 КБ.
pub(crate) const PREVIEW_SOURCE_MAX_BYTES: usize = 2 * 1024 * 1024;

/// Строка-заглушка: показываем причину, а не пустоту.
fn preview_note(text: &str) -> String {
    format!("@@ {text} @@\n")
}

/// Unified-diff того, что сделает вызов, либо None.
///
/// None — «нечего показывать»: не файловый инструмент или путь не
/// резолвится. Ошибку применения (old_string не найден, файл не читается)
/// тоже не прячем за выдуманным diff: preview=None, а сводка и результат
/// исполнения остаются честными.
pub(crate) fn approval_preview(call: &ToolCall, cwd: &std::path::Path) -> Option<String> {
    if !PREVIEWABLE_TOOLS.contains(&call.name.as_str()) {
        return None;
    }
    let path = sandbox_path(cwd, &arg_str(call, "path")).ok()?;
    let exists = path.exists();
    let current = std::fs::read_to_string(&path).unwrap_or_default();
    /* Огромный файл отсекаем до patch_text: и LCS, и поиск вхождений на
    мегабайтах стоили бы секунд прямо в ходе, а показать их всё равно нельзя
    (предел preview — 64 КБ). */
    if current.len() > PREVIEW_SOURCE_MAX_BYTES {
        return Some(preview_note(
            "файл больше 2 МБ: diff не считался, решение по сводке",
        ));
    }

    let proposed = match call.name.as_str() {
        "write" => arg_str(call, "content"),
        /* patch несуществующего файла — ошибка исполнения («old_string не
        найден»), а не «создадим файл»: показывать все строки добавленными
        было бы ложью о поведении инструмента. */
        "patch" if exists => {
            let old = arg_str(call, "old_string");
            let new = arg_str(call, "new_string");
            let replace_all = call
                .arguments
                .get("replace_all")
                .and_then(|v| v.as_bool())
                .unwrap_or(false);
            match swagcod_fsx::tools::patch_text(&current, &old, &new, replace_all) {
                Ok((patched, _)) => patched,
                Err(e) => return Some(preview_note(&format!("правка не применима: {e}"))),
            }
        }
        _ => return None,
    };

    if current.len() + proposed.len() > PREVIEW_SOURCE_MAX_BYTES {
        return Some(preview_note(
            "файл больше 2 МБ: diff не считался, решение по сводке",
        ));
    }
    let d = swagcod_fsx::diff_text(&current, &proposed);
    if d.is_empty() {
        return Some(preview_note("изменений нет: содержимое совпадает"));
    }
    let (mut text, truncated) =
        swagcod_fsx::format_unified_preview(&d, swagcod_fsx::PREVIEW_MAX_BYTES);
    if truncated {
        text.push_str(&preview_note(&format!(
            "diff обрезан: предел {} КБ, всего +{} -{}",
            swagcod_fsx::PREVIEW_MAX_BYTES / 1024,
            d.added,
            d.removed
        )));
    }
    Some(text)
}

/* ── Фича 2: folder-scoped rules ─────────────────────────────────────────────
 * Резолв правил рабочей папки и фильтрация инструментов. Вынесено из тела
 * хода, чтобы логику можно было проверить на in-memory AppState без провайдера.
 */

/// Резолвить правила для рабочей папки: глобальные glob-правила плюс дерево
/// `.swagcod/rules.json` вверх до корня ФС.
///
/// Отравленный мьютекс кэша не роняет ход: возвращаем пустые правила, и сессия
/// идёт как до появления Фичи 2. Молча терять запреты хуже, чем молча терять
/// удобства, поэтому факт потери пишется в лог.
pub(crate) fn resolve_folder_rules(
    state: &AppState,
    cwd: &std::path::Path,
) -> swagcod_core::folder_rules::ResolvedFolderRules {
    match state.folder_rules.lock() {
        Ok(mut cache) => cache.resolve(cwd),
        Err(e) => {
            eprintln!("folder-rules: кэш заблокирован ({e}), ход без правил папки");
            swagcod_core::folder_rules::ResolvedFolderRules::default()
        }
    }
}

/// Плагин отключён правилом папки. Сравниваем по имени без префикса `js:` и
/// без учёта регистра: владелец пишет `Shout`, а реестр хранит `shout`.
///
/// Пустые и пробельные строки правил игнорируются: иначе одна опечатка в
/// `rules.json` (`"disabled_plugins": [""]`) совпала бы с любым именем и
/// молча выключила все плагины сессии.
pub(crate) fn plugin_disabled(disabled: &[String], name: &str) -> bool {
    let bare = name
        .strip_prefix(crate::jsplugins::JS_PREFIX)
        .unwrap_or(name);
    disabled.iter().any(|d| {
        !d.trim().is_empty() && (d.eq_ignore_ascii_case(bare) || d.eq_ignore_ascii_case(name))
    })
}

/* E-7: ядро старта хода без Tauri-State — один вход для IPC-команды и
loopback REST API. Поведение идентично: та же машина хода, та же шина. */
pub(crate) async fn start_turn_core(
    state: Arc<AppState>,
    session_id: String,
    message: String,
    model: Option<String>,
    temperature: Option<f64>,
    attachments: Option<Vec<String>>,
) -> Result<String, String> {
    /* Скрепка: вложения подшиваются к тексту одним сообщением — история
    и контекст видят их как часть реплики пользователя. */
    let mut message = message.trim().to_string();
    if let Some(paths) = attachments.as_ref().filter(|a| !a.is_empty()) {
        message.push_str(&render_attachments(paths));
        message = message.trim().to_string();
    }
    if message.is_empty() {
        return Err("сообщение не может быть пустым".into());
    }

    // C1-фикс: создаём провайдер ДО мутации статуса сессии.
    // Активный провайдер из настроек (default = .env/DPAPI как раньше).
    let provider = providers::build_chat_provider(&state).map_err(|e| format!("провайдер: {e}"))?;

    let (turn_id, history, session_summary, session_model, cwd, mut cfg) = {
        let mut sessions = state.sessions.lock().await;
        let session = sessions
            .iter_mut()
            .find(|s| s.id.to_string() == session_id)
            .ok_or_else(|| format!("сессия не найдена: {session_id}"))?;

        if !session.status.can_start_turn() {
            return Err(format!("сессия занята: {:?}", session.status));
        }

        /* D-121: второй потребитель полной сессии — ход. История грузится
        из базы лениво: в покое RAM не держит чужие транскрипты. */
        rehydrate_session(&state, session);

        let ok = session.push_user_message(&message);
        if !ok {
            return Err("не удалось начать ход".into());
        }

        let turn_id = swagcod_core::TurnId::new(format!("t-{}", short_id()));
        session.current_turn = Some(turn_id.clone());
        session.status = swagcod_core::session::SessionStatus::Running;

        let mut cfg = state.config.lock().await.clone();
        // B-7: per-session политика подтверждений перекрывает глобальную.
        if let Some(p) = session.approval_policy {
            cfg.approval_policy = p;
        }
        /* F-3 + Фича 2: списки команд собираются ПОСЛЕ выхода из-под лока
        сессий. Folder-rules рабочей папки дополняют prefs, а читать файлы
        правил, держа мьютекс сессий, — значит сериализовать ходы на диске. */
        (
            turn_id.to_string(),
            session.history.clone(),
            session.summary.clone(),
            session.model.clone(),
            session.cwd.clone(),
            cfg,
        )
    };

    // W-6 фикс: используем переданную модель если есть, иначе из сессии.
    let effective_model = model.unwrap_or(session_model);

    /* Фича 2: folder-scoped rules. Резолвятся один раз на ход от рабочей
    папки сессии: дерево `.swagcod/rules.json` вверх до корня ФС плюс
    глобальные glob-правила. Кэш сверяет mtime, поэтому правка файла
    подхватывается со следующего хода без перезапуска приложения. */
    let folder = resolve_folder_rules(&state, std::path::Path::new(&cwd));

    /* F-3: allow/deny из prefs читаются на каждый ход (правка в настройках
    действует сразу), и folder-rules папки их дополняют. Deny остаётся
    сильнее allow — порядок проверки внутри CommandRules, а не здесь. */
    let (mut allow_lines, mut deny_lines) = state
        .store
        .lock()
        .map(|s| crate::command_rule_lines(s.as_ref()))
        // Отравленный мьютекс не должен снимать запреты: дефолты F-3.
        .unwrap_or_else(|_| crate::default_rule_lines());
    allow_lines.extend(folder.allow.iter().cloned());
    deny_lines.extend(folder.deny.iter().cloned());
    cfg.rules = swagcod_core::rules::CommandRules::from_lines(allow_lines, deny_lines);
    if !folder.is_noop() {
        eprintln!(
            "folder-rules для {cwd}: allow+{} deny+{} mcp-{} plugins-{} skills-{} источников {}",
            folder.allow.len(),
            folder.deny.len(),
            folder.disabled_mcp.len(),
            folder.disabled_plugins.len(),
            folder.disabled_skills.len(),
            folder.sources.len()
        );
    }

    // Тулзы уходят в каждом запросе: без определений модель не может их вызвать.
    // B-5: встроенные плюс зарегистрированные плагины — модель видит один список.
    let plugins: Vec<PluginTool> = state.plugins.lock().map(|p| p.clone()).unwrap_or_default();
    let mut specs = builtin_tool_specs();
    specs.extend(
        plugins
            .iter()
            // Фича 2: disabled_plugins глушит и Rust-плагины — для модели
            // это один класс «плагин», различать их в правилах смысла нет.
            .filter(|p| !plugin_disabled(&folder.disabled_plugins, &p.name))
            .map(|p| ToolSpec {
                name: p.name.clone(),
                description: p.description.clone(),
                parameters: p.parameters.clone(),
            }),
    );
    /* E-3: инструменты MCP-серверов видны модели одним списком с
    встроенными и плагинами B-5. Фича 2: disabled_mcp глушит сервер целиком —
    все его инструменты, а не по одному имени. */
    {
        let reg = state.mcp.lock().await;
        specs.extend(
            reg.tools
                .iter()
                .filter(|t| !folder.disabled_mcp.iter().any(|s| s == &t.server))
                .map(|t| ToolSpec {
                    name: t.name.clone(),
                    description: t.description.clone(),
                    parameters: t.parameters.clone(),
                }),
        );
    }
    /* E-4: JS-плагины sidecar — тот же единый список (имена js:*).
    Фича 2: disabled_plugins матчится по short-имени, без префикса. */
    if let Ok(tools) = state.js_tools.lock() {
        specs.extend(
            tools
                .iter()
                .filter(|t| !plugin_disabled(&folder.disabled_plugins, &t.short))
                .map(|t| ToolSpec {
                    name: t.name.clone(),
                    description: t.description.clone(),
                    parameters: t.parameters.clone(),
                }),
        );
    }
    /* rev63: скиллы владельца встают в тот же список, что встроенные
    инструменты, плагины и MCP. Список берётся по cwd хода, поэтому правило
    disabled_skills папки глушит скилл у модели так же, как disabled_plugins
    глушит плагин, - и «skills visible» в настройках наконец что-то значит.
    Скиллов нет - инструмента нет: пустой тул только тратил бы внимание модели. */
    let skills = crate::visible_skills(state.as_ref(), Some(cwd.as_str()));
    if let Some(skill_spec) = crate::skill_tool_spec(&skills) {
        specs.push(skill_spec);
    }
    /* Текст правил папки уходит в задачу хода отдельной строкой: сам
    ResolvedFolderRules нужен здесь (фильтрация tools, allow/deny), а в задаче
    из него используется только промпт-секция. */
    let folder_prompt = folder.prompt_text();
    let tool_wire: Vec<serde_json::Value> = ChatRequest::new(&effective_model, Vec::new())
        .with_tools(&specs)
        .tools;
    let temperature = temperature.map(|t| t as f32);

    let bus = state.bus.clone();
    let turn = turn_id.clone();
    let sid = session_id.clone();
    let app_state = state.clone();
    let cancelled = Arc::new(std::sync::atomic::AtomicBool::new(false));
    let cancelled_in_task = cancelled.clone();

    let task = tauri::async_runtime::spawn(async move {
        let tid = swagcod_core::TurnId::new(&turn);
        let cwd = std::path::PathBuf::from(&cwd);
        let tool_timeout = cfg.tool_timeout;
        /* TurnStarted обязан уходить в шину ПЕРВЫМ: фронт строит по нему
        карту turn→session, и без неё стрим либо терялся, либо (раньше)
        сваливался в активный чат — отсюда росли «слияния» чатов. */
        bus.publish(EventKind::TurnStarted {
            turn: tid.clone(),
            session: swagcod_core::SessionId::new(&sid),
            parent: None,
        });
        /* F-8: хуки владельца. Конфигурация читается один раз на ход: правка в
        настройках действует со следующего хода, а перечитывать prefs на каждом
        инструменте значило бы платить запросом к базе внутри цикла.
        pre_turn идёт ДО снимка git: отказавший ход не должен оставлять
        артефактов (лишних объектов в .git). */
        let hooks_cfg = hooks::load_hooks(&app_state);
        let mut pre_turn_refusal: Option<String> = None;
        if !hooks_cfg.pre_turn.is_empty() {
            let ctx = hooks::HookCtx {
                session: sid.as_str(),
                event: swagcod_core::hooks::EVENT_PRE_TURN,
                tool: "",
                cwd: &cwd,
                ok: None,
            };
            let run = hooks::run_event_hooks(&app_state, &hooks_cfg.pre_turn, &ctx).await;
            if !run.ok {
                bus.publish(EventKind::Error {
                    turn: Some(tid.clone()),
                    message: run.message.clone(),
                });
                pre_turn_refusal = Some(run.message);
            }
        }
        /* F-5 + rev64: снимок worktree ДО первого инструмента хода. В репозитории
        это git-объект, вне репозитория - теневая копия дерева: до rev64 вне git
        отката не было вовсе, и правки хода в обычной папке были необратимы.
        Снимать нечего только когда каталога нет. Ошибка снимка - не повод
        ронять ход: сообщаем в статус и работаем без страховки, но честно. */
        let checkpoint_sha = match swagcod_fsx::checkpoint::create(&cwd).await {
            Ok(sha) => sha,
            Err(e) => {
                bus.publish(EventKind::Status {
                    message: format!("снимок перед ходом не создан: {e}"),
                });
                None
            }
        };
        /* E-2: в начале каждого хода фоном освежаем семантический индекс.
        Кулдаун 5 минут + busy-флаг: частые ходы не гоняют обход
        репозитория и не толкаются в одной базе. */
        spawn_semantic_index(app_state.clone(), cwd.clone(), false);
        /* B-8: watchdog получает запись о живом ходе. Время — now_ms из
        шины: диагностика и журнал живут на одних часах. */
        if let Ok(mut activity) = app_state.turn_activity.lock() {
            let now = swagcod_core::bus::now_ms();
            activity.insert(
                turn.clone(),
                TurnActivity {
                    session: sid.clone(),
                    started_ms: now,
                    last_ms: now,
                    waiting_human: false,
                    warned_ms: None,
                },
            );
        }

        /* B-3: контекст — калиброванный счёт, компакция сайд-запросом и
        память проекта. Всё до старта машины: машина получает уже
        подготовленную историю. F-2: компакция вынесена в run_compaction —
        её же дёргает /compact из палитры команд. */
        use swagcod_core::context;
        let calib = app_state
            .store
            .lock()
            .ok()
            .and_then(|s| s.get_pref("token_calibration").ok().flatten());
        let cpt = context::chars_per_token(&effective_model, calib.as_deref());
        let (summary, history, _) = run_compaction(
            &app_state,
            &provider,
            &sid,
            &effective_model,
            false,
            session_summary,
            history,
        )
        .await;
        // Память проекта инжектится в system-промпт каждого запроса хода.
        let memory_md =
            std::fs::read_to_string(cwd.join(".swagcod").join("MEMORY.md")).unwrap_or_default();
        /* F-7: факты из базы приложения — второй источник памяти, живущий вне
        рабочей директории. Читаются один раз на ход: промпт пересобирается на
        каждом запросе, но факт, добавленный этим же ходом, станет видимым
        модели со следующего хода (сам вызов memory_add уже вернул
        подтверждение в истории, так что модель о нём знает).
        rev56: сюда же доезжают факты рабочей папки (область folder) — по cwd
        хода, побайтово и без наследования в подпапки. */
        let memory_facts = memory_facts_for_prompt(&app_state, &cwd);
        /* Фича 2: правила рабочей папки — третья секция инструкций после
        памяти проекта и фактов. Пустая строка даёт промпт, байт-в-байт равный
        прежнему: правил нет — ход идёт как до Фичи 2. */
        let system_msg =
            context::system_prompt_with(&summary, &memory_md, &memory_facts, &folder_prompt);

        let (mut machine, mut step) = TurnMachine::new(cfg, history);
        let mut fail_reason: Option<String> = None;
        // B-3: точный счёт от провайдера, если он отдаёт usage в стриме.
        let mut last_usage: Option<(u32, u32)> = None;
        /* rev62: автономия хода под счётчиком. Раунд - это запрос модели;
        расход считается по тому же материалу, что уходит провайдеру, поэтому
        бюджет работает и на эндпоинтах, где usage приходит null (живой шлюз). */
        let (turn_max_rounds, turn_budget) = turn_limits();
        let mut rounds_used: u32 = 0;
        let mut turn_tokens: u64 = 0;

        let outcome = loop {
            /* F-8: отказ pre_turn-хука закрывает ход до первого запроса
            модели. Охранник, который не может остановить ход, бесполезен:
            «не запускай агента на грязной ветке» — именно такой случай. */
            if let Some(reason) = pre_turn_refusal.take() {
                fail_reason = Some(reason);
                break TurnOutcome::Failed;
            }
            // B-8: каждый шаг машины — признак жизни для watchdog.
            touch_turn(&app_state, &turn, None);
            if cancelled_in_task.load(std::sync::atomic::Ordering::SeqCst) {
                step = machine.cancel();
            }
            match step {
                TurnStep::RequestModel { history } => {
                    /* rev62: проверка перед очередным запросом. Останавливаем
                    ДО обращения к провайдеру, поэтому в журнал уезжает ровно
                    сделанное число раундов, а не «сделано плюс один». */
                    if let Some(reason) =
                        turn_guard_reason(rounds_used, turn_tokens, turn_max_rounds, turn_budget)
                    {
                        fail_reason = Some(reason.clone());
                        bus.publish(EventKind::Error {
                            turn: Some(tid.clone()),
                            message: reason,
                        });
                        step = TurnStep::Finish {
                            outcome: TurnOutcome::Failed,
                        };
                        continue;
                    }
                    rounds_used += 1;
                    turn_tokens +=
                        swagcod_core::context::estimate_history_tokens(&history, cpt) as u64;
                    // System не хранится в истории сессии: подставляется
                    // свежим в каждый запрос, иначе сообщения множились бы.
                    let messages = {
                        let mut v = Vec::with_capacity(history.len() + 1);
                        v.push(system_msg.clone());
                        v.extend(history);
                        v
                    };
                    let request = ChatRequest {
                        /* rev69: провайдер умеет картинки, но ядро их пока не прикладывает - это следующий шаг. Список пуст намеренно, а не забыт. */
                        images: Vec::new(),
                        model: effective_model.clone(),
                        messages,
                        stream: true,
                        temperature,
                        // Щедрый потолок вывода: ahmia и прочие шлюзы при
                        // max_tokens=null режут ответ по своему внутреннему
                        // дефолту на полуслове («...тон. Ж»). 32k хватает на
                        // любой ход; реальная длина всё равно ограничивается
                        // stop-сигналом модели, а не этим числом.
                        max_tokens: Some(32_768),
                        // Включаем поток мыслей для reasoning-моделей. Без этого
                        // флага часть маршрутов rustvy (например abyss/ahmia-v6-lite)
                        // думает молча — «мыслей не видно». Провайдеры без поддержки
                        // поля его тихо игнорируют.
                        reasoning_effort: Some("high".to_string()),
                        tools: tool_wire.clone(),
                    };
                    let stream_started = std::time::Instant::now();
                    let (mut rx, handle) = match provider.stream(request) {
                        Ok(pair) => pair,
                        Err(e) => {
                            fail_reason = Some(format!("ошибка стрима: {e}"));
                            bus.publish(EventKind::Error {
                                turn: Some(tid.clone()),
                                message: format!("ошибка стрима: {e}"),
                            });
                            step = TurnStep::Finish {
                                outcome: TurnOutcome::Failed,
                            };
                            continue;
                        }
                    };
                    let mut acc = swagcod_core::turn::StreamAccumulator::default();
                    let mut stopped = false;
                    let mut latency_recorded = false;
                    let mut stream_error: Option<String> = None;
                    while let Some(event) = rx.recv().await {
                        // B-8: токен — событие. Долгая генерация reasoning не
                        // должна выглядеть для watchdog как зависание.
                        touch_turn(&app_state, &turn, None);
                        /* E-8: латентность провайдера — время до первого
                        события стрима. Одна точка на запрос, fire-and-
                        forget: телеметрия не должна ломать ход. */
                        if !latency_recorded {
                            latency_recorded = true;
                            let ms = stream_started.elapsed().as_secs_f64() * 1000.0;
                            if let Ok(g) = app_state.store.lock() {
                                let _ = g.metrics_insert(
                                    swagcod_core::bus::now_ms(),
                                    "provider_latency_ms",
                                    ms,
                                );
                            }
                        }
                        if cancelled_in_task.load(std::sync::atomic::Ordering::SeqCst) {
                            stopped = true;
                            handle.abort();
                            break;
                        }
                        match &event {
                            StreamEvent::Reasoning(text) => {
                                acc.reasoning.push_str(text);
                                bus.publish(EventKind::Reasoning {
                                    turn: tid.clone(),
                                    text: text.clone(),
                                });
                            }
                            StreamEvent::Content(text) => {
                                acc.content.push_str(text);
                                bus.publish(EventKind::Content {
                                    turn: tid.clone(),
                                    text: text.clone(),
                                });
                            }
                            StreamEvent::ToolCallStart { id, name, .. } => {
                                bus.publish(EventKind::ToolCallStart {
                                    turn: tid.clone(),
                                    call_id: id.clone(),
                                    name: name.clone(),
                                });
                            }
                            StreamEvent::ToolCallComplete(tc) => {
                                acc.tool_calls.push(tc.clone());
                                bus.publish(EventKind::ToolCall {
                                    turn: tid.clone(),
                                    call_id: tc.id.clone(),
                                    name: tc.name.clone(),
                                    arguments: serde_json::Value::Object(tc.arguments.clone()),
                                });
                            }
                            StreamEvent::Done { .. } => {}
                            StreamEvent::Error(msg) => stream_error = Some(msg.clone()),
                            StreamEvent::Usage {
                                input_tokens,
                                output_tokens,
                            } => {
                                last_usage = Some((*input_tokens, *output_tokens));
                            }
                        }
                    }
                    if stopped {
                        step = machine.cancel();
                    } else if let Some(msg) = stream_error {
                        fail_reason = Some(msg.clone());
                        bus.publish(EventKind::Error {
                            turn: Some(tid.clone()),
                            message: msg,
                        });
                        step = TurnStep::Finish {
                            outcome: TurnOutcome::Failed,
                        };
                    } else {
                        /* rev62: выход раунда в тот же счётчик, что и вход -
                        иначе бюджет видел бы только контекст и не замечал
                        генерацию. Оценка снята до on_stream: он забирает acc. */
                        turn_tokens += swagcod_core::context::estimate_tokens(&acc.content, cpt)
                            as u64
                            + swagcod_core::context::estimate_tokens(&acc.reasoning, cpt) as u64;
                        step = machine.on_stream(acc);
                    }
                }
                TurnStep::AwaitApproval { call } => {
                    let call_id = call.id.clone();
                    /* F-6: diff считается только здесь — то есть только когда
                    диалог действительно будет показан. При политике Never и
                    при совпадении с allow-списком (F-3) машина до этого шага
                    не доходит, и лишней работы с файлами нет. */
                    let preview = approval_preview(&call, &cwd);
                    bus.publish(EventKind::ApprovalRequired {
                        turn: tid.clone(),
                        call_id: call_id.clone(),
                        tool: call.name.clone(),
                        summary: describe_call(&call),
                        preview,
                    });
                    let (tx, rx) = oneshot::channel();
                    {
                        let mut approvals = app_state.approvals.lock().await;
                        // Каналы завершившихся ходов выметаем, чтобы карта не росла.
                        approvals.retain(|_, sender| !sender.is_closed());
                        approvals.insert(call_id.clone(), tx);
                    }
                    // Потеря канала (ход остановлен) считаем отказом: молчаливое
                    // «одобрено» было бы дырой в политике подтверждений.
                    // B-8: на время ожидания человека watchdog замолкает —
                    // диалог подтверждения не зависание.
                    touch_turn(&app_state, &turn, Some(true));
                    let res = rx.await;
                    touch_turn(&app_state, &turn, Some(false));
                    let actor = if res.is_ok() { "user" } else { "system" };
                    let decision = res.unwrap_or(ApprovalDecision::Denied);
                    /* B-7: журнал подтверждений — кто, что, когда. actor=system
                    означает решение без человека (канал потерян). */
                    if let Ok(store) = app_state.store.lock() {
                        let _ = store.log_approval(&swagcod_core::store::ApprovalEntry {
                            session_id: sid.clone(),
                            turn_id: tid.to_string(),
                            call_id: call_id.clone(),
                            tool: call.name.clone(),
                            summary: describe_call(&call),
                            decision: match decision {
                                ApprovalDecision::Approved => "approved",
                                ApprovalDecision::Denied => "denied",
                            }
                            .into(),
                            actor: actor.into(),
                            decided_ms: swagcod_core::bus::now_ms(),
                        });
                    }
                    app_state.approvals.lock().await.remove(&call_id);
                    step = machine.on_approval(decision);
                }
                /* F-3: вызов запрещён deny-списком. Диалога нет — решение
                принято правилом заранее. В журнал пишем actor=system:
                автоматический отказ должен быть виден в аудите наравне с
                человеческими, иначе «кто запретил» останется загадкой. */
                TurnStep::Blocked { call, reason } => {
                    bus.publish(EventKind::ToolResult {
                        turn: tid.clone(),
                        call_id: call.id.clone(),
                        ok: false,
                        output: truncate_output(&reason),
                        elapsed_ms: 0,
                    });
                    if let Ok(store) = app_state.store.lock() {
                        let _ = store.log_approval(&swagcod_core::store::ApprovalEntry {
                            session_id: sid.clone(),
                            turn_id: tid.to_string(),
                            call_id: call.id.clone(),
                            tool: call.name.clone(),
                            summary: describe_call(&call),
                            decision: "denied".into(),
                            actor: "system".into(),
                            decided_ms: swagcod_core::bus::now_ms(),
                        });
                    }
                    step = machine.on_blocked(&reason);
                }
                TurnStep::ExecuteTool { call } => {
                    let started = std::time::Instant::now();
                    /* F-8: pre_tool-хук решает до исполнения вызова. Отказ
                    оформляется как blocked (тот же путь, что у deny-списка
                    F-3): вызов закрывается сообщением tool с причиной, ход
                    продолжается, и модель может сделать иначе. В журнал
                    подтверждений пишем actor=system — автоматический отказ
                    должен быть виден в аудите наравне с человеческим. */
                    if let Some(reason) = hooks::run_pre_tool_hooks(
                        &app_state,
                        &hooks_cfg,
                        sid.as_str(),
                        &call.name,
                        &cwd,
                    )
                    .await
                    {
                        bus.publish(EventKind::ToolResult {
                            turn: tid.clone(),
                            call_id: call.id.clone(),
                            ok: false,
                            output: truncate_output(&reason),
                            elapsed_ms: started.elapsed().as_millis() as u64,
                        });
                        if let Ok(store) = app_state.store.lock() {
                            let _ = store.log_approval(&swagcod_core::store::ApprovalEntry {
                                session_id: sid.clone(),
                                turn_id: tid.to_string(),
                                call_id: call.id.clone(),
                                tool: call.name.clone(),
                                summary: describe_call(&call),
                                decision: "denied".into(),
                                actor: "hook".into(),
                                decided_ms: swagcod_core::bus::now_ms(),
                            });
                        }
                        step = machine.on_blocked(&reason);
                        continue;
                    }
                    /* E-2: semantic_search исполняется здесь, а не в
                    execute_tool — ему нужны AppState (индекс, DPAPI-ключ)
                    и сеть, а execute_tool остаётся чистой и тестируемой.
                    E-3: то же для mcp:* — вызов идёт в живой
                    stdio-процесс сервера из реестра AppState. */
                    let tool_outcome = if call.name == "semantic_search" {
                        run_semantic_search(&app_state, &call, &cwd).await
                    } else if is_memory_tool(&call.name) {
                        /* F-7: память приложения — нужен стор из AppState.
                        В whitelist суб-агента этих имён нет, поэтому ветка
                        писать память не может. */
                        run_memory_tool(&app_state, sid.as_str(), &call).await
                    } else if call.name == crate::SKILL_TOOL {
                        /* rev63: скилл разворачивается в текст - ни сети, ни
                        диска, поэтому подтверждения не требует. В whitelist
                        суб-агента имени нет: ветка не наследует скиллы. */
                        crate::run_skill_tool(&app_state, &call).await
                    } else if call.name.starts_with(mcp::MCP_PREFIX) {
                        run_mcp_call(&app_state, &call).await
                    } else if call.name.starts_with(jsplugins::JS_PREFIX) {
                        run_js_call(&app_state, &call).await
                    } else if call.name == "subagent" {
                        /* E-6: ветка суб-агента — собственный стрим и история;
                        снимок истории сессии нужен её save_turn (messages
                        заменяются целиком). */
                        run_subagent(
                            &app_state,
                            &provider,
                            &call,
                            &sid,
                            &turn,
                            &effective_model,
                            temperature,
                            &cwd,
                            &summary,
                            tool_timeout,
                            machine.history(),
                        )
                        .await
                    } else {
                        execute_tool(&call, &cwd, tool_timeout, &plugins).await
                    };
                    let elapsed_ms = started.elapsed().as_millis() as u64;
                    bus.publish(EventKind::ToolResult {
                        turn: tid.clone(),
                        call_id: call.id.clone(),
                        ok: tool_outcome.ok,
                        output: truncate_output(&tool_outcome.output),
                        elapsed_ms,
                    });
                    step = machine.on_tool_result(tool_outcome);
                }
                TurnStep::Finish { outcome: o } => break o,
            }
        };

        // TurnEnded публикуется ровно один раз и только из исхода цикла:
        // UI закрывает строки хода по нему и не дедуплирует несколько финалов.
        let reason = match outcome {
            TurnOutcome::Completed => None,
            TurnOutcome::IterationLimit => Some("превышен лимит итераций".to_string()),
            TurnOutcome::Cancelled => Some(
                fail_reason
                    .clone()
                    .unwrap_or_else(|| "остановлено пользователем".to_string()),
            ),
            TurnOutcome::Failed => Some(
                fail_reason
                    .clone()
                    .unwrap_or_else(|| "ошибка хода".to_string()),
            ),
        };
        bus.publish(EventKind::TurnEnded {
            turn: tid.clone(),
            session: swagcod_core::SessionId::new(&sid),
            ok: outcome == TurnOutcome::Completed,
            reason,
        });

        let ended_ms = swagcod_core::bus::now_ms();
        let report = machine.report(outcome, ended_ms);
        let calls: Vec<ToolCall> = machine
            .history()
            .iter()
            .flat_map(|m| m.tool_calls.clone())
            .collect();
        /* B-3: счёт токенов — точный usage от провайдера, когда он его
        отдаёт, иначе калиброванная оценка. Честный ноль в журнале больше
        не живёт. */
        let (est_in, est_out) = match last_usage {
            Some((i, o)) => (i, o),
            None => (
                swagcod_core::context::estimate_history_tokens(machine.history(), cpt),
                swagcod_core::context::estimate_tokens(&report.content, cpt)
                    + swagcod_core::context::estimate_tokens(&report.reasoning, cpt),
            ),
        };
        let mut record = report.into_record(tid.clone(), calls, est_in, est_out);
        /* rev68: цена хода - в ту же метрику, что задержка и прочая телеметрия.
        Деньги должны быть записаны, а не только посчитаны: иначе через месяц
        никто не скажет, во что обошлась ночь. Модели нет в таблице цен - точки
        нет: ноль соврал бы про бесплатный прогон. */
        if let Some(cost) =
            swagcod_core::pricing::cost_usd(&effective_model, est_in as u64, est_out as u64)
        {
            if let Ok(g) = app_state.store.lock() {
                let _ = g.metrics_insert(swagcod_core::bus::now_ms(), "turn_cost_usd", cost);
            }
        }
        // F-5: снимок принадлежит этому ходу — вместе с ним и хранится.
        record.checkpoint_sha = checkpoint_sha;
        /* F-8: конкретная причина (отказ хука, ошибка провайдера) важнее
        общей «ошибка хода» из машины: журнал читают и после перезапуска,
        когда событие TurnEnded уже никто не увидит. */
        if let Some(reason) = fail_reason.clone() {
            record.failure = Some(reason);
        }
        {
            let mut sessions = app_state.sessions.lock().await;
            if let Some(session) = sessions.iter_mut().find(|s| s.id.to_string() == sid) {
                // История сессии — это история машины: assistant с tool_calls
                // всегда закрыт ответами tool, следующий запрос валиден.
                session.history = machine.history().to_vec();
                session.turns.push(record.clone());
                session.status = match outcome {
                    TurnOutcome::Cancelled => swagcod_core::session::SessionStatus::Cancelled,
                    TurnOutcome::Failed => swagcod_core::session::SessionStatus::Failed,
                    _ => swagcod_core::session::SessionStatus::Idle,
                };
                session.current_turn = None;
                /* B-1: ход и снимок истории уходят в store одной транзакцией
                прямо здесь, в конце хода. */
                if let Ok(store) = app_state.store.lock() {
                    if let Err(e) = store.save_turn(&sid, &record, &session.history) {
                        eprintln!("store: ход не записан: {e}");
                    }
                }
                // D-121: сайдбар видит свежие счётчики без полной сессии.
                if let Ok(mut m) = app_state.session_meta.lock() {
                    m.insert(sid.clone(), meta_of(session));
                }
            }
        }

        // Ход закончился — убираем ручку, чтобы «стоп» не бил по прошлому.
        let mut turns = app_state.turns.lock().await;
        turns.remove(&turn);
        // B-8: watchdog больше не следит за мёртвым ходом.
        if let Ok(mut activity) = app_state.turn_activity.lock() {
            activity.remove(&turn);
        }
        /* F-8: post_turn — после записи хода, поэтому хук видит полный
        журнал. Код возврата ни на что не влияет (ход уже случился): отказ
        попадает в шину и метрику. Ругаться на владельца за упавший линтер,
        спрятав от него результат хода, было бы самонадеянно. */
        if !hooks_cfg.post_turn.is_empty() {
            let ctx = hooks::HookCtx {
                session: sid.as_str(),
                event: swagcod_core::hooks::EVENT_POST_TURN,
                tool: "",
                cwd: &cwd,
                ok: Some(record.ok),
            };
            let _ = hooks::run_event_hooks(&app_state, &hooks_cfg.post_turn, &ctx).await;
        }
    });

    // Ручка хода доступна команде stop_turn сразу после старта.
    {
        let mut turns = state.turns.lock().await;
        turns.insert(
            turn_id.clone(),
            TurnHandle {
                session: session_id.clone(),
                cancelled,
                task,
            },
        );
    }

    Ok(turn_id)
}

/* F-9: ядро остановки — общее для IPC и loopback REST: кнопка «стоп» на
телефоне обязана останавливать ход ровно так же, как на десктопе.

Остановка ДВУХСТУПЕНЧАТАЯ. Мгновенный `task.abort()` убивал задачу на
ближайшем await ДО публикации TurnEnded и сброса статуса сессии: ход
оставался «живым» навсегда (UI висел в «думает», стоп выглядел мёртвым,
сессия — занятой). Теперь: флаг отмены → стрим замечает его на следующем
событии и ход завершается ЧИСТО (TurnEnded, запись журнала, статус) → и
только если за грейс-период задача не умерла сама (залипла в тулзе или в
немом стриме), бьём abort и убираем за ней вручную. */
pub(crate) async fn stop_turn_core(state: &AppState, session_id: String) -> Result<(), String> {
    let (turn_id, handle) = {
        let mut turns = state.turns.lock().await;
        let victim = turns
            .iter()
            .find(|(_, h)| h.session == session_id)
            .map(|(tid, _)| tid.clone());
        match victim {
            Some(tid) => match turns.remove(&tid) {
                Some(h) => (tid, h),
                None => return Err("нет живого хода у сессии".into()),
            },
            None => return Err("нет живого хода у сессии".into()),
        }
    };
    handle
        .cancelled
        .store(true, std::sync::atomic::Ordering::SeqCst);

    // Ступень 1: даём ходу завершиться самому — стрим прервётся по флагу,
    // цикл дойдёт до TurnEnded и уборки. Грейс щедрый, но не бесконечный:
    // залипший тулз не должен держать кнопку стопа заложницей.
    // select! с &mut task (не timeout!): JoinHandle остаётся в руках и на
    // ветке таймера — задачу можно добить abort, timeout бы её съел.
    let mut task = handle.task;
    tokio::select! {
        _ = &mut task => return Ok(()),
        _ = tokio::time::sleep(std::time::Duration::from_secs(5)) => {
            task.abort();
        }
    }

    // Ступень 2: задача не умерла за грейс — убираем вручную то, что не
    // успел сделать цикл хода. Ручку сняли выше, повторный remove не нужен.
    state.bus.publish(swagcod_core::bus::EventKind::TurnEnded {
        turn: swagcod_core::TurnId::new(&turn_id),
        session: swagcod_core::SessionId::new(&session_id),
        ok: false,
        reason: Some("остановлено пользователем".to_string()),
    });
    {
        let mut sessions = state.sessions.lock().await;
        if let Some(session) = sessions.iter_mut().find(|s| s.id.to_string() == session_id) {
            session.status = swagcod_core::session::SessionStatus::Cancelled;
            session.current_turn = None;
        }
    }
    if let Ok(mut activity) = state.turn_activity.lock() {
        activity.remove(&turn_id);
    }
    Ok(())
}

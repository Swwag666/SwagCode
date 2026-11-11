/*! Исполнитель тулзов агента (вынесен из lib.rs — D-011: в крейте app
живёт только шелл и команды, содержательное в подмодулях).

Здесь всё, что превращает `ToolCall` в результат: резолв пути в песочнице
cwd, декодирование байтов (UTF-16/OEM/UTF-8), запуск `bash`/плагинов с
таймаутом и убийством дерева процессов, диспетчеризация в fsx. Подтверждения
и очередь вызовов живут в [`crate::turn_driver`] и машине хода
(swagcod-core): этот модуль исполняет уже одобренный вызов.
*/

use swagcod_core::turn::ToolOutcome;
use swagcod_provider::types::ToolCall;

use crate::PluginTool;

/* ── Исполнитель тулзов агента ────────────────────────────────────────────
 * Песочница — cwd сессии: относительные пути модели не читают и не пишут
 * вне рабочей директории, даже если человек подтвердил вызов. Абсолютный
 * путь вне cwd при OnDangerous уводит вызов на подтверждение человека
 * (решение в core::turn::escapes_workspace) и сюда попадает только после
 * явного «да»; при Never исполняется сразу — это осознанный выбор владельца.
 * Команды `run_command` в IPC больше нет: шелл из WebView был дырой,
 * а агентскому циклу хватает этого исполнителя. */

/// Резолвить путь внутри cwd сессии; всё снаружи отклоняется.
pub(crate) fn sandbox_path(cwd: &std::path::Path, raw: &str) -> Result<std::path::PathBuf, String> {
    let p = std::path::Path::new(raw);
    // Абсолютный путь разрешён везде: доступ к опасным операциям уже
    // регулируется approval-политикой сессии (путь вне воркспейса при
    // OnDangerous требует диалога — см. core::turn::escapes_workspace),
    // и второй молчаливый запрет поверх решения пользователя только
    // ломает работу модели.
    // Относительные выходы за cwd (`../..`) по-прежнему отклоняются.
    if p.is_absolute() {
        return Ok(p.to_path_buf());
    }
    let abs = cwd.join(p);
    if !swagcod_fsx::is_within(cwd, &abs) {
        return Err(format!("путь вне рабочей директории сессии: {raw}"));
    }
    Ok(abs)
}

/// Резолв субпути поиска (grep/glob): та же песочница, что у read/write.
/// Ревизия 84, из протыкания: раньше аргумент `path` уходил в fsx как есть,
/// и `../outside` или абсолютный субпуть спокойно индексировали каталоги
/// вне cwd. Внутри cwd отдаём относительный вид — вывод путей остаётся
/// от корня сессии; подтверждённый абсолютный отдаётся как есть.
fn sandbox_search_root(cwd: &std::path::Path, raw: &str) -> Result<Option<String>, String> {
    let raw = raw.trim();
    if raw.is_empty() {
        return Ok(None);
    }
    let abs = sandbox_path(cwd, raw)?;
    Ok(Some(match abs.strip_prefix(cwd) {
        Ok(rel) if !rel.as_os_str().is_empty() => rel.to_string_lossy().into_owned(),
        _ => abs.to_string_lossy().into_owned(),
    }))
}

/// Имя указывает на DOS-устройство Windows (NUL/CON/PRN/AUX/COM1-9/LPT1-9)?
/// Win32 резолвит их мимо файловой системы: write «в nul» отвечает
/// успехом, а файл не появляется — модель уверена, что данные записаны.
#[cfg(windows)]
fn is_dos_device_name(path: &std::path::Path) -> bool {
    let Some(name) = path.file_name().and_then(|n| n.to_str()) else {
        return false;
    };
    let base = name.split_once('.').map(|(b, _)| b).unwrap_or(name);
    let b = base.to_ascii_lowercase();
    if matches!(b.as_str(), "nul" | "con" | "prn" | "aux") {
        return true;
    }
    if b.len() == 4 && (b.starts_with("com") || b.starts_with("lpt")) {
        let d = b.as_bytes()[3];
        return d.is_ascii_digit() && d != b'0';
    }
    false
}

pub(crate) fn arg_str(call: &ToolCall, key: &str) -> String {
    call.arguments
        .get(key)
        .and_then(|v| v.as_str())
        .unwrap_or_default()
        .to_string()
}

/// Декодирование текстовых байтов для тулзов: UTF-8 (с BOM и без),
/// UTF-16LE/BE по BOM (так пишет PowerShell `>` redirect), дальше OEM-кодовая
/// страница Windows (cp866/cp437 — так пишет cmd redirect), и лишь потом
/// lossy-фолбэк. Строгая read_to_string здесь падала на каждом не-UTF-8 файле.
pub(crate) fn decode_text_bytes(bytes: &[u8]) -> String {
    if bytes.starts_with(&[0xFF, 0xFE]) {
        return decode_utf16_lossy(&bytes[2..], false);
    }
    if bytes.starts_with(&[0xFE, 0xFF]) {
        return decode_utf16_lossy(&bytes[2..], true);
    }
    let body = if bytes.starts_with(&[0xEF, 0xBB, 0xBF]) {
        &bytes[3..]
    } else {
        bytes
    };
    if let Ok(s) = std::str::from_utf8(body) {
        return s.to_string();
    }
    #[cfg(windows)]
    if let Some(s) = decode_oem_cp(body) {
        return s;
    }
    String::from_utf8_lossy(body).into_owned()
}

fn decode_utf16_lossy(bytes: &[u8], big_endian: bool) -> String {
    let mut units: Vec<u16> = Vec::with_capacity(bytes.len() / 2 + 1);
    for c in bytes.chunks_exact(2) {
        units.push(if big_endian {
            u16::from_be_bytes([c[0], c[1]])
        } else {
            u16::from_le_bytes([c[0], c[1]])
        });
    }
    String::from_utf16_lossy(&units)
}

#[cfg(windows)]
fn decode_oem_cp(bytes: &[u8]) -> Option<String> {
    use windows_sys::Win32::Globalization::{GetOEMCP, MultiByteToWideChar};
    if bytes.is_empty() {
        return Some(String::new());
    }
    unsafe {
        let cp = GetOEMCP();
        let len = MultiByteToWideChar(
            cp,
            0,
            bytes.as_ptr(),
            bytes.len() as i32,
            std::ptr::null_mut(),
            0,
        );
        if len <= 0 {
            return None;
        }
        let mut buf = vec![0u16; len as usize];
        let n = MultiByteToWideChar(
            cp,
            0,
            bytes.as_ptr(),
            bytes.len() as i32,
            buf.as_mut_ptr(),
            len,
        );
        if n <= 0 {
            return None;
        }
        buf.truncate(n as usize);
        Some(String::from_utf16_lossy(&buf))
    }
}

/// Команда без всплывающего консольного окна на Windows
/// (CREATE_NO_WINDOW). Иначе каждый git/cmd/taskkill моргает
/// консолью поверх окна приложения.
/// F-8: pub(crate) — хуки запускают команды владельца тем же способом.
pub(crate) fn silent_cmd(program: &str) -> tokio::process::Command {
    let mut cmd = tokio::process::Command::new(program);
    #[cfg(windows)]
    cmd.creation_flags(0x0800_0000);
    cmd
}

pub(crate) async fn execute_tool(
    call: &ToolCall,
    cwd: &std::path::Path,
    timeout: std::time::Duration,
    plugins: &[PluginTool],
) -> ToolOutcome {
    let fail = |output: String| ToolOutcome { ok: false, output };
    match call.name.as_str() {
        "read" => {
            let path = match sandbox_path(cwd, &arg_str(call, "path")) {
                Ok(p) => p,
                Err(e) => return fail(e),
            };
            let meta = match std::fs::metadata(&path) {
                Ok(m) => m,
                Err(e) => return fail(format!("read: {e}")),
            };
            if meta.len() > 1_048_576 {
                return fail("файл больше 1 МБ, читайте частями".into());
            }
            match std::fs::read(&path) {
                Ok(bytes) => ToolOutcome {
                    ok: true,
                    output: decode_text_bytes(&bytes),
                },
                Err(e) => fail(format!("read: {e}")),
            }
        }
        "list" => {
            let path = match sandbox_path(cwd, &arg_str(call, "path")) {
                Ok(p) => p,
                Err(e) => return fail(e),
            };
            let rd = match std::fs::read_dir(&path) {
                Ok(rd) => rd,
                Err(e) => return fail(format!("list: {e}")),
            };
            let mut lines: Vec<String> = Vec::new();
            for entry in rd.flatten() {
                let is_dir = entry.file_type().map(|t| t.is_dir()).unwrap_or(false);
                lines.push(format!(
                    "{}{}",
                    if is_dir { "d " } else { "  " },
                    entry.file_name().to_string_lossy()
                ));
            }
            lines.sort();
            ToolOutcome {
                ok: true,
                output: lines.join("\n"),
            }
        }
        "write" => {
            let path = match sandbox_path(cwd, &arg_str(call, "path")) {
                Ok(p) => p,
                Err(e) => return fail(e),
            };
            // Ревизия 84, из протыкания: DOS-устройства Win32 резолвятся
            // мимо файловой системы — write «в nul» отвечает успехом, а
            // файла нет. Честный отказ вместо тихой потери данных.
            #[cfg(windows)]
            if is_dos_device_name(&path) {
                return fail(
                    "путь указывает на DOS-устройство Windows (nul/con/prn/aux/com*/lpt*): \
                     запись молча теряется, выберите настоящее имя файла"
                        .into(),
                );
            }
            let content = arg_str(call, "content");
            if let Some(parent) = path.parent() {
                if let Err(e) = std::fs::create_dir_all(parent) {
                    return fail(format!("mkdir: {e}"));
                }
            }
            match std::fs::write(&path, content.as_bytes()) {
                Ok(()) => ToolOutcome {
                    ok: true,
                    output: format!("записано {} байт", content.len()),
                },
                Err(e) => fail(format!("write: {e}")),
            }
        }
        /* B-3: память проекта. Заметка дописывается в .swagcod/MEMORY.md
        внутри cwd сессии — песочница та же, подтверждение не требуется:
        это блокнот агента, а не изменение кода пользователя. */
        "memory_append" => {
            let text = arg_str(call, "text");
            if text.trim().is_empty() {
                return fail("пустая заметка".into());
            }
            let dir = cwd.join(".swagcod");
            if let Err(e) = std::fs::create_dir_all(&dir) {
                return fail(format!("memory_append: mkdir: {e}"));
            }
            let path = dir.join("MEMORY.md");
            use std::io::Write as _;
            let entry = format!("\n## {}\n{}\n", swagcod_core::bus::now_ms(), text.trim());
            match std::fs::OpenOptions::new()
                .create(true)
                .append(true)
                .open(&path)
            {
                Ok(mut f) => match f.write_all(entry.as_bytes()) {
                    Ok(()) => ToolOutcome {
                        ok: true,
                        output: "заметка добавлена в .swagcod/MEMORY.md".into(),
                    },
                    Err(e) => fail(format!("memory_append: {e}")),
                },
                Err(e) => fail(format!("memory_append: {e}")),
            }
        }
        "bash" => {
            let command = arg_str(call, "command");
            if command.trim().is_empty() {
                return fail("пустая команда".into());
            }
            let child = silent_cmd("cmd")
                .arg("/C")
                .arg(&command)
                .current_dir(cwd)
                // Без piped-стандартов wait_with_output возвращает пустые
                // векторы: GUI-процессу нечего наследовать, вывод команды
                // терялся целиком, доезжал только exit-код.
                .stdin(std::process::Stdio::null())
                .stdout(std::process::Stdio::piped())
                .stderr(std::process::Stdio::piped())
                .spawn();
            let child = match child {
                Ok(c) => c,
                Err(e) => return fail(format!("запуск: {e}")),
            };
            // Таймаут обязателен: зависшая команда не должна вешать ход.
            // wait_with_output уносит child в future, поэтому pid берём заранее
            // и по таймауту убиваем дерево процессов через taskkill /T.
            let pid = child.id();
            match tokio::time::timeout(timeout, child.wait_with_output()).await {
                Ok(Ok(output)) => ToolOutcome {
                    ok: output.status.success(),
                    output: format!(
                        "stdout:\n{}\nstderr:\n{}\nexit: {}",
                        decode_text_bytes(&output.stdout),
                        decode_text_bytes(&output.stderr),
                        output.status.code().unwrap_or(-1)
                    ),
                },
                Ok(Err(e)) => fail(format!("запуск: {e}")),
                Err(_) => {
                    if let Some(pid) = pid {
                        let _ = silent_cmd("taskkill")
                            .args(["/PID", &pid.to_string(), "/T", "/F"])
                            .output()
                            .await;
                    }
                    fail(format!(
                        "команда не завершилась за {} с и остановлена",
                        timeout.as_secs()
                    ))
                }
            }
        }
        /* B-5: grep/glob/patch/fetch_url. Чистая логика живёт в fsx::tools,
        здесь только песочница cwd и оформление результата. */
        "grep" => {
            let pattern = arg_str(call, "pattern");
            if pattern.is_empty() {
                return fail("пустой pattern".into());
            }
            let sub = match sandbox_search_root(cwd, &arg_str(call, "path")) {
                Ok(s) => s,
                Err(e) => return fail(format!("grep: {e}")),
            };
            let context = call
                .arguments
                .get("context")
                .and_then(|v| v.as_u64())
                .unwrap_or(2) as usize;
            /* Ревизия 85, из зондажа «пацаны»: молчаливый пропуск больших
            файлов и тихая обрезка на 300 хитах — та же ложь, что потерянный
            результат. Пометки честные: что пропущено, где обрезано. */
            match swagcod_fsx::tools::grep_files_detailed(cwd, sub.as_deref(), &pattern, context) {
                Ok(report) if report.hits.is_empty() => {
                    let mut output = String::from("совпадений нет");
                    if !report.skipped_large.is_empty() {
                        let names = report.skipped_large.join(", ");
                        output.push_str(&format!(
                            "; не прочитаны файлы больше 4 МБ (совпадения в них \
                             могли быть пропущены): {names}"
                        ));
                    }
                    ToolOutcome { ok: true, output }
                }
                Ok(report) => {
                    let mut output = swagcod_fsx::tools::format_hits(&report.hits);
                    if report.truncated {
                        output.push_str(&format!(
                            "\n(показаны первые {} совпадений — в дереве есть ещё, \
                             уточните путь или паттерн)",
                            swagcod_fsx::tools::MAX_GREP_HITS
                        ));
                    }
                    if !report.skipped_large.is_empty() {
                        output.push_str(&format!(
                            "\n(не прочитаны файлы больше 4 МБ: {})",
                            report.skipped_large.join(", ")
                        ));
                    }
                    ToolOutcome { ok: true, output }
                }
                Err(e) => fail(format!("grep: {e}")),
            }
        }
        "glob" => {
            let pattern = arg_str(call, "pattern");
            if pattern.is_empty() {
                return fail("пустой pattern".into());
            }
            let sub = match sandbox_search_root(cwd, &arg_str(call, "path")) {
                Ok(s) => s,
                Err(e) => return fail(format!("glob: {e}")),
            };
            match swagcod_fsx::tools::glob_files(cwd, sub.as_deref(), &pattern) {
                Ok(files) if files.is_empty() => ToolOutcome {
                    ok: true,
                    output: "совпадений нет".into(),
                },
                Ok(files) => {
                    let mut output = files.join("\n");
                    if files.len() >= swagcod_fsx::tools::MAX_GLOB_RESULTS {
                        output.push_str(&format!(
                            "\n(показаны первые {} путей — в дереве есть ещё)",
                            swagcod_fsx::tools::MAX_GLOB_RESULTS
                        ));
                    }
                    ToolOutcome { ok: true, output }
                }
                Err(e) => fail(format!("glob: {e}")),
            }
        }
        "patch" => {
            let path = match sandbox_path(cwd, &arg_str(call, "path")) {
                Ok(p) => p,
                Err(e) => return fail(e),
            };
            let old = arg_str(call, "old_string");
            let new = arg_str(call, "new_string");
            let replace_all = call
                .arguments
                .get("replace_all")
                .and_then(|v| v.as_bool())
                .unwrap_or(false);
            let content = match std::fs::read_to_string(&path) {
                Ok(c) => c,
                Err(e) => return fail(format!("patch: {e}")),
            };
            match swagcod_fsx::tools::patch_text(&content, &old, &new, replace_all) {
                Ok((patched, n)) => match std::fs::write(&path, patched.as_bytes()) {
                    Ok(()) => ToolOutcome {
                        ok: true,
                        output: format!("замен: {n}"),
                    },
                    Err(e) => fail(format!("patch write: {e}")),
                },
                Err(e) => fail(format!("patch: {e}")),
            }
        }
        /* F-4: веб-поиск. Движок выдачи — env `SWAGCOD_SEARCH_URL`
        (SearXNG с format=json), иначе DuckDuckGo HTML без ключа. Таймаут
        короче общего: чужой поиск не должен держать ход дольше 10 с. */
        "web_search" => {
            let query = arg_str(call, "query");
            if query.trim().is_empty() {
                return fail("web_search: пустой query".into());
            }
            let max = call
                .arguments
                .get("max_results")
                .and_then(|v| v.as_u64())
                .unwrap_or(swagcod_fsx::search::DEFAULT_RESULTS as u64)
                as usize;
            let endpoint = std::env::var("SWAGCOD_SEARCH_URL")
                .ok()
                .filter(|s| !s.trim().is_empty());
            let search_timeout = timeout.min(std::time::Duration::from_secs(10));
            match swagcod_fsx::search::web_search(&query, max, endpoint.as_deref(), search_timeout)
                .await
            {
                Ok(hits) => ToolOutcome {
                    ok: true,
                    output: swagcod_fsx::search::format_results(&query, &hits),
                },
                // Ошибка fsx уже содержит имя инструмента — не удваиваем.
                Err(e) => fail(e.to_string()),
            }
        }
        "fetch_url" => {
            let url = arg_str(call, "url");
            if url.trim().is_empty() {
                return fail("пустой url".into());
            }
            let fetch_timeout = timeout.min(std::time::Duration::from_secs(30));
            match swagcod_fsx::tools::fetch_url(&url, fetch_timeout).await {
                Ok(text) => ToolOutcome {
                    ok: true,
                    output: text,
                },
                Err(e) => fail(format!("fetch_url: {e}")),
            }
        }
        other => {
            /* B-5: слой плагинов. Не-встроенное имя ищется в реестре;
            подтверждение уже пройдено (политика считает неизвестные
            имена опасными), здесь только исполнение. */
            match plugins.iter().find(|p| p.name == other) {
                None => fail(format!("неизвестный инструмент: {other}")),
                Some(plugin) => run_plugin(plugin, call, cwd, timeout).await,
            }
        }
    }
}

/// Исполнить внешний инструмент: команда через shell, аргументы JSON-ом
/// в stdin, cwd — рабочая директория сессии, таймаут как у bash.
async fn run_plugin(
    plugin: &PluginTool,
    call: &ToolCall,
    cwd: &std::path::Path,
    timeout: std::time::Duration,
) -> ToolOutcome {
    use tokio::io::AsyncWriteExt;
    let fail = |output: String| ToolOutcome { ok: false, output };
    let args = serde_json::to_string(&call.arguments).unwrap_or_else(|_| "{}".into());
    let (shell, flag) = if cfg!(windows) {
        ("cmd", "/C")
    } else {
        ("sh", "-c")
    };
    let mut child = match silent_cmd(shell)
        .arg(flag)
        .arg(&plugin.command)
        .current_dir(cwd)
        .stdin(std::process::Stdio::piped())
        .stdout(std::process::Stdio::piped())
        .stderr(std::process::Stdio::piped())
        .spawn()
    {
        Ok(c) => c,
        Err(e) => return fail(format!("плагин {}: запуск: {e}", plugin.name)),
    };
    if let Some(mut stdin) = child.stdin.take() {
        // Ошибка записи не фатальна: плагин мог не читать stdin.
        let _ = stdin.write_all(args.as_bytes()).await;
    }
    let pid = child.id();
    match tokio::time::timeout(timeout, child.wait_with_output()).await {
        Ok(Ok(output)) => ToolOutcome {
            ok: output.status.success(),
            output: format!(
                "stdout:\n{}\nstderr:\n{}\nexit: {}",
                decode_text_bytes(&output.stdout),
                decode_text_bytes(&output.stderr),
                output.status.code().unwrap_or(-1)
            ),
        },
        Ok(Err(e)) => fail(format!("плагин {}: {e}", plugin.name)),
        Err(_) => {
            if let Some(pid) = pid {
                if cfg!(windows) {
                    let _ = silent_cmd("taskkill")
                        .args(["/PID", &pid.to_string(), "/T", "/F"])
                        .output()
                        .await;
                } else {
                    let _ = silent_cmd("kill")
                        .args(["-9", &pid.to_string()])
                        .output()
                        .await;
                }
            }
            fail(format!(
                "плагин {} не завершился за {} с и остановлен",
                plugin.name,
                timeout.as_secs()
            ))
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::short_id;

    fn call(name: &str, args: serde_json::Value) -> ToolCall {
        ToolCall {
            id: "c1".into(),
            name: name.into(),
            arguments: args.as_object().cloned().unwrap_or_default(),
        }
    }

    #[tokio::test]
    async fn execute_tool_rejects_paths_outside_session_cwd() {
        let dir = std::env::temp_dir().join(format!("swagcod-sandbox-{}", short_id()));
        std::fs::create_dir_all(&dir).unwrap();
        std::fs::write(dir.join("inside.txt"), "ok").unwrap();
        let timeout = std::time::Duration::from_secs(5);

        let out = execute_tool(
            &call("read", serde_json::json!({"path": "../outside.txt"})),
            &dir,
            timeout,
            &[],
        )
        .await;
        assert!(!out.ok, "выход за cwd обязан отклоняться: {out:?}");

        let out = execute_tool(
            &call("read", serde_json::json!({"path": "inside.txt"})),
            &dir,
            timeout,
            &[],
        )
        .await;
        assert!(
            out.ok && out.output == "ok",
            "относительный путь внутри cwd: {out:?}"
        );

        let out = execute_tool(
            &call(
                "write",
                serde_json::json!({"path": "../../evil.txt", "content": "x"}),
            ),
            &dir,
            timeout,
            &[],
        )
        .await;
        assert!(!out.ok, "запись вне cwd обязана отклоняться: {out:?}");

        std::fs::remove_dir_all(&dir).ok();
    }

    /* Исполнитель после подтверждения: абсолютный путь вне cwd в главном
    ходе пришёл через диалог (core::turn::escapes_workspace → AwaitApproval)
    и раз человек подтвердил — путь читается. Проверяем именно это: пароль
    диалога не должен ломать легитимный доступ. Само гейтинг-поведение
    (диалог до исполнения) покрыто тестами машины в core::turn. */
    #[tokio::test]
    async fn execute_tool_allows_absolute_paths_outside_cwd() {
        let dir = std::env::temp_dir().join(format!("swagcod-abs-{}", short_id()));
        let other = std::env::temp_dir().join(format!("swagcod-other-{}", short_id()));
        std::fs::create_dir_all(&dir).unwrap();
        std::fs::create_dir_all(&other).unwrap();
        std::fs::write(other.join("far.txt"), "далеко").unwrap();

        let out = execute_tool(
            &call(
                "read",
                serde_json::json!({"path": other.join("far.txt").to_string_lossy()}),
            ),
            &dir,
            std::time::Duration::from_secs(5),
            &[],
        )
        .await;
        assert!(
            out.ok && out.output.contains("далеко"),
            "абсолютный путь вне cwd разрешён после подтверждения: {out:?}"
        );

        std::fs::remove_dir_all(&dir).ok();
        std::fs::remove_dir_all(&other).ok();
    }

    #[tokio::test]
    async fn execute_tool_read_decodes_utf16_and_non_utf8() {
        let dir = std::env::temp_dir().join(format!("swagcod-enc-{}", short_id()));
        std::fs::create_dir_all(&dir).unwrap();

        // UTF-16LE с BOM — так пишет powershell `> file`.
        let text = "Привет, диск";
        let mut bytes = vec![0xFFu8, 0xFE];
        for u in text.encode_utf16() {
            bytes.extend_from_slice(&u.to_le_bytes());
        }
        std::fs::write(dir.join("ps.txt"), &bytes).unwrap();

        let out = execute_tool(
            &call("read", serde_json::json!({"path": "ps.txt"})),
            &dir,
            std::time::Duration::from_secs(5),
            &[],
        )
        .await;
        assert!(
            out.ok && out.output.contains("Привет, диск"),
            "UTF-16LE обязан декодироваться: {out:?}"
        );

        // Бинарный мусор не UTF-8 — read больше не падает, отдаёт lossy.
        std::fs::write(dir.join("raw.bin"), [0xC3u8, 0x28, 0xA0, 0xA1]).unwrap();
        let out = execute_tool(
            &call("read", serde_json::json!({"path": "raw.bin"})),
            &dir,
            std::time::Duration::from_secs(5),
            &[],
        )
        .await;
        assert!(out.ok, "не-UTF-8 файл читается без ошибки: {out:?}");

        std::fs::remove_dir_all(&dir).ok();
    }

    #[test]
    fn decode_text_bytes_handles_bom_and_plain_utf8() {
        assert_eq!(decode_text_bytes(b"plain"), "plain");
        assert_eq!(decode_text_bytes(&[0xEF, 0xBB, 0xBF, b'a']), "a");
        let mut le = vec![0xFF, 0xFE];
        le.extend_from_slice(&0x044Fu16.to_le_bytes()); // 'я' в UTF-16LE
        assert_eq!(decode_text_bytes(&le), "я");
    }

    #[cfg(windows)]
    #[tokio::test]
    async fn execute_tool_bash_returns_stdout_and_exit_code() {
        let dir = std::env::temp_dir().join(format!("swagcod-bash-{}", short_id()));
        std::fs::create_dir_all(&dir).unwrap();
        let out = execute_tool(
            &call(
                "bash",
                serde_json::json!({"command": "echo swagprobe & exit /b 7"}),
            ),
            &dir,
            std::time::Duration::from_secs(15),
            &[],
        )
        .await;
        assert!(
            out.output.contains("swagprobe"),
            "stdout обязан доезжать: {out:?}"
        );
        assert!(
            out.output.contains("exit: 7"),
            "exit-код обязан пробрасываться: {out:?}"
        );
        assert!(!out.ok, "ненулевой exit — неуспех");
        std::fs::remove_dir_all(&dir).ok();
    }

    #[tokio::test]
    async fn execute_tool_unknown_tool_is_a_result_not_a_panic() {
        let out = execute_tool(
            &call("fly", serde_json::json!({})),
            std::path::Path::new("."),
            std::time::Duration::from_secs(1),
            &[],
        )
        .await;
        assert!(!out.ok);
        assert!(
            out.output.contains("неизвестный инструмент"),
            "модель должна понять отказ: {}",
            out.output
        );
    }

    // ---- B-5: grep/glob/patch через исполнитель ----

    #[tokio::test]
    async fn execute_tool_grep_glob_patch_roundtrip() {
        let dir = std::env::temp_dir().join(format!("swagcod-b5-{}", short_id()));
        std::fs::create_dir_all(dir.join("src")).unwrap();
        std::fs::write(
            dir.join("src").join("a.rs"),
            "fn main() {\n    let x = 1;\n}\n",
        )
        .unwrap();
        std::fs::write(dir.join("b.txt"), "needle here\n").unwrap();
        let timeout = std::time::Duration::from_secs(5);

        let out = execute_tool(
            &call("grep", serde_json::json!({"pattern": "needle"})),
            &dir,
            timeout,
            &[],
        )
        .await;
        assert!(
            out.ok && out.output.contains("b.txt:1") || out.output.contains("b.txt"),
            "{out:?}"
        );
        assert!(out.output.contains("needle here"), "{out:?}");

        let out = execute_tool(
            &call("glob", serde_json::json!({"pattern": "*.rs"})),
            &dir,
            timeout,
            &[],
        )
        .await;
        assert!(out.ok && out.output.contains("src/a.rs"), "{out:?}");

        let out = execute_tool(
            &call(
                "patch",
                serde_json::json!({"path": "src/a.rs", "old_string": "let x = 1;", "new_string": "let x = 2;"}),
            ),
            &dir,
            timeout,
            &[],
        )
        .await;
        assert!(out.ok && out.output.contains("замен: 1"), "{out:?}");
        let patched = std::fs::read_to_string(dir.join("src").join("a.rs")).unwrap();
        assert!(patched.contains("let x = 2;"));

        // patch вне cwd отклоняется песочницей.
        let out = execute_tool(
            &call(
                "patch",
                serde_json::json!({"path": "../evil.rs", "old_string": "a", "new_string": "b"}),
            ),
            &dir,
            timeout,
            &[],
        )
        .await;
        assert!(!out.ok);

        std::fs::remove_dir_all(&dir).ok();
    }

    #[tokio::test]
    async fn execute_tool_fetch_url_rejects_non_http() {
        let out = execute_tool(
            &call(
                "fetch_url",
                serde_json::json!({"url": "file:///etc/passwd"}),
            ),
            std::path::Path::new("."),
            std::time::Duration::from_secs(5),
            &[],
        )
        .await;
        assert!(!out.ok);
        assert!(out.output.contains("http"), "{out:?}");
    }

    /* F-4: web_search — диспетчеризация инструмента, env-эндпоинт и
    честная ошибка на не-http. Env-переменная одна на процесс, поэтому
    обе проверки в одном тесте: параллельные set_var дали бы гонку. */
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn execute_tool_web_search_uses_env_endpoint_and_formats_hits() {
        use tokio::io::{AsyncReadExt, AsyncWriteExt};
        use tokio::net::TcpListener;

        let out = execute_tool(
            &call("web_search", serde_json::json!({"query": "  "})),
            std::path::Path::new("."),
            std::time::Duration::from_secs(5),
            &[],
        )
        .await;
        assert!(!out.ok, "пустой query — ошибка, а не пустая выдача");
        assert!(out.output.contains("query"), "{out:?}");

        // Не-http эндпоинт отклоняется до выхода в сеть.
        std::env::set_var("SWAGCOD_SEARCH_URL", "file:///tmp/search");
        let bad = execute_tool(
            &call("web_search", serde_json::json!({"query": "x"})),
            std::path::Path::new("."),
            std::time::Duration::from_secs(5),
            &[],
        )
        .await;
        assert!(!bad.ok, "{bad:?}");
        assert!(bad.output.contains("http(s)"), "{bad:?}");

        // Живой mock-движок (SearXNG-вкус): JSON → пронумерованный список.
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let port = listener.local_addr().unwrap().port();
        tokio::spawn(async move {
            if let Ok((mut sock, _)) = listener.accept().await {
                let mut buf = [0u8; 4096];
                let _ =
                    tokio::time::timeout(std::time::Duration::from_secs(5), sock.read(&mut buf))
                        .await;
                let body = r#"{"results":[
                    {"title":"Rust async book","url":"https://example.org/async","content":"how async works"},
                    {"title":"Tokio","url":"https://tokio.rs","content":"runtime"}
                ]}"#;
                let resp = format!(
                    "HTTP/1.1 200 OK\r\ncontent-type: application/json\r\ncontent-length: {}\r\n\r\n{}",
                    body.len(),
                    body
                );
                let _ = sock.write_all(resp.as_bytes()).await;
                let _ = sock.shutdown().await;
            }
        });
        std::env::set_var(
            "SWAGCOD_SEARCH_URL",
            format!("http://127.0.0.1:{port}/search"),
        );
        let out = execute_tool(
            &call(
                "web_search",
                serde_json::json!({"query": "rust async", "max_results": 5}),
            ),
            std::path::Path::new("."),
            std::time::Duration::from_secs(10),
            &[],
        )
        .await;
        std::env::remove_var("SWAGCOD_SEARCH_URL");

        assert!(out.ok, "{out:?}");
        assert!(
            out.output.contains("[web_search: «rust async» — 2 рез.]"),
            "{}",
            out.output
        );
        assert!(out.output.contains("1. Rust async book"), "{}", out.output);
        assert!(
            out.output.contains("https://example.org/async"),
            "{}",
            out.output
        );
        assert!(out.output.contains("how async works"), "{}", out.output);
    }

    #[tokio::test]
    #[cfg(windows)]
    async fn plugin_tool_executes_registered_command() {
        let dir = std::env::temp_dir().join(format!("swagcod-plugin-{}", short_id()));
        std::fs::create_dir_all(&dir).unwrap();
        let plugin = PluginTool {
            name: "echo_args".into(),
            description: "test plugin".into(),
            parameters: serde_json::json!({"type": "object", "properties": {}}),
            // findstr читает stdin; просто отдадим текст через cmd /C echo.
            command: "echo plugin-out".into(),
        };
        let out = execute_tool(
            &call("echo_args", serde_json::json!({"a": 1})),
            &dir,
            std::time::Duration::from_secs(10),
            std::slice::from_ref(&plugin),
        )
        .await;
        assert!(out.ok, "{out:?}");
        assert!(out.output.contains("plugin-out"), "{out:?}");
        std::fs::remove_dir_all(&dir).ok();
    }

    /* ── Кривые Руки (бета-тест): кривые пути, границы размеров, кодировки,
     * зависания и тихие сказки. Каждая группа гоняется с --nocapture,
     * [probe]-строки фиксируют фактическое поведение для отчёта. ──────── */

    fn clumsy(tag: &str) -> std::path::PathBuf {
        let dir = std::env::temp_dir().join(format!("swagcod-clumsy-{tag}-{}", short_id()));
        std::fs::create_dir_all(&dir).unwrap();
        dir
    }

    /// Выполнить тулз с таймаутом 5 с в каталоге cwd.
    async fn run_tool(name: &str, args: serde_json::Value, cwd: &std::path::Path) -> ToolOutcome {
        execute_tool(
            &call(name, args),
            cwd,
            std::time::Duration::from_secs(5),
            &[],
        )
        .await
    }

    #[test]
    fn clumsy_sandbox_path_windows_path_forms() {
        let cwd = clumsy("sbx");
        let ok_path = |raw: &str| sandbox_path(&cwd, raw);

        // пустой путь резолвится в сам cwd
        assert_eq!(ok_path("").unwrap(), cwd);
        // путь из пробелов допускается (файла нет — тулзы потом честно скажут ошибку)
        assert!(ok_path(" ").is_ok());
        // `..` отклоняется
        assert!(ok_path("..").is_err());
        // `..` глубже корня диска отклоняется
        assert!(
            ok_path("..\\..\\..\\..\\..\\..\\..\\..\\..\\..\\..\\..").is_err(),
            "глубокий .. обязан отклоняться"
        );
        // несуществующий хвост с `..` наружу отклоняется
        assert!(ok_path("newdir/../../outside.txt").is_err());
        // `..` в середине, остающийся внутри cwd, — разрешён
        assert!(ok_path("sub/../f.txt").is_ok());
        // абсолютные формы разрешены by design (гейтинг в core::turn)
        assert!(ok_path("C:/forward/slash").is_ok());
        assert!(ok_path("\\\\?\\C:\\verbatim\\x").is_ok());
        assert!(ok_path("\\\\localhost\\c$\\unc\\x").is_ok());
        // абсолютная вставка внутрь относительного пути не даёт побега
        match ok_path("foo\\C:\\bar") {
            Ok(p) => assert!(p.starts_with(&cwd), "путь обязан остаться в cwd: {p:?}"),
            Err(e) => eprintln!("[probe] 'foo\\C:\\bar' отклонён: {e}"),
        }
        std::fs::remove_dir_all(&cwd).ok();
    }

    #[tokio::test]
    async fn clumsy_read_size_and_encoding_edges() {
        let cwd = clumsy("read");

        // ровно 1 МБ — читается
        std::fs::write(cwd.join("exact.bin"), vec![b'a'; 1_048_576]).unwrap();
        let out = run_tool("read", serde_json::json!({"path": "exact.bin"}), &cwd).await;
        assert!(out.ok, "1 МБ ровно — читается: {out:?}");
        assert_eq!(out.output.len(), 1_048_576);

        // 1 МБ + 1 байт — отклоняется
        std::fs::write(cwd.join("over.bin"), vec![b'a'; 1_048_577]).unwrap();
        let out = run_tool("read", serde_json::json!({"path": "over.bin"}), &cwd).await;
        assert!(!out.ok, "1 МБ + 1 — отказ: {out:?}");
        eprintln!("[probe] read 1MB+1 → {}", out.output);

        // нулевой длины
        std::fs::write(cwd.join("empty.txt"), b"").unwrap();
        let out = run_tool("read", serde_json::json!({"path": "empty.txt"}), &cwd).await;
        assert!(out.ok && out.output.is_empty(), "нулевой файл: {out:?}");

        // каталог вместо файла
        let out = run_tool("read", serde_json::json!({"path": "."}), &cwd).await;
        assert!(!out.ok, "каталог вместо файла — ошибка: {out:?}");
        eprintln!("[probe] read каталога → {}", out.output);

        // UTF-16BE с BOM
        let text = "Тест BE ё";
        let mut bytes = vec![0xFEu8, 0xFF];
        for u in text.encode_utf16() {
            bytes.extend_from_slice(&u.to_be_bytes());
        }
        std::fs::write(cwd.join("be.txt"), &bytes).unwrap();
        let out = run_tool("read", serde_json::json!({"path": "be.txt"}), &cwd).await;
        assert!(
            out.ok && out.output.contains("Тест BE ё"),
            "UTF-16BE: {out:?}"
        );

        // latin-1-подобный мусор: не UTF-8, без BOM
        std::fs::write(cwd.join("l1.bin"), [0xC3u8, 0x28, 0xA0, 0xA1]).unwrap();
        let out = run_tool("read", serde_json::json!({"path": "l1.bin"}), &cwd).await;
        assert!(out.ok, "не-UTF-8 без BOM читается: {out:?}");
        eprintln!("[probe] latin-1 decode → {:?}", out.output);

        // UTF-16LE с непарным хвостовым байтом
        let odd = [0xFFu8, 0xFE, b'A', 0x00, b'B'];
        std::fs::write(cwd.join("odd.txt"), odd).unwrap();
        let out = run_tool("read", serde_json::json!({"path": "odd.txt"}), &cwd).await;
        assert!(out.ok, "непарный UTF-16 не валит read: {out:?}");
        eprintln!(
            "[probe] UTF-16LE непарный хвост → {:?} (хвостовой байт 'B' потерян молча?)",
            out.output
        );

        // NUL-байт в пути — ошибка, не паника
        let out = run_tool(
            "read",
            serde_json::json!({"path": "bad\u{0}name.txt"}),
            &cwd,
        )
        .await;
        assert!(!out.ok, "NUL в пути — ошибка: {out:?}");
        eprintln!("[probe] read NUL-путь → {}", out.output);

        // trailing dot / trailing space — Win32-нормализация имён
        std::fs::write(cwd.join("dot.txt"), "dotcontent").unwrap();
        let out = run_tool("read", serde_json::json!({"path": "dot.txt."}), &cwd).await;
        eprintln!("[probe] read 'dot.txt.' → ok={} {:?}", out.ok, out.output);
        let out2 = run_tool("read", serde_json::json!({"path": "dot.txt "}), &cwd).await;
        eprintln!("[probe] read 'dot.txt ' → ok={} {:?}", out2.ok, out2.output);

        std::fs::remove_dir_all(&cwd).ok();
    }

    // Ревизия 84 (бывшая находка): write в имя DOS-устройства 'nul' пишет в
    // устройство и отчитывается успехом, хотя файла нет — тихая ложь для
    // модели. Теперь — честный отказ (is_dos_device_name в ветке write).
    #[cfg(windows)]
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn clumsy_read_windows_device_names() {
        let cwd = clumsy("dev");
        let t = std::time::Duration::from_secs(5);

        // read по имени DOS-устройства nul
        let out = execute_tool(
            &call("read", serde_json::json!({"path": "nul"})),
            &cwd,
            t,
            &[],
        )
        .await;
        eprintln!("[probe] read 'nul' → ok={} {:?}", out.ok, out.output);

        // read по имени устройства con: возможное блокирующее чтение консоли —
        // проверяем таймаутом, зависший воркер утешит завершение процесса.
        let started = std::time::Instant::now();
        let res = tokio::time::timeout(
            std::time::Duration::from_secs(5),
            execute_tool(
                &call("read", serde_json::json!({"path": "con"})),
                &cwd,
                t,
                &[],
            ),
        )
        .await;
        match res {
            Ok(out) => eprintln!("[probe] read 'con' → ok={} {:?}", out.ok, out.output),
            Err(_) => panic!(
                "read 'con' не вернулся за 5 с (блокирующее чтение CON, \
                 у read-тулза нет таймаута), прошло {:?}",
                started.elapsed()
            ),
        }

        // write в nul: если тулз отчитался успехом — файл обязан существовать
        let out = execute_tool(
            &call(
                "write",
                serde_json::json!({"path": "nul", "content": "xyz"}),
            ),
            &cwd,
            t,
            &[],
        )
        .await;
        eprintln!("[probe] write 'nul' → ok={} {:?}", out.ok, out.output);
        let entries: Vec<String> = std::fs::read_dir(&cwd)
            .unwrap()
            .flatten()
            .map(|e| e.file_name().to_string_lossy().into_owned())
            .collect();
        eprintln!("[probe] записи каталога после write 'nul': {:?}", entries);
        assert!(
            !out.ok || entries.iter().any(|e| e == "nul"),
            "FINDING: write 'nul' отчитался успехом ({:?}), но файла нет: {:?}",
            out.output,
            entries
        );

        std::fs::remove_dir_all(&cwd).ok();
    }

    #[cfg(windows)]
    #[tokio::test]
    async fn clumsy_read_locked_file_fails() {
        let cwd = clumsy("lock");
        let target = cwd.join("locked.txt");
        std::fs::write(&target, "secret").unwrap();
        use std::os::windows::process::CommandExt as _;
        // powershell держит файл эксклюзивно (FileShare::None) 6 секунд
        let child = std::process::Command::new("powershell")
            .arg("-NoProfile")
            .arg("-Command")
            .arg(format!(
                "$f=[System.IO.File]::Open('{}','Open','Read','None'); Start-Sleep -Seconds 6",
                target.display()
            ))
            .creation_flags(0x0800_0000)
            .spawn();
        match child {
            Ok(mut child) => {
                std::thread::sleep(std::time::Duration::from_millis(900));
                /* Холодный старт powershell плавает от нагрузки машины:
                ждём фактического лока, а не слепой паузы — иначе read
                успевает прочитать незалоченный файл и тест флапает. */
                let mut out = ToolOutcome {
                    ok: true,
                    output: String::new(),
                };
                for _ in 0..50 {
                    out = run_tool("read", serde_json::json!({"path": "locked.txt"}), &cwd).await;
                    if !out.ok {
                        break;
                    }
                    tokio::time::sleep(std::time::Duration::from_millis(100)).await;
                }
                assert!(!out.ok, "занятый файл — ошибка чтения: {out:?}");
                eprintln!("[probe] read занятого файла → {}", out.output);
                let _ = child.kill();
                let _ = child.wait();
                let _ = std::fs::remove_dir_all(&cwd);
            }
            Err(e) => eprintln!("[probe] powershell недоступен ({e}) — пропуск"),
        }
    }

    #[tokio::test]
    async fn clumsy_list_edges() {
        let cwd = clumsy("list");

        // пустой каталог
        let out = run_tool("list", serde_json::json!({"path": "."}), &cwd).await;
        assert!(out.ok && out.output.is_empty(), "пустой каталог: {out:?}");

        // файл вместо каталога
        std::fs::write(cwd.join("f.txt"), "x").unwrap();
        let out = run_tool("list", serde_json::json!({"path": "f.txt"}), &cwd).await;
        assert!(!out.ok, "файл вместо каталога: {out:?}");
        eprintln!("[probe] list файла → {}", out.output);

        // 5000 файлов: не виснет, отдаёт все
        let many = cwd.join("many");
        std::fs::create_dir_all(&many).unwrap();
        let t0 = std::time::Instant::now();
        for i in 0..5000 {
            std::fs::write(many.join(format!("f{i}.txt")), "x").unwrap();
        }
        let build = t0.elapsed();
        let t1 = std::time::Instant::now();
        let out = run_tool("list", serde_json::json!({"path": "many"}), &cwd).await;
        let scan = t1.elapsed();
        assert!(out.ok, "листинг 5000: {out:?}");
        assert_eq!(out.output.lines().count(), 5000, "все 5000 строк на месте");
        eprintln!("[probe] list 5000 файлов: создание {build:?}, листинг {scan:?}");

        std::fs::remove_dir_all(&cwd).ok();
    }

    #[tokio::test]
    async fn clumsy_write_edges() {
        let cwd = clumsy("write");

        // read-only файл
        let ro = cwd.join("ro.txt");
        std::fs::write(&ro, "old").unwrap();
        let mut perm = std::fs::metadata(&ro).unwrap().permissions();
        perm.set_readonly(true);
        std::fs::set_permissions(&ro, perm).unwrap();
        let out = run_tool(
            "write",
            serde_json::json!({"path": "ro.txt", "content": "new"}),
            &cwd,
        )
        .await;
        assert!(!out.ok, "read-only файл — отказ: {out:?}");
        eprintln!("[probe] write read-only файла → {}", out.output);
        let mut perm = std::fs::metadata(&ro).unwrap().permissions();
        #[allow(clippy::permissions_set_readonly_false)]
        perm.set_readonly(false);
        std::fs::set_permissions(&ro, perm).unwrap();

        // read-only каталог (атрибут, Windows его для каталогов игнорирует)
        let rodir = cwd.join("rodir");
        std::fs::create_dir_all(&rodir).unwrap();
        let mut perm = std::fs::metadata(&rodir).unwrap().permissions();
        perm.set_readonly(true);
        std::fs::set_permissions(&rodir, perm).unwrap();
        let out = run_tool(
            "write",
            serde_json::json!({"path": "rodir/x.txt", "content": "x"}),
            &cwd,
        )
        .await;
        eprintln!(
            "[probe] write в read-only каталог → ok={} {:?}",
            out.ok, out.output
        );
        let mut perm = std::fs::metadata(&rodir).unwrap().permissions();
        #[allow(clippy::permissions_set_readonly_false)]
        perm.set_readonly(false);
        std::fs::set_permissions(&rodir, perm).unwrap();

        // путь указывает на существующий каталог
        let out = run_tool(
            "write",
            serde_json::json!({"path": ".", "content": "x"}),
            &cwd,
        )
        .await;
        assert!(!out.ok, "запись в каталог — отказ: {out:?}");
        eprintln!("[probe] write в каталог → {}", out.output);

        // родитель — обычный файл
        std::fs::write(cwd.join("blocker.txt"), "x").unwrap();
        let out = run_tool(
            "write",
            serde_json::json!({"path": "blocker.txt/sub.txt", "content": "x"}),
            &cwd,
        )
        .await;
        assert!(!out.ok, "родитель-файл — отказ: {out:?}");
        eprintln!("[probe] write под файлом → {}", out.output);

        // \r\n + юникод: счётчик байтов обязан быть точным
        let content = "строка1\r\nline2\r\nмир";
        let out = run_tool(
            "write",
            serde_json::json!({"path": "u.txt", "content": content}),
            &cwd,
        )
        .await;
        assert!(out.ok, "юникод-запись: {out:?}");
        assert_eq!(
            out.output,
            format!("записано {} байт", content.len()),
            "счётчик байтов: {}",
            out.output
        );
        assert_eq!(
            std::fs::read(cwd.join("u.txt")).unwrap(),
            content.as_bytes()
        );

        // юникод-имя файла + двойные точки в имени
        let out = run_tool(
            "write",
            serde_json::json!({"path": "юникод-測試..txt", "content": "ok"}),
            &cwd,
        )
        .await;
        assert!(out.ok, "юникод-имя: {out:?}");
        let back = run_tool(
            "read",
            serde_json::json!({"path": "юникод-測試..txt"}),
            &cwd,
        )
        .await;
        assert!(
            back.ok && back.output == "ok",
            "roundtrip юникод-имени: {back:?}"
        );

        // пустой content
        let out = run_tool(
            "write",
            serde_json::json!({"path": "e.txt", "content": ""}),
            &cwd,
        )
        .await;
        assert!(
            out.ok && out.output == "записано 0 байт",
            "пустой content: {out:?}"
        );
        assert_eq!(std::fs::metadata(cwd.join("e.txt")).unwrap().len(), 0);

        // NUL в пути
        let out = run_tool(
            "write",
            serde_json::json!({"path": "bad\u{0}.txt", "content": "x"}),
            &cwd,
        )
        .await;
        assert!(!out.ok, "NUL в пути: {out:?}");
        eprintln!("[probe] write NUL-путь → {}", out.output);

        std::fs::remove_dir_all(&cwd).ok();
    }

    #[cfg(windows)]
    #[tokio::test]
    async fn clumsy_write_windows_name_normalization() {
        let cwd = clumsy("winname");
        // trailing dot: Win32 съедает точку
        let out = run_tool(
            "write",
            serde_json::json!({"path": "dot.", "content": "dotcontent"}),
            &cwd,
        )
        .await;
        eprintln!("[probe] write 'dot.' → ok={} {:?}", out.ok, out.output);
        // имя из точек как «каталог»
        let out2 = run_tool(
            "write",
            serde_json::json!({"path": ".../x.txt", "content": "dotsdir"}),
            &cwd,
        )
        .await;
        eprintln!(
            "[probe] write '.../x.txt' → ok={} {:?}",
            out2.ok, out2.output
        );
        let entries: Vec<String> = std::fs::read_dir(&cwd)
            .unwrap()
            .flatten()
            .map(|e| e.file_name().to_string_lossy().into_owned())
            .collect();
        eprintln!("[probe] записи каталога: {:?}", entries);
        // читаем назад через оба имени
        let back1 = run_tool("read", serde_json::json!({"path": "dot."}), &cwd).await;
        eprintln!("[probe] read 'dot.' → ok={} {:?}", back1.ok, back1.output);
        let back2 = run_tool("read", serde_json::json!({"path": "x.txt"}), &cwd).await;
        eprintln!("[probe] read 'x.txt' → ok={} {:?}", back2.ok, back2.output);
        std::fs::remove_dir_all(&cwd).ok();
    }

    /* ── Кривые Руки: patch ─────────────────────────────────────────── */

    #[tokio::test]
    async fn clumsy_patch_edge_cases() {
        let cwd = clumsy("patch");
        std::fs::write(cwd.join("p.txt"), "aaa bbb\n").unwrap();

        // пустой old_string — честный отказ
        let out = run_tool(
            "patch",
            serde_json::json!({"path": "p.txt", "old_string": "", "new_string": "x"}),
            &cwd,
        )
        .await;
        assert!(!out.ok && out.output.contains("old_string пуст"), "{out:?}");

        // replace_all + пустой old — тоже отказ
        let out = run_tool(
            "patch",
            serde_json::json!({"path": "p.txt", "old_string": "", "new_string": "x", "replace_all": true}),
            &cwd,
        )
        .await;
        assert!(!out.ok, "replace_all + пустой old: {out:?}");

        // old == new
        let out = run_tool(
            "patch",
            serde_json::json!({"path": "p.txt", "old_string": "aaa", "new_string": "aaa"}),
            &cwd,
        )
        .await;
        eprintln!("[probe] patch old==new → ok={} {:?}", out.ok, out.output);
        assert_eq!(
            std::fs::read_to_string(cwd.join("p.txt")).unwrap(),
            "aaa bbb\n",
            "содержимое не должно портиться"
        );

        // old длиннее файла
        let out = run_tool(
            "patch",
            serde_json::json!({"path": "p.txt", "old_string": "aaa bbb ccc ddd eee fff", "new_string": "x"}),
            &cwd,
        )
        .await;
        assert!(!out.ok, "old длиннее файла: {out:?}");
        eprintln!("[probe] patch old>файла → {}", out.output);

        // NFC-файл против NFD-old (ё): литеральный патчер — честная ошибка
        std::fs::write(cwd.join("nfc.txt"), "ёжик\n").unwrap(); // ё = U+0451 (NFC)
        let nfd_old = "е\u{0308}жик"; // NFD: е + combining diaeresis
        let out = run_tool(
            "patch",
            serde_json::json!({"path": "nfc.txt", "old_string": nfd_old, "new_string": "x"}),
            &cwd,
        )
        .await;
        assert!(
            !out.ok,
            "NFD old против NFC файла — ошибка, не ложь: {out:?}"
        );
        eprintln!("[probe] patch NFD/NFC → {}", out.output);

        // бинарный файл: read_to_string обязан отказаться
        std::fs::write(cwd.join("bin.bin"), [0x00u8, 0xFF, 0xFE, 0x42]).unwrap();
        let out = run_tool(
            "patch",
            serde_json::json!({"path": "bin.bin", "old_string": "B", "new_string": "C"}),
            &cwd,
        )
        .await;
        assert!(!out.ok, "бинарный файл — ошибка: {out:?}");
        eprintln!("[probe] patch бинарника → {}", out.output);

        std::fs::remove_dir_all(&cwd).ok();
    }

    // Ревизия 84 (бывшая находка): patch через whitespace-уровень (level 2
    // fsx::tools::patch_text) пересобирал файл join("\n") — CRLF-концы
    // терялись у ВСЕХ строк, включая нетронутые. Теперь концы строк живут
    // внутри кусков split_inclusive и сохраняются.
    #[tokio::test]
    async fn clumsy_patch_crlf_silently_converted_to_lf() {
        let cwd = clumsy("crlf");
        std::fs::write(cwd.join("c.txt"), b"line1\r\nline2\r\nline3\r\n").unwrap();
        let out = run_tool(
            "patch",
            serde_json::json!({
                "path": "c.txt",
                "old_string": "line1\nline2",
                "new_string": "REPL"
            }),
            &cwd,
        )
        .await;
        eprintln!("[probe] patch CRLF → ok={} {:?}", out.ok, out.output);
        let after = std::fs::read(cwd.join("c.txt")).unwrap();
        eprintln!(
            "[probe] байты после патча: {:?}",
            String::from_utf8_lossy(&after)
        );
        assert!(out.ok, "патч сам проходит: {out:?}");
        assert!(
            after.windows(2).any(|w| w == b"\r\n"),
            "FINDING: CRLF-концы обязаны сохраняться у нетронутых строк, \
             файл после патча: {:?}",
            String::from_utf8_lossy(&after)
        );
        std::fs::remove_dir_all(&cwd).ok();
    }

    /* ── Кривые Руки: grep ──────────────────────────────────────────── */

    // Ревизия 84 (бывшая находка, критично): у grep аргумент `path` миновал
    // sandbox_path (в отличие от read/list/write/patch), а гейт
    // escapes_workspace наверху ловил только абсолютные пути —
    // относительный '../outside' читал файлы вне cwd тихо. Теперь субпуть
    // идёт через sandbox_search_root: тот же запрет, что у read/write.
    #[tokio::test]
    async fn clumsy_grep_subpath_escapes_sandbox() {
        let parent = clumsy("grep-esc");
        let cwd = parent.join("cwd");
        std::fs::create_dir_all(&cwd).unwrap();
        let outside = parent.join("outside");
        std::fs::create_dir_all(&outside).unwrap();
        std::fs::write(outside.join("secret.txt"), "GREPSECRET-1\n").unwrap();

        let out = run_tool(
            "grep",
            serde_json::json!({"pattern": "GREPSECRET", "path": "../outside"}),
            &cwd,
        )
        .await;
        eprintln!("[probe] grep '../outside' → ok={} {:?}", out.ok, out.output);

        // абсолютный субпуть: Path::join с абсолютным аргументом заменяет
        // путь целиком — тоже уходит из cwd (наверху это ловит escapes_workspace,
        // на уровне исполнителя — нет).
        let out_abs = run_tool(
            "grep",
            serde_json::json!({"pattern": "GREPSECRET", "path": outside.to_string_lossy()}),
            &cwd,
        )
        .await;
        eprintln!(
            "[probe] grep абсолютный субпуть → ok={} {:?}",
            out_abs.ok, out_abs.output
        );

        assert!(
            !out.ok,
            "FINDING: grep с path='../outside' обязан отказывать (вне cwd), а нашёл: {}",
            out.output
        );
        std::fs::remove_dir_all(&parent).ok();
    }

    // Ревизия 84 (бывшая находка, критично): у glob аргумент `path` тоже
    // шёл мимо sandbox_path — '../outside' индексировал чужой каталог,
    // выдача содержала пути ВНЕ cwd. Теперь — тот же запрет, что у read.
    #[tokio::test]
    async fn clumsy_glob_subpath_escapes_sandbox() {
        let parent = clumsy("glob-esc");
        let cwd = parent.join("cwd");
        std::fs::create_dir_all(&cwd).unwrap();
        let outside = parent.join("outside");
        std::fs::create_dir_all(&outside).unwrap();
        std::fs::write(outside.join("leakme.txt"), "x").unwrap();

        let out = run_tool(
            "glob",
            serde_json::json!({"pattern": "leakme.txt", "path": "../outside"}),
            &cwd,
        )
        .await;
        eprintln!("[probe] glob '../outside' → ok={} {:?}", out.ok, out.output);
        assert!(
            !out.ok,
            "FINDING: glob с path='../outside' обязан отказывать, а выдал: {}",
            out.output
        );
        std::fs::remove_dir_all(&parent).ok();
    }

    #[tokio::test]
    async fn clumsy_grep_pattern_edges() {
        let cwd = clumsy("grep");
        std::fs::write(cwd.join("a.rs"), "needle one\nnope\nNeEdLe two\n").unwrap();

        // пустой pattern
        let out = run_tool("grep", serde_json::json!({"pattern": ""}), &cwd).await;
        assert!(!out.ok && out.output.contains("пустой pattern"), "{out:?}");

        // lookahead не поддерживается движком — честная ошибка
        let out = run_tool("grep", serde_json::json!({"pattern": "needle(?=x)"}), &cwd).await;
        assert!(
            !out.ok && out.output.contains("некорректный regex"),
            "{out:?}"
        );
        eprintln!("[probe] grep lookahead → {}", out.output);

        // паттерн из одного '\' — незакрытый escape
        let out = run_tool("grep", serde_json::json!({"pattern": "\\"}), &cwd).await;
        assert!(!out.ok, "незакрытый escape: {out:?}");
        eprintln!("[probe] grep '\\' → {}", out.output);

        // регистронезависимость
        let out = run_tool("grep", serde_json::json!({"pattern": "(?i)needle"}), &cwd).await;
        assert!(out.ok && out.output.contains("NeEdLe two"), "{out:?}");

        // субпуть-файл
        let out = run_tool(
            "grep",
            serde_json::json!({"pattern": "x", "path": "a.rs"}),
            &cwd,
        )
        .await;
        assert!(!out.ok, "путь-файл для grep — ошибка: {out:?}");
        eprintln!("[probe] grep путь-файл → {}", out.output);

        // context строкой вместо числа: молчаливый дефолт?
        let out = run_tool(
            "grep",
            serde_json::json!({"pattern": "needle one", "context": "10"}),
            &cwd,
        )
        .await;
        eprintln!(
            "[probe] grep context:'10' (строкой) → ok={} {:?}",
            out.ok, out.output
        );

        std::fs::remove_dir_all(&cwd).ok();
    }

    #[tokio::test]
    async fn clumsy_grep_catastrophic_regex_timing() {
        let cwd = clumsy("reboom");
        let line = "a".repeat(100_000);
        std::fs::write(cwd.join("boom.txt"), format!("{line}\n")).unwrap();
        for pat in ["(a+)+b", "(a|a)*b", "(x+x+)+y"] {
            let t0 = std::time::Instant::now();
            let out = run_tool("grep", serde_json::json!({"pattern": pat}), &cwd).await;
            let dt = t0.elapsed();
            eprintln!("[probe] grep '{pat}' на 100k 'a' → ok={} за {dt:?}", out.ok);
            assert!(out.ok, "паттерн {pat} обязан компилироваться: {out:?}");
            assert!(
                dt < std::time::Duration::from_secs(30),
                "FINDING: катастрофический бэктрекинг: {pat} занял {dt:?}"
            );
        }
        std::fs::remove_dir_all(&cwd).ok();
    }

    #[tokio::test]
    async fn clumsy_grep_binary_files() {
        let cwd = clumsy("grepbin");
        // NUL в первых 8 КБ — файл пропускается как бинарник
        std::fs::write(cwd.join("b.bin"), b"target\x00\x01\x02\n").unwrap();
        let out = run_tool("grep", serde_json::json!({"pattern": "target"}), &cwd).await;
        assert!(
            out.ok && out.output.contains("совпадений нет"),
            "бинарник пропущен: {out:?}"
        );

        // NUL после 8 КБ — файл считается текстом, NUL едет в выдачу
        let mut far = vec![b'x'; 9000];
        far.extend_from_slice(b"\nneedle-after-nul\x00tail\n");
        std::fs::write(cwd.join("far.txt"), &far).unwrap();
        let out = run_tool("grep", serde_json::json!({"pattern": "nul"}), &cwd).await;
        eprintln!(
            "[probe] grep NUL>8K → ok={} output={:?}",
            out.ok, out.output
        );
        assert!(out.ok, "{out:?}");
        std::fs::remove_dir_all(&cwd).ok();
    }

    /* ── Кривые Руки: glob ──────────────────────────────────────────── */

    #[tokio::test]
    async fn clumsy_glob_pattern_edges() {
        let cwd = clumsy("glob");
        std::fs::create_dir_all(cwd.join("sub")).unwrap();
        std::fs::write(cwd.join("a.txt"), "x").unwrap();
        std::fs::write(cwd.join("axxxb"), "x").unwrap();
        std::fs::write(cwd.join("sub").join("b.txt"), "x").unwrap();

        // пустой паттерн
        let out = run_tool("glob", serde_json::json!({"pattern": ""}), &cwd).await;
        assert!(!out.ok && out.output.contains("пустой pattern"), "{out:?}");

        // незакрытый char class
        let out = run_tool("glob", serde_json::json!({"pattern": "[.."}), &cwd).await;
        eprintln!("[probe] glob '[..' → ok={} {:?}", out.ok, out.output);
        assert!(!out.ok, "некорректный класс — ошибка, не паника: {out:?}");

        // 'a**b' — ** внутри сегмента
        let out = run_tool("glob", serde_json::json!({"pattern": "a**b"}), &cwd).await;
        eprintln!("[probe] glob 'a**b' → ok={} {:?}", out.ok, out.output);

        // '**/**/**/*'
        let out = run_tool("glob", serde_json::json!({"pattern": "**/**/**/*"}), &cwd).await;
        eprintln!("[probe] glob '**/**/**/*' → ok={} {:?}", out.ok, out.output);
        assert!(out.ok, "паттерн компилируется: {out:?}");

        // глубокая вложенность: 60 уровней
        let mut deep = cwd.join("deep");
        for i in 0..60 {
            deep = deep.join(format!("d{i}"));
        }
        std::fs::create_dir_all(&deep).unwrap();
        std::fs::write(deep.join("deep.txt"), "x").unwrap();
        let t0 = std::time::Instant::now();
        let out = run_tool("glob", serde_json::json!({"pattern": "**/deep.txt"}), &cwd).await;
        eprintln!(
            "[probe] glob 60-вглубь → ok={} за {:?} {:?}",
            out.ok,
            t0.elapsed(),
            out.output
        );
        assert!(out.ok, "глубокая вложенность не вешает: {out:?}");
        assert!(out.output.contains("deep.txt"), "{out:?}");

        std::fs::remove_dir_all(&cwd).ok();
    }

    /* ── Кривые Руки: fetch_url ─────────────────────────────────────── */

    #[tokio::test]
    async fn clumsy_fetch_url_scheme_edges() {
        let dot = std::path::Path::new(".");
        for url in [
            "http://",
            "https://",
            "data:text/plain,hi",
            "javascript:alert(1)",
            "ftp://127.0.0.1",
            "//evil.com",
            "http:/127.0.0.1",
            "file:///etc/passwd",
        ] {
            let out = run_tool("fetch_url", serde_json::json!({"url": url}), dot).await;
            eprintln!("[probe] fetch '{url}' → ok={} {}", out.ok, out.output);
            assert!(!out.ok, "URL '{url}' должен отклоняться: {out:?}");
        }
    }

    #[tokio::test]
    async fn clumsy_fetch_url_host_and_size_edges() {
        let dot = std::path::Path::new(".");

        // whitespace-padded: схема проверяется по trim, запрос идёт по trim
        let out = run_tool(
            "fetch_url",
            serde_json::json!({"url": "  http://127.0.0.1:1/x  "}),
            dot,
        )
        .await;
        eprintln!("[probe] fetch padded → ok={} {:?}", out.ok, out.output);
        assert!(!out.ok, "{out:?}");

        // юникод в пути URL
        let out = run_tool(
            "fetch_url",
            serde_json::json!({"url": "http://127.0.0.1:1/путь-測試"}),
            dot,
        )
        .await;
        eprintln!("[probe] fetch юникод-путь → ok={} {:?}", out.ok, out.output);
        assert!(!out.ok, "{out:?}");

        // управляющий символ в URL
        let out = run_tool(
            "fetch_url",
            serde_json::json!({"url": "http://127.0.0.1:1/a\nb"}),
            dot,
        )
        .await;
        eprintln!("[probe] fetch URL с \\n → ok={} {:?}", out.ok, out.output);
        assert!(!out.ok, "{out:?}");

        // URL длиной 100000
        let url = format!("http://127.0.0.1:1/{}", "a".repeat(100_000));
        let t0 = std::time::Instant::now();
        let out = run_tool("fetch_url", serde_json::json!({"url": url}), dot).await;
        eprintln!(
            "[probe] fetch 100k URL → ok={} за {:?} — {:?}",
            out.ok,
            t0.elapsed(),
            out.output
        );
        assert!(!out.ok, "{out:?}");
    }

    /* ── Кривые Руки: bash ──────────────────────────────────────────── */

    #[cfg(windows)]
    #[tokio::test]
    async fn clumsy_bash_timeout_kills_and_unicode_roundtrip() {
        let cwd = clumsy("bash");
        // таймаут: ping на 60 с обязан умереть через 2 с
        let t0 = std::time::Instant::now();
        let call = call(
            "bash",
            serde_json::json!({"command": "ping -n 60 127.0.0.1"}),
        );
        let out = execute_tool(&call, &cwd, std::time::Duration::from_secs(2), &[]).await;
        eprintln!(
            "[probe] bash ping таймаут → ok={} за {:?}: {}",
            out.ok,
            t0.elapsed(),
            out.output
        );
        assert!(
            !out.ok && out.output.contains("не завершилась"),
            "таймаут обязан срабатывать: {out:?}"
        );
        assert!(
            t0.elapsed() < std::time::Duration::from_secs(15),
            "убийство дерева не должно затягиваться: {:?}",
            t0.elapsed()
        );

        // юникод-раундтрип: powershell пишет юникод в stdout-пайп стабильно
        // на любой локали ([Console]::OutputEncoding = UTF-8), в отличие от
        // cmd: на US-раннере (OEM cp437) cmd echo кириллицы молча даёт
        // «??????», chcp 65001 внутри cmd не лечит — аргумент уже испорчен
        // при разборе. Ревизия 86: проверка декодера UTF-8-пути стала
        // детерминированной; OEM-путь (cp866) проверяется локально.
        let out = run_tool(
            "bash",
            serde_json::json!({"command": "powershell -NoProfile -Command \"[Console]::OutputEncoding=[System.Text.Encoding]::UTF8; 'Кривые-Руки-Тест'\""}),
            &cwd,
        )
        .await;
        eprintln!("[probe] bash echo юникод (powershell utf8) → {:?}", out.output);
        assert!(
            out.output.contains("Кривые-Руки-Тест"),
            "кириллица в stdout обязана декодироваться: {out:?}"
        );
        std::fs::remove_dir_all(&cwd).ok();
    }
}

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
            let sub = arg_str(call, "path");
            let context = call
                .arguments
                .get("context")
                .and_then(|v| v.as_u64())
                .unwrap_or(2) as usize;
            match swagcod_fsx::tools::grep_files(
                cwd,
                if sub.is_empty() {
                    None
                } else {
                    Some(sub.as_str())
                },
                &pattern,
                context,
            ) {
                Ok(hits) if hits.is_empty() => ToolOutcome {
                    ok: true,
                    output: "совпадений нет".into(),
                },
                Ok(hits) => ToolOutcome {
                    ok: true,
                    output: swagcod_fsx::tools::format_hits(&hits),
                },
                Err(e) => fail(format!("grep: {e}")),
            }
        }
        "glob" => {
            let pattern = arg_str(call, "pattern");
            if pattern.is_empty() {
                return fail("пустой pattern".into());
            }
            let sub = arg_str(call, "path");
            match swagcod_fsx::tools::glob_files(
                cwd,
                if sub.is_empty() {
                    None
                } else {
                    Some(sub.as_str())
                },
                &pattern,
            ) {
                Ok(files) if files.is_empty() => ToolOutcome {
                    ok: true,
                    output: "совпадений нет".into(),
                },
                Ok(files) => ToolOutcome {
                    ok: true,
                    output: files.join("\n"),
                },
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
}

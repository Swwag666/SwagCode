//! E-4: JS-плагины в sidecar — горячая загрузка без отдельных exe (B-5).
//!
//! Каталог `plugins/*.js` читает отдельный node-процесс
//! (`sidecar/plugin-server.js`), каждый плагин живёт в собственном
//! vm-контексте с жёстким дедлайном: синхронный handler убивается по
//! таймауту vm, поэтому «плагин обязан отвечать в дедлайн» — гарантия
//! транспорта, а не вежливость автора. Мост к хранилищу — только
//! read-only API prefs (не сессий: приватность).
//!
//! Имя инструмента в модели — `js:<name>`: не входит в
//! ApprovalPolicy::BUILTIN, поэтому политика подтверждений и журнал B-7
//! работают как для MCP (префикс виден в журнале). Транспорт — общий
//! stdio_rpc (JSON-RPC 2.0 newline-delimited).

use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::Duration;

use serde::Serialize;
use serde_json::{json, Value};

use crate::stdio_rpc::{self, RpcConn};

/// Префикс имён JS-плагинов в модели и журнале.
pub const JS_PREFIX: &str = "js:";
/// Дедлайн вызова handler в vm (override SWAGCOD_JS_TIMEOUT_MS).
pub const JS_CALL_TIMEOUT_MS: u64 = 30_000;
/// Дедлайн загрузки каталога (чтение файлов + исполнение манифестов).
pub const JS_LOAD_TIMEOUT_MS: u64 = 15_000;
/// Запас поверх vm-дедлайна: сервер сам отвечает честной ошибкой
/// «не ответил в дедлайн» раньше, чем сработает rpc-таймаут.
const RPC_SLACK_MS: u64 = 10_000;

/// Инструмент загруженного плагина — то, что видит модель и UI.
#[derive(Debug, Clone, Serialize)]
pub struct JsPluginTool {
    /// Полное имя для модели: `js:<short>`.
    pub name: String,
    pub short: String,
    pub description: String,
    pub parameters: Value,
    pub file: String,
}

fn call_timeout_ms() -> u64 {
    std::env::var("SWAGCOD_JS_TIMEOUT_MS")
        .ok()
        .and_then(|v| v.parse::<u64>().ok())
        .filter(|v| (50..=300_000).contains(v))
        .unwrap_or(JS_CALL_TIMEOUT_MS)
}

/// Каталог плагинов: явный SWAGCOD_PLUGINS_DIR → `plugins` рядом с exe →
/// `plugins` от cwd и вверх по предкам. Если ничего не существует —
/// `plugins` рядом с exe (загрузка честно сообщит, что каталога нет).
pub fn plugins_dir() -> PathBuf {
    if let Ok(d) = std::env::var("SWAGCOD_PLUGINS_DIR") {
        if !d.trim().is_empty() {
            return PathBuf::from(d);
        }
    }
    if let Ok(exe) = std::env::current_exe() {
        if let Some(dir) = exe.parent() {
            let p = dir.join("plugins");
            if p.is_dir() {
                return p;
            }
        }
    }
    if let Ok(cwd) = std::env::current_dir() {
        let mut dir = cwd.as_path();
        for _ in 0..6 {
            let p = dir.join("plugins");
            if p.is_dir() {
                return p;
            }
            match dir.parent() {
                Some(parent) => dir = parent,
                None => break,
            }
        }
    }
    std::env::current_exe()
        .ok()
        .and_then(|e| e.parent().map(|d| d.join("plugins")))
        .unwrap_or_else(|| PathBuf::from("plugins"))
}

/// Живой sidecar плагинов: соединение + загруженные инструменты +
/// честные ошибки загрузки.
pub struct JsHost {
    conn: Arc<RpcConn>,
    pub tools: Vec<JsPluginTool>,
    pub errors: Vec<String>,
    pub dir: String,
}

/// Разбор ответа load/list — чистая функция, тестируется на JSON.
pub fn parse_plugin_list(result: &Value) -> (Vec<JsPluginTool>, Vec<String>) {
    let tools = result
        .get("plugins")
        .and_then(|v| v.as_array())
        .map(|arr| {
            arr.iter()
                .filter_map(|p| {
                    let short = p.get("name")?.as_str()?.to_string();
                    Some(JsPluginTool {
                        name: format!("{JS_PREFIX}{short}"),
                        short,
                        description: p
                            .get("description")
                            .and_then(|d| d.as_str())
                            .unwrap_or_default()
                            .to_string(),
                        parameters: p
                            .get("parameters")
                            .cloned()
                            .unwrap_or_else(|| json!({"type": "object"})),
                        file: p
                            .get("file")
                            .and_then(|f| f.as_str())
                            .unwrap_or_default()
                            .to_string(),
                    })
                })
                .collect()
        })
        .unwrap_or_default();
    let errors = result
        .get("errors")
        .and_then(|v| v.as_array())
        .map(|arr| {
            arr.iter()
                .map(|e| {
                    format!(
                        "{}: {}",
                        e.get("file").and_then(|f| f.as_str()).unwrap_or("?"),
                        e.get("error").and_then(|x| x.as_str()).unwrap_or("?")
                    )
                })
                .collect()
        })
        .unwrap_or_default();
    (tools, errors)
}

impl JsHost {
    /// Поднять sidecar и загрузить каталог. `db` — путь базы стора для
    /// prefs-моста (плагины видят только prefs, только на чтение).
    pub async fn start(dir: &Path, db: Option<&Path>) -> Result<JsHost, String> {
        let node = swagcod_core::node_store::node_binary();
        let script = swagcod_core::node_store::sidecar_file("plugin-server.js")
            .ok_or_else(|| "sidecar/plugin-server.js не найден (бандл или vendor)".to_string())?;
        let conn = stdio_rpc::spawn(
            &node,
            &[script.to_string_lossy().to_string()],
            &std::collections::HashMap::new(),
            "js-plugins",
        )?;
        let resp = conn
            .request(
                "load",
                json!({
                    "dir": dir.to_string_lossy(),
                    "db": db.map(|d| d.to_string_lossy().to_string()),
                }),
                Duration::from_millis(JS_LOAD_TIMEOUT_MS),
            )
            .await;
        let resp = match resp {
            Ok(r) => r,
            Err(e) => {
                conn.kill().await;
                return Err(e);
            }
        };
        let result = match stdio_rpc::unwrap_response(&resp, "js-plugins load") {
            Ok(r) => r,
            Err(e) => {
                conn.kill().await;
                return Err(e);
            }
        };
        let (tools, errors) = parse_plugin_list(&result);
        Ok(JsHost {
            conn,
            tools,
            errors,
            dir: dir.to_string_lossy().to_string(),
        })
    }

    /// Перечитать каталог (горячая перезагрузка без перезапуска процесса).
    pub async fn reload(&mut self, dir: &Path, db: Option<&Path>) -> Result<(), String> {
        let resp = self
            .conn
            .request(
                "load",
                json!({
                    "dir": dir.to_string_lossy(),
                    "db": db.map(|d| d.to_string_lossy().to_string()),
                }),
                Duration::from_millis(JS_LOAD_TIMEOUT_MS),
            )
            .await?;
        let result = stdio_rpc::unwrap_response(&resp, "js-plugins load")?;
        let (tools, errors) = parse_plugin_list(&result);
        self.tools = tools;
        self.errors = errors;
        self.dir = dir.to_string_lossy().to_string();
        Ok(())
    }

    /// Вызов плагина. vm-дедлайн исполняет сервер; rpc-таймаут — страховка
    /// поверх (если sidecar сам завис, ход не висит вместе с ним).
    pub async fn call_plugin(&self, short: &str, args: Value) -> Result<(bool, String), String> {
        let vm_ms = call_timeout_ms();
        let resp = self
            .conn
            .request(
                "call",
                json!({"name": short, "arguments": args, "timeout_ms": vm_ms}),
                Duration::from_millis(vm_ms + RPC_SLACK_MS),
            )
            .await?;
        let result = stdio_rpc::unwrap_response(&resp, &format!("js:{short}"))?;
        let ok = result.get("ok").and_then(|v| v.as_bool()).unwrap_or(false);
        let output = result
            .get("output")
            .and_then(|v| v.as_str())
            .unwrap_or_default()
            .to_string();
        Ok((ok, output))
    }

    pub async fn kill(&self) {
        let _ = self
            .conn
            .request("shutdown", json!({}), Duration::from_millis(2_000))
            .await;
        self.conn.kill().await;
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parse_plugin_list_maps_tools_and_errors() {
        let resp = json!({
            "plugins": [
                {"name": "shout", "description": "кричит",
                 "parameters": {"type": "object", "properties": {"text": {"type": "string"}}},
                 "file": "shout.js"},
                {"name": "bare", "file": "bare.js"}
            ],
            "errors": [{"file": "broken.js", "error": "синтаксис"}],
            "dir": "plugins"
        });
        let (tools, errors) = parse_plugin_list(&resp);
        assert_eq!(tools.len(), 2);
        assert_eq!(tools[0].name, "js:shout");
        assert_eq!(tools[0].short, "shout");
        assert_eq!(tools[0].file, "shout.js");
        assert_eq!(tools[0].parameters["type"], "object");
        assert_eq!(
            tools[1].parameters,
            json!({"type": "object"}),
            "без схемы — заглушка"
        );
        assert_eq!(errors, vec!["broken.js: синтаксис".to_string()]);
    }

    #[test]
    fn parse_plugin_list_survives_garbage() {
        let (tools, errors) = parse_plugin_list(&json!({}));
        assert!(tools.is_empty() && errors.is_empty());
        let (tools, _) = parse_plugin_list(&json!({"plugins": "не массив"}));
        assert!(tools.is_empty());
    }

    #[test]
    fn plugins_dir_honors_env_override() {
        let dir = std::env::temp_dir().join(format!("swagcod-jsplug-dir-{}", crate::short_id()));
        std::fs::create_dir_all(&dir).unwrap();
        // Последовательный тест: env-переменная глобальна.
        std::env::set_var("SWAGCOD_PLUGINS_DIR", &dir);
        assert_eq!(plugins_dir(), dir);
        std::env::remove_var("SWAGCOD_PLUGINS_DIR");
        let _ = std::fs::remove_dir_all(&dir);
    }

    const SHOUT_JS: &str = r#"
swagcod.define({
  name: 'shout',
  description: 'кричит текст и показывает pref',
  parameters: { type: 'object', properties: { text: { type: 'string' } }, required: ['text'] },
  handler: (args, api) => String(args.text || '').toUpperCase() + ':' + api.prefs.get('js_pref_probe'),
});
"#;

    const BROKEN_JS: &str = "это не js (((\n";

    const HANG_JS: &str = r#"
swagcod.define({
  name: 'hang',
  description: 'вечный цикл',
  parameters: { type: 'object' },
  handler: () => { while (true) {} },
});
"#;

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn js_host_loads_calls_prefs_and_enforces_deadline() {
        let Some(node) = crate::mcp::tests_node() else {
            return;
        };
        if swagcod_core::node_store::sidecar_file("plugin-server.js").is_none() {
            return; // нет sidecar-расклада — честно пропускаем
        }
        std::env::set_var("SWAGCOD_NODE", &node);

        let dir = std::env::temp_dir().join(format!("swagcod-jsplug-{}", crate::short_id()));
        std::fs::create_dir_all(&dir).unwrap();
        std::fs::write(dir.join("shout.js"), SHOUT_JS).unwrap();
        std::fs::write(dir.join("broken.js"), BROKEN_JS).unwrap();
        std::fs::write(dir.join("hang.js"), HANG_JS).unwrap();

        /* База с prefs для моста: файл готовим встроенным стором
        (sqlite-режим, без sidecar-гонки в тесте). */
        let db = dir.join("prefs.db");
        std::env::set_var("SWAGCOD_STORE", "sqlite");
        {
            let store = swagcod_core::store::open(&db).expect("тестовая база");
            store.set_pref("js_pref_probe", "42").unwrap();
        }

        let mut host = JsHost::start(&dir, Some(&db))
            .await
            .expect("sidecar поднят");
        // shout + hang загружены, broken — честная ошибка, список жив.
        assert_eq!(host.tools.len(), 2, "{:?}", host.errors);
        assert!(host.tools.iter().any(|t| t.short == "shout"));
        assert!(host.tools.iter().any(|t| t.short == "hang"));
        assert_eq!(host.errors.len(), 1);
        assert!(host.errors[0].contains("broken.js"), "{:?}", host.errors);

        // Вызов: текст + prefs-мост (read-only).
        let (ok, out) = host
            .call_plugin("shout", json!({"text": "привет"}))
            .await
            .expect("вызов shout");
        assert!(ok, "{out}");
        assert_eq!(out, "ПРИВЕТ:42");

        // Дедлайн: вечный цикл убит vm, ход не завис.
        std::env::set_var("SWAGCOD_JS_TIMEOUT_MS", "500");
        let (ok, out) = host
            .call_plugin("hang", json!({}))
            .await
            .expect("вызов hang");
        assert!(!ok);
        assert!(out.contains("дедлайн"), "{out}");
        std::env::remove_var("SWAGCOD_JS_TIMEOUT_MS");

        // Незнакомый плагин — честная ошибка.
        let (ok, out) = host
            .call_plugin("ghost", json!({}))
            .await
            .expect("вызов ghost");
        assert!(!ok);
        assert!(out.contains("нет плагина"), "{out}");

        // Горячая перезагрузка: удалили shout.js — списка нет.
        std::fs::remove_file(dir.join("shout.js")).unwrap();
        host.reload(&dir, Some(&db)).await.expect("reload");
        assert_eq!(host.tools.len(), 1);
        assert_eq!(host.tools[0].short, "hang");

        host.kill().await;
        std::env::remove_var("SWAGCOD_STORE");
        std::env::remove_var("SWAGCOD_NODE");
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn missing_plugins_dir_is_honest_not_fatal() {
        let Some(node) = crate::mcp::tests_node() else {
            return;
        };
        if swagcod_core::node_store::sidecar_file("plugin-server.js").is_none() {
            return;
        }
        std::env::set_var("SWAGCOD_NODE", &node);
        let dir = std::env::temp_dir().join(format!("swagcod-jsplug-none-{}", crate::short_id()));
        // Каталог НЕ создаём: загрузка обязана вернуть честную ошибку в
        // errors, а не уронить хост.
        let host = JsHost::start(&dir, None)
            .await
            .expect("хост поднят даже без каталога");
        assert!(host.tools.is_empty());
        assert!(
            host.errors.iter().any(|e| e.contains("не читается")),
            "{:?}",
            host.errors
        );
        host.kill().await;
        std::env::remove_var("SWAGCOD_NODE");
    }
}

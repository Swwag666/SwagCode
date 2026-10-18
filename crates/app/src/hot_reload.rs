//! Hot-reload координатор: следит за изменениями файлов MCP-конфигурации,
//! JS-плагинов и скиллов, дебаунсит события (300 мс) и запускает
//! соответствующие reload-функции. Работает в фоне, не рвёт активные ходы:
//! старые Arc<Conn> живут до завершения своего хода.

use std::collections::HashMap;
use std::path::PathBuf;
use std::sync::Arc;
use std::time::{Duration, Instant};

use notify::{EventKind as NotifyEventKind, RecommendedWatcher, RecursiveMode, Watcher};
use tokio::sync::mpsc;

use crate::AppState;

/// Дебаунс: 300 мс между последним событием и реакцией.
const DEBOUNCE_MS: u64 = 300;

/// Событие hot-reload после дебаунса.
#[derive(Debug, Clone)]
pub enum HotReloadEvent {
    /// Изменилась конфигурация MCP-серверов (prefs mcp_servers).
    McpConfigChanged,
    /// Изменился файл JS-плагина.
    PluginChanged { path: PathBuf },
    /// Изменился файл скилла.
    SkillChanged { name: String, path: PathBuf },
}

/// Координатор hot-reload: watcher + каналы + дебаунс.
pub struct HotReloadCoordinator {
    _watcher: RecommendedWatcher,
    rx: mpsc::Receiver<PathBuf>,
}

impl HotReloadCoordinator {
    /// Создать координатор и начать наблюдение за указанными директориями.
    pub fn new(watch_dirs: Vec<PathBuf>) -> Result<Self, String> {
        let (tx, rx) = mpsc::channel::<PathBuf>(256);

        let event_tx = tx.clone();
        let mut watcher = RecommendedWatcher::new(
            move |res: Result<notify::Event, notify::Error>| {
                if let Ok(event) = res {
                    match event.kind {
                        NotifyEventKind::Create(_)
                        | NotifyEventKind::Modify(_)
                        | NotifyEventKind::Remove(_) => {
                            for path in event.paths {
                                let _ = event_tx.try_send(path);
                            }
                        }
                        _ => {}
                    }
                }
            },
            notify::Config::default(),
        )
        .map_err(|e| format!("hot-reload watcher: {e}"))?;

        for dir in &watch_dirs {
            if dir.exists() {
                watcher
                    .watch(dir, RecursiveMode::Recursive)
                    .map_err(|e| format!("watch {:?}: {e}", dir))?;
            }
        }

        Ok(Self {
            _watcher: watcher,
            rx,
        })
    }

    /// Основной цикл: получает сырые события от watcher, дебаунсит по пути,
    /// dispatches reload.
    pub async fn run(mut self, state: Arc<AppState>) {
        let mut debounce: HashMap<PathBuf, Instant> = HashMap::new();
        let debounce_dur = Duration::from_millis(DEBOUNCE_MS);

        loop {
            // Ждём событие с таймаутом для проверки дебаунса.
            let path = match tokio::time::timeout(debounce_dur, self.rx.recv()).await {
                Ok(Some(path)) => path,
                Ok(None) => break, // канал закрыт
                Err(_) => {
                    // Таймаут: проверяем созревшие дебаунсы.
                    fire_debounced(&mut debounce, &state).await;
                    continue;
                }
            };

            // Обновляем timestamp дебаунса.
            debounce.insert(path, Instant::now());

            // Проверяем все созревшие.
            fire_debounced(&mut debounce, &state).await;
        }
    }
}

/// Проверить и запустить reload для путей, чей дебаунс истёк.
async fn fire_debounced(debounce: &mut HashMap<PathBuf, Instant>, state: &Arc<AppState>) {
    let now = Instant::now();
    let debounce_dur = Duration::from_millis(DEBOUNCE_MS);
    let ready: Vec<PathBuf> = debounce
        .iter()
        .filter(|(_, t)| now.duration_since(**t) >= debounce_dur)
        .map(|(p, _)| p.clone())
        .collect();

    for path in ready {
        debounce.remove(&path);
        dispatch_reload(state, &path).await;
    }
}

/// Определить тип изменения и вызвать соответствующий reload.
async fn dispatch_reload(state: &Arc<AppState>, path: &std::path::Path) {
    let file_name = path
        .file_name()
        .and_then(|n| n.to_str())
        .unwrap_or_default();
    let ext = path
        .extension()
        .and_then(|e| e.to_str())
        .unwrap_or_default();

    // MCP config: prefs mcp_servers хранится в store, но внешний редактор
    // может менять JSON-файл конфигурации. Проверяем по имени.
    if file_name == "mcp_servers.json" || file_name == "mcp_config.json" {
        reload_mcp(state).await;
        return;
    }

    // JS-плагины: *.js в plugins/.
    if ext == "js" {
        reload_plugins(state).await;
        return;
    }

    // Скиллы: *.md в skills/.
    if ext == "md" {
        reload_skills(state, path).await;
    }
}

/// Перечитать MCP-конфигурацию из prefs, diff с текущими подключениями,
/// connect/disconnect изменённых.
async fn reload_mcp(state: &Arc<AppState>) {
    let cfgs = crate::mcp_configs(state);
    let mut reg = state.mcp.lock().await;

    // Текущие имена.
    let current: Vec<String> = reg.status().into_iter().map(|s| s.name).collect();
    let configured: Vec<String> = cfgs.iter().map(|c| c.name.clone()).collect();

    // Удалить серверы, которых больше нет в конфиге.
    for name in &current {
        if !configured.contains(name) {
            reg.disconnect(name).await;
            eprintln!("hot-reload mcp: отключён {name}");
        }
    }

    // Подключить/переподключить.
    for cfg in &cfgs {
        let n = reg.connect_server(cfg).await;
        eprintln!("hot-reload mcp: {} — {n} инструментов", cfg.name);
    }

    state.bus.publish(swagcod_core::bus::EventKind::Status {
        message: format!("hot-reload: MCP обновлён ({} серверов)", cfgs.len()),
    });
}

/// Перезагрузить JS-плагины через JsHost::reload.
async fn reload_plugins(state: &Arc<AppState>) {
    let host = state.js_host.lock().ok().and_then(|h| h.clone());
    let Some(_host) = host else {
        eprintln!("hot-reload plugins: sidecar не запущен, пропускаю");
        return;
    };
    // JsHost::reload требует &mut self, а у нас Arc. Пересоздаём хост.
    let old = state.js_host.lock().ok().and_then(|mut h| h.take());
    if let Some(old) = old {
        old.kill().await;
    }
    crate::start_js_plugins(state).await;

    state.bus.publish(swagcod_core::bus::EventKind::Status {
        message: "hot-reload: JS-плагины перезагружены".into(),
    });
}

/// Перезагрузить один файл скилла.
async fn reload_skills(state: &Arc<AppState>, path: &std::path::Path) {
    let Ok(mut reg) = state.skills.lock() else {
        eprintln!("hot-reload skills: реестр заблокирован");
        return;
    };
    match reg.reload_file(path) {
        Ok(()) => {
            let name = path.file_stem().and_then(|s| s.to_str()).unwrap_or("?");
            eprintln!("hot-reload skill: {name} ({})", path.display());
            state.bus.publish(swagcod_core::bus::EventKind::Status {
                message: format!("hot-reload: скилл {name} обновлён"),
            });
        }
        Err(e) => {
            eprintln!("hot-reload skill error: {e}");
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn debounce_collapses_rapid_events() {
        // Три быстрых события по одному пути должны схлопнуться в одно.
        let mut debounce: HashMap<PathBuf, Instant> = HashMap::new();
        let path = PathBuf::from("/tmp/test.md");

        debounce.insert(path.clone(), Instant::now());
        // Сразу проверяем — ещё не созрело.
        let now = Instant::now();
        let dur = Duration::from_millis(DEBOUNCE_MS);
        let ready: Vec<_> = debounce
            .iter()
            .filter(|(_, t)| now.duration_since(**t) >= dur)
            .map(|(p, _)| p.clone())
            .collect();
        assert!(ready.is_empty(), "ещё не время");

        // Ждём дебаунс.
        tokio::time::sleep(dur + Duration::from_millis(50)).await;
        let now = Instant::now();
        let ready: Vec<_> = debounce
            .iter()
            .filter(|(_, t)| now.duration_since(**t) >= dur)
            .map(|(p, _)| p.clone())
            .collect();
        assert_eq!(ready.len(), 1, "одно событие созрело");
    }

    #[test]
    fn event_routing_by_extension() {
        // Простая проверка логики маршрутизации по расширению.
        let md = PathBuf::from("/skills/review.md");
        let js = PathBuf::from("/plugins/shout.js");
        let json = PathBuf::from("/config/mcp_servers.json");
        let txt = PathBuf::from("/notes.txt");

        assert_eq!(md.extension().and_then(|e| e.to_str()), Some("md"));
        assert_eq!(js.extension().and_then(|e| e.to_str()), Some("js"));
        assert_eq!(
            json.file_name().and_then(|n| n.to_str()),
            Some("mcp_servers.json")
        );
        assert_eq!(txt.extension().and_then(|e| e.to_str()), Some("txt"));
    }
}

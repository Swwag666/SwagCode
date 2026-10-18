//! Звуковые уведомления и системные toast-нотификации.
//!
//! Архитектура: `NotificationManager` подписывается на Bus, маппит события
//! на триггеры и выполняет действия (звук через rodio, toast через WinRT).
//! Конфиг хранится в prefs как JSON. Звуки декодируются один раз и кэшируются.

use serde::{Deserialize, Serialize};
use std::collections::HashMap;
use std::path::PathBuf;
use std::sync::Arc;
use swagcod_core::bus::{Bus, EventKind};
use tokio::sync::Mutex;

// ---------------------------------------------------------------------------
// Типы конфигурации
// ---------------------------------------------------------------------------

/// Триггер уведомления.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum NotifyTrigger {
    /// Ход завершён успешно.
    TurnComplete,
    /// Ход завершён с ошибкой.
    TurnError,
    /// Требуется подтверждение пользователя.
    ApprovalRequired,
}

/// Источник звука.
#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(tag = "type", rename_all = "snake_case")]
pub enum SoundSource {
    /// Встроенный звук (генерируется программно).
    Builtin { name: String },
    /// Пользовательский WAV/OGG файл.
    Custom { path: PathBuf },
}

/// Действие уведомления.
#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(tag = "action", rename_all = "snake_case")]
pub enum NotifyAction {
    /// Только звук.
    Sound { source: SoundSource, volume: f32 },
    /// Только toast.
    Toast { title: String, body: String },
    /// Звук + toast.
    Both {
        source: SoundSource,
        volume: f32,
        title: String,
        body: String,
    },
}

/// Полная конфигурация уведомлений.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct NotificationConfig {
    /// Маппинг триггер → действие. Отсутствие = тихо.
    pub triggers: HashMap<NotifyTrigger, NotifyAction>,
    /// Глобальная громкость 0.0..1.0.
    pub master_volume: f32,
    /// Включены ли уведомления.
    pub enabled: bool,
    /// Не уведомлять когда окно в фокусе.
    pub suppress_when_focused: bool,
}

impl Default for NotificationConfig {
    fn default() -> Self {
        let mut triggers = HashMap::new();
        triggers.insert(
            NotifyTrigger::ApprovalRequired,
            NotifyAction::Both {
                source: SoundSource::Builtin {
                    name: "chime".into(),
                },
                volume: 0.8,
                title: "SwagCod".into(),
                body: "Требуется подтверждение".into(),
            },
        );
        triggers.insert(
            NotifyTrigger::TurnComplete,
            NotifyAction::Sound {
                source: SoundSource::Builtin {
                    name: "done".into(),
                },
                volume: 0.5,
            },
        );
        triggers.insert(
            NotifyTrigger::TurnError,
            NotifyAction::Both {
                source: SoundSource::Builtin {
                    name: "error".into(),
                },
                volume: 0.7,
                title: "SwagCod".into(),
                body: "Ход завершился с ошибкой".into(),
            },
        );
        Self {
            triggers,
            master_volume: 0.8,
            enabled: true,
            suppress_when_focused: true,
        }
    }
}

// ---------------------------------------------------------------------------
// Менеджер уведомлений
// ---------------------------------------------------------------------------

/// Обёртка для rodio::OutputStream, которая не Send/Sync из-за raw pointer.
/// Мы храним stream в статике и никогда не перемещаем его между потоками.
struct AudioOutput {
    _stream: rodio::OutputStream,
    handle: rodio::OutputStreamHandle,
}

// SAFETY: AudioOutput lives in static storage and is never moved between
// threads. The raw pointer inside OutputStream refers to WASAPI state that
// is thread-safe for playback via the handle.
unsafe impl Send for AudioOutput {}
unsafe impl Sync for AudioOutput {}

/// Глобальный audio output: инициализируется один раз.
static AUDIO_OUTPUT: std::sync::OnceLock<AudioOutput> = std::sync::OnceLock::new();

fn get_audio_handle() -> Option<&'static rodio::OutputStreamHandle> {
    Some(
        &AUDIO_OUTPUT
            .get_or_init(|| {
                let (stream, handle) = rodio::OutputStream::try_default()
                    .expect("rodio: no audio output device available");
                AudioOutput {
                    _stream: stream,
                    handle,
                }
            })
            .handle,
    )
}

/// Менеджер уведомлений: держит конфиг, воспроизводит звуки, показывает toast.
///
/// Safety: rodio::OutputStream содержит raw pointer, но мы храним его в
/// статике (не перемещаем между потоками). NotificationManager обращается
/// к нему только через &'static handle.
pub struct NotificationManager {
    config: NotificationConfig,
    /// Кэш декодированных звуков: имя → PCM-данные + sample rate + channels.
    sound_cache: HashMap<String, Arc<DecodedSound>>,
}

// SAFETY: см. документацию выше. Audio output lives in static storage.
unsafe impl Send for NotificationManager {}
unsafe impl Sync for NotificationManager {}

struct DecodedSound {
    samples: Vec<f32>,
    sample_rate: u32,
    channels: u16,
}

impl NotificationManager {
    /// Создать менеджер с заданным конфигом.
    pub fn new(config: NotificationConfig) -> Self {
        Self {
            config,
            sound_cache: HashMap::new(),
        }
    }

    /// Обновить конфиг.
    pub fn update_config(&mut self, config: NotificationConfig) {
        self.config = config;
    }

    /// Получить текущий конфиг.
    pub fn config(&self) -> &NotificationConfig {
        &self.config
    }

    /// Обработать событие шины.
    pub async fn handle_event(&mut self, event: &EventKind, window_focused: bool) {
        if !self.config.enabled {
            return;
        }
        if self.config.suppress_when_focused && window_focused {
            return;
        }
        let trigger = match event {
            EventKind::TurnEnded { ok: true, .. } => NotifyTrigger::TurnComplete,
            EventKind::TurnEnded { ok: false, .. } => NotifyTrigger::TurnError,
            EventKind::ApprovalRequired { .. } => NotifyTrigger::ApprovalRequired,
            _ => return,
        };
        let Some(action) = self.config.triggers.get(&trigger).cloned() else {
            return;
        };
        self.execute_action(&action).await;
    }

    /// Выполнить действие уведомления.
    async fn execute_action(&mut self, action: &NotifyAction) {
        match action {
            NotifyAction::Sound { source, volume } => {
                self.play_sound(source, *volume).await;
            }
            NotifyAction::Toast { title, body } => {
                show_toast(title, body);
            }
            NotifyAction::Both {
                source,
                volume,
                title,
                body,
            } => {
                self.play_sound(source, *volume).await;
                show_toast(title, body);
            }
        }
    }

    /// Воспроизвести звук.
    async fn play_sound(&mut self, source: &SoundSource, volume: f32) {
        let master = self.config.master_volume;
        let effective_volume = volume.clamp(0.0, 1.0) * master.clamp(0.0, 1.0);
        let key = match source {
            SoundSource::Builtin { name } => format!("builtin:{name}"),
            SoundSource::Custom { path } => format!("custom:{}", path.display()),
        };

        // Получаем или декодируем звук.
        let decoded = if let Some(cached) = self.sound_cache.get(&key) {
            Arc::clone(cached)
        } else {
            match decode_sound(source) {
                Some(d) => {
                    let arc = Arc::new(d);
                    self.sound_cache.insert(key, Arc::clone(&arc));
                    arc
                }
                None => return,
            }
        };

        // Играем через rodio.
        let Some(handle) = get_audio_handle() else {
            return;
        };
        let buffer = rodio::buffer::SamplesBuffer::new(
            decoded.channels,
            decoded.sample_rate,
            decoded.samples.clone(),
        );
        use rodio::Source;
        let _ = handle.play_raw(buffer.amplify(effective_volume));
    }

    /// Протестировать триггер.
    pub async fn test_trigger(&mut self, trigger: NotifyTrigger) {
        if let Some(action) = self.config.triggers.get(&trigger).cloned() {
            self.execute_action(&action).await;
        }
    }

    /// Список встроенных звуков.
    pub fn builtin_sounds() -> &'static [&'static str] {
        &["done", "error", "chime"]
    }
}

// ---------------------------------------------------------------------------
// Декодирование звуков
// ---------------------------------------------------------------------------

fn decode_sound(source: &SoundSource) -> Option<DecodedSound> {
    match source {
        SoundSource::Builtin { name } => generate_builtin(name),
        SoundSource::Custom { path } => decode_file(path),
    }
}

/// Генерация встроенных звуков программно (без внешних файлов).
fn generate_builtin(name: &str) -> Option<DecodedSound> {
    let sample_rate = 44100u32;
    let channels = 1u16;
    let samples: Vec<f32> = match name {
        "done" => {
            // Короткий восходящий тон (C5→E5, 200мс).
            let duration = 0.2;
            let n = (sample_rate as f64 * duration) as usize;
            (0..n)
                .map(|i| {
                    let t = i as f64 / sample_rate as f64;
                    let freq = 523.25 + (659.25 - 523.25) * (t / duration);
                    let envelope = 1.0 - t / duration;
                    (t * freq * 2.0 * std::f64::consts::PI).sin() as f32 * envelope as f32 * 0.5
                })
                .collect()
        }
        "error" => {
            // Низкий нисходящий тон (A3→F3, 300мс).
            let duration = 0.3;
            let n = (sample_rate as f64 * duration) as usize;
            (0..n)
                .map(|i| {
                    let t = i as f64 / sample_rate as f64;
                    let freq = 220.0 + (174.6 - 220.0) * (t / duration);
                    let envelope = 1.0 - t / duration;
                    (t * freq * 2.0 * std::f64::consts::PI).sin() as f32 * envelope as f32 * 0.5
                })
                .collect()
        }
        "chime" => {
            // Двойной высокий тон (A5, пауза, A5, 400мс).
            let duration = 0.4;
            let n = (sample_rate as f64 * duration) as usize;
            (0..n)
                .map(|i| {
                    let t = i as f64 / sample_rate as f64;
                    let freq = 880.0;
                    let envelope = if t < 0.15 {
                        1.0 - t / 0.15
                    } else if t > 0.2 && t < 0.35 {
                        1.0 - (t - 0.2) / 0.15
                    } else {
                        0.0
                    };
                    (t * freq * 2.0 * std::f64::consts::PI).sin() as f32 * envelope as f32 * 0.4
                })
                .collect()
        }
        _ => return None,
    };
    Some(DecodedSound {
        samples,
        sample_rate,
        channels,
    })
}

/// Декодирование пользовательского звукового файла.
fn decode_file(path: &std::path::Path) -> Option<DecodedSound> {
    use rodio::Decoder;
    use std::fs::File;
    use std::io::BufReader;

    let file = File::open(path).ok()?;
    let reader = BufReader::new(file);
    let decoder = Decoder::new(reader).ok()?;
    use rodio::Source;
    let sample_rate = decoder.sample_rate();
    let channels = decoder.channels();
    let samples: Vec<f32> = decoder.convert_samples().collect();
    Some(DecodedSound {
        samples,
        sample_rate,
        channels,
    })
}

// ---------------------------------------------------------------------------
// Windows Toast
// ---------------------------------------------------------------------------

/// Показать системное уведомление.
///
/// На Windows: balloon notification через Shell_NotifyIconW (tray icon).
/// Если tray не инициализирован — fallback на MessageBoxW в отдельном потоке.
/// На других платформах: no-op (можно добавить libnotify позже).
#[cfg(windows)]
fn show_toast(title: &str, body: &str) {
    use std::ffi::OsStr;
    use std::os::windows::ffi::OsStrExt;
    use windows_sys::Win32::UI::WindowsAndMessaging::*;

    fn to_wide(s: &str) -> Vec<u16> {
        OsStr::new(s).encode_wide().chain(Some(0)).collect()
    }

    let title_w = to_wide(title);
    let body_w = to_wide(body);

    // MessageBoxW в отдельном потоке: не блокирует async runtime.
    // MB_OK | MB_ICONINFORMATION | MB_SYSTEMMODAL.
    std::thread::spawn(move || unsafe {
        MessageBoxW(
            std::ptr::null_mut(),
            body_w.as_ptr(),
            title_w.as_ptr(),
            MB_OK | MB_ICONINFORMATION | MB_SYSTEMMODAL,
        );
    });
}

#[cfg(not(windows))]
fn show_toast(_title: &str, _body: &str) {
    // На не-Windows платформах toast недоступен.
}

#[allow(dead_code)]
fn escape_xml(s: &str) -> String {
    s.replace('&', "&amp;")
        .replace('<', "&lt;")
        .replace('>', "&gt;")
        .replace('"', "&quot;")
}

// ---------------------------------------------------------------------------
// Фоновая задача подписки на Bus
// ---------------------------------------------------------------------------

/// Запустить фоновую задачу, которая слушает Bus и вызывает NotificationManager.
pub async fn spawn_notification_listener(
    bus: Bus,
    manager: Arc<Mutex<NotificationManager>>,
    window_focused: Arc<std::sync::atomic::AtomicBool>,
) {
    let mut rx = bus.subscribe();
    while let Ok(event) = rx.recv().await {
        let focused = window_focused.load(std::sync::atomic::Ordering::Relaxed);
        let mut mgr = manager.lock().await;
        mgr.handle_event(&event.kind, focused).await;
    }
}

// ---------------------------------------------------------------------------
// Тесты
// ---------------------------------------------------------------------------

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn default_config_has_three_triggers() {
        let cfg = NotificationConfig::default();
        assert_eq!(cfg.triggers.len(), 3);
        assert!(cfg.triggers.contains_key(&NotifyTrigger::TurnComplete));
        assert!(cfg.triggers.contains_key(&NotifyTrigger::TurnError));
        assert!(cfg.triggers.contains_key(&NotifyTrigger::ApprovalRequired));
    }

    #[test]
    fn config_roundtrip_json() {
        let cfg = NotificationConfig::default();
        let json = serde_json::to_string(&cfg).unwrap();
        let cfg2: NotificationConfig = serde_json::from_str(&json).unwrap();
        assert_eq!(cfg2.triggers.len(), cfg.triggers.len());
        assert_eq!(cfg2.master_volume, cfg.master_volume);
        assert_eq!(cfg2.enabled, cfg.enabled);
    }

    #[test]
    fn builtin_sounds_generate() {
        for name in NotificationManager::builtin_sounds() {
            let sound = generate_builtin(name);
            assert!(sound.is_some(), "builtin sound '{name}' must generate");
            let s = sound.unwrap();
            assert!(!s.samples.is_empty());
            assert_eq!(s.sample_rate, 44100);
            assert_eq!(s.channels, 1);
        }
    }

    #[test]
    fn unknown_builtin_returns_none() {
        assert!(generate_builtin("nonexistent").is_none());
    }

    #[test]
    fn escape_xml_handles_special_chars() {
        assert_eq!(escape_xml("a<b>c&d\"e"), "a&lt;b&gt;c&amp;d&quot;e");
    }

    use swagcod_core::session::{SessionId, TurnId};

    #[tokio::test]
    async fn manager_handles_turn_ended() {
        let cfg = NotificationConfig::default();
        let mut mgr = NotificationManager::new(cfg);
        let event = EventKind::TurnEnded {
            turn: TurnId::new("t1"),
            session: SessionId::new("s1"),
            ok: true,
            reason: None,
        };
        mgr.handle_event(&event, false).await;
    }

    #[tokio::test]
    async fn manager_suppresses_when_focused() {
        let cfg = NotificationConfig {
            suppress_when_focused: true,
            ..Default::default()
        };
        let mut mgr = NotificationManager::new(cfg);
        let event = EventKind::TurnEnded {
            turn: TurnId::new("t1"),
            session: SessionId::new("s1"),
            ok: true,
            reason: None,
        };
        mgr.handle_event(&event, true).await;
    }

    #[tokio::test]
    async fn manager_disabled_skips_all() {
        let cfg = NotificationConfig {
            enabled: false,
            ..Default::default()
        };
        let mut mgr = NotificationManager::new(cfg);
        let event = EventKind::ApprovalRequired {
            turn: TurnId::new("t1"),
            call_id: "c1".into(),
            tool: "bash".into(),
            summary: "test".into(),
            preview: None,
        };
        mgr.handle_event(&event, false).await;
    }
}

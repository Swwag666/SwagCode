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
        // Ревизия 85: дефолты — тосты Action Center со живыми текстами
        // (прежде TurnComplete был только звук, тост вылезал модальным
        // окном MessageBox — см. show_toast). Звук тоста отключён
        // системный: rodio-звук действия остаётся единственным звуком.
        triggers.insert(
            NotifyTrigger::ApprovalRequired,
            NotifyAction::Both {
                source: SoundSource::Builtin {
                    name: "chime".into(),
                },
                volume: 0.8,
                title: "SwagCod — вопрос".into(),
                body: "Нейронка ждёт подтверждения".into(),
            },
        );
        triggers.insert(
            NotifyTrigger::TurnComplete,
            NotifyAction::Both {
                source: SoundSource::Builtin {
                    name: "done".into(),
                },
                volume: 0.5,
                title: "SwagCod — готово".into(),
                body: "Нейронка сделала: ход завершён".into(),
            },
        );
        triggers.insert(
            NotifyTrigger::TurnError,
            NotifyAction::Both {
                source: SoundSource::Builtin {
                    name: "error".into(),
                },
                volume: 0.7,
                title: "SwagCod — ошибка".into(),
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

impl NotificationConfig {
    /// Доля громкости в 0.0..1.0 из значения, которое могло прийти процентами.
    /// Значение `1.0` читается как доля (100%), `> 1.0` - как проценты: так
    /// старые записи вида 0.8 и новые вида 80 сходятся к одному смыслу.
    fn normalize_volume(v: f32) -> f32 {
        if !v.is_finite() {
            return 0.8;
        }
        if v <= 1.0 {
            v.clamp(0.0, 1.0)
        } else {
            (v / 100.0).clamp(0.0, 1.0)
        }
    }

    /// rev61: привести конфиг к границам шкалы. Слайдер настроек писал
    /// проценты (0..100) прямо в поле, которое ядро читает как долю, поэтому
    /// в prefs владельца могли лежать значения вида 80 - громкость уходила на
    /// границу clamp, а подпись показывала «1%» вместо «80%». Нормализация
    /// идёт и на загрузке, и на каждой записи: чинит уже испорченный конфиг
    /// без правки руками.
    pub fn normalize(&mut self) {
        self.master_volume = Self::normalize_volume(self.master_volume);
        for action in self.triggers.values_mut() {
            match action {
                NotifyAction::Sound { volume, .. } | NotifyAction::Both { volume, .. } => {
                    *volume = Self::normalize_volume(*volume);
                }
                NotifyAction::Toast { .. } => {}
            }
        }
    }
}

/// Итог попытки проиграть звук.
///
/// rev61: раньше `play_sound` молча возвращался на любой неудаче, поэтому
/// кнопка «Проверить» выглядела одинаково мёртвой и когда звук сыграл, и
/// когда аудиоустройства не было вовсе - отличить одно от другого было нечем.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum PlayOutcome {
    /// Звук ушёл в устройство.
    Played,
    /// Аудиоустройство недоступно (нет WASAPI или устройства вывода).
    NoDevice,
    /// Источник не декодировался (битый файл или неизвестный встроенный звук).
    NoSound,
}

/// Отчёт о тесте триггера - для UI кнопки «Проверить».
#[derive(Debug, Clone, Serialize)]
pub struct NotifyTestReport {
    /// played | no_device | no_sound | toast_only | off
    pub outcome: String,
    /// Имя встроенного звука (для подписи), если источник встроенный.
    pub sound: Option<String>,
    /// Итоговая громкость в процентах, уже с учётом общей громкости.
    pub percent: f32,
    /// Был ли показан toast.
    pub toast: bool,
}

/// Имя встроенного звука источника, если он встроенный.
fn builtin_name(source: &SoundSource) -> Option<String> {
    match source {
        SoundSource::Builtin { name } => Some(name.clone()),
        SoundSource::Custom { path } => path.file_name().map(|n| n.to_string_lossy().to_string()),
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

/// Глобальный audio output: инициализируется один раз. None = аудиоустройства
/// нет вовсе (headless CI, RDP-сессия без звука): звук молча пропускается,
/// toast и остальная логика уведомлений работают как обычно.
static AUDIO_OUTPUT: std::sync::OnceLock<Option<AudioOutput>> = std::sync::OnceLock::new();

fn get_audio_handle() -> Option<&'static rodio::OutputStreamHandle> {
    AUDIO_OUTPUT
        .get_or_init(|| {
            rodio::OutputStream::try_default()
                .ok()
                .map(|(stream, handle)| AudioOutput {
                    _stream: stream,
                    handle,
                })
        })
        .as_ref()
        .map(|output| &output.handle)
}

/// Менеджер уведомлений: держит конфиг, воспроизводит звуки, показывает toast.
///
/// Safety: rodio::OutputStream содержит raw pointer, но мы храним его в
/// статике (не перемещаем между потоками). NotificationManager обращается
/// к нему только через &'static handle.
pub struct NotificationManager {
    config: NotificationConfig,
    /// Ночной прогон: ходовые «готово» глушатся (UX-02-008).
    night_mode: bool,
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
    pub fn new(mut config: NotificationConfig) -> Self {
        config.normalize();
        Self {
            config,
            night_mode: false,
            sound_cache: HashMap::new(),
        }
    }

    /// Обновить конфиг.
    pub fn update_config(&mut self, mut config: NotificationConfig) {
        config.normalize();
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
        /* UX-02-008 (ревизия 86): ночной прогон гоняет десятки раундов —
        тост на каждый TurnComplete превратил бы Action Center в спам.
        Ночью глушится только «готово» (ходовой шум); ошибки и вопросы
        approval остаются: они редкие и их важно видеть. Итоговый отчёт
        шлётся вручную одним тостом в конце прогона. */
        if self.night_mode && matches!(trigger, NotifyTrigger::TurnComplete) {
            return;
        }
        let Some(action) = self.config.triggers.get(&trigger).cloned() else {
            return;
        };
        /* UX-02-003 (ревизия 86): тело тоста несёт живой контекст события
        (причину падения, суть вопроса approval), а не только статичную
        подпись из конфига. Раньше «ход завершился с ошибкой» без причины
        заставляло идти смотреть в окно, хотя причина уже была в событии. */
        let detail = match event {
            EventKind::TurnEnded {
                reason: Some(r), ..
            } => Some(short_detail(r)),
            EventKind::ApprovalRequired { summary, .. } => Some(short_detail(summary)),
            _ => None,
        };
        self.execute_action(&action, detail.as_deref()).await;
    }

    /// Ночной режим (глушит TurnComplete): включается/выключается прогоном.
    pub fn set_night_mode(&mut self, on: bool) {
        self.night_mode = on;
    }

    /// Тост вручную (итоговый отчёт ночного прогона): без триггеров.
    pub async fn notify_manual(&mut self, title: &str, body: &str) -> bool {
        show_toast(title, body)
    }

    /// Выполнить действие уведомления.
    ///
    /// Возвращает итог проигрывания (None - звука в действии не было) и факт
    /// отправки toast в Action Center: это нужно тесту триггера, обычный путь
    /// результат не читает. `detail` (UX-02-003) дописывается в тело тоста
    /// второй строкой — живой контекст события рядом со статичной подписью.
    async fn execute_action(
        &mut self,
        action: &NotifyAction,
        detail: Option<&str>,
    ) -> (Option<PlayOutcome>, bool) {
        match action {
            NotifyAction::Sound { source, volume } => {
                (Some(self.play_sound(source, *volume).await), false)
            }
            NotifyAction::Toast { title, body } => {
                let shown = show_toast(title, &with_detail(body, detail));
                (None, shown)
            }
            NotifyAction::Both {
                source,
                volume,
                title,
                body,
            } => {
                let outcome = self.play_sound(source, *volume).await;
                let shown = show_toast(title, &with_detail(body, detail));
                (Some(outcome), shown)
            }
        }
    }

    /// Воспроизвести звук и честно сказать, что вышло.
    async fn play_sound(&mut self, source: &SoundSource, volume: f32) -> PlayOutcome {
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
                None => return PlayOutcome::NoSound,
            }
        };

        // Играем через rodio.
        let Some(handle) = get_audio_handle() else {
            return PlayOutcome::NoDevice;
        };
        let buffer = rodio::buffer::SamplesBuffer::new(
            decoded.channels,
            decoded.sample_rate,
            decoded.samples.clone(),
        );
        use rodio::Source;
        let _ = handle.play_raw(buffer.amplify(effective_volume));
        PlayOutcome::Played
    }

    /// Протестировать триггер и вернуть отчёт для UI.
    ///
    /// Тест сознательно не смотрит на `enabled` и `suppress_when_focused`:
    /// владелец жмёт кнопку сам, окно в этот момент в фокусе, и глушить
    /// проверку по фокусу значило бы гасить её ровно там, где её ждут.
    pub async fn test_trigger(&mut self, trigger: NotifyTrigger) -> NotifyTestReport {
        let Some(action) = self.config.triggers.get(&trigger).cloned() else {
            return NotifyTestReport {
                outcome: "off".into(),
                sound: None,
                percent: 0.0,
                toast: false,
            };
        };

        let (sound, volume) = match &action {
            NotifyAction::Sound { source, volume } => (builtin_name(source), *volume),
            NotifyAction::Both { source, volume, .. } => (builtin_name(source), *volume),
            NotifyAction::Toast { .. } => (None, 0.0),
        };
        let master = self.config.master_volume.clamp(0.0, 1.0);
        let percent = (volume.clamp(0.0, 1.0) * master * 100.0).round();

        let (play, toast) = self.execute_action(&action, None).await;
        let outcome = match play {
            Some(PlayOutcome::Played) => "played",
            Some(PlayOutcome::NoDevice) => "no_device",
            Some(PlayOutcome::NoSound) => "no_sound",
            None => {
                /* UX-02-010 (ревизия 86): «toast_only» раньше ставился и
                когда тост провалился (headless, нет Action Center): кнопка
                «Проверить» докладывала успех молча провалившемуся тосту.
                Теперь исход честный в обе стороны. */
                if toast {
                    "toast_only"
                } else {
                    "toast_failed"
                }
            }
        };

        NotifyTestReport {
            outcome: outcome.into(),
            sound,
            percent,
            toast,
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
// Windows Toast (Action Center, ревизия 85)
// ---------------------------------------------------------------------------

/// AppUserModelId приложения — под ним тосты живут в Action Center.
/// Регистрируется в HKCU с DisplayName, чтобы центр показывал «SwagCod»
/// вместо голого идентификатора.
#[cfg(windows)]
const AUMID: &str = "SwagCod.App";

/// XML тоста для Action Center: ToastGeneric (крупный заголовок + текст),
/// звук виндов отключён — звуком управляет действие конфига (rodio), тост
/// не должен дублировать его системным диньгом.
#[cfg_attr(not(windows), allow(dead_code))]
fn build_toast_xml(title: &str, body: &str) -> String {
    format!(
        "<toast duration=\"short\"><visual><binding template=\"ToastGeneric\">\
<text>{}</text><text>{}</text>\
</binding></visual><audio silent=\"true\"/></toast>",
        escape_xml(title),
        escape_xml(body)
    )
}

/// Зарегистрировать AUMID в HKCU (один раз за процесс): без записи
/// DisplayName тост на Win10/11 может не показаться или выйти безымянным.
#[cfg(windows)]
fn register_aumid() {
    use std::ffi::OsStr;
    use std::os::windows::ffi::OsStrExt;
    use windows_sys::Win32::System::Registry::{
        RegCloseKey, RegCreateKeyExW, RegSetValueExW, HKEY_CURRENT_USER, KEY_SET_VALUE,
        REG_OPTION_NON_VOLATILE, REG_SZ,
    };

    static ONCE: std::sync::Once = std::sync::Once::new();
    ONCE.call_once(|| unsafe {
        fn to_wide(s: &str) -> Vec<u16> {
            OsStr::new(s).encode_wide().chain(Some(0)).collect()
        }
        let subkey = to_wide("Software\\Classes\\AppUserModelId\\SwagCod.App");
        let mut hkey = std::ptr::null_mut();
        if RegCreateKeyExW(
            HKEY_CURRENT_USER,
            subkey.as_ptr(),
            0,
            std::ptr::null_mut(),
            REG_OPTION_NON_VOLATILE,
            KEY_SET_VALUE,
            std::ptr::null(),
            &mut hkey,
            std::ptr::null_mut(),
        ) != 0
        {
            return; // без регистрации тост всё равно пробуем показать
        }
        let display = to_wide("SwagCod");
        let _ = RegSetValueExW(
            hkey,
            to_wide("DisplayName").as_ptr(),
            0,
            REG_SZ,
            display.as_ptr().cast(),
            (display.len() * 2) as u32,
        );
        RegCloseKey(hkey);
    });
}

/// Показать системное уведомление в Action Center (тост сбоку экрана).
///
/// Ревизия 85: прежний путь — MessageBoxW с MB_SYSTEMMODAL — вылезал
/// модальным окном поверх всего и блокировал внимание; теперь нативный
/// WinRT-тост: тихий, сворачивается в центр, не крадёт фокус.
/// Ревизия 86: возвращает факт отправки в Action Center — тест триггера
/// обязан отличать «показан» от «провалился headless-сессией».
#[cfg(windows)]
#[cfg(windows)]
struct ToastWorker {
    tx: std::sync::mpsc::Sender<ToastRequest>,
}

#[cfg(windows)]
struct ToastRequest {
    xml: String,
    reply: std::sync::mpsc::Sender<bool>,
}

/// Показать системное уведомление в Action Center (тост сбоку экрана).
///
/// Ревизия 85: прежний путь — MessageBoxW с MB_SYSTEMMODAL — вылезал
/// модальным окном поверх всего и блокировал внимание; теперь нативный
/// WinRT-тост: тихий, сворачивается в центр, не крадёт фокус.
/// Ревизия 86: тосты обслуживает ОДИН стационарный MTA-поток на процесс.
/// Прежний «поток на каждый тост» на CI-раннере падал AV при повторной
/// активации XmlDocument после инициализации аудио-стека в основном
/// потоке (cpal CoInitializeEx меняет апартаменты COM процесса): второй
/// RoGetActivationFactory в свежем потоке уходил в 0xC0000005.
/// Стационарный поток инициализирует WinRT ровно один раз и живёт до
/// конца процесса — штатный паттерн для WinRT-фоновых уведомлений.
#[cfg(windows)]
fn show_toast(title: &str, body: &str) -> bool {
    use std::sync::mpsc;
    let xml = build_toast_xml(title, body);
    let worker = toast_worker();
    let (reply_tx, reply_rx) = mpsc::channel::<bool>();
    if worker
        .tx
        .send(ToastRequest {
            xml,
            reply: reply_tx,
        })
        .is_err()
    {
        return false;
    }
    // 400 мс: тост-вызов быстрый, но живёт в стационарном потоке WinRT.
    matches!(
        reply_rx.recv_timeout(std::time::Duration::from_millis(400)),
        Ok(true)
    )
}

/// Стационарный тост-поток: единожды RoInitialize(MTA), дальше цикл
/// запросов. Паника внутри потока не уносит процесс: канал закрывается,
/// следующие show_toast честно возвращают false.
#[cfg(windows)]
fn toast_worker() -> &'static ToastWorker {
    static WORKER: std::sync::OnceLock<ToastWorker> = std::sync::OnceLock::new();
    WORKER.get_or_init(|| {
        use std::sync::mpsc;
        let (tx, rx) = mpsc::channel::<ToastRequest>();
        std::thread::Builder::new()
            .name("swagcod-toast".into())
            .spawn(move || unsafe {
                // WinRT из этого потока: тост можно отправлять из MTA.
                // Один раз на процесс — см. доку show_toast.
                let _ = windows::Win32::System::WinRT::RoInitialize(
                    windows::Win32::System::WinRT::RO_INIT_MULTITHREADED,
                );
                register_aumid();
                use windows::core::HSTRING;
                use windows::Data::Xml::Dom::XmlDocument;
                use windows::UI::Notifications::{ToastNotification, ToastNotificationManager};
                for req in rx {
                    let shown = (|| -> bool {
                        let doc = match XmlDocument::new().and_then(|d| {
                            d.LoadXml(&HSTRING::from(&req.xml))?;
                            Ok(d)
                        }) {
                            Ok(d) => d,
                            Err(e) => {
                                eprintln!("swagcod: toast XML не собрался: {e}");
                                return false;
                            }
                        };
                        let toast = match ToastNotification::CreateToastNotification(&doc) {
                            Ok(t) => t,
                            Err(e) => {
                                eprintln!("swagcod: тост не создан: {e}");
                                return false;
                            }
                        };
                        let notifier = match ToastNotificationManager::CreateToastNotifierWithId(
                            &HSTRING::from(AUMID),
                        ) {
                            Ok(n) => n,
                            Err(e) => {
                                eprintln!("swagcod: notifer недоступен (headless?): {e}");
                                return false;
                            }
                        };
                        match notifier.Show(&toast) {
                            Ok(()) => true,
                            Err(e) => {
                                eprintln!("swagcod: тост не показан: {e}");
                                false
                            }
                        }
                    })();
                    let _ = req.reply.send(shown);
                }
            })
            .expect("тост-поток не создан");
        ToastWorker { tx }
    })
}

#[cfg(not(windows))]
fn show_toast(_title: &str, _body: &str) -> bool {
    // На не-Windows платформах тост недоступен.
    false
}

/* UX-02-003 (ревизия 86): контекст события в теле тоста — одна строка,
переводы строк схлопнуты, обрезка подписана. */
fn short_detail(s: &str) -> String {
    let flat: String = s
        .chars()
        .map(|c| if c == '\n' || c == '\r' { ' ' } else { c })
        .collect();
    let flat = flat.trim();
    let mut out: String = flat.chars().take(140).collect();
    let total = flat.chars().count();
    if total > 140 {
        out.push_str(&format!(" … [+{} симв.]", total - 140));
    }
    out
}

fn with_detail(body: &str, detail: Option<&str>) -> String {
    match detail {
        Some(d) if !d.trim().is_empty() => format!("{body}\n{d}"),
        _ => body.to_string(),
    }
}

#[cfg_attr(windows, allow(dead_code))]
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

    #[test]
    #[cfg(windows)]
    fn toast_xml_is_valid_and_silent() {
        let xml = build_toast_xml("SwagCod — готово", "Нейронка сделала: ход <завершён>");
        assert!(xml.contains("ToastGeneric"), "шаблон ToastGeneric: {xml}");
        assert!(
            xml.contains("<audio silent=\"true\"/>"),
            "звук тоста отключён — звуком управляет rodio: {xml}"
        );
        assert!(xml.contains("&lt;завершён&gt;"), "экранирование XML: {xml}");
    }

    #[test]
    #[cfg(windows)]
    fn default_triggers_are_toasts_with_live_texts() {
        // Ревизия 85: все три триггера — Both (звук + тост Action Center),
        // тексты живые, а не «SwagCod / Требуется подтверждение».
        let cfg = NotificationConfig::default();
        for (trigger, want) in [
            (NotifyTrigger::TurnComplete, "Нейронка сделала"),
            (NotifyTrigger::TurnError, "Ход завершился с ошибкой"),
            (
                NotifyTrigger::ApprovalRequired,
                "Нейронка ждёт подтверждения",
            ),
        ] {
            match cfg.triggers.get(&trigger) {
                Some(NotifyAction::Both { body, .. }) => {
                    assert!(body.contains(want), "{trigger:?}: body={body}");
                }
                other => panic!("{trigger:?} должен быть Both (звук+тост), а не {other:?}"),
            }
        }
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
            cost_usd: None,
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
            cost_usd: None,
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

    /* ── rev61: шкала громкости и честный отчёт теста ── */

    #[test]
    fn percent_volume_is_read_as_percent() {
        let mut cfg = NotificationConfig {
            master_volume: 80.0,
            ..Default::default()
        };
        cfg.triggers.insert(
            NotifyTrigger::TurnComplete,
            NotifyAction::Sound {
                source: SoundSource::Builtin {
                    name: "done".into(),
                },
                volume: 50.0,
            },
        );
        cfg.normalize();
        assert_eq!(
            cfg.master_volume, 0.8,
            "80 читается как 80%, а не как 80-кратная доля"
        );
        match cfg.triggers.get(&NotifyTrigger::TurnComplete).unwrap() {
            NotifyAction::Sound { volume, .. } => assert_eq!(*volume, 0.5),
            other => panic!("ожидался Sound, получено {other:?}"),
        }
    }

    #[test]
    fn fraction_volume_stays_fraction() {
        let mut cfg = NotificationConfig {
            master_volume: 1.0,
            ..Default::default()
        };
        cfg.normalize();
        assert_eq!(cfg.master_volume, 1.0, "ровно 1.0 - это доля, то есть 100%");
    }

    #[test]
    fn normalize_clamps_and_survives_nan() {
        let mut cfg = NotificationConfig {
            master_volume: 250.0,
            ..Default::default()
        };
        cfg.normalize();
        assert_eq!(cfg.master_volume, 1.0, "250% упирается в потолок доли");

        let mut nan = NotificationConfig {
            master_volume: f32::NAN,
            ..Default::default()
        };
        nan.normalize();
        assert!(
            nan.master_volume.is_finite(),
            "NaN не должен доезжать до clamp"
        );
    }

    #[test]
    fn normalize_keeps_toast_actions_untouched() {
        let mut cfg = NotificationConfig::default();
        cfg.triggers.insert(
            NotifyTrigger::TurnComplete,
            NotifyAction::Toast {
                title: "t".into(),
                body: "b".into(),
            },
        );
        cfg.normalize();
        assert!(matches!(
            cfg.triggers.get(&NotifyTrigger::TurnComplete),
            Some(NotifyAction::Toast { .. })
        ));
    }

    #[test]
    fn manager_normalizes_on_construction() {
        let cfg = NotificationConfig {
            master_volume: 80.0,
            ..Default::default()
        };
        let mgr = NotificationManager::new(cfg);
        assert_eq!(mgr.config().master_volume, 0.8);
    }

    #[tokio::test]
    async fn test_trigger_reports_sound_and_effective_percent() {
        /* Громкость отчёта не зависит от машины: она посчитана из конфига,
        а не измерена после проигрывания. Ревизия 85: TurnComplete — Both
        (звук + тост Action Center), дефолт вырос из «только звук». */
        let cfg = NotificationConfig {
            master_volume: 0.8,
            ..Default::default()
        };
        let mut mgr = NotificationManager::new(cfg);
        let report = mgr.test_trigger(NotifyTrigger::TurnComplete).await;
        assert_eq!(report.sound.as_deref(), Some("done"));
        assert_eq!(report.percent, 40.0, "0.5 * 0.8 = 40%");
        assert!(report.toast, "ревизия 85: TurnComplete — звук + тост");
    }

    #[tokio::test]
    async fn test_trigger_without_action_reports_off() {
        let mut cfg = NotificationConfig::default();
        cfg.triggers.clear();
        let mut mgr = NotificationManager::new(cfg);
        let report = mgr.test_trigger(NotifyTrigger::TurnComplete).await;
        assert_eq!(report.outcome, "off");
        assert_eq!(report.percent, 0.0);
    }

    #[tokio::test]
    async fn test_trigger_toast_only_reports_toast() {
        let mut cfg = NotificationConfig::default();
        cfg.triggers.insert(
            NotifyTrigger::TurnComplete,
            NotifyAction::Toast {
                title: "t".into(),
                body: "b".into(),
            },
        );
        let mut mgr = NotificationManager::new(cfg);
        let report = mgr.test_trigger(NotifyTrigger::TurnComplete).await;
        assert_eq!(report.outcome, "toast_only");
        assert!(report.toast);
        assert!(report.sound.is_none());
    }

    #[tokio::test]
    async fn test_trigger_unknown_sound_reports_no_sound() {
        /* Проверка детерминирована: битый источник отсекается до обращения к
        аудиоустройству, поэтому исход одинаков и на машине со звуком, и в CI. */
        let mut cfg = NotificationConfig::default();
        cfg.triggers.insert(
            NotifyTrigger::TurnComplete,
            NotifyAction::Sound {
                source: SoundSource::Builtin {
                    name: "nonexistent".into(),
                },
                volume: 0.5,
            },
        );
        let mut mgr = NotificationManager::new(cfg);
        let report = mgr.test_trigger(NotifyTrigger::TurnComplete).await;
        assert_eq!(report.outcome, "no_sound");
    }
}

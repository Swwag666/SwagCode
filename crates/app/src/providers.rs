/*! Провайдеры чата: каталог пресетов, custom-провайдеры, ключи, fetch моделей.

Модель хранения (всё в prefs/SQLite, ключи — только в DPAPI):
- `provider_active` — id активного (`default` = старое поведение .env/DPAPI);
- `providers_custom` — JSON-массив `[{id, label, base_url, flavor}]` (БЕЗ ключей);
- `provider_models` — JSON-карта `{id: [модели]}` (сохранённое юзером);
- `provider_keys_dpapi` — DPAPI-hex от JSON-карты `{id: ключ}`;
- `api_key_dpapi` (legacy) — ключ для `default`, как раньше.

Ключи никогда не уходят во фронт: `providers_state` отдаёт только `has_key`.
`fetch_provider_models` ходит в сеть с переданным ключом и НЕ сохраняет —
сохранение только явной командой `save_provider` после вопроса юзеру.
*/

use std::collections::HashMap;
use std::sync::Arc;

use serde::{Deserialize, Serialize};
use tauri::State;

use swagcod_core::store::Store;
use swagcod_provider::{
    find_preset, AnthropicProvider, AnyProvider, OpenAiProvider, ProviderFlavor, ProviderPreset,
    Router, RouterEndpoint, PRESETS,
};

use super::{dpapi, short_id, AppState};

/// Активный по умолчанию: старое поведение — `SWAGCOD_BASE_URL` (+фолбэки)
/// и ключ из env/DPAPI. Ничего не ломаем тем, кто уже настроился.
pub const DEFAULT_PROVIDER_ID: &str = "default";
/// Дефолтная база, если env пуст (исторический живой эндпоинт).
pub const DEFAULT_BASE_URL: &str = "https://rustvy.xyz/v1";

/// Custom-провайдер юзера. Ключ здесь НЕ хранится — только в DPAPI-карте.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct CustomProvider {
    pub id: String,
    pub label: String,
    pub base_url: String,
    pub flavor: ProviderFlavor,
}

/// Модель из `GET /models` после нормализации (id + человеческое имя).
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ProviderModel {
    pub id: String,
    pub label: String,
}

/// Разрешённый активный провайдер: куда стучаться и чем.
pub struct ResolvedProvider {
    pub id: String,
    pub label: String,
    pub base_url: String,
    pub flavor: ProviderFlavor,
    pub key: String,
}

fn is_local(base: &str) -> bool {
    let lower = base.to_lowercase();
    lower.contains("localhost") || lower.contains("127.0.0.1")
}

fn read_customs(store: &dyn Store) -> Vec<CustomProvider> {
    store
        .get_pref("providers_custom")
        .ok()
        .flatten()
        .and_then(|s| serde_json::from_str(&s).ok())
        .unwrap_or_default()
}

fn write_customs(store: &dyn Store, customs: &[CustomProvider]) -> Result<(), String> {
    let s = serde_json::to_string(customs).map_err(|e| format!("провайдеры: json: {e}"))?;
    store
        .set_pref("providers_custom", &s)
        .map_err(|e| e.to_string())
}

fn read_models_map(store: &dyn Store) -> HashMap<String, Vec<String>> {
    store
        .get_pref("provider_models")
        .ok()
        .flatten()
        .and_then(|s| serde_json::from_str(&s).ok())
        .unwrap_or_default()
}

fn write_models_map(store: &dyn Store, map: &HashMap<String, Vec<String>>) -> Result<(), String> {
    let s = serde_json::to_string(map).map_err(|e| format!("модели: json: {e}"))?;
    store
        .set_pref("provider_models", &s)
        .map_err(|e| e.to_string())
}

/// Карта ключей `{id: ключ}` из DPAPI. Любая порча — пустая карта
/// (громко в лог): лучше честный «нет ключа», чем падение настроек.
fn read_keys_map(store: &dyn Store) -> HashMap<String, String> {
    let blob: String = match store.get_pref("provider_keys_dpapi").ok().flatten() {
        Some(b) if !b.trim().is_empty() => b,
        _ => return HashMap::new(),
    };
    let json = match dpapi::unprotect_hex(&blob) {
        Ok(j) => j,
        Err(e) => {
            eprintln!("providers: DPAPI-карта ключей не расшифровалась: {e}");
            return HashMap::new();
        }
    };
    serde_json::from_str(&json).unwrap_or_default()
}

fn write_keys_map(store: &dyn Store, map: &HashMap<String, String>) -> Result<(), String> {
    let json = serde_json::to_string(map).map_err(|e| format!("ключи: json: {e}"))?;
    let blob = dpapi::protect_hex(&json)?;
    store
        .set_pref("provider_keys_dpapi", &blob)
        .map_err(|e| e.to_string())
}

fn active_id(store: &dyn Store) -> String {
    store
        .get_pref("provider_active")
        .ok()
        .flatten()
        .filter(|s| !s.trim().is_empty())
        .unwrap_or_else(|| DEFAULT_PROVIDER_ID.to_string())
}

/// Разрешить ЛЮБОГО провайдера по id (не только активного).
/// Общая логика для `resolve_active` и fetch по сохранённому ключу.
fn resolve_id(
    store: &dyn Store,
    keys: &HashMap<String, String>,
    customs: &[CustomProvider],
    models_map: &HashMap<String, Vec<String>>,
    key_override: Option<String>,
    id: &str,
) -> Result<(ResolvedProvider, Vec<String>), String> {
    // Вне default ключ из карты не светим наружу — только внутрь соединения.
    let stored_key = keys.get(id).cloned().unwrap_or_default();
    if id == DEFAULT_PROVIDER_ID {
        let base = std::env::var("SWAGCOD_BASE_URL")
            .ok()
            .filter(|s| !s.trim().is_empty())
            .unwrap_or_else(|| DEFAULT_BASE_URL.to_string());
        let env_key = std::env::var("SWAGCOD_API_KEY").unwrap_or_default();
        let key = key_override
            .filter(|k| !k.trim().is_empty())
            .or_else(|| {
                if !env_key.trim().is_empty() {
                    Some(env_key)
                } else {
                    None
                }
            })
            .or_else(|| {
                if !stored_key.trim().is_empty() {
                    Some(stored_key)
                } else {
                    None
                }
            })
            .unwrap_or_default();
        if key.trim().is_empty() {
            // Legacy-blob — последний шанс перед «нет ключа».
            let legacy = store
                .get_pref("api_key_dpapi")
                .ok()
                .flatten()
                .filter(|b| !b.trim().is_empty())
                .and_then(|b| dpapi::unprotect_hex(&b).ok())
                .unwrap_or_default();
            let models = models_map.get(id).cloned().unwrap_or_default();
            return Ok((
                ResolvedProvider {
                    id: id.to_string(),
                    label: "По умолчанию (.env / DPAPI)".into(),
                    base_url: base,
                    flavor: ProviderFlavor::OpenAi,
                    key: legacy,
                },
                models,
            ));
        }
        let models = models_map.get(id).cloned().unwrap_or_default();
        return Ok((
            ResolvedProvider {
                id: id.to_string(),
                label: "По умолчанию (.env / DPAPI)".into(),
                base_url: base,
                flavor: ProviderFlavor::OpenAi,
                key,
            },
            models,
        ));
    }
    if let Some(p) = find_preset(id) {
        let key = key_override
            .filter(|k| !k.trim().is_empty())
            .unwrap_or(stored_key);
        let models = models_map
            .get(id)
            .cloned()
            .unwrap_or_else(|| p.default_models.iter().map(|s| s.to_string()).collect());
        return Ok((
            ResolvedProvider {
                id: p.id.to_string(),
                label: p.label.to_string(),
                base_url: p.base_url.to_string(),
                flavor: p.flavor,
                key,
            },
            models,
        ));
    }
    if let Some(c) = customs.iter().find(|c| c.id == id) {
        let key = key_override
            .filter(|k| !k.trim().is_empty())
            .unwrap_or(stored_key);
        let models = models_map.get(id).cloned().unwrap_or_default();
        return Ok((
            ResolvedProvider {
                id: c.id.clone(),
                label: c.label.clone(),
                base_url: c.base_url.clone(),
                flavor: c.flavor,
                key,
            },
            models,
        ));
    }
    Err(format!(
        "неизвестный провайдер «{id}» — выберите из каталога"
    ))
}

/// Разрешить активного провайдера + сохранённые модели.
/// Возвращает (конфиг, модели): модели — сохранённые юзером, иначе дефолты пресета.
pub fn resolve_active(state: &AppState) -> Result<(ResolvedProvider, Vec<String>), String> {
    let store = state.store.lock().map_err(|e| e.to_string())?;
    let store: &dyn Store = &**store;
    let id = active_id(store);
    let customs = read_customs(store);
    let models_map = read_models_map(store);
    let keys = read_keys_map(store);
    resolve_id(store, &keys, &customs, &models_map, None, &id)
}

/// Собрать чат-провайдер для хода. `default` идёт старым путём
/// (env + SWAGCOD_FALLBACKS); остальные — одиночной точкой.
pub fn build_chat_provider(state: &AppState) -> Result<AnyProvider, String> {
    let (cfg, _) = resolve_active(state)?;
    if cfg.id == DEFAULT_PROVIDER_ID {
        let router = Router::from_env_with_key(super::dpapi_key(state))
            .map_err(|e| format!("провайдер: {e}"))?;
        return Ok(AnyProvider::OpenAi(router));
    }
    if cfg.base_url.trim().is_empty() {
        return Err(format!("{}: пустой endpoint", cfg.label));
    }
    match cfg.flavor {
        ProviderFlavor::OpenAi => {
            let p = if cfg.key.trim().is_empty() {
                if is_local(&cfg.base_url) {
                    OpenAiProvider::new_open(&cfg.base_url)
                } else {
                    return Err(format!(
                        "{}: нет ключа — откройте вкладку провайдеров и добавьте ключ",
                        cfg.label
                    ));
                }
            } else {
                OpenAiProvider::new(&cfg.base_url, &cfg.key)
            }
            .map_err(|e| format!("провайдер: {e}"))?;
            let router = Router::new(vec![RouterEndpoint {
                provider: p,
                model: None,
            }])
            .map_err(|e| e.to_string())?;
            Ok(AnyProvider::OpenAi(router))
        }
        ProviderFlavor::Anthropic => {
            let p = if cfg.key.trim().is_empty() {
                if is_local(&cfg.base_url) {
                    AnthropicProvider::new_open(&cfg.base_url)
                } else {
                    return Err(format!(
                        "{}: нет ключа — откройте вкладку провайдеров и добавьте ключ",
                        cfg.label
                    ));
                }
            } else {
                AnthropicProvider::new(&cfg.base_url, &cfg.key)
            }
            .map_err(|e| format!("провайдер: {e}"))?;
            Ok(AnyProvider::Anthropic(p))
        }
    }
}

/// Каталог пресетов для UI.
#[tauri::command]
pub fn list_provider_presets() -> Vec<ProviderPreset> {
    PRESETS.to_vec()
}

/// Всё состояние вкладки провайдеров. Ключей здесь нет — только `has_key`.
#[tauri::command]
pub fn providers_state(state: State<'_, Arc<AppState>>) -> Result<serde_json::Value, String> {
    let store = state.store.lock().map_err(|e| e.to_string())?;
    let store: &dyn Store = &**store;
    let id = active_id(store);
    let customs = read_customs(store);
    let models_map = read_models_map(store);
    let keys = read_keys_map(store);
    let env_key = std::env::var("SWAGCOD_API_KEY").unwrap_or_default();
    let legacy_blob = store
        .get_pref("api_key_dpapi")
        .ok()
        .flatten()
        .unwrap_or_default();

    let mut providers = Vec::new();
    // default первым — текущее поведение видно сразу.
    let env_base = std::env::var("SWAGCOD_BASE_URL")
        .ok()
        .filter(|s| !s.trim().is_empty())
        .unwrap_or_else(|| DEFAULT_BASE_URL.to_string());
    providers.push(serde_json::json!({
        "id": DEFAULT_PROVIDER_ID,
        "label": "По умолчанию (.env / DPAPI)",
        "base_url": env_base,
        "flavor": ProviderFlavor::OpenAi.as_str(),
        "built_in": true,
        "needs_key": true,
        "has_key": !env_key.trim().is_empty() || keys.contains_key(DEFAULT_PROVIDER_ID) || !legacy_blob.trim().is_empty(),
        "key_url": "",
        "models": models_map.get(DEFAULT_PROVIDER_ID).cloned().unwrap_or_default(),
    }));
    for p in PRESETS {
        providers.push(serde_json::json!({
            "id": p.id,
            "label": p.label,
            "base_url": p.base_url,
            "flavor": p.flavor.as_str(),
            "built_in": true,
            "needs_key": p.needs_key,
            "has_key": keys.contains_key(p.id),
            "key_url": p.key_url,
            "models": models_map.get(p.id).cloned().unwrap_or_else(|| p.default_models.iter().map(|s| s.to_string()).collect::<Vec<_>>()),
        }));
    }
    for c in &customs {
        providers.push(serde_json::json!({
            "id": c.id,
            "label": c.label,
            "base_url": c.base_url,
            "flavor": c.flavor.as_str(),
            "built_in": false,
            "needs_key": !is_local(&c.base_url),
            "has_key": keys.contains_key(&c.id),
            "key_url": "",
            "models": models_map.get(&c.id).cloned().unwrap_or_default(),
        }));
    }
    Ok(serde_json::json!({ "active": id, "providers": providers }))
}

/// Нормализация `GET /models`: OpenAI — `data[].id`, Anthropic —
/// `data[]` с `display_name`. Чистая функция — тестируется.
pub fn normalize_models(flavor: ProviderFlavor, v: &serde_json::Value) -> Vec<ProviderModel> {
    let mut out = Vec::new();
    let arr = v
        .get("data")
        .and_then(|d| d.as_array())
        .cloned()
        .unwrap_or_default();
    for m in arr {
        let id = m.get("id").and_then(|i| i.as_str()).unwrap_or("").trim();
        if id.is_empty() {
            continue;
        }
        let label = match flavor {
            ProviderFlavor::Anthropic => m
                .get("display_name")
                .and_then(|n| n.as_str())
                .filter(|n| !n.trim().is_empty())
                .map(|n| n.to_string())
                .unwrap_or_else(|| id.to_string()),
            ProviderFlavor::OpenAi => m
                .get("name")
                .and_then(|n| n.as_str())
                .filter(|n| !n.trim().is_empty() && *n != id)
                .map(|n| n.to_string())
                .unwrap_or_else(|| id.to_string()),
        };
        out.push(ProviderModel {
            id: id.to_string(),
            label,
        });
    }
    out
}

/// Fetch моделей БЕЗ сохранения: ходит в сеть и возвращает нормализованный
/// список. Сохранение — только `save_provider` после вопроса юзеру.
/// Если `base_url` пуст, а `id` известен — берём endpoint/протокол/ключ
/// из сохранённого (ключ из формы приоритетнее сохранённого).
#[tauri::command]
pub async fn fetch_provider_models(
    state: State<'_, Arc<AppState>>,
    id: Option<String>,
    base_url: String,
    key: Option<String>,
    flavor: String,
) -> Result<Vec<ProviderModel>, String> {
    let base_in = base_url.trim().trim_end_matches('/').to_string();
    let (base, key, flavor) = if base_in.is_empty() {
        let id = id
            .map(|s| s.trim().to_string())
            .filter(|s| !s.is_empty())
            .ok_or_else(|| "пустой endpoint — укажите URL или выберите провайдера".to_string())?;
        let store = state.store.lock().map_err(|e| e.to_string())?;
        let store: &dyn Store = &**store;
        let customs = read_customs(store);
        let models_map = read_models_map(store);
        let keys = read_keys_map(store);
        let (cfg, _) = resolve_id(store, &keys, &customs, &models_map, key, &id)?;
        (cfg.base_url, cfg.key, cfg.flavor)
    } else {
        let flavor = ProviderFlavor::parse(&flavor)
            .ok_or_else(|| format!("неизвестный протокол «{flavor}» (openai|anthropic)"))?;
        (base_in, key.unwrap_or_default(), flavor)
    };
    if base.is_empty() {
        return Err("пустой endpoint".into());
    }
    let raw = match flavor {
        ProviderFlavor::OpenAi => {
            let p = if key.trim().is_empty() {
                if is_local(&base) {
                    OpenAiProvider::new_open(&base)
                } else {
                    return Err("пустой ключ".into());
                }
            } else {
                OpenAiProvider::new(&base, &key)
            }
            .map_err(|e| format!("fetch: {e}"))?;
            p.list_models().await.map_err(|e| format!("fetch: {e}"))?
        }
        ProviderFlavor::Anthropic => {
            let p = if key.trim().is_empty() {
                if is_local(&base) {
                    AnthropicProvider::new_open(&base)
                } else {
                    return Err("пустой ключ".into());
                }
            } else {
                AnthropicProvider::new(&base, &key)
            }
            .map_err(|e| format!("fetch: {e}"))?;
            p.list_models().await.map_err(|e| format!("fetch: {e}"))?
        }
    };
    let models = normalize_models(flavor, &raw);
    if models.is_empty() {
        return Err("эндпоинт ответил, но моделей в `data[]` нет".into());
    }
    Ok(models)
}

/// Сохранить провайдера (после вопроса юзеру).
/// - `id: None` → новый custom (`custom-<id>`);
/// - `id` пресета/`default` → обновить только ключ и модели (конфиг каталога не трогаем);
/// - `id` custom → обновить поля.
/// `key: None` — ключ не трогать; `Some("")` — забыть; иначе — положить в DPAPI.
#[tauri::command]
pub fn save_provider(
    state: State<'_, Arc<AppState>>,
    id: Option<String>,
    label: String,
    base_url: String,
    flavor: String,
    key: Option<String>,
    models: Vec<String>,
) -> Result<String, String> {
    let flavor =
        ProviderFlavor::parse(&flavor).ok_or_else(|| format!("неизвестный протокол «{flavor}»"))?;
    let base = base_url.trim().trim_end_matches('/').to_string();
    if base.is_empty() {
        return Err("пустой endpoint".into());
    }
    let label = label.trim().to_string();
    if label.is_empty() {
        return Err("пустое название".into());
    }
    let models: Vec<String> = models
        .into_iter()
        .map(|m| m.trim().to_string())
        .filter(|m| !m.is_empty())
        .take(500)
        .collect();

    let store = state.store.lock().map_err(|e| e.to_string())?;
    let store: &dyn Store = &**store;
    let target: String = match id.as_deref().map(str::trim).unwrap_or("") {
        "" => {
            let mut customs = read_customs(store);
            let nid = format!("custom-{}", short_id());
            customs.push(CustomProvider {
                id: nid.clone(),
                label,
                base_url: base,
                flavor,
            });
            write_customs(store, &customs)?;
            nid
        }
        s if s == DEFAULT_PROVIDER_ID || find_preset(s).is_some() => s.to_string(),
        s => {
            let mut customs = read_customs(store);
            let c = customs
                .iter_mut()
                .find(|c| c.id == s)
                .ok_or_else(|| format!("неизвестный провайдер «{s}»"))?;
            c.label = label;
            c.base_url = base;
            c.flavor = flavor;
            write_customs(store, &customs)?;
            s.to_string()
        }
    };
    if !models.is_empty() {
        let mut map = read_models_map(store);
        map.insert(target.clone(), models);
        write_models_map(store, &map)?;
    }
    if let Some(k) = key {
        let mut keys = read_keys_map(store);
        if k.trim().is_empty() {
            keys.remove(&target);
        } else {
            keys.insert(target.clone(), k.trim().to_string());
        }
        write_keys_map(store, &keys)?;
    }
    Ok(target)
}

/// Удалить custom-провайдера (вместе с ключом и моделями).
/// Пресеты и `default` удалить нельзя. Активный сбрасывается на `default`.
#[tauri::command]
pub fn delete_provider(state: State<'_, Arc<AppState>>, id: String) -> Result<(), String> {
    let id = id.trim().to_string();
    if id == DEFAULT_PROVIDER_ID || find_preset(&id).is_some() {
        return Err("встроенного провайдера удалить нельзя".into());
    }
    let store = state.store.lock().map_err(|e| e.to_string())?;
    let store: &dyn Store = &**store;
    let customs: Vec<CustomProvider> = read_customs(store)
        .into_iter()
        .filter(|c| c.id != id)
        .collect();
    write_customs(store, &customs)?;
    let mut models = read_models_map(store);
    models.remove(&id);
    write_models_map(store, &models)?;
    let mut keys = read_keys_map(store);
    keys.remove(&id);
    write_keys_map(store, &keys)?;
    if active_id(store) == id {
        store
            .set_pref("provider_active", DEFAULT_PROVIDER_ID)
            .map_err(|e| e.to_string())?;
    }
    Ok(())
}

/// Выбрать активного провайдера для ходов и списка моделей.
#[tauri::command]
pub fn set_active_provider(state: State<'_, Arc<AppState>>, id: String) -> Result<(), String> {
    let id = id.trim().to_string();
    if id != DEFAULT_PROVIDER_ID && find_preset(&id).is_none() {
        let store = state.store.lock().map_err(|e| e.to_string())?;
        let store: &dyn Store = &**store;
        if !read_customs(store).iter().any(|c| c.id == id) {
            return Err(format!("неизвестный провайдер «{id}»"));
        }
    }
    let store = state.store.lock().map_err(|e| e.to_string())?;
    store
        .set_pref("provider_active", &id)
        .map_err(|e| e.to_string())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn normalize_openai_takes_ids() {
        let v = serde_json::json!({"object": "list", "data": [
            {"id": "gpt-5.5", "object": "model", "owned_by": "openai"},
            {"id": "", "object": "model"},
            {"object": "model"},
        ]});
        let out = normalize_models(ProviderFlavor::OpenAi, &v);
        assert_eq!(out.len(), 1);
        assert_eq!(out[0].id, "gpt-5.5");
    }

    #[test]
    fn normalize_anthropic_prefers_display_name() {
        let v = serde_json::json!({"data": [
            {"type": "model", "id": "claude-opus-4-5", "display_name": "Claude Opus 4.5"},
            {"type": "model", "id": "m2"},
        ]});
        let out = normalize_models(ProviderFlavor::Anthropic, &v);
        assert_eq!(out.len(), 2);
        assert_eq!(out[0].label, "Claude Opus 4.5");
        assert_eq!(out[1].label, "m2");
    }

    #[test]
    fn normalize_empty_data_is_empty() {
        assert!(normalize_models(ProviderFlavor::OpenAi, &serde_json::json!({})).is_empty());
        assert!(
            normalize_models(ProviderFlavor::OpenAi, &serde_json::json!({"data": []})).is_empty()
        );
    }

    #[test]
    fn resolve_active_defaults_without_prefs() {
        let store = swagcod_core::store::open_memory();
        // Без AppState не собрать — проверяем дефолты чтения напрямую.
        assert_eq!(active_id(store.as_ref()), DEFAULT_PROVIDER_ID);
        assert!(read_customs(store.as_ref()).is_empty());
        assert!(read_models_map(store.as_ref()).is_empty());
        assert!(read_keys_map(store.as_ref()).is_empty());
    }
}

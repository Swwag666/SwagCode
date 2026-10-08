/*! Каталог провайдеров: пресеты с проверенными конфигами + custom.

Источники эндпоинтов (проверены вживую, октябрь 2026):
- Kimi: https://platform.kimi.ai/docs/api/overview — base
  `https://api.moonshot.ai/v1`, OpenAI-совместимый, Bearer, `GET /v1/models`
  с флагами возможностей. Модели: kimi-k2.5/k2.6/k3, kimi-k2-thinking.
- Anthropic: https://docs.anthropic.com/ja/api/messages — `POST /v1/messages`,
  заголовки `x-api-key` + `anthropic-version: 2023-06-01`, `max_tokens`
  обязателен, стрим — SSE с типами message_start/content_block_delta/...;
  `GET /v1/models` отдаёт `{data: [{id, display_name}]}`.
- OpenRouter: https://openrouter.ai/docs — единая точка
  `https://openrouter.ai/api/v1`, OpenAI-совместимая, Bearer,
  `GET /api/v1/models`, id вида `openai/gpt-4o`.
- OpenCode Zen: https://opencode.ai/docs/zen — база `https://opencode.ai/zen/v1`,
  каталог `/v1/models`; нашему клиенту годятся только модели на
  `/v1/chat/completions` (deepseek-v4-*, qwen3.8-max, nemotron-*-free),
  остальные сидят на `/responses` и `/messages` — их не предлагаем.
- OpenAI: https://platform.openai.com/docs/models — `https://api.openai.com/v1`,
  флагманы gpt-5.6-sol/terra/luna, gpt-5.5, gpt-5.4-mini.
- DeepSeek: https://platform.deepseek.com/api-docs — БЕЗ суффикса `/v1`
  (`https://api.deepseek.com` + `/chat/completions`), модели
  deepseek-v4-pro/flash.
- Gemini через OpenAI-совместимую точку:
  `https://generativelanguage.googleapis.com/v1beta/openai/`.
- Groq `https://api.groq.com/openai/v1`, Mistral `https://api.mistral.ai/v1`,
  xAI `https://api.x.ai/v1` — все OpenAI-совместимые с `/models`.
- Ollama `http://localhost:11434/v1` и LM Studio `http://localhost:1234/v1` —
  локальные, ключ не нужен (пустой ключ = без заголовка Authorization).

`default_models` — лишь подсказка до первого Fetch: истина всегда
приходит из `GET /models` живого эндпоинта.
*/

use serde::{Deserialize, Serialize};

/// Wire-протокол провайдера. OpenAI-совместимых — большинство; Anthropic
/// говорит своим `/v1/messages` (см. [`crate::anthropic`]).
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum ProviderFlavor {
    OpenAi,
    Anthropic,
}

impl ProviderFlavor {
    pub fn as_str(self) -> &'static str {
        match self {
            Self::OpenAi => "openai",
            Self::Anthropic => "anthropic",
        }
    }

    pub fn parse(s: &str) -> Option<Self> {
        match s.trim().to_ascii_lowercase().as_str() {
            "openai" | "openai-compatible" | "oai" => Some(Self::OpenAi),
            "anthropic" | "claude" | "messages" => Some(Self::Anthropic),
            _ => None,
        }
    }
}

/// Один пресет каталога. `base_url` — полный корень БЕЗ путей-методов:
/// клиент сам дописывает `/chat/completions`, `/models`, `/messages`.
/// Только Serialize: пресеты — compile-time константы, из JSON не читаются.
#[derive(Debug, Clone, Serialize)]
pub struct ProviderPreset {
    pub id: &'static str,
    pub label: &'static str,
    pub base_url: &'static str,
    pub flavor: ProviderFlavor,
    /// false = localhost без ключа (Ollama, LM Studio).
    pub needs_key: bool,
    /// Куда за ключом (консоль провайдера).
    pub key_url: &'static str,
    /// Подсказка до первого Fetch models.
    pub default_models: &'static [&'static str],
}

pub const PRESETS: &[ProviderPreset] = &[
    ProviderPreset {
        id: "openai",
        label: "OpenAI",
        base_url: "https://api.openai.com/v1",
        flavor: ProviderFlavor::OpenAi,
        needs_key: true,
        key_url: "https://platform.openai.com/api-keys",
        default_models: &["gpt-5.6-sol", "gpt-5.5", "gpt-5.4-mini", "gpt-4o"],
    },
    ProviderPreset {
        id: "anthropic",
        label: "Anthropic Claude",
        base_url: "https://api.anthropic.com/v1",
        flavor: ProviderFlavor::Anthropic,
        needs_key: true,
        key_url: "https://console.anthropic.com/",
        default_models: &[
            "claude-opus-4-5",
            "claude-sonnet-4-5",
            "claude-sonnet-4-20250514",
        ],
    },
    ProviderPreset {
        id: "kimi",
        label: "Kimi (Moonshot)",
        base_url: "https://api.moonshot.ai/v1",
        flavor: ProviderFlavor::OpenAi,
        needs_key: true,
        key_url: "https://platform.kimi.ai/",
        default_models: &["kimi-k3", "kimi-k2.6", "kimi-k2-thinking", "kimi-k2.5"],
    },
    ProviderPreset {
        id: "deepseek",
        label: "DeepSeek",
        base_url: "https://api.deepseek.com",
        flavor: ProviderFlavor::OpenAi,
        needs_key: true,
        key_url: "https://platform.deepseek.com/",
        default_models: &["deepseek-v4-pro", "deepseek-v4-flash"],
    },
    ProviderPreset {
        id: "openrouter",
        label: "OpenRouter",
        base_url: "https://openrouter.ai/api/v1",
        flavor: ProviderFlavor::OpenAi,
        needs_key: true,
        key_url: "https://openrouter.ai/keys",
        default_models: &[
            "anthropic/claude-sonnet-4",
            "openai/gpt-4o",
            "deepseek/deepseek-chat",
        ],
    },
    ProviderPreset {
        id: "zen",
        label: "OpenCode Zen",
        base_url: "https://opencode.ai/zen/v1",
        flavor: ProviderFlavor::OpenAi,
        needs_key: true,
        key_url: "https://opencode.ai/auth",
        default_models: &["deepseek-v4-flash", "qwen3.8-max", "nemotron-3-ultra-free"],
    },
    ProviderPreset {
        id: "gemini",
        label: "Google Gemini",
        base_url: "https://generativelanguage.googleapis.com/v1beta/openai",
        flavor: ProviderFlavor::OpenAi,
        needs_key: true,
        key_url: "https://aistudio.google.com/apikey",
        default_models: &["gemini-2.5-pro", "gemini-2.5-flash"],
    },
    ProviderPreset {
        id: "groq",
        label: "Groq",
        base_url: "https://api.groq.com/openai/v1",
        flavor: ProviderFlavor::OpenAi,
        needs_key: true,
        key_url: "https://console.groq.com/keys",
        default_models: &["llama-3.3-70b-versatile", "openai/gpt-oss-120b"],
    },
    ProviderPreset {
        id: "mistral",
        label: "Mistral",
        base_url: "https://api.mistral.ai/v1",
        flavor: ProviderFlavor::OpenAi,
        needs_key: true,
        key_url: "https://console.mistral.ai/api-keys",
        default_models: &["mistral-large-latest", "codestral-latest"],
    },
    ProviderPreset {
        id: "xai",
        label: "xAI Grok",
        base_url: "https://api.x.ai/v1",
        flavor: ProviderFlavor::OpenAi,
        needs_key: true,
        key_url: "https://console.x.ai/",
        default_models: &["grok-4", "grok-3"],
    },
    ProviderPreset {
        id: "ollama",
        label: "Ollama (local)",
        base_url: "http://localhost:11434/v1",
        flavor: ProviderFlavor::OpenAi,
        needs_key: false,
        key_url: "https://ollama.com/",
        default_models: &[],
    },
    ProviderPreset {
        id: "lmstudio",
        label: "LM Studio (local)",
        base_url: "http://localhost:1234/v1",
        flavor: ProviderFlavor::OpenAi,
        needs_key: false,
        key_url: "https://lmstudio.ai/",
        default_models: &[],
    },
];

/// Найти пресет по id. None — значит custom или `default` (.env).
pub fn find_preset(id: &str) -> Option<&'static ProviderPreset> {
    PRESETS.iter().find(|p| p.id == id)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::HashSet;

    #[test]
    fn presets_are_unique_and_well_formed() {
        let mut ids = HashSet::new();
        assert!(!PRESETS.is_empty());
        for p in PRESETS {
            assert!(ids.insert(p.id), "дубль id: {}", p.id);
            assert!(!p.label.is_empty());
            let lower = p.base_url.to_lowercase();
            assert!(
                lower.starts_with("http://") || lower.starts_with("https://"),
                "{}: {}",
                p.id,
                p.base_url
            );
            assert!(!p.base_url.ends_with('/'), "{}: trailing /", p.id);
            if !p.needs_key {
                assert!(
                    lower.contains("localhost") || lower.contains("127.0.0.1"),
                    "{}: ключ не нужен только у локальных",
                    p.id
                );
            }
        }
    }

    #[test]
    fn flavor_parses_aliases() {
        assert_eq!(
            ProviderFlavor::parse("openai"),
            Some(ProviderFlavor::OpenAi)
        );
        assert_eq!(
            ProviderFlavor::parse("Claude"),
            Some(ProviderFlavor::Anthropic)
        );
        assert_eq!(ProviderFlavor::parse("oai-compatible"), None);
        assert_eq!(ProviderFlavor::parse("grpc"), None);
    }
}

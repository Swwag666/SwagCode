/* F-10: разбор сниппета полнотекстового поиска.

   Ядро подсвечивает совпадения маркерами `[`…`]` (см. SEARCH_MESSAGES_SQL в
   crates/core/src/store.rs). Вставлять такой сниппет через {@html} нельзя:
   содержимое сообщений — это текст модели и человека, то есть недоверенные
   данные, и {@html} вставил бы их как разметку. Поэтому сниппет режется на
   части, а компонент рисует их обычным текстом, оборачивая совпадения в
   <mark>. Чистая функция без DOM — живёт в lib/ и тестируется в node. */

export interface SnippetPart {
  text: string
  /** true — это совпадение, компонент рисует его подсветкой. */
  hit: boolean
}

/**
 * Режет сниппет на текст и подсвеченные совпадения по маркерам `[`…`]`.
 *
 * Граничные случаи намеренно «текстовые», а не ошибочные: сами сообщения
 * могут содержать скобки, и SQLite их не экранирует, поэтому незакрытая `[`,
 * одинокая `]` и пустая `[]` обязаны доехать до экрана символами, а не
 * пропасть. Совпадение ищется по ПЕРВОЙ закрывающей скобке.
 */
export function parseSnippet(snippet: string): SnippetPart[] {
  const parts: SnippetPart[] = []
  let plain = ''
  let i = 0
  while (i < snippet.length) {
    if (snippet[i] === '[') {
      const close = snippet.indexOf(']', i + 1)
      if (close === -1) {
        plain += '['
        i += 1
        continue
      }
      const inner = snippet.slice(i + 1, close)
      if (inner.length > 0) {
        if (plain) {
          parts.push({ text: plain, hit: false })
          plain = ''
        }
        parts.push({ text: inner, hit: true })
      } else {
        // Пустая подсветка бессмысленна: обе скобки остаются текстом.
        plain += '[]'
      }
      i = close + 1
      continue
    }
    plain += snippet[i]
    i += 1
  }
  if (plain) parts.push({ text: plain, hit: false })
  return parts
}

/** Заголовок находки: у сессии без названия показываем её id. */
export function hitTitle(title: string, sessionId: string): string {
  const t = title.trim()
  return t ? t : sessionId
}

/**
 * UX-01-006 (ревизия 86): якорь для перемотки транскрипта к найденному
 * сообщению. Из подсвеченного сниппета берётся первая пара «текст до
 * совпадения + совпадение» — такого куска почти всегда хватает, чтобы
 * найти строку в транскрипте включением, и он не зависит от подсветки.
 */
export function hitAnchor(snippet: string): string | null {
  const parts = parseSnippet(snippet)
  for (let i = 0; i < parts.length; i++) {
    if (parts[i].hit) {
      const before = parts[i - 1]?.text ?? ''
      const anchor = `${before.slice(-24)}${parts[i].text}`.trim()
      if (anchor.length >= 8) return anchor
      const after = parts[i + 1]?.text ?? ''
      const joined = (parts[i].text + after.slice(0, 16)).trim()
      if (joined.length >= 8) return joined
    }
  }
  const flat = snippet.replace(/[[\]]/g, '').trim()
  return flat.length >= 8 ? flat.slice(0, 48) : null
}

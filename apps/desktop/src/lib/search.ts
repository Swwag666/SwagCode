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

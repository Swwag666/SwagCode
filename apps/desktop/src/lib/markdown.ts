/**
 * Инкрементальный markdown-рендерер (Этап 1).
 *
 * C-4 фикс: не репарсим весь документ на каждый токен.
 * Стримящийся текст рендерится как plain text с курсором (O(1) на токен).
 * Полный markdown-парсинг — только когда блок закрывается (done=true).
 */
import { marked } from 'marked'
import hljs from 'highlight.js/lib/core'

// Регистрируем только нужные грамматики (N-2: не тянем все)
import javascript from 'highlight.js/lib/languages/javascript'
import typescript from 'highlight.js/lib/languages/typescript'
import python from 'highlight.js/lib/languages/python'
import rust from 'highlight.js/lib/languages/rust'
import json from 'highlight.js/lib/languages/json'
import bash from 'highlight.js/lib/languages/bash'
import css from 'highlight.js/lib/languages/css'
import xml from 'highlight.js/lib/languages/xml'

hljs.registerLanguage('javascript', javascript)
hljs.registerLanguage('js', javascript)
hljs.registerLanguage('typescript', typescript)
hljs.registerLanguage('ts', typescript)
hljs.registerLanguage('python', python)
hljs.registerLanguage('py', python)
hljs.registerLanguage('rust', rust)
hljs.registerLanguage('rs', rust)
hljs.registerLanguage('json', json)
hljs.registerLanguage('bash', bash)
hljs.registerLanguage('sh', bash)
hljs.registerLanguage('shell', bash)
hljs.registerLanguage('css', css)
hljs.registerLanguage('html', xml)
hljs.registerLanguage('xml', xml)

// Конфигурация marked
marked.setOptions({
  gfm: true,
  breaks: true,
})

/**
 * Рендерит завершённый markdown-текст в HTML с подсветкой кода.
 * Вызывается ОДИН раз когда элемент становится done.
 */
export function renderMarkdown(text: string): string {
  if (!text) return ''

  const html = marked.parse(text, { async: false }) as string

  // Подсветка код-блоков
  return html.replace(
    /<pre><code class="language-(\w+)">([\s\S]*?)<\/code><\/pre>/g,
    (_match, lang, code) => {
      const decoded = decodeHtml(code)
      let highlighted: string
      try {
        highlighted = hljs.highlight(decoded, { language: lang }).value
      } catch {
        highlighted = escapeHtml(decoded)
      }
      return `<div class="code-block-wrap"><button class="copy-btn" data-copy>копировать</button><pre class="code-block"><code class="hljs language-${lang}">${highlighted}</code></pre></div>`
    }
  )
}

/**
 * C-4 фикс: стримящийся текст рендерится как plain text с курсором.
 * Никакого marked.parse, никакого hljs — O(1) на токен.
 * Полный парсинг произойдёт когда элемент станет done.
 */
export function renderStreaming(text: string): string {
  if (!text) return '<span class="cursor">▊</span>'
  return `<span class="streaming-text">${escapeHtml(text)}</span><span class="cursor">▊</span>`
}

function escapeHtml(s: string): string {
  return s
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
}

function decodeHtml(s: string): string {
  return s
    .replace(/&amp;/g, '&')
    .replace(/&lt;/g, '<')
    .replace(/&gt;/g, '>')
    .replace(/&quot;/g, '"')
    .replace(/&#39;/g, "'")
}

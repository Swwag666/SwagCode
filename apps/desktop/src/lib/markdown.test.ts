// @vitest-environment jsdom
import { describe, expect, it } from 'vitest'
import { renderMarkdown, renderStreaming } from './markdown'

describe('renderMarkdown: санитизация вывода модели', () => {
  it('вырезает script и inline-обработчики', () => {
    const html = renderMarkdown(
      'привет <img src=x onerror="alert(1)"> <script>alert(2)</script> <iframe src="http://evil"></iframe>'
    )
    expect(html).not.toContain('onerror')
    expect(html).not.toContain('<script')
    expect(html).not.toContain('<iframe')
    expect(html).toContain('привет')
  })

  it('сохраняет код-блоки с подсветкой и кнопкой копирования', () => {
    const html = renderMarkdown('```ts\nconst a = 1\n```')
    expect(html).toContain('code-block-wrap')
    expect(html).toContain('data-copy')
    // hljs разбивает код на span'ы, поэтому ищем токен, а не всю строку
    expect(html).toContain('const')
    expect(html).toContain('hljs')
  })

  it('ссылки и списки переживают санитизацию', () => {
    const html = renderMarkdown('- пункт\n- [ссылка](https://example.com)')
    expect(html).toContain('https://example.com')
    expect(html).toContain('пункт')
  })
})

describe('renderStreaming: стрим без парсинга', () => {
  it('экранирует разметку в сыром тексте', () => {
    const html = renderStreaming('<b>не html</b>')
    expect(html).not.toContain('<b>')
    expect(html).toContain('&lt;b&gt;')
  })
})

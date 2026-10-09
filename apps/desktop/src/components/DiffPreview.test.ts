// @vitest-environment jsdom
import { cleanup, render, screen } from '@testing-library/svelte'
import { afterEach, describe, expect, it } from 'vitest'
import DiffPreview from './DiffPreview.svelte'
import { STR } from '../lib/strings'

afterEach(cleanup)

/* Текст — ровно тот, что отдаёт ядро (`fsx::format_unified_preview`):
маркер в первом символе, регионы разделены заголовком `@@`. */
const PREVIEW = [
  '@@ -1,7 +1,7 @@',
  ' a',
  ' b',
  ' c',
  '-d',
  '+X',
  ' e',
  ' f',
  ' g',
  '',
].join('\n')

describe('DiffPreview: smoke', () => {
  it('красит строки по маркеру и считает статистику', () => {
    const { container } = render(DiffPreview, { text: PREVIEW })
    expect(screen.getByTestId('diff-preview')).toBeTruthy()
    expect(screen.getByTestId('dp-adds').textContent).toBe('+1')
    expect(screen.getByTestId('dp-dels').textContent).toBe('−1')
    expect(container.querySelectorAll('.dp-line.add').length).toBe(1)
    expect(container.querySelectorAll('.dp-line.del').length).toBe(1)
    expect(container.querySelectorAll('.dp-line.ctx').length).toBe(6)
    expect(container.querySelectorAll('.dp-line.hunk').length).toBe(1)
    // Сам маркер в текст строки не попадает: иначе «-d» читалось бы как «--d».
    expect(container.querySelector('.dp-line.del .dp-text')?.textContent).toBe('d')
    expect(container.querySelector('.dp-line.add .dp-text')?.textContent).toBe('X')
  })

  it('несколько регионов дают несколько заголовков', () => {
    const text = '@@ -1,3 +1,3 @@\n a\n-b\n+B\n@@ -40,3 +40,3 @@\n z\n-y\n+Y\n'
    const { container } = render(DiffPreview, { text })
    expect(container.querySelectorAll('.dp-line.hunk').length).toBe(2)
    expect(screen.getByTestId('dp-adds').textContent).toBe('+2')
  })

  it('режет по maxLines и честно говорит, сколько не показано', () => {
    const many = Array.from({ length: 20 }, (_, i) => `+line ${i}`).join('\n') + '\n'
    const { container } = render(DiffPreview, {
      text: many,
      maxLines: 5,
      hiddenLabel: STR.ru.diffHidden,
    })
    expect(container.querySelectorAll('.dp-line').length).toBe(5)
    // Статистика считается по всем строкам, а не по показанным.
    expect(screen.getByTestId('dp-adds').textContent).toBe('+20')
    expect(screen.getByTestId('dp-hidden').textContent).toContain('15')
  })

  it('без hiddenLabel обрезка молчит', () => {
    const many = Array.from({ length: 10 }, (_, i) => `+line ${i}`).join('\n') + '\n'
    render(DiffPreview, { text: many, maxLines: 2 })
    expect(screen.queryByTestId('dp-hidden')).toBeNull()
  })

  it('пустой текст не роняет компонент', () => {
    render(DiffPreview, { text: '' })
    expect(screen.getByTestId('diff-preview')).toBeTruthy()
    expect(screen.getByTestId('dp-adds').textContent).toBe('+0')
  })

  it('строка-заглушка ядра показывается как есть', () => {
    const { container } = render(DiffPreview, {
      text: '@@ изменений нет: содержимое совпадает @@\n',
    })
    expect(container.querySelector('.dp-line.hunk')?.textContent).toContain('изменений нет')
    expect(screen.getByTestId('dp-adds').textContent).toBe('+0')
  })
})

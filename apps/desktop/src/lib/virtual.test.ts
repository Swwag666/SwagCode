import { describe, expect, it } from 'vitest'
import {
  bottomScrollTop,
  clampWindow,
  computeWindow,
  ESTIMATED_ITEM_HEIGHT,
  OVERSCAN,
  recordHeight,
  shouldStickToBottom,
  type HeightCache,
} from './virtual'

describe('computeWindow', () => {
  it('возвращает пустое окно для пустого списка', () => {
    const w = computeWindow(0, 0, 600, new Map())
    expect(w.indices).toEqual([])
    expect(w.totalHeight).toBe(0)
  })

  it('рендерит только видимое плюс overscan, а не весь список', () => {
    // Это и есть требование бюджета: 100k элементов не должны попадать в DOM.
    const heights: HeightCache = new Map()
    const w = computeWindow(100_000, 0, 600, heights)
    expect(w.indices.length).toBeLessThan(40)
    expect(w.indices.length).toBeGreaterThan(0)
    expect(w.totalHeight).toBe(100_000 * ESTIMATED_ITEM_HEIGHT)
  })

  it('общая высота не зависит от позиции скролла', () => {
    const heights: HeightCache = new Map()
    const a = computeWindow(1000, 0, 600, heights)
    const b = computeWindow(1000, 5000, 600, heights)
    expect(a.totalHeight).toBe(b.totalHeight)
  })

  it('в середине списка окно содержит нужные индексы', () => {
    const heights: HeightCache = new Map()
    // Каждый элемент 72px; скролл 7200 => видимый первый индекс 100.
    const w = computeWindow(1000, 7200, 600, heights)
    expect(w.indices).toContain(100)
    expect(w.start).toBeLessThanOrEqual(100)
    expect(w.start).toBe(100 - OVERSCAN)
  })

  it('offsetTop совпадает с суммой высот до start', () => {
    const heights: HeightCache = new Map()
    const w = computeWindow(1000, 7200, 600, heights)
    expect(w.offsetTop).toBe(w.start * ESTIMATED_ITEM_HEIGHT)
  })

  it('учитывает измеренные высоты вместо оценки', () => {
    const heights: HeightCache = new Map()
    heights.set(0, 500)
    heights.set(1, 500)
    const w = computeWindow(10, 0, 600, heights)
    expect(w.totalHeight).toBe(500 + 500 + 8 * ESTIMATED_ITEM_HEIGHT)
  })

  it('скролл за пределы контента не падает и не даёт пустое окно', () => {
    const heights: HeightCache = new Map()
    const w = computeWindow(10, 999_999, 600, heights)
    expect(w.indices.length).toBeGreaterThan(0)
    expect(w.end).toBeLessThanOrEqual(10)
  })

  it('отрицательный скролл обрабатывается как ноль', () => {
    const heights: HeightCache = new Map()
    const w = computeWindow(10, -500, 600, heights)
    expect(w.start).toBe(0)
    expect(w.offsetTop).toBe(0)
  })

  it('clampWindow ограничивает pathological случай', () => {
    const heights: HeightCache = new Map()
    // Огромный viewport не должен растягивать окно на весь список.
    const w = computeWindow(100_000, 0, 500_000, heights)
    const clamped = clampWindow(w, 50)
    expect(clamped.indices.length).toBeLessThanOrEqual(50)
    expect(clamped.end).toBe(clamped.indices[clamped.indices.length - 1] + 1)
  })

  it('clampWindow не трогает нормальное окно', () => {
    const heights: HeightCache = new Map()
    const w = computeWindow(100, 0, 600, heights)
    expect(clampWindow(w, 50)).toBe(w)
  })
})

describe('recordHeight', () => {
  it('записывает первую измеренную высоту', () => {
    const h: HeightCache = new Map()
    expect(recordHeight(h, 3, 140)).toBe(true)
    expect(h.get(3)).toBe(140)
  })

  it('игнорирует субпиксельные колебания', () => {
    // Иначе кэш инвалидируется на каждый кадр и окно пересчитывается зря.
    const h: HeightCache = new Map()
    recordHeight(h, 0, 100)
    expect(recordHeight(h, 0, 100.4)).toBe(false)
    expect(h.get(0)).toBe(100)
  })

  it('принимает реальное изменение высоты', () => {
    const h: HeightCache = new Map()
    recordHeight(h, 0, 100)
    expect(recordHeight(h, 0, 180)).toBe(true)
    expect(h.get(0)).toBe(180)
  })

  it('отклоняет неположительные высоты', () => {
    // Нулевая высота элемента схлопнула бы список и сломала расчёт.
    const h: HeightCache = new Map()
    expect(recordHeight(h, 0, 0)).toBe(false)
    expect(recordHeight(h, 0, -10)).toBe(false)
    expect(h.size).toBe(0)
  })
})

describe('shouldStickToBottom', () => {
  it('прижимает к низу когда пользователь у низа', () => {
    expect(shouldStickToBottom(940, 600, 1540)).toBe(true)
  })

  it('не прижимает когда пользователь читает историю выше', () => {
    // Автопрокрутка при чтении истории — отдельный класс раздражающих багов.
    expect(shouldStickToBottom(0, 600, 100000)).toBe(false)
  })

  it('допуск работает в пределах tolerance', () => {
    expect(shouldStickToBottom(900, 600, 1540, 48)).toBe(true)
    expect(shouldStickToBottom(800, 600, 1540, 48)).toBe(false)
  })

  it('bottomScrollTop не уходит в отрицательные значения', () => {
    expect(bottomScrollTop(100, 600)).toBe(0)
    expect(bottomScrollTop(2000, 600)).toBe(1400)
  })
})

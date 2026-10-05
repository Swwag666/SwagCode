/**
 * Виртуализация списка сообщений.
 *
 * Требование из бюджета (DECISIONS.md §2): транскрипция на 100k токенов
 * должна скроллиться в 60 fps. В DOM нельзя держать 100k токенов — значит
 * рендерим только видимое окно плюс небольшой запас (overscan) сверху и
 * снизу, а высоту задаём распоркой.
 *
 * Высота строк неизвестна заранее (markdown, код, таблицы), поэтому высоты
 * измеряются по факту и кэшируются, а до измерения используется оценка.
 */

/** Оценка высоты элемента до первого измерения, px. */
export const ESTIMATED_ITEM_HEIGHT = 72
/** Сколько элементов рендерить вне видимой области с каждой стороны. */
export const OVERSCAN = 4

export interface VirtualWindow {
  /** Индекс первого рендеримого элемента. */
  start: number
  /** Индекс после последнего рендеримого (exclusive). */
  end: number
  /** Высота распорки сверху, px. */
  offsetTop: number
  /** Полная высота списка, px. */
  totalHeight: number
  /** Индексы видимых элементов — удобство для рендера. */
  indices: number[]
}

/** Кэш высот: индекс → px. */
export type HeightCache = Map<number, number>

/**
 * Рассчитать видимое окно.
 *
 * Чистая функция — без DOM, поэтому детерминированно тестируется.
 */
export function computeWindow(
  itemCount: number,
  scrollTop: number,
  viewportHeight: number,
  heights: HeightCache,
): VirtualWindow {
  if (itemCount <= 0) {
    return { start: 0, end: 0, offsetTop: 0, totalHeight: 0, indices: [] }
  }

  const heightOf = (i: number): number => heights.get(i) ?? ESTIMATED_ITEM_HEIGHT

  // Полная высота и поиск старта — линейным проходом. W-15: выходим из цикла
  // после found, досчитываем total отдельно — экономим ~50% итераций в среднем.
  let total = 0
  let start = 0
  let offsetTop = 0
  let found = false
  for (let i = 0; i < itemCount; i++) {
    const h = heightOf(i)
    if (!found && scrollTop < total + h) {
      start = i
      offsetTop = total
      found = true
      // Не выходим сразу — нужно досчитать total для bottomSpacer
    }
    total += h
  }
  if (!found) {
    // scrollTop за пределами контента — встаём на последний элемент.
    start = itemCount - 1
    offsetTop = total - heightOf(start)
  }

  // W-15: применяем overscan и пересчитываем offsetTop только если нужно
  const rawStart = start
  start = Math.max(0, start - OVERSCAN)
  if (start !== rawStart) {
    // offsetTop нужно уменьшить на высоты элементов от start до rawStart
    for (let i = start; i < rawStart; i++) {
      offsetTop -= heightOf(i)
    }
  }

  // Считаем, сколько влезает вниз от start.
  let acc = offsetTop
  let end = start
  const bottom = scrollTop + viewportHeight
  while (end < itemCount && acc < bottom) {
    acc += heightOf(end)
    end++
  }
  end = Math.min(itemCount, end + OVERSCAN)

  const indices: number[] = []
  for (let i = start; i < end; i++) indices.push(i)

  return { start, end, offsetTop, totalHeight: total, indices }
}

/** Записать измеренную высоту, только если она изменилась. */
export function recordHeight(
  heights: HeightCache,
  index: number,
  measured: number,
): boolean {
  if (measured <= 0) return false
  const prev = heights.get(index)
  // Допуск в 1px: субпиксельные колебания не должны инвалидировать кэш и
  // вызывать пересчёт окна на каждый кадр.
  if (prev !== undefined && Math.abs(prev - measured) < 1) return false
  heights.set(index, measured)
  return true
}

/**
 * Ограничить число рендеримых элементов жёстким потолком.
 *
 * Защита от pathological случая: если viewportHeight пришёл огромный
 * (окно на весь экран + баг в расчёте), окно может вырасти до всего списка.
 */
export function clampWindow(w: VirtualWindow, maxItems: number): VirtualWindow {
  if (w.indices.length <= maxItems) return w
  const indices = w.indices.slice(0, maxItems)
  return {
    ...w,
    end: indices.length > 0 ? indices[indices.length - 1] + 1 : w.start,
    indices,
  }
}

/**
 * Нужно ли при новом батче прокрутить список вниз.
 *
 * Автопрокрутка только если пользователь уже у самого низа: иначе чтение
 * истории будет ломаться при каждом токене стрима — это отдельный класс
 * раздражающих багов в агентских UI.
 */
export function shouldStickToBottom(
  scrollTop: number,
  viewportHeight: number,
  totalHeight: number,
  tolerance = 48,
): boolean {
  return scrollTop + viewportHeight >= totalHeight - tolerance
}

/** Позиция прокрутки, прижимающая к низу. */
export function bottomScrollTop(totalHeight: number, viewportHeight: number): number {
  return Math.max(0, totalHeight - viewportHeight)
}

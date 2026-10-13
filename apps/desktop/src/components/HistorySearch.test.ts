// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen } from '@testing-library/svelte'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { ComponentProps } from 'svelte'
import HistorySearch, { type MessageHitDto } from './HistorySearch.svelte'
import { STR } from '../lib/strings'

// globals выключены, поэтому авто-размонтирования нет: снимаем сами,
// иначе рендеры копятся и screen находит дубликаты из прошлого теста.
afterEach(cleanup)

const t = (key: keyof typeof STR.ru) => STR.ru[key]

function hit(patch: Partial<MessageHitDto> = {}): MessageHitDto {
  return {
    session_id: 's-1',
    session_title: 'рефакторинг ядра',
    ord: 3,
    role: 'user',
    snippet: 'до [слово] после',
    rank: -1.5,
    ...patch,
  }
}

/* Фабрика пропсов: свои vi.fn() на каждый тест, иначе вызовы соседнего теста
   попадают в подсчёт и «не вызывалось» перестаёт что-либо значить. Тип — ровно
   контракт компонента, чтобы svelte-check ловил опечатку в имени пропа. */
type SearchProps = ComponentProps<typeof HistorySearch>

function base(patch: Partial<SearchProps> = {}): SearchProps {
  return {
    open: true,
    hits: [hit()],
    error: '',
    busy: false,
    limit: 50,
    t,
    onquery: vi.fn(),
    onpick: vi.fn(),
    ...patch,
  }
}

const input = () => screen.getByPlaceholderText(STR.ru.searchPlaceholder)
const items = (c: HTMLElement) => Array.from(c.querySelectorAll('.hsearch-item'))
const status = () => (screen.getByRole('dialog').querySelector('.hsearch-status') as HTMLElement)

describe('HistorySearch: smoke', () => {
  it('закрытая панель ничего не рисует', () => {
    const { container } = render(HistorySearch, base({ open: false }))
    expect(screen.queryByRole('dialog')).toBeNull()
    expect(container.querySelector('.hsearch-overlay')).toBeNull()
  })

  it('открытая показывает поле, заголовок и подсказку', () => {
    render(HistorySearch, base({ hits: [] }))
    expect(screen.getByRole('dialog')).toBeTruthy()
    expect(screen.getByRole('dialog').getAttribute('aria-label')).toBe(STR.ru.searchTitle)
    expect(screen.getByText(STR.ru.searchTitle)).toBeTruthy()
    expect(input()).toBeTruthy()
    // Пустой запрос — подсказка, а не «ничего не найдено»: искать ещё нечего.
    expect(status().textContent).toContain(STR.ru.searchHint)
  })

  it('рисует строку на каждую находку: роль, сессию, номер и сниппет', () => {
    const { container } = render(
      HistorySearch,
      base({
        hits: [
          hit(),
          hit({ session_id: 's-2', session_title: '', ord: 0, role: 'assistant', snippet: 'ответ' }),
        ],
      }),
    )
    const rows = items(container)
    expect(rows.length).toBe(2)
    expect(rows[0].querySelector('.hsearch-role')?.textContent).toBe('user')
    expect(rows[0].querySelector('.hsearch-ses')?.textContent).toBe('рефакторинг ядра')
    expect(rows[0].querySelector('.hsearch-ord')?.textContent).toBe('#3')
    // У сессии без названия показываем id: иначе строку не с чем сопоставить.
    expect(rows[1].querySelector('.hsearch-ses')?.textContent).toBe('s-2')
    expect(rows[1].querySelector('.hsearch-ord')?.textContent).toBe('#0')
    expect(rows[1].querySelector('.hsearch-role')?.textContent).toBe('assistant')
  })

  it('считает находки в статусе, когда запрос набран', async () => {
    render(HistorySearch, base({ hits: [hit(), hit({ session_id: 's-2' })] }))
    await fireEvent.input(input(), { target: { value: 'ядро' } })
    expect(status().textContent).toContain(`2 ${STR.ru.searchFound}`)
  })

  it('при пустом поле статус зовёт ввести слово, даже если находки уже есть', () => {
    /* Наблюдаемое поведение, а не моё ожидание: строки рисуются из пропа hits,
       а статус смотрит на ЛОКАЛЬНЫЙ запрос. Значит родитель обязан чистить hits,
       когда поле стёрто, иначе человек видит строки рядом с подсказкой
       «введите слово». Тест держит этот контракт видимым. */
    const { container } = render(
      HistorySearch,
      base({ hits: [hit(), hit({ session_id: 's-2' })] }),
    )
    expect(status().textContent).toContain(STR.ru.searchHint)
    expect(items(container).length).toBe(2)
  })
})

/* Сниппет приходит из ядра с маркерами `[`…`]` (snippet() в SEARCH_MESSAGES_SQL)
   и содержит текст сообщений — то есть недоверенные данные. Компонент рисует
   его через parseSnippet обычным текстом, намеренно без {@html}. Эти два теста
   и держат это решение: если кто-то «упростит» рендер до {@html}, они упадут. */
describe('HistorySearch: разметка сниппета не исполняется', () => {
  it('совпадение попадает в <mark>, а скобки маркеров не доезжают до экрана', () => {
    const { container } = render(HistorySearch, base({ hits: [hit({ snippet: 'до [слово] после' })] }))
    const snip = container.querySelector('.hsearch-snip') as HTMLElement
    expect(snip.querySelectorAll('mark').length).toBe(1)
    expect(snip.querySelector('mark')?.textContent).toBe('слово')
    // Ровно тот текст, что человек увидел бы в сообщении: ни скобок, ни потерь.
    expect(snip.textContent).toBe('до слово после')
    expect(snip.textContent).not.toContain('[')
    expect(snip.textContent).not.toContain(']')
  })

  it('несколько совпадений подсвечены все', () => {
    const { container } = render(
      HistorySearch,
      base({ hits: [hit({ snippet: '[а] и [б]' })] }),
    )
    const snip = container.querySelector('.hsearch-snip') as HTMLElement
    expect(Array.from(snip.querySelectorAll('mark')).map((m) => m.textContent)).toEqual(['а', 'б'])
    expect(snip.textContent).toBe('а и б')
  })

  it('скобки в самом тексте сообщения остаются текстом', () => {
    // Незакрытая скобка — обычное дело в коде из сообщения; она не должна
    // съедать хвост сниппета.
    const { container } = render(
      HistorySearch,
      base({ hits: [hit({ snippet: 'массив [0 и ещё' })] }),
    )
    const snip = container.querySelector('.hsearch-snip') as HTMLElement
    expect(snip.textContent).toBe('массив [0 и ещё')
    expect(snip.querySelectorAll('mark').length).toBe(0)
  })

  it('HTML из сообщения рисуется как текст, а не как элементы', () => {
    const payload = '<img src=x onerror=alert(1)>'
    const { container } = render(
      HistorySearch,
      base({ hits: [hit({ snippet: `до ${payload} после` })] }),
    )
    const snip = container.querySelector('.hsearch-snip') as HTMLElement
    expect(container.querySelector('img')).toBeNull()
    expect(container.querySelector('script')).toBeNull()
    expect(snip.textContent).toContain(payload)
    // И на пути подсветки то же самое: mark оборачивает текст, а не разметку.
    const marked = render(
      HistorySearch,
      base({ hits: [hit({ snippet: `[${payload}]` })] }),
    )
    const markedSnip = marked.container.querySelector('.hsearch-snip') as HTMLElement
    expect(marked.container.querySelectorAll('img').length).toBe(0)
    expect(markedSnip.querySelector('mark')?.textContent).toBe(payload)
    expect(markedSnip.innerHTML).not.toContain('<img')
  })
})

describe('HistorySearch: состояния', () => {
  it('показывает текст ошибки ядра как есть', () => {
    render(HistorySearch, base({ error: 'пустой поисковый запрос', hits: [] }))
    expect(status().textContent).toContain('пустой поисковый запрос')
    expect(status().classList.contains('err')).toBe(true)
  })

  it('ошибка важнее флага занятости', () => {
    // Иначе человек видит «ищем…» вечно и не понимает, что запрос отвергнут.
    render(HistorySearch, base({ error: 'слишком длинный запрос', busy: true, hits: [] }))
    expect(status().textContent).toContain('слишком длинный запрос')
    expect(status().textContent).not.toContain(STR.ru.searchBusy)
  })

  it('в состоянии занятости говорит, что идёт поиск', () => {
    render(HistorySearch, base({ busy: true, hits: [] }))
    expect(status().textContent).toContain(STR.ru.searchBusy)
  })

  it('пустая выдача на непустой запрос — это «ничего не найдено»', async () => {
    render(HistorySearch, base({ hits: [] }))
    await fireEvent.input(input(), { target: { value: 'зелёные' } })
    expect(status().textContent).toContain(STR.ru.searchEmpty)
  })

  it('упор в предел выдачи помечен: совпадений может быть больше', () => {
    render(HistorySearch, base({ limit: 2, hits: [hit(), hit({ session_id: 's-2' })] }))
    expect(status().textContent).toContain(STR.ru.searchTruncated)
  })

  it('неполная выдача не обещает продолжения', () => {
    render(HistorySearch, base({ limit: 50, hits: [hit()] }))
    expect(status().textContent).not.toContain(STR.ru.searchTruncated)
  })
})

describe('HistorySearch: выбор находки', () => {
  it('Enter отдаёт родителю выделенную находку и закрывает панель', async () => {
    const onpick = vi.fn()
    const hits = [hit(), hit({ session_id: 's-2', ord: 7 })]
    render(HistorySearch, base({ hits, onpick }))
    await fireEvent.keyDown(input(), { key: 'ArrowDown' })
    await fireEvent.keyDown(input(), { key: 'Enter' })
    expect(onpick).toHaveBeenCalledTimes(1)
    expect(onpick).toHaveBeenCalledWith(hits[1])
    expect(screen.queryByRole('dialog')).toBeNull()
  })

  it('Enter без стрелок берёт первую находку', async () => {
    const onpick = vi.fn()
    const hits = [hit(), hit({ session_id: 's-2' })]
    render(HistorySearch, base({ hits, onpick }))
    await fireEvent.keyDown(input(), { key: 'Enter' })
    expect(onpick).toHaveBeenCalledWith(hits[0])
  })

  it('Enter на пустой выдаче не открывает случайную сессию', async () => {
    const onpick = vi.fn()
    render(HistorySearch, base({ hits: [], onquery: vi.fn() }))
    await fireEvent.input(input(), { target: { value: 'zzz' } })
    await fireEvent.keyDown(input(), { key: 'Enter' })
    expect(onpick).not.toHaveBeenCalled()
    expect(screen.getByRole('dialog')).toBeTruthy()
  })

  it('клик по строке отдаёт её находку', async () => {
    const onpick = vi.fn()
    const hits = [hit(), hit({ session_id: 's-2' })]
    const { container } = render(HistorySearch, base({ hits, onpick }))
    await fireEvent.click(items(container)[1])
    expect(onpick).toHaveBeenCalledWith(hits[1])
    expect(screen.queryByRole('dialog')).toBeNull()
  })

  it('стрелки двигают выделение и не уходят за границы', async () => {
    const { container } = render(HistorySearch, base({ hits: [hit(), hit({ session_id: 's-2' })] }))
    const sel = () => items(container).findIndex((r) => r.classList.contains('sel'))
    expect(sel()).toBe(0)
    await fireEvent.keyDown(input(), { key: 'ArrowDown' })
    expect(sel()).toBe(1)
    await fireEvent.keyDown(input(), { key: 'ArrowDown' })
    expect(sel()).toBe(1)
    await fireEvent.keyDown(input(), { key: 'ArrowUp' })
    expect(sel()).toBe(0)
    await fireEvent.keyDown(input(), { key: 'ArrowUp' })
    expect(sel()).toBe(0)
  })

  it('Escape закрывает панель', async () => {
    render(HistorySearch, base())
    await fireEvent.keyDown(input(), { key: 'Escape' })
    expect(screen.queryByRole('dialog')).toBeNull()
  })

  it('клик по фону закрывает, клик по панели — нет', async () => {
    const { container } = render(HistorySearch, base())
    await fireEvent.click(container.querySelector('.hsearch') as HTMLElement)
    expect(screen.getByRole('dialog')).toBeTruthy()
    await fireEvent.click(container.querySelector('.hsearch-overlay') as HTMLElement)
    expect(screen.queryByRole('dialog')).toBeNull()
  })
})

describe('HistorySearch: запрос к родителю', () => {
  it('открытая панель с пустым полем сразу сообщает «запроса нет»', () => {
    const onquery = vi.fn()
    render(HistorySearch, base({ onquery, hits: [] }))
    expect(onquery).toHaveBeenCalledWith('')
  })

  it('закрытая панель не спрашивает ничего', () => {
    const onquery = vi.fn()
    render(HistorySearch, base({ open: false, onquery }))
    expect(onquery).not.toHaveBeenCalled()
  })

  it('набор уходит родителю с задержкой, а не на каждое нажатие', async () => {
    vi.useFakeTimers()
    try {
      const onquery = vi.fn()
      render(HistorySearch, base({ onquery, hits: [] }))
      onquery.mockClear()
      await fireEvent.input(input(), { target: { value: 'ток' } })
      expect(onquery).not.toHaveBeenCalled()
      vi.advanceTimersByTime(219)
      expect(onquery).not.toHaveBeenCalled()
      vi.advanceTimersByTime(1)
      expect(onquery).toHaveBeenCalledTimes(1)
      expect(onquery).toHaveBeenCalledWith('ток')
    } finally {
      vi.useRealTimers()
    }
  })

  it('быстрая правка запроса отменяет прежний, ещё не ушедший запрос', async () => {
    // Поздний ответ не должен перекрывать ранний: на экран уходит только
    // последнее набранное слово.
    vi.useFakeTimers()
    try {
      const onquery = vi.fn()
      render(HistorySearch, base({ onquery, hits: [] }))
      onquery.mockClear()
      await fireEvent.input(input(), { target: { value: 'зел' } })
      vi.advanceTimersByTime(100)
      await fireEvent.input(input(), { target: { value: 'зелёные' } })
      vi.advanceTimersByTime(220)
      expect(onquery).toHaveBeenCalledTimes(1)
      expect(onquery).toHaveBeenCalledWith('зелёные')
    } finally {
      vi.useRealTimers()
    }
  })

  it('стертое до пробелов поле снова сообщает «запроса нет»', async () => {
    const onquery = vi.fn()
    render(HistorySearch, base({ onquery, hits: [] }))
    await fireEvent.input(input(), { target: { value: '   ' } })
    // Пробелы — не запрос: ядро отвергло бы их с ошибкой, а человек увидел бы
    // «пустой поисковый запрос» на ровном месте.
    expect(onquery).toHaveBeenLastCalledWith('')
  })
})

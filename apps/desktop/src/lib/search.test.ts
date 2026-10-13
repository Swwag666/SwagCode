import { describe, it, expect } from 'vitest'
import { hitTitle, parseSnippet } from './search'

/* F-10: сниппет приходит из ядра с маркерами `[`…`]` (snippet() в
   SEARCH_MESSAGES_SQL). Компонент рисует его текстом через parseSnippet, а не
   через {@html}: содержимое сообщений — недоверенные данные. Поэтому разбор
   обязан быть точным и на «грязых» входах — скобки встречаются и в самом
   тексте сообщений, и SQLite их не экранирует. */

describe('parseSnippet', () => {
  it('режет сниппет на текст и совпадения', () => {
    expect(parseSnippet('до [слово] после')).toEqual([
      { text: 'до ', hit: false },
      { text: 'слово', hit: true },
      { text: ' после', hit: false },
    ])
  })

  it('возвращает одну часть, когда совпадений нет', () => {
    expect(parseSnippet('просто текст')).toEqual([{ text: 'просто текст', hit: false }])
  })

  it('пустая строка — пустой список (ничего не рисуем)', () => {
    expect(parseSnippet('')).toEqual([])
  })

  it('несколько совпадений, в том числе подряд', () => {
    expect(parseSnippet('[а] и [б][в]')).toEqual([
      { text: 'а', hit: true },
      { text: ' и ', hit: false },
      { text: 'б', hit: true },
      { text: 'в', hit: true },
    ])
  })

  it('совпадение в начале и в конце строки', () => {
    expect(parseSnippet('[начало] хвост')).toEqual([
      { text: 'начало', hit: true },
      { text: ' хвост', hit: false },
    ])
    expect(parseSnippet('голова [конец]')).toEqual([
      { text: 'голова ', hit: false },
      { text: 'конец', hit: true },
    ])
  })

  it('незакрытая скобка остаётся текстом и не съедает хвост', () => {
    expect(parseSnippet('массив [0] и [без конца')).toEqual([
      { text: 'массив ', hit: false },
      { text: '0', hit: true },
      { text: ' и [без конца', hit: false },
    ])
  })

  it('одинокая закрывающая скобка — текст', () => {
    expect(parseSnippet('a] b')).toEqual([{ text: 'a] b', hit: false }])
  })

  it('пустая подсветка не теряет символы', () => {
    expect(parseSnippet('x[]y')).toEqual([{ text: 'x[]y', hit: false }])
  })

  it('вложенные скобки читаются по первой закрывающей', () => {
    // Соглашение зафиксировано: inner = «a[b», хвост «c]» — обычный текст.
    expect(parseSnippet('[a[b]c]')).toEqual([
      { text: 'a[b', hit: true },
      { text: 'c]', hit: false },
    ])
  })

  it('маркеры ядра (многоточие и префикс) доезжают как текст', () => {
    // snippet() обрезает фрагмент многоточием «…» — оно не маркер.
    expect(parseSnippet('…хвост [тока]…')).toEqual([
      { text: '…хвост ', hit: false },
      { text: 'тока', hit: true },
      { text: '…', hit: false },
    ])
  })
})

describe('hitTitle', () => {
  it('показывает название сессии', () => {
    expect(hitTitle('рефакторинг', 's-1')).toBe('рефакторинг')
  })

  it('у сессии без названия показывает id', () => {
    expect(hitTitle('', 's-0a0b881c00000001')).toBe('s-0a0b881c00000001')
    expect(hitTitle('   ', 's-2')).toBe('s-2')
  })
})

import { describe, it, expect } from 'vitest'
import { filterCommands, normalizeQuery, paletteScore, type PaletteCommand } from './palette'
import type { StrKey } from './strings'

/* F-10: палитра вынесена в lib/palette.ts, и контракт F-2 теперь зафиксирован
   тестами: скоринг, стабильный порядок, нормализация запроса. Раньше это
   проверялось только руками через приложение. */

const DICT: Partial<Record<StrKey, string>> = {
  cmdExportDesc: 'экспорт транскрипта в файл',
  cmdNewDesc: 'новая сессия',
  cmdCompactDesc: 'сжать контекст',
}

function t(key: StrKey): string {
  return DICT[key] ?? String(key)
}

function cmd(slash: string, desc: StrKey): PaletteCommand {
  return { slash, group: 'palGroupSession', desc, action: () => {} }
}

const COMMANDS: PaletteCommand[] = [
  cmd('/new', 'cmdNewDesc'),
  cmd('/export', 'cmdExportDesc'),
  cmd('/compact', 'cmdCompactDesc'),
  cmd('/perm:never', 'cmdPermNeverDesc'),
]

describe('palette (F-2, вынесено в F-10)', () => {
  it('normalizeQuery: регистр, пробелы и ведущий слэш не важны', () => {
    expect(normalizeQuery('  /EXPORT ')).toBe('export')
    expect(normalizeQuery('exp')).toBe('exp')
    expect(normalizeQuery('')).toBe('')
  })

  it('paletteScore: префикс сильнее подстроки, та сильнее описания', () => {
    expect(paletteScore(cmd('/export', 'cmdExportDesc'), 'exp', t)).toBe(0)
    expect(paletteScore(cmd('/compact', 'cmdCompactDesc'), 'act', t)).toBe(1)
    // «транскрипт» есть только в описании.
    expect(paletteScore(cmd('/export', 'cmdExportDesc'), 'транскрипт', t)).toBe(2)
  })

  it('paletteScore: аббревиатура подпоследовательностью находит команду', () => {
    // Пример из комментария F-2: «expmd» — не префикс и не подстрока slash,
    // но подпоследовательность «e-x-p» плюс совпадение по описанию не нужно.
    expect(paletteScore(cmd('/export', 'cmdExportDesc'), 'eprt', t)).toBe(3)
    expect(paletteScore(cmd('/export', 'cmdExportDesc'), 'zzz', t)).toBe(4)
  })

  it('filterCommands: пустой запрос возвращает всё в исходном порядке', () => {
    expect(filterCommands(COMMANDS, '   ', t)).toEqual(COMMANDS)
  })

  it('filterCommands: несовпавшие команды отсеиваются', () => {
    const found = filterCommands(COMMANDS, 'zzz', t)
    expect(found).toEqual([])
  })

  it('filterCommands: порядок по скорингу, при равенстве — стабильный', () => {
    // «per» — префикс только у /perm:never, у остальных подстрочное/описательное.
    const found = filterCommands(COMMANDS, 'per', t).map((c) => c.slash)
    expect(found[0]).toBe('/perm:never')
    // Обе команды с подстрочным совпадением идут в исходном порядке.
    const ties = filterCommands(COMMANDS, 'o', t).map((c) => c.slash)
    expect(ties.indexOf('/export')).toBeLessThan(ties.indexOf('/compact'))
  })

  it('filterCommands: запрос со слэшем ищет то же, что без него', () => {
    expect(filterCommands(COMMANDS, '/new', t)).toEqual(filterCommands(COMMANDS, 'new', t))
  })
})

/* F-10: палитра команд вынесена из App.svelte. Скоринг и фильтрация — чистая
   логика без DOM и без состояния приложения, поэтому живёт в lib/ рядом с
   charts и virtual и тестируется в node-окружении. */
import type { StrKey } from './strings'

export interface PaletteCommand {
  slash: string
  group: StrKey
  desc: StrKey
  action: () => void
}

/** Перевод строки по ключу — тот же контракт, что у `t()` в App.svelte. */
export type Translate = (key: StrKey) => string

/** Запрос приводится к нижнему регистру, ведущий «/» не обязателен. */
export function normalizeQuery(raw: string): string {
  return raw.trim().toLowerCase().replace(/^\//, '')
}

/* F-2: скоринг поиска — префикс slash сильнее подстроки slash, та сильнее
   описания, а дальше аббревиатуры подпоследовательностью («expmd»
   находит /export). 4 = не совпало совсем. */
export function paletteScore(c: PaletteCommand, q: string, t: Translate): number {
  const s = c.slash.slice(1).toLowerCase()
  if (s.startsWith(q)) return 0
  if (s.includes(q)) return 1
  if (t(c.desc).toLowerCase().includes(q)) return 2
  let i = 0
  for (const ch of s) if (i < q.length && ch === q[i]) i++
  return i === q.length ? 3 : 4
}

/* Порядок: сначала скоринг, при равенстве — исходный порядок списка, то есть
   сортировка стабильна и группы команд не перемешиваются между нажатиями. */
export function filterCommands(
  commands: PaletteCommand[],
  rawQuery: string,
  t: Translate,
): PaletteCommand[] {
  const q = normalizeQuery(rawQuery)
  if (!q) return commands
  return commands
    .map((c, idx) => ({ c, idx, sc: paletteScore(c, q, t) }))
    .filter((x) => x.sc < 4)
    .sort((a, b) => a.sc - b.sc || a.idx - b.idx)
    .map((x) => x.c)
}

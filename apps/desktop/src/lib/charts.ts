/* E-8: чистые хелперы графиков телеметрии — без DOM, тестируются vitest.
   SVG строится в компоненте из этих строк/чисел. */

/** Масштабировать значения в точки полилинии SVG (w×h), Y инвертирован.
 *  Пустой список — пустая строка. Один пункт — две точки на середине. */
export function polylinePoints(values: number[], w: number, h: number): string {
  if (values.length === 0) return ''
  const max = Math.max(...values, 1)
  if (values.length === 1) {
    const y = h - (values[0] / max) * h
    return `0,${y} ${w},${y}`
  }
  return values
    .map((v, i) => {
      const x = (i / (values.length - 1)) * w
      const y = h - (v / max) * h
      return `${x.toFixed(1)},${y.toFixed(1)}`
    })
    .join(' ')
}

/** Высоты столбцов (0..h) для барчарта: максимум масштабируется на h. */
export function barHeights(values: number[], h: number): number[] {
  const max = Math.max(...values, 1)
  return values.map((v) => (v / max) * h)
}

/** Накопительные счётчики → дельты между соседними точками.
 *  Отрицательные скачки (перезапуск приложения) зажимаются в 0. */
export function counterDeltas(points: { ts_ms: number; value: number }[]): { ts_ms: number; value: number }[] {
  const out: { ts_ms: number; value: number }[] = []
  for (let i = 1; i < points.length; i++) {
    out.push({ ts_ms: points[i].ts_ms, value: Math.max(0, points[i].value - points[i - 1].value) })
  }
  return out
}

/** Разложить точки метрик по именам (порядок строк сохранён). */
export function groupByName(rows: { name: string; ts_ms: number; value: number }[]): Map<string, { ts_ms: number; value: number }[]> {
  const out = new Map<string, { ts_ms: number; value: number }[]>()
  for (const r of rows) {
    const list = out.get(r.name)
    if (list) list.push({ ts_ms: r.ts_ms, value: r.value })
    else out.set(r.name, [{ ts_ms: r.ts_ms, value: r.value }])
  }
  return out
}

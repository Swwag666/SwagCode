import { describe, it, expect } from 'vitest'
import { polylinePoints, barHeights, counterDeltas, groupByName } from './charts'

describe('charts (E-8)', () => {
  it('polylinePoints: пусто — пустая строка', () => {
    expect(polylinePoints([], 300, 60)).toBe('')
  })

  it('polylinePoints: масштабирует и инвертирует Y', () => {
    const pts = polylinePoints([0, 10], 100, 60)
    expect(pts).toBe('0.0,60.0 100.0,0.0')
    // Значение 5 при максимуме 10 — середина высоты.
    const mid = polylinePoints([5, 10, 0], 200, 60)
    expect(mid.split(' ')[0]).toBe('0.0,30.0')
    expect(mid.split(' ')[1]).toBe('100.0,0.0')
  })

  it('polylinePoints: одна точка — горизонталь', () => {
    expect(polylinePoints([5], 100, 60)).toBe('0,0 100,0')
    expect(polylinePoints([0], 100, 60)).toBe('0,60 100,60')
  })

  it('barHeights: максимум занимает всю высоту, ноль не делит на ноль', () => {
    expect(barHeights([5, 10], 20)).toEqual([10, 20])
    expect(barHeights([0, 0], 20)).toEqual([0, 0])
    expect(barHeights([], 20)).toEqual([])
  })

  it('counterDeltas: дельты между соседями, перезапуск зажимается в 0', () => {
    const d = counterDeltas([
      { ts_ms: 1, value: 10 },
      { ts_ms: 2, value: 15 },
      { ts_ms: 3, value: 12 }, // счётчик сбросился (перезапуск) — дельта 0
    ])
    expect(d).toEqual([
      { ts_ms: 2, value: 5 },
      { ts_ms: 3, value: 0 },
    ])
    expect(counterDeltas([])).toEqual([])
    expect(counterDeltas([{ ts_ms: 1, value: 3 }])).toEqual([])
  })

  it('groupByName: группирует, сохраняя порядок строк', () => {
    const g = groupByName([
      { name: 'a', ts_ms: 1, value: 1 },
      { name: 'b', ts_ms: 2, value: 2 },
      { name: 'a', ts_ms: 3, value: 3 },
    ])
    expect(g.get('a')).toEqual([
      { ts_ms: 1, value: 1 },
      { ts_ms: 3, value: 3 },
    ])
    expect(g.get('b')).toEqual([{ ts_ms: 2, value: 2 }])
    expect(g.get('c')).toBeUndefined()
  })
})

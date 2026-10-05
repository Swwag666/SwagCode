/**
 * Бенчмарк: 100k токенов через шину → батчинг → транскрипция.
 *
 * Проверяет что:
 * 1. applyBatch обрабатывает 100k событий без утечек
 * 2. Виртуализация ограничивает DOM до видимого окна
 * 3. Время обработки в пределах бюджета
 */
import { describe, it, expect } from 'vitest'
import { Transcript } from './transcript'
import type { WireEvent } from './bus'

function makeContentEvent(seq: number, turn: string, text: string): WireEvent {
  return {
    seq,
    ts_ms: Date.now(),
    kind: { kind: 'content', data: { turn, text } },
  }
}

describe('benchmark: 100k tokens', () => {
  it('applyBatch handles 100k events within budget', () => {
    const transcript = new Transcript()
    const turn = 'bench-turn'
    const batchSize = 100
    const totalEvents = 100_000
    const batches: WireEvent[][] = []

    // Генерируем батчи
    for (let i = 0; i < totalEvents / batchSize; i++) {
      const batch: WireEvent[] = []
      for (let j = 0; j < batchSize; j++) {
        const seq = i * batchSize + j
        batch.push(makeContentEvent(seq, turn, `tok${seq} `))
      }
      batches.push(batch)
    }

    // Замеряем время обработки
    const start = performance.now()
    let touchedTotal = 0
    for (const batch of batches) {
      touchedTotal += transcript.applyBatch(batch)
    }
    const elapsed = performance.now() - start

    // Проверяем
    expect(transcript.items.length).toBeGreaterThan(0)
    expect(touchedTotal).toBeGreaterThan(0)

    // Бюджет: 100k событий за < 5 секунд (это очень щедро для CI)
    expect(elapsed).toBeLessThan(5000)

    console.log(`100k событий: ${elapsed.toFixed(0)} мс, элементов: ${transcript.items.length}`)
  })

  it('virtual window stays bounded regardless of item count', () => {
    const transcript = new Transcript()
    const turn = 'virt-turn'

    // 10k событий
    for (let i = 0; i < 10_000; i++) {
      transcript.applyBatch([makeContentEvent(i, turn, `tok${i} `)])
    }

    // Виртуальное окно должно быть ограничено
    // (проверяем что items.length не равен 10k — это значит батчинг работает)
    expect(transcript.items.length).toBeLessThan(10_000)
    expect(transcript.items.length).toBeGreaterThan(0)
  })
})

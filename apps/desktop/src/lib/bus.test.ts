import { describe, expect, it, vi } from 'vitest'
import { EventBusClient, type WireEvent } from './bus'

/**
 * Тесты батчинга. requestAnimationFrame в Node отсутствует, поэтому
 * подменяем его контролируемой реализацией — это заодно делает тест
 * детерминированным: кадр наступает только когда мы скажем.
 */
function installFakeRaf() {
  let queue: FrameRequestCallback[] = []
  let id = 0
  const g = globalThis as unknown as {
    requestAnimationFrame: (cb: FrameRequestCallback) => number
  }
  g.requestAnimationFrame = (cb) => {
    queue.push(cb)
    return ++id
  }
  return {
    /** Сколько кадров ждёт исполнения. */
    pending: () => queue.length,
    /** Исполнить все накопленные кадры. */
    runAll: () => {
      const batch = queue
      queue = []
      for (const cb of batch) cb(performance.now())
    },
  }
}

function ev(seq: number, text: string): WireEvent {
  return {
    seq,
    ts_ms: 1000 + seq,
    kind: { kind: 'content', data: { turn: 't1', text } },
  }
}

describe('EventBusClient · батчинг по кадрам', () => {
  it('одно событие за кадр, сколько бы событий ни пришло', () => {
    // Это требование бюджета: 200 событий/с не должны давать 200 рендеров.
    const raf = installFakeRaf()
    const bus = new EventBusClient()
    let calls = 0
    let total = 0
    bus.subscribe((batch) => {
      calls++
      total += batch.length
    })
    for (let i = 1; i <= 500; i++) bus.ingest(ev(i, 'x'))
    expect(calls).toBe(0)
    raf.runAll()
    expect(calls).toBe(1)
    expect(total).toBe(500)
  })

  it('несколько кадров дают несколько батчей', () => {
    const raf = installFakeRaf()
    const bus = new EventBusClient()
    const batches: number[] = []
    bus.subscribe((b) => batches.push(b.length))
    bus.ingest(ev(1, 'a'))
    raf.runAll()
    bus.ingest(ev(2, 'b'))
    bus.ingest(ev(3, 'c'))
    raf.runAll()
    expect(batches).toEqual([1, 2])
  })

  it('пустой батч не вызывает подписчиков', () => {
    const raf = installFakeRaf()
    const bus = new EventBusClient()
    let calls = 0
    bus.subscribe(() => calls++)
    raf.runAll()
    bus.flush()
    expect(calls).toBe(0)
  })

  it('порядок событий сохраняется внутри батча', () => {
    const raf = installFakeRaf()
    const bus = new EventBusClient()
    let seen: number[] = []
    bus.subscribe((b) => (seen = b.map((e) => e.seq)))
    for (let i = 1; i <= 5; i++) bus.ingest(ev(i, 'x'))
    raf.runAll()
    expect(seen).toEqual([1, 2, 3, 4, 5])
  })

  it('seq отслеживает последнее полученное событие', () => {
    const raf = installFakeRaf()
    const bus = new EventBusClient()
    bus.ingest(ev(7, 'x'))
    bus.ingest(ev(42, 'y'))
    raf.runAll()
    expect(bus.seq).toBe(42)
  })

  it('seq 0 не затирает реальный счётчик', () => {
    // Событие Lagged приходит с seq 0 — оно служебное.
    const raf = installFakeRaf()
    const bus = new EventBusClient()
    bus.ingest(ev(9, 'x'))
    bus.ingest({ seq: 0, ts_ms: 1, kind: { kind: 'status', data: { message: 'lag' } } })
    raf.runAll()
    expect(bus.seq).toBe(9)
  })

  it('отписка останавливает доставку', () => {
    const raf = installFakeRaf()
    const bus = new EventBusClient()
    let calls = 0
    const off = bus.subscribe(() => calls++)
    bus.ingest(ev(1, 'a'))
    raf.runAll()
    off()
    bus.ingest(ev(2, 'b'))
    raf.runAll()
    expect(calls).toBe(1)
  })

  it('несколько подписчиков получают один и тот же батч', () => {
    const raf = installFakeRaf()
    const bus = new EventBusClient()
    let a = 0
    let b = 0
    bus.subscribe((batch) => (a += batch.length))
    bus.subscribe((batch) => (b += batch.length))
    bus.ingest(ev(1, 'x'))
    bus.ingest(ev(2, 'y'))
    raf.runAll()
    expect(a).toBe(2)
    expect(b).toBe(2)
  })

  it('исключение в подписчике не роняет остальных', () => {
    // Один кривый обработчик не должен ослеплять весь UI.
    const raf = installFakeRaf()
    const bus = new EventBusClient()
    let reached = 0
    const errSpy = vi.spyOn(console, 'error').mockImplementation(() => {})
    bus.subscribe(() => {
      throw new Error('boom')
    })
    bus.subscribe(() => reached++)
    bus.ingest(ev(1, 'x'))
    raf.runAll()
    expect(reached).toBe(1)
    expect(bus.failedListeners).toBe(1)
    errSpy.mockRestore()
  })

  it('события, добавленные подписчиком во время сброса, уходят в следующий кадр', () => {
    const raf = installFakeRaf()
    const bus = new EventBusClient()
    let depth = 0
    bus.subscribe((batch) => {
      depth++
      if (depth === 1) {
        for (const e of batch) {
          if (e.seq === 1) bus.ingest(ev(2, 'reentrant'))
        }
      }
    })
    bus.ingest(ev(1, 'a'))
    raf.runAll()
    // Реентерабельное событие не потеряно и не попало в текущий батч.
    expect(bus.pendingCount).toBe(1)
    raf.runAll()
    expect(bus.pendingCount).toBe(0)
  })

  it('pendingCount отражает накопленное до сброса', () => {
    const raf = installFakeRaf()
    const bus = new EventBusClient()
    bus.ingest(ev(1, 'a'))
    bus.ingest(ev(2, 'b'))
    expect(bus.pendingCount).toBe(2)
    raf.runAll()
    expect(bus.pendingCount).toBe(0)
  })

  it('flush можно вызвать вручную', () => {
    // Нужно для тестов и для принудительного сброса при закрытии окна.
    const bus = new EventBusClient()
    installFakeRaf()
    let calls = 0
    bus.subscribe(() => calls++)
    bus.ingest(ev(1, 'a'))
    bus.flush()
    expect(calls).toBe(1)
  })
})

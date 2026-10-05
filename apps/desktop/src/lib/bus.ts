/**
 * Клиент шины событий. Единственная точка входа событий ядра в UI.
 *
 * Зачем батчинг (DECISIONS.md §2): провайдер шлёт 50–200 событий в секунду.
 * Если рендерить каждое — получим сотни апдейтов DOM на кадр и убитый fps.
 * Здесь события копятся в массив и сбрасываются один раз за кадр (rAF),
 * то есть максимум ~60 раз в секунду независимо от частоты стрима.
 *
 * Ключ к Tauri-эмиттеру — 'swagcod://event', см. crates/app/src/lib.rs.
 */

import { listen, type UnlistenFn } from '@tauri-apps/api/event'

/** Зеркало EventKind из Rust (crates/core/src/bus.rs). */
export type EventKind =
  | { kind: 'user'; data: { turn: string; text: string } }
  | { kind: 'reasoning'; data: { turn: string; text: string } }
  | { kind: 'content'; data: { turn: string; text: string } }
  | { kind: 'tool_call_start'; data: { turn: string; call_id: string; name: string } }
  | {
      kind: 'tool_call'
      data: { turn: string; call_id: string; name: string; arguments: unknown }
    }
  | {
      kind: 'tool_result'
      data: { turn: string; call_id: string; ok: boolean; output: string; elapsed_ms: number }
    }
  | { kind: 'turn_started'; data: { turn: string; session: string } }
  | {
      kind: 'turn_ended'
      data: { turn: string; session: string; ok: boolean; reason: string | null }
    }
  | {
      kind: 'approval_required'
      data: { turn: string; call_id: string; tool: string; summary: string }
    }
  | { kind: 'error'; data: { turn: string | null; message: string } }
  | { kind: 'status'; data: { message: string } }

export interface WireEvent {
  seq: number
  ts_ms: number
  kind: EventKind
}

export type EventListener = (batch: WireEvent[]) => void

/**
 * Копилка событий с сбросом по кадру.
 *
 * Хвост событий хранится отдельно от подписчиков: если подписчик медленный,
 * он теряет только свои кадры, а не вешает приём.
 */
export class EventBusClient {
  private pending: WireEvent[] = []
  private frameRequested = false
  private listeners = new Set<EventListener>()
  private unlisten: UnlistenFn | null = null
  private lastSeq = 0
  private droppedFrames = 0

  /** Подписаться на батчи (один вызов за кадр). */
  subscribe(fn: EventListener): () => void {
    this.listeners.add(fn)
    return () => this.listeners.delete(fn)
  }

  /** Начать приём событий из ядра. */
  async start(): Promise<void> {
    if (this.unlisten) return
    this.unlisten = await listen<WireEvent>('swagcod://event', (e) => this.ingest(e.payload))
  }

  async stop(): Promise<void> {
    if (this.unlisten) {
      this.unlisten()
      this.unlisten = null
    }
  }

  /**
   * Принять одно событие. Намеренно не рендерит ничего — только копит.
   * Публичен для тестов и для локального внедрения событий без ядра.
   */
  ingest(ev: WireEvent): void {
    if (ev.seq > 0) this.lastSeq = ev.seq
    this.pending.push(ev)
    this.scheduleFlush()
  }

  private scheduleFlush(): void {
    if (this.frameRequested) return
    this.frameRequested = true
    // rAF, а не setTimeout: сброс ровно перед отрисовкой, без лишних кадров.
    requestAnimationFrame(() => this.flush())
  }

  /** Сбросить накопленное всем подписчикам одним батчем. */
  flush(): void {
    this.frameRequested = false
    if (this.pending.length === 0) return
    // Меняем массив, а не копируем: сброс может произойти внутри обработчика.
    const batch = this.pending
    this.pending = []
    for (const fn of this.listeners) {
      try {
        fn(batch)
      } catch (err) {
        // Один кривой подписчик не должен ронять приём событий.
        this.droppedFrames++
        console.error('event listener failed', err)
      }
    }
  }

  /** Последний полученный seq — нужен для снапшотов и resume. */
  get seq(): number {
    return this.lastSeq
  }

  /** Сколько событий ждёт сброса (для диагностики переполнения). */
  get pendingCount(): number {
    return this.pending.length
  }

  get failedListeners(): number {
    return this.droppedFrames
  }
}

/** Один экземпляр на приложение. */
export const bus = new EventBusClient()

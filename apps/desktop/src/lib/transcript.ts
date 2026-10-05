/**
 * Модель транскрипции: события шины → элементы списка.
 *
 * Здесь живёт правило, которое нельзя нарушить (DECISIONS.md §5.6, находка 1):
 * `reasoning` и `content` — это ДВА разных элемента, не склеенных в один.
 * Наивная реализация, пишущая reasoning в текст ответа, показывает пустоту на
 * несколько секунд в начале каждого хода — ровно то, что убивает ощущение
 * скорости.
 */

import type { WireEvent } from './bus'

export type ItemKind =
  | 'user'
  | 'assistant'
  | 'reasoning'
  | 'tool_call'
  | 'tool_result'
  | 'approval'
  | 'error'
  | 'status'

export interface TranscriptItem {
  /** Стабильный ключ для рендера. Не индекс: индексы поедут при стриме. */
  key: string
  kind: ItemKind
  /** Основной текст. Для стримящихся элементов растёт на месте. */
  text: string
  /** Идентификатор хода. */
  turn?: string
  /** Для tool_result: успешность и длительность. */
  ok?: boolean
  elapsedMs?: number
  /** Для approval: что именно подтверждают. */
  tool?: string
  callId?: string
  /** Завершён ли элемент (стрим закончен). */
  done: boolean
  ts: number
}

/** Транскрипция с индексами для быстрого доступа по ходу. */
export class Transcript {
  readonly items: TranscriptItem[] = []
  /** turn -> индексы элементов этого хода, чтобы не сканировать всё. */
  private byTurn = new Map<string, number[]>()
  /** call_id -> индекс, для обновления результата тулза. */
  private byCall = new Map<string, number>()
  private nextKey = 0

  private key(prefix: string): string {
    return `${prefix}-${this.nextKey++}`
  }

  private push(item: TranscriptItem): number {
    const idx = this.items.length
    this.items.push(item)
    if (item.turn) {
      const arr = this.byTurn.get(item.turn)
      if (arr) arr.push(idx)
      else this.byTurn.set(item.turn, [idx])
    }
    return idx
  }

  /** Пользовательское сообщение (создаётся локально, не из шины). */
  addUser(text: string): void {
    this.push({ key: this.key('u'), kind: 'user', text, done: true, ts: Date.now() })
  }

  /** Ход закончился: все его стримящиеся строки переводим в готовый вид.
      Без этого «ДУМАЕТ» продолжает мигать волной после ответа модели. */
  closeTurn(turn: string): void {
    const idxs = this.byTurn.get(turn)
    if (!idxs) return
    for (const i of idxs) {
      this.items[i].done = true
    }
  }

  /** Найти стримящийся элемент хода указанного вида. */
  private findStreaming(turn: string, kind: ItemKind): TranscriptItem | undefined {
    const idxs = this.byTurn.get(turn)
    if (!idxs) return undefined
    for (let i = idxs.length - 1; i >= 0; i--) {
      const it = this.items[idxs[i]]
      if (it.kind === kind && !it.done) return it
    }
    return undefined
  }

  /**
   * Применить батч событий. Возвращает число изменённых элементов —
   * по нему UI решает, нужна ли перерисовка (лишние апдейты = потерянные fps).
   */
  applyBatch(batch: WireEvent[]): number {
    let touched = 0
    for (const ev of batch) {
      if (this.applyOne(ev)) touched++
    }
    return touched
  }

  private applyOne(ev: WireEvent): boolean {
    const k = ev.kind
    switch (k.kind) {
      case 'user': {
        // Восстановление истории с ядра: ход пользователя из журнала сессии.
        this.push({
          key: this.key('u'),
          kind: 'user',
          text: k.data.text,
          turn: k.data.turn,
          done: true,
          ts: ev.ts_ms,
        })
        return true
      }

      case 'reasoning': {
        // Дописываем в существующий reasoning-элемент этого хода, а не
        // создаём новый на каждый токен: иначе список вырастет в тысячи строк.
        const ex = this.findStreaming(k.data.turn, 'reasoning')
        if (ex) {
          ex.text += k.data.text
          return true
        }
        this.push({
          key: this.key('r'),
          kind: 'reasoning',
          text: k.data.text,
          turn: k.data.turn,
          done: false,
          ts: ev.ts_ms,
        })
        return true
      }

      case 'content': {
        const ex = this.findStreaming(k.data.turn, 'assistant')
        if (ex) {
          ex.text += k.data.text
          return true
        }
        this.push({
          key: this.key('a'),
          kind: 'assistant',
          text: k.data.text,
          turn: k.data.turn,
          done: false,
          ts: ev.ts_ms,
        })
        return true
      }

      case 'tool_call_start': {
        // Start — только сигнал для UI «вызов начался»; аргументы ещё не
        // собраны, поэтому элемент создаём на tool_call (complete).
        this.push({
          key: this.key('ts'),
          kind: 'status',
          text: `${k.data.name} …`,
          turn: k.data.turn,
          tool: k.data.name,
          callId: k.data.call_id,
          done: true,
          ts: ev.ts_ms,
        })
        return true
      }

      case 'tool_call': {
        const idx = this.push({
          key: this.key('tc'),
          kind: 'tool_call',
          text: safeStringify(k.data.arguments),
          turn: k.data.turn,
          tool: k.data.name,
          callId: k.data.call_id,
          done: true,
          ts: ev.ts_ms,
        })
        if (k.data.call_id) this.byCall.set(k.data.call_id, idx)
        return true
      }

      case 'tool_result': {
        // Закрываем стримящийся reasoning/assistant этого хода: ход перешёл
        // к исполнению, значит генерация текста закончилась.
        this.closeStreaming(k.data.turn)
        this.push({
          key: this.key('tr'),
          kind: 'tool_result',
          text: k.data.output,
          turn: k.data.turn,
          callId: k.data.call_id,
          ok: k.data.ok,
          elapsedMs: k.data.elapsed_ms,
          done: true,
          ts: ev.ts_ms,
        })
        return true
      }

      case 'approval_required': {
        this.push({
          key: this.key('ap'),
          kind: 'approval',
          text: k.data.summary,
          turn: k.data.turn,
          tool: k.data.tool,
          callId: k.data.call_id,
          done: true,
          ts: ev.ts_ms,
        })
        return true
      }

      case 'turn_started': {
        return true
      }

      case 'turn_ended': {
        this.closeStreaming(k.data.turn)
        if (!k.data.ok && k.data.reason) {
          this.push({
            key: this.key('err'),
            kind: 'error',
            text: k.data.reason,
            turn: k.data.turn,
            done: true,
            ts: ev.ts_ms,
          })
        }
        return true
      }

      case 'error': {
        this.push({
          key: this.key('err'),
          kind: 'error',
          text: k.data.message,
          turn: k.data.turn ?? undefined,
          done: true,
          ts: ev.ts_ms,
        })
        return true
      }

      case 'status': {
        this.push({
          key: this.key('st'),
          kind: 'status',
          text: k.data.message,
          done: true,
          ts: ev.ts_ms,
        })
        return true
      }

      default: {
        // Неизвестный вид: TypeScript исчерпал варианты, но runtime должен
        // пережить расширение протокола со стороны ядра без падения.
        return false
      }
    }
  }

  private closeStreaming(turn: string): void {
    const idxs = this.byTurn.get(turn)
    if (!idxs) return
    for (const i of idxs) {
      const it = this.items[i]
      if (!it.done && (it.kind === 'reasoning' || it.kind === 'assistant')) {
        it.done = true
      }
    }
  }

  /** Закрыть все незакрытые элементы (например, при остановке хода). */
  closeAll(): void {
    for (const it of this.items) it.done = true
  }

  get length(): number {
    return this.items.length
  }

  item(index: number): TranscriptItem | undefined {
    return this.items[index]
  }

  clear(): void {
    this.items.length = 0
    this.byTurn.clear()
    this.byCall.clear()
  }
}

/**
 * Сериализовать аргументы тулза для показа.
 *
 * Обрезаем: у `write` аргументом может быть файл на мегабайты, и его полная
 * строка убила бы рендер одного элемента списка.
 */
export function safeStringify(value: unknown, max = 2000): string {
  let s: string
  try {
    s = typeof value === 'string' ? value : JSON.stringify(value) ?? ''
  } catch {
    s = String(value)
  }
  if (s.length <= max) return s
  return s.slice(0, max) + `… [+${s.length - max}]`
}

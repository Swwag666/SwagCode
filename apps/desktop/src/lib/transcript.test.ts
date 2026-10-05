import { describe, expect, it } from 'vitest'
import { safeStringify, Transcript } from './transcript'
import type { WireEvent } from './bus'

/** Помощник: собрать событие шины без лишнего шума в тестах. */
function ev(seq: number, kind: WireEvent['kind'], ts = 1000 + seq): WireEvent {
  return { seq, ts_ms: ts, kind }
}

describe('Transcript · reasoning отдельным каналом', () => {
  it('reasoning не склеивается с content', () => {
    // Это правило из DECISIONS.md §5.6 находка 1: нарушение показывает пустоту
    // в начале каждого хода и убивает ощущение скорости.
    const t = new Transcript()
    t.applyBatch([
      ev(1, { kind: 'reasoning', data: { turn: 't1', text: 'думаю' } }),
      ev(2, { kind: 'content', data: { turn: 't1', text: 'ответ' } }),
    ])
    const reasoning = t.items.find((i) => i.kind === 'reasoning')
    const assistant = t.items.find((i) => i.kind === 'assistant')
    expect(reasoning?.text).toBe('думаю')
    expect(assistant?.text).toBe('ответ')
    expect(assistant?.text).not.toContain('думаю')
  })

  it('reasoning идёт до content по порядку', () => {
    const t = new Transcript()
    t.applyBatch([
      ev(1, { kind: 'reasoning', data: { turn: 't1', text: 'r' } }),
      ev(2, { kind: 'content', data: { turn: 't1', text: 'c' } }),
    ])
    expect(t.items[0].kind).toBe('reasoning')
    expect(t.items[1].kind).toBe('assistant')
  })

  it('последовательные куски reasoning дописываются в один элемент', () => {
    // Иначе на длинном рассуждении список вырастет в тысячи строк.
    const t = new Transcript()
    t.applyBatch([
      ev(1, { kind: 'reasoning', data: { turn: 't1', text: 'а' } }),
      ev(2, { kind: 'reasoning', data: { turn: 't1', text: 'б' } }),
      ev(3, { kind: 'reasoning', data: { turn: 't1', text: 'в' } }),
    ])
    const rs = t.items.filter((i) => i.kind === 'reasoning')
    expect(rs).toHaveLength(1)
    expect(rs[0].text).toBe('абв')
  })

  it('content-куски одного хода сливаются в один элемент', () => {
    const t = new Transcript()
    t.applyBatch([
      ev(1, { kind: 'content', data: { turn: 't1', text: 'при' } }),
      ev(2, { kind: 'content', data: { turn: 't1', text: 'вет' } }),
    ])
    const as = t.items.filter((i) => i.kind === 'assistant')
    expect(as).toHaveLength(1)
    expect(as[0].text).toBe('привет')
  })

  it('два хода не смешивают текст', () => {
    const t = new Transcript()
    t.applyBatch([
      ev(1, { kind: 'content', data: { turn: 't1', text: 'первый' } }),
      ev(2, { kind: 'turn_ended', data: { turn: 't1', session: 's', ok: true, reason: null } }),
      ev(3, { kind: 'content', data: { turn: 't2', text: 'второй' } }),
    ])
    const as = t.items.filter((i) => i.kind === 'assistant')
    expect(as).toHaveLength(2)
    expect(as[0].text).toBe('первый')
    expect(as[1].text).toBe('второй')
  })

  it('turn_ended закрывает стримящиеся элементы', () => {
    const t = new Transcript()
    t.applyBatch([
      ev(1, { kind: 'reasoning', data: { turn: 't1', text: 'r' } }),
      ev(2, { kind: 'content', data: { turn: 't1', text: 'c' } }),
    ])
    expect(t.items.every((i) => !i.done)).toBe(true)
    t.applyBatch([
      ev(3, { kind: 'turn_ended', data: { turn: 't1', session: 's', ok: true, reason: null } }),
    ])
    expect(t.items.every((i) => i.done)).toBe(true)
  })

  it('новый content после закрытия создаёт новый элемент', () => {
    const t = new Transcript()
    t.applyBatch([ev(1, { kind: 'content', data: { turn: 't1', text: 'a' } })])
    t.applyBatch([
      ev(2, { kind: 'turn_ended', data: { turn: 't1', session: 's', ok: true, reason: null } }),
    ])
    t.applyBatch([ev(3, { kind: 'content', data: { turn: 't1', text: 'b' } })])
    const as = t.items.filter((i) => i.kind === 'assistant')
    expect(as).toHaveLength(2)
    expect(as[0].text).toBe('a')
    expect(as[1].text).toBe('b')
  })
})

describe('Transcript · tool calls', () => {
  it('полный вызов тулза создаёт элемент с аргументами', () => {
    const t = new Transcript()
    t.applyBatch([
      ev(1, {
        kind: 'tool_call',
        data: { turn: 't1', call_id: 'c1', name: 'read', arguments: { path: 'a.txt' } },
      }),
    ])
    const tc = t.items.find((i) => i.kind === 'tool_call')
    expect(tc?.tool).toBe('read')
    expect(tc?.callId).toBe('c1')
    expect(tc?.text).toContain('a.txt')
  })

  it('start не создаёт tool_call с пустыми аргументами', () => {
    // Start — сигнал «вызов начался»; аргументы ещё не собраны (находка 3).
    const t = new Transcript()
    t.applyBatch([
      ev(1, { kind: 'tool_call_start', data: { turn: 't1', call_id: 'c1', name: 'bash' } }),
    ])
    expect(t.items.some((i) => i.kind === 'tool_call')).toBe(false)
    expect(t.items.some((i) => i.kind === 'status')).toBe(true)
  })

  it('tool_result закрывает стримящийся ответ хода', () => {
    const t = new Transcript()
    t.applyBatch([ev(1, { kind: 'content', data: { turn: 't1', text: 'сейчас прочитаю' } })])
    t.applyBatch([
      ev(2, {
        kind: 'tool_result',
        data: { turn: 't1', call_id: 'c1', ok: true, output: 'содержимое', elapsed_ms: 12 },
      }),
    ])
    expect(t.items.find((i) => i.kind === 'assistant')?.done).toBe(true)
    const tr = t.items.find((i) => i.kind === 'tool_result')
    expect(tr?.ok).toBe(true)
    expect(tr?.elapsedMs).toBe(12)
    expect(tr?.text).toBe('содержимое')
  })

  it('approval_required создаёт элемент подтверждения', () => {
    const t = new Transcript()
    t.applyBatch([
      ev(1, {
        kind: 'approval_required',
        data: { turn: 't1', call_id: 'c1', tool: 'bash', summary: 'bash {"cmd":"rm -rf build"}' },
      }),
    ])
    const ap = t.items.find((i) => i.kind === 'approval')
    expect(ap?.tool).toBe('bash')
    expect(ap?.text).toContain('rm -rf build')
  })
})

describe('Transcript · ошибки и статусы', () => {
  it('неудачный turn_ended порождает ошибку с причиной', () => {
    const t = new Transcript()
    t.applyBatch([
      ev(1, {
        kind: 'turn_ended',
        data: { turn: 't1', session: 's', ok: false, reason: 'превышен лимит итераций' },
      }),
    ])
    const err = t.items.find((i) => i.kind === 'error')
    expect(err?.text).toBe('превышен лимит итераций')
  })

  it('успешный turn_ended не создаёт ошибку', () => {
    const t = new Transcript()
    t.applyBatch([
      ev(1, { kind: 'turn_ended', data: { turn: 't1', session: 's', ok: true, reason: null } }),
    ])
    expect(t.items.some((i) => i.kind === 'error')).toBe(false)
  })

  it('событие error попадает в транскрипцию', () => {
    const t = new Transcript()
    t.applyBatch([ev(1, { kind: 'error', data: { turn: null, message: 'provider 429' } })])
    expect(t.items[0].kind).toBe('error')
    expect(t.items[0].text).toBe('provider 429')
  })

  it('статус не теряет текст', () => {
    const t = new Transcript()
    t.applyBatch([ev(1, { kind: 'status', data: { message: 'провайдер подключён' } })])
    expect(t.items[0].kind).toBe('status')
    expect(t.items[0].text).toBe('провайдер подключён')
  })

  it('неизвестный вид события не роняет приём', () => {
    // Ядро может расширить протокол; UI обязан пережить это без падения.
    const t = new Transcript()
    const touched = t.applyBatch([
      ev(1, { kind: 'brand_new_event' } as unknown as WireEvent['kind']),
      ev(2, { kind: 'status', data: { message: 'ok' } }),
    ])
    expect(touched).toBe(1)
    expect(t.items).toHaveLength(1)
  })
})

describe('Transcript · ключи и порядок', () => {
  it('ключи уникальны — иначе Svelte перепутает элементы при стриме', () => {
    const t = new Transcript()
    t.applyBatch([
      ev(1, { kind: 'content', data: { turn: 't1', text: 'a' } }),
      ev(2, { kind: 'content', data: { turn: 't1', text: 'b' } }),
      ev(3, { kind: 'status', data: { message: 's' } }),
      ev(4, { kind: 'error', data: { turn: null, message: 'e' } }),
    ])
    t.addUser('привет')
    const keys = t.items.map((i) => i.key)
    expect(new Set(keys).size).toBe(keys.length)
  })

  it('applyBatch возвращает число изменённых элементов', () => {
    const t = new Transcript()
    expect(t.applyBatch([ev(1, { kind: 'content', data: { turn: 't', text: 'x' } })])).toBe(1)
    // Пустой батч ничего не меняет — UI не перерисовывается.
    expect(t.applyBatch([])).toBe(0)
  })

  it('clear очищает и индексы', () => {
    const t = new Transcript()
    t.applyBatch([ev(1, { kind: 'content', data: { turn: 't1', text: 'a' } })])
    t.clear()
    expect(t.length).toBe(0)
    // После clear новый ход не должен дописывать в старые элементы.
    t.applyBatch([ev(2, { kind: 'content', data: { turn: 't1', text: 'b' } })])
    expect(t.items).toHaveLength(1)
    expect(t.items[0].text).toBe('b')
  })

  it('closeAll закрывает всё', () => {
    const t = new Transcript()
    t.applyBatch([ev(1, { kind: 'content', data: { turn: 't1', text: 'a' } })])
    t.closeAll()
    expect(t.items.every((i) => i.done)).toBe(true)
  })

  it('addUser добавляет завершённый элемент', () => {
    const t = new Transcript()
    t.addUser('вопрос')
    expect(t.items[0].kind).toBe('user')
    expect(t.items[0].done).toBe(true)
  })
})

describe('safeStringify', () => {
  it('сериализует объект', () => {
    expect(safeStringify({ a: 1 })).toBe('{"a":1}')
  })

  it('строку отдаёт как есть', () => {
    expect(safeStringify('просто текст')).toBe('просто текст')
  })

  it('обрезает гигантские аргументы', () => {
    // У write аргументом может быть файл на мегабайты.
    const big = 'x'.repeat(50_000)
    const out = safeStringify(big, 2000)
    expect(out.length).toBeLessThan(2100)
    expect(out).toContain('…')
  })

  it('не падает на циклических структурах', () => {
    const a: Record<string, unknown> = {}
    a.self = a
    expect(() => safeStringify(a)).not.toThrow()
  })

  it('обрабатывает undefined и null', () => {
    expect(typeof safeStringify(undefined)).toBe('string')
    expect(safeStringify(null)).toBe('null')
  })
})

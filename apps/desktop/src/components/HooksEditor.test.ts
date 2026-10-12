// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen } from '@testing-library/svelte'
import { afterEach, describe, expect, it, vi } from 'vitest'
import HooksEditor from './HooksEditor.svelte'

// globals выключены: размонтируем сами, иначе screen находит дубликаты.
afterEach(cleanup)

const base = {
  title: 'Хуки',
  desc: 'команды владельца на границах хода',
  hint: 'код возврата 0 — пропустить, иначе отклонить',
  saveLabel: 'Сохранить',
  savedLabel: 'Сохраняю…',
  raw: '{"pre_turn":["git status --porcelain"],"pre_tool":[{"match":"^write$","cmd":"guard.cmd"}]}',
  counts: { pre_turn: 1, post_turn: 0, pre_tool: 1 },
  timeoutSecs: 15,
  maxPerEvent: 8,
}

describe('HooksEditor (F-8): редактор хуков', () => {
  it('показывает сохранённую конфигурацию и счётчики событий', () => {
    render(HooksEditor, { ...base, onSave: vi.fn() })
    const text = screen.getByTestId('hooks-text') as HTMLTextAreaElement
    expect(text.value).toContain('pre_turn')
    expect(text.value).toContain('^write$')
    const counts = screen.getByTestId('hooks-counts').textContent ?? ''
    expect(counts).toContain('pre_turn')
    expect(counts).toContain('pre_tool')
    // Пределы контракта видны: владелец знает про таймаут и очередь.
    expect(counts).toContain('15s')
    expect(counts).toContain('≤8')
  })

  it('сохранение отдаёт родителю текст как есть, но без хвостовых пробелов', async () => {
    const onSave = vi.fn()
    render(HooksEditor, { ...base, onSave })
    const cfg = '{"post_turn":["lint.cmd"]}'
    await fireEvent.input(screen.getByTestId('hooks-text'), {
      target: { value: `  ${cfg}\n\n` },
    })
    await fireEvent.click(screen.getByTestId('hooks-save'))
    expect(onSave).toHaveBeenCalledOnce()
    // Форматирование не трогаем (это JSON владельца), хвосты убираем.
    expect(onSave.mock.calls[0][0]).toBe(cfg)
  })

  it('пустой текст валиден: хуков нет, сохранение уходит с пустой строкой', async () => {
    const onSave = vi.fn()
    render(HooksEditor, { ...base, raw: '', counts: { pre_turn: 0, post_turn: 0, pre_tool: 0 }, onSave })
    await fireEvent.input(screen.getByTestId('hooks-text'), { target: { value: '   \n ' } })
    // Локальной ошибки нет: пустая конфигурация — осознанное «хуков нет».
    expect(screen.queryByTestId('hooks-json-error')).toBeNull()
    expect(screen.getByText(base.hint)).toBeTruthy()
    await fireEvent.click(screen.getByTestId('hooks-save'))
    expect(onSave.mock.calls[0][0]).toBe('')
  })

  it('битый JSON подсвечивается до сохранения, подсказка контракта уходит', async () => {
    const onSave = vi.fn()
    render(HooksEditor, { ...base, onSave })
    await fireEvent.input(screen.getByTestId('hooks-text'), {
      target: { value: '{"pre_turn": [' },
    })
    const err = screen.getByTestId('hooks-json-error')
    expect(err.textContent).toContain('JSON:')
    expect(screen.queryByText(base.hint)).toBeNull()
    // Кнопка остаётся доступной: решает бэкенд, а не локальная проверка.
    expect((screen.getByTestId('hooks-save') as HTMLButtonElement).disabled).toBe(false)
    await fireEvent.click(screen.getByTestId('hooks-save'))
    expect(onSave).toHaveBeenCalledOnce()
  })

  it('исправленный текст убирает локальную ошибку', async () => {
    render(HooksEditor, { ...base, onSave: vi.fn() })
    const el = screen.getByTestId('hooks-text')
    await fireEvent.input(el, { target: { value: '{oops' } })
    expect(screen.getByTestId('hooks-json-error')).toBeTruthy()
    await fireEvent.input(el, { target: { value: '{"pre_turn":["a.cmd"]}' } })
    expect(screen.queryByTestId('hooks-json-error')).toBeNull()
    expect(screen.getByText(base.hint)).toBeTruthy()
  })

  it('отказ бэкенда виден как ошибка, а не как статус', () => {
    render(HooksEditor, {
      ...base,
      error: 'hooks: json: expected value at line 1',
      status: null,
      onSave: vi.fn(),
    })
    const alert = screen.getByRole('alert')
    expect(alert.textContent).toContain('expected value')
    expect(screen.getByTestId('hooks-error').textContent).toContain('hooks: json')
  })

  it('на время сохранения кнопка блокируется и показывает процесс', () => {
    const onSave = vi.fn()
    render(HooksEditor, { ...base, busy: true, status: 'сохраняю', onSave })
    expect((screen.getByTestId('hooks-save') as HTMLButtonElement).disabled).toBe(true)
    expect(screen.getByTestId('hooks-save').textContent).toContain('Сохраняю…')
    expect(screen.getByRole('status').textContent).toContain('сохраняю')
    fireEvent.click(screen.getByTestId('hooks-save'))
    expect(onSave).not.toHaveBeenCalled()
  })
})

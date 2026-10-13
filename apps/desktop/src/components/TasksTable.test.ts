// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen } from '@testing-library/svelte'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { ComponentProps } from 'svelte'
import TasksTable from './TasksTable.svelte'
import { STR } from '../lib/strings'

// globals выключены, поэтому авто-размонтирования нет: снимаем сами,
// иначе рендеры копятся и screen находит дубликаты из прошлого теста.
afterEach(cleanup)

type Props = ComponentProps<typeof TasksTable>
/* Тип строки берём из контракта компонента, а не из SettingsTabs: тест ломается
   вместе с пропсом, а не вместе с чужим объявлением. */
type TaskRow = Props['tasks'][number]

const t = (key: keyof typeof STR.ru) => STR.ru[key]

function row(patch: Partial<TaskRow> = {}): TaskRow {
  return {
    id: 'task-1',
    kind: 'background_job',
    state: 'queued',
    attempts: 0,
    next_try_ms: 0,
    last_error: '',
    ...patch,
  }
}

function base(patch: Partial<Props> = {}): Props {
  return { tasks: [row()], t, onCancel: vi.fn(), ...patch }
}

const bodyRows = (c: HTMLElement) => Array.from(c.querySelectorAll('tbody tr'))
const cells = (tr: Element) => Array.from(tr.querySelectorAll('td')).map((td) => td.textContent)
const cancelButtons = () => screen.queryAllByRole('button', { name: STR.ru.taskCancel })

describe('TasksTable: smoke', () => {
  it('рисует строку на каждую задачу с её полями', () => {
    const { container } = render(
      TasksTable,
      base({
        tasks: [
          row({ id: 'task-a', kind: 'background_job', state: 'queued', attempts: 0 }),
          row({
            id: 'task-b',
            kind: 'subagent',
            state: 'failed',
            attempts: 3,
            last_error: 'rpc: таймаут load (15000 мс)',
          }),
        ],
      }),
    )
    const rows = bodyRows(container)
    expect(rows.length).toBe(2)
    // Порядок колонок — тот, что человек видит: id, kind, состояние, попытки,
    // следующая попытка, последняя ошибка.
    expect(cells(rows[0]).slice(0, 4)).toEqual(['task-a', 'background_job', 'queued', '0'])
    expect(cells(rows[1]).slice(0, 4)).toEqual(['task-b', 'subagent', 'failed', '3'])
    expect(cells(rows[1])[5]).toBe('rpc: таймаут load (15000 мс)')
    // Заголовки подписаны переводом, а не именами полей ядра.
    const heads = Array.from(container.querySelectorAll('thead th')).map((th) => th.textContent)
    expect(heads).toContain(STR.ru.taskColState)
    expect(heads).toContain(STR.ru.taskColAttempts)
    expect(heads).toContain(STR.ru.taskColNext)
    expect(heads).toContain(STR.ru.taskColLog)
  })

  it('пустой список показывает подпись, а не пустую таблицу', () => {
    const { container } = render(TasksTable, base({ tasks: [] }))
    expect(screen.getByText(STR.ru.telemetryEmpty)).toBeTruthy()
    expect(container.querySelector('table')).toBeNull()
  })
})

describe('TasksTable: кнопка отмены', () => {
  it('есть только у queued-задачи, у остальных прочерк', () => {
    const { container } = render(
      TasksTable,
      base({
        tasks: [
          row({ id: 'task-q', state: 'queued' }),
          row({ id: 'task-r', state: 'running' }),
          row({ id: 'task-f', state: 'failed' }),
          row({ id: 'task-d', state: 'done' }),
        ],
      }),
    )
    // Отменить можно лишь то, что ещё не началось: у запущенной задачи кнопка
    // обещала бы то, чего ядро сделать не может.
    expect(cancelButtons().length).toBe(1)
    const rows = bodyRows(container)
    expect(rows[0].querySelector('button')).toBeTruthy()
    for (const tr of rows.slice(1)) {
      expect(tr.querySelector('button')).toBeNull()
      expect(cells(tr)[6]).toBe('—')
    }
  })

  it('отмена передаёт id своей задачи, а не первой в списке', async () => {
    const onCancel = vi.fn()
    render(
      TasksTable,
      base({
        onCancel,
        tasks: [row({ id: 'task-a' }), row({ id: 'task-b' }), row({ id: 'task-c' })],
      }),
    )
    const buttons = cancelButtons()
    expect(buttons.length).toBe(3)
    await fireEvent.click(buttons[1])
    expect(onCancel).toHaveBeenCalledTimes(1)
    expect(onCancel).toHaveBeenCalledWith('task-b')
    await fireEvent.click(buttons[2])
    expect(onCancel).toHaveBeenCalledTimes(2)
    expect(onCancel).toHaveBeenLastCalledWith('task-c')
  })

  it('одна задача — одна кнопка, и она про свою задачу', async () => {
    const onCancel = vi.fn()
    render(TasksTable, base({ onCancel, tasks: [row({ id: 'only-one' })] }))
    expect(cancelButtons().length).toBe(1)
    await fireEvent.click(cancelButtons()[0])
    expect(onCancel).toHaveBeenCalledWith('only-one')
  })
})

describe('TasksTable: следующая попытка', () => {
  it('без запланированной попытки показывает прочерк', () => {
    const { container } = render(TasksTable, base({ tasks: [row({ next_try_ms: 0 })] }))
    expect(cells(bodyRows(container)[0])[4]).toBe('—')
  })

  it('с запланированной попыткой показывает время, а не ноль', () => {
    const { container } = render(
      TasksTable,
      base({ tasks: [row({ next_try_ms: 1_760_000_000_000 })] }),
    )
    const text = cells(bodyRows(container)[0])[4] as string
    // Точную строку не проверяем намеренно: toLocaleString() зависит от локали
    // машины, а тест обязан проходить везде. Существенно, что это не прочерк
    // и не голый номер миллисекунд.
    expect(text).not.toBe('—')
    expect(text).not.toBe('1760000000000')
    expect(text).toMatch(/\d/)
  })
})

// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen } from '@testing-library/svelte'
import { afterEach, describe, expect, it, vi } from 'vitest'
import Palette from './Palette.svelte'
import type { PaletteCommand, Translate } from '../lib/palette'
import { STR } from '../lib/strings'

// globals выключены, поэтому авто-размонтирования нет: снимаем сами,
// иначе рендеры копятся и screen находит дубликаты из прошлого теста.
afterEach(cleanup)

const t: Translate = (key) => STR.ru[key]

/* Фабрика, а не общая константа: у каждой команды своё действие, и тест обязан
   видеть, что сработало именно оно, а не накопленные вызовы соседа. */
function fixture(): PaletteCommand[] {
  return [
    { slash: '/new', group: 'palGroupSession', desc: 'cmdNewDesc', action: vi.fn() },
    { slash: '/search', group: 'palGroupSession', desc: 'cmdSearchDesc', action: vi.fn() },
    { slash: '/export', group: 'palGroupExport', desc: 'cmdExportDesc', action: vi.fn() },
    { slash: '/models', group: 'palGroupAgent', desc: 'cmdModelDesc', action: vi.fn() },
  ]
}

const bySlash = (cmds: PaletteCommand[], slash: string) =>
  cmds.find((c) => c.slash === slash)?.action as ReturnType<typeof vi.fn>

describe('Palette: smoke', () => {
  it('закрытая палитра ничего не рисует', () => {
    const { container } = render(Palette, { open: false, commands: fixture(), t })
    expect(screen.queryByRole('dialog')).toBeNull()
    expect(container.querySelector('.palette-overlay')).toBeNull()
  })

  it('открытая показывает поле с подсказкой, команды и заголовки групп', () => {
    render(Palette, { open: true, commands: fixture(), t })
    expect(screen.getByRole('dialog')).toBeTruthy()
    expect(screen.getByPlaceholderText(STR.ru.palPlaceholder)).toBeTruthy()
    expect(screen.getAllByRole('option').length).toBe(4)
    // Группа печатается один раз на смену группы, а не у каждой строки.
    expect(screen.getByText(STR.ru.palGroupSession)).toBeTruthy()
    expect(screen.getByText(STR.ru.palGroupExport)).toBeTruthy()
    expect(screen.getByText(STR.ru.palGroupAgent)).toBeTruthy()
    expect(screen.getByText('/search')).toBeTruthy()
    expect(screen.getByText(STR.ru.cmdSearchDesc)).toBeTruthy()
  })

  it('ввод оставляет только подходящие команды', async () => {
    render(Palette, { open: true, commands: fixture(), t })
    await fireEvent.input(screen.getByPlaceholderText(STR.ru.palPlaceholder), {
      target: { value: 'sea' },
    })
    const options = screen.getAllByRole('option')
    expect(options.length).toBe(1)
    expect(options[0].textContent).toContain('/search')
  })

  it('ввод без совпадений показывает пустое состояние, а не пустоту', async () => {
    render(Palette, { open: true, commands: fixture(), t })
    await fireEvent.input(screen.getByPlaceholderText(STR.ru.palPlaceholder), {
      target: { value: 'zzz' },
    })
    expect(screen.queryAllByRole('option').length).toBe(0)
    expect(screen.getByText(STR.ru.palEmpty)).toBeTruthy()
  })

  it('стрелки двигают выделение и не уходят за границы', async () => {
    render(Palette, { open: true, commands: fixture(), t })
    const input = screen.getByPlaceholderText(STR.ru.palPlaceholder)
    const selected = () =>
      screen.getAllByRole('option').findIndex((o) => o.getAttribute('aria-selected') === 'true')

    expect(selected()).toBe(0)
    await fireEvent.keyDown(input, { key: 'ArrowDown' })
    expect(selected()).toBe(1)
    await fireEvent.keyDown(input, { key: 'ArrowUp' })
    expect(selected()).toBe(0)
    // Выше первой строки выделения нет: иначе Enter исполнил бы ничего.
    await fireEvent.keyDown(input, { key: 'ArrowUp' })
    expect(selected()).toBe(0)
    // Ниже последней — тоже.
    for (let i = 0; i < 10; i += 1) await fireEvent.keyDown(input, { key: 'ArrowDown' })
    expect(selected()).toBe(3)
  })

  it('Enter исполняет выделенную команду, а не первую', async () => {
    const cmds = fixture()
    render(Palette, { open: true, commands: cmds, t })
    const input = screen.getByPlaceholderText(STR.ru.palPlaceholder)
    await fireEvent.keyDown(input, { key: 'ArrowDown' })
    await fireEvent.keyDown(input, { key: 'Enter' })
    expect(bySlash(cmds, '/search')).toHaveBeenCalledTimes(1)
    expect(bySlash(cmds, '/new')).not.toHaveBeenCalled()
    // Исполненная команда закрывает палитру: человек видит результат, а не список.
    expect(screen.queryByRole('dialog')).toBeNull()
  })

  it('Enter на пустой выдаче не исполняет ничего', async () => {
    const cmds = fixture()
    render(Palette, { open: true, commands: cmds, t })
    const input = screen.getByPlaceholderText(STR.ru.palPlaceholder)
    await fireEvent.input(input, { target: { value: 'zzz' } })
    await fireEvent.keyDown(input, { key: 'Enter' })
    for (const c of cmds) expect(c.action).not.toHaveBeenCalled()
  })

  it('клик по строке исполняет её команду', async () => {
    const cmds = fixture()
    render(Palette, { open: true, commands: cmds, t })
    await fireEvent.click(screen.getByText('/export'))
    expect(bySlash(cmds, '/export')).toHaveBeenCalledTimes(1)
    expect(screen.queryByRole('dialog')).toBeNull()
  })

  it('ввод сбрасывает выделение на первую строку', async () => {
    render(Palette, { open: true, commands: fixture(), t })
    const input = screen.getByPlaceholderText(STR.ru.palPlaceholder)
    await fireEvent.keyDown(input, { key: 'ArrowDown' })
    await fireEvent.keyDown(input, { key: 'ArrowDown' })
    await fireEvent.input(input, { target: { value: 'e' } })
    // Иначе Enter после правки запроса исполнил бы команду, которой человек
    // уже не видит выделенной.
    expect(
      screen.getAllByRole('option')[0].getAttribute('aria-selected'),
    ).toBe('true')
  })

  it('Escape закрывает палитру и сообщает родителю', async () => {
    const onclose = vi.fn()
    render(Palette, { open: true, commands: fixture(), t, onclose })
    await fireEvent.keyDown(screen.getByPlaceholderText(STR.ru.palPlaceholder), {
      key: 'Escape',
    })
    expect(screen.queryByRole('dialog')).toBeNull()
    expect(onclose).toHaveBeenCalledTimes(1)
  })

  it('клик по фону закрывает, клик по панели — нет', async () => {
    const { container, rerender } = render(Palette, {
      open: true,
      commands: fixture(),
      t,
      onclose: vi.fn(),
    })
    await fireEvent.click(container.querySelector('.palette') as HTMLElement)
    expect(screen.getByRole('dialog')).toBeTruthy()
    await fireEvent.click(container.querySelector('.palette-overlay') as HTMLElement)
    expect(screen.queryByRole('dialog')).toBeNull()

    // Повторное открытие показывает полный список: запрос и выделение сброшены,
    // иначе палитра открылась бы с прошлым фильтром.
    await rerender({ open: true, commands: fixture(), t, onclose: vi.fn() })
    expect(screen.getAllByRole('option').length).toBe(4)
    expect((screen.getByPlaceholderText(STR.ru.palPlaceholder) as HTMLInputElement).value).toBe('')
  })
})

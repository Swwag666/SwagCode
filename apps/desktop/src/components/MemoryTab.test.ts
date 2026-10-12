// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen } from '@testing-library/svelte'
import { afterEach, describe, expect, it, vi } from 'vitest'
import MemoryTab, { type MemoryItem } from './MemoryTab.svelte'
import { STR } from '../lib/strings'

afterEach(cleanup)

const items: MemoryItem[] = [
  { id: 2, scope: 'global', text: 'деплой: tools\\build-release.ps1', created_ms: 1_760_000_000_000 },
  { id: 1, scope: 'global', text: 'сборка: cargo test --workspace', created_ms: 1_759_000_000_000 },
]

const props = {
  title: STR.ru.memoryTitle,
  desc: STR.ru.memoryDesc,
  globalLabel: STR.ru.memoryGlobal,
  sessionLabel: STR.ru.memorySession,
  globalHint: STR.ru.memoryGlobalHint,
  sessionHint: STR.ru.memorySessionHint,
  placeholder: STR.ru.memoryPlaceholder,
  addLabel: STR.ru.memoryAdd,
  forgetLabel: STR.ru.memoryForget,
  emptyLabel: STR.ru.memoryEmpty,
  items,
  scope: 'global' as const,
  onScope: vi.fn(),
  onAdd: vi.fn(),
  onForget: vi.fn(),
}

describe('MemoryTab: smoke', () => {
  it('показывает строки памяти с id и подписью области', () => {
    render(MemoryTab, { ...props })
    expect(screen.getByText(STR.ru.memoryTitle)).toBeTruthy()
    expect(screen.getByTestId('memory-list')).toBeTruthy()
    expect(screen.getByText('сборка: cargo test --workspace')).toBeTruthy()
    // id нужен человеку, чтобы сопоставить строку с тем, что удалит агент.
    expect(screen.getByText('#2')).toBeTruthy()
    expect(screen.queryByTestId('memory-empty')).toBeNull()
  })

  it('пустой список говорит об этом, а не оставляет пустоту', () => {
    render(MemoryTab, { ...props, items: [] })
    expect(screen.getByTestId('memory-empty').textContent).toBe(STR.ru.memoryEmpty)
    expect(screen.queryByTestId('memory-list')).toBeNull()
  })

  it('отправляет текст и очищает черновик', async () => {
    const onAdd = vi.fn()
    render(MemoryTab, { ...props, onAdd })
    const input = screen.getByTestId('memory-input') as HTMLTextAreaElement
    const button = screen.getByTestId('memory-add') as HTMLButtonElement
    // Пустой черновик — кнопка не активна: ядро всё равно отвергло бы.
    expect(button.disabled).toBe(true)
    await fireEvent.input(input, { target: { value: '  владелец любит краткость  ' } })
    expect(button.disabled).toBe(false)
    await fireEvent.click(button)
    expect(onAdd).toHaveBeenCalledWith('владелец любит краткость')
    expect(input.value).toBe('')
  })

  it('Ctrl+Enter сохраняет, обычный Enter оставляет перевод строки', async () => {
    const onAdd = vi.fn()
    render(MemoryTab, { ...props, onAdd })
    const input = screen.getByTestId('memory-input') as HTMLTextAreaElement
    await fireEvent.input(input, { target: { value: 'факт' } })
    await fireEvent.keyDown(input, { key: 'Enter', ctrlKey: true })
    expect(onAdd).toHaveBeenCalledWith('факт')
    await fireEvent.input(input, { target: { value: 'вторая\nстрока' } })
    await fireEvent.keyDown(input, { key: 'Enter' })
    expect(onAdd).toHaveBeenCalledTimes(1)
  })

  it('переключение области уходит родителю и меняет подсказку', async () => {
    const onScope = vi.fn()
    render(MemoryTab, { ...props, onScope })
    expect(screen.getByText(STR.ru.memoryGlobalHint)).toBeTruthy()
    await fireEvent.click(screen.getByTestId('scope-session'))
    expect(onScope).toHaveBeenCalledWith('session')
  })

  it('удаление передаёт id строки', async () => {
    const onForget = vi.fn()
    const { getAllByTestId } = render(MemoryTab, { ...props, onForget })
    await fireEvent.click(getAllByTestId('memory-forget')[0])
    expect(onForget).toHaveBeenCalledWith(2)
  })

  it('в состоянии busy кнопки не работают', async () => {
    const onForget = vi.fn()
    const onAdd = vi.fn()
    const { getAllByTestId } = render(MemoryTab, { ...props, busy: true, onForget, onAdd })
    await fireEvent.click(getAllByTestId('memory-forget')[0])
    expect(onForget).not.toHaveBeenCalled()
    await fireEvent.input(screen.getByTestId('memory-input'), { target: { value: 'факт' } })
    const add = screen.getByTestId('memory-add') as HTMLButtonElement
    expect(add.disabled).toBe(true)
    await fireEvent.click(add)
    expect(onAdd).not.toHaveBeenCalled()
  })

  it('показывает статус от родителя', () => {
    render(MemoryTab, { ...props, status: STR.ru.memoryAdded })
    expect(screen.getByTestId('memory-status').textContent).toBe(STR.ru.memoryAdded)
  })
})

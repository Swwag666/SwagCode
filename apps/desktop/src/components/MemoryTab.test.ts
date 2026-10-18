// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen } from '@testing-library/svelte'
import { tick } from 'svelte'
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

  /* F-11: ветка metaKey не была закрыта ни одним тестом. На macOS жест тот же,
     что Ctrl+Enter — память добавляют одной рукой, не отрываясь от чтения;
     если при правке оставить один ctrlKey, сохранение молча сломается только
     на Mac, и прежние тесты этого не заметили бы. */
  it('Cmd+Enter сохраняет так же, как Ctrl+Enter', async () => {
    const onAdd = vi.fn()
    render(MemoryTab, { ...props, onAdd })
    const input = screen.getByTestId('memory-input')
    await fireEvent.input(input, { target: { value: 'факт' } })
    await fireEvent.keyDown(input, { key: 'Enter', metaKey: true })
    expect(onAdd).toHaveBeenCalledWith('факт')
    // Enter без модификатора по-прежнему вставляет перевод строки, а не сохраняет.
    await fireEvent.input(input, { target: { value: 'две\nстроки' } })
    await fireEvent.keyDown(input, { key: 'Enter' })
    expect(onAdd).toHaveBeenCalledTimes(1)
  })

  it('активная область помечена, а не только подсказкой', () => {
    // Подсказка уже проверялась; сам маркер — единственное, что показывает,
    // куда попадёт новая строка, когда подсказку не читают.
    render(MemoryTab, { ...props, scope: 'session' })
    expect(screen.getByTestId('scope-session').classList.contains('active')).toBe(true)
    expect(screen.getByTestId('scope-global').classList.contains('active')).toBe(false)
  })

  /* ── rev55: выбор сессии и импорт промпта из txt/md ───────────────────────
     Заметки привязаны к одной сессии, но править можно любую: без селектора
     память показывала только активный чат, и чтобы поправить заметки другой
     сессии, её пришлось бы делать активной. */
  const sessionProps = {
    sessions: [
      { id: 's-aaa11111', title: 'Ревизия 54' },
      { id: 's-bbb22222', title: 'D:/SwagCod' },
    ],
    sessionId: 's-bbb22222',
    pickSessionLabel: STR.ru.memoryPickSession,
    pickSessionHint: STR.ru.memoryPickSessionHint,
  }

  it('в области session показывает селектор и передаёт выбор родителю', async () => {
    const onPickSession = vi.fn()
    render(MemoryTab, { ...props, scope: 'session', ...sessionProps, onPickSession })
    const select = screen.getByTestId('memory-session-select') as HTMLSelectElement
    expect(select.value).toBe('s-bbb22222')
    expect(select.options.length).toBe(2)
    await fireEvent.change(select, { target: { value: 's-aaa11111' } })
    expect(onPickSession).toHaveBeenCalledWith('s-aaa11111')
  })

  it('в глобальной области селектора нет: там сессия ни на что не влияет', () => {
    render(MemoryTab, { ...props, scope: 'global', ...sessionProps, onPickSession: vi.fn() })
    expect(screen.queryByTestId('memory-session-select')).toBeNull()
  })

  it('пустой список сессий не рисует пустой селектор', () => {
    render(MemoryTab, { ...props, scope: 'session', sessions: [], onPickSession: vi.fn() })
    expect(screen.queryByTestId('memory-session-select')).toBeNull()
  })

  it('без колбэка выбора селектора нет: компонент не может выбирать сам', () => {
    render(MemoryTab, { ...props, scope: 'session', sessions: sessionProps.sessions })
    expect(screen.queryByTestId('memory-session-select')).toBeNull()
  })

  it('импорт из файла кладёт текст в черновик, сохранение отправляет его дословно', async () => {
    const onAdd = vi.fn()
    const text = '# Роль\nТы ревьюер.\nПроверяй тесты.'
    const onImportFile = vi.fn().mockResolvedValue(text)
    render(MemoryTab, {
      ...props,
      onAdd,
      onImportFile,
      importLabel: STR.ru.memoryFromFile,
      importingLabel: STR.ru.memoryImporting,
    })
    await fireEvent.click(screen.getByTestId('memory-import'))
    await tick()
    const input = screen.getByTestId('memory-input') as HTMLTextAreaElement
    expect(input.value).toBe(text)
    /* Многострочный факт уходит целиком: промпт из файла — не «одна строка»,
       и ломать его переносы на пути в базу нельзя. */
    await fireEvent.keyDown(input, { key: 'Enter', ctrlKey: true })
    expect(onAdd).toHaveBeenCalledWith(text)
  })

  it('отмена диалога импорта не стирает набранный руками черновик', async () => {
    const onImportFile = vi.fn().mockResolvedValue(null)
    render(MemoryTab, { ...props, onImportFile, importLabel: STR.ru.memoryFromFile })
    await fireEvent.input(screen.getByTestId('memory-input'), { target: { value: 'руками' } })
    await fireEvent.click(screen.getByTestId('memory-import'))
    await tick()
    expect(onImportFile).toHaveBeenCalled()
    expect((screen.getByTestId('memory-input') as HTMLTextAreaElement).value).toBe('руками')
  })

  it('без колбэка импорта кнопки нет, а в busy она не работает', async () => {
    render(MemoryTab, { ...props })
    expect(screen.queryByTestId('memory-import')).toBeNull()
    const onImportFile = vi.fn()
    const { getByTestId } = render(MemoryTab, { ...props, busy: true, onImportFile, importLabel: 'x' })
    const btn = getByTestId('memory-import') as HTMLButtonElement
    expect(btn.disabled).toBe(true)
    await fireEvent.click(btn)
    expect(onImportFile).not.toHaveBeenCalled()
  })

  /* ── rev56: третья область — факты рабочей папки ── */

  const folderProps = {
    folderLabel: STR.ru.memoryFolder,
    folderHint: STR.ru.memoryFolderHint,
    folderDir: 'D:/proj-a',
    pickFolderLabel: STR.ru.memoryPickFolder,
    folderNoneLabel: STR.ru.memoryFolderNone,
    browseLabel: STR.ru.memoryBrowse,
  }

  it('чип папки появляется с подписью и переключает область', async () => {
    const onScope = vi.fn()
    /* Без folderLabel чипа нет: пустая подпись оставила бы в ряду дырку,
       а область folder тогда всё равно недоступна. */
    render(MemoryTab, { ...props, onScope })
    expect(screen.queryByTestId('scope-folder')).toBeNull()
    cleanup()
    render(MemoryTab, { ...props, onScope, ...folderProps })
    expect(screen.getByTestId('scope-folder').textContent).toBe(STR.ru.memoryFolder)
    await fireEvent.click(screen.getByTestId('scope-folder'))
    expect(onScope).toHaveBeenCalledWith('folder')
  })

  it('в области folder показаны путь папки и её подсказка', () => {
    render(MemoryTab, { ...props, scope: 'folder', ...folderProps })
    expect(screen.getByTestId('memory-folder-dir').textContent).toBe('D:/proj-a')
    expect(screen.getByText(STR.ru.memoryFolderHint)).toBeTruthy()
    // Селектор сессий в этой области не нужен: факт принадлежит папке.
    expect(screen.queryByTestId('memory-session-select')).toBeNull()
  })

  it('без папки показана честная подпись, а не пустое место', () => {
    render(MemoryTab, { ...props, scope: 'folder', ...folderProps, folderDir: null })
    expect(screen.getByTestId('memory-folder-dir').textContent).toBe(STR.ru.memoryFolderNone)
  })

  it('кнопка выбора папки зовёт колбэк родителя', async () => {
    const onPickFolder = vi.fn().mockResolvedValue(undefined)
    render(MemoryTab, { ...props, scope: 'folder', ...folderProps, onPickFolder })
    await fireEvent.click(screen.getByTestId('memory-folder-browse'))
    expect(onPickFolder).toHaveBeenCalled()
  })

  it('строки выбора папки нет вне области folder и без колбэка', () => {
    render(MemoryTab, { ...props, scope: 'global', ...folderProps, onPickFolder: vi.fn() })
    expect(screen.queryByTestId('memory-folder-browse')).toBeNull()
    cleanup()
    /* Без колбэка кнопка была бы мёртвой: диалог открывает родитель, компонент
       про Tauri не знает. */
    render(MemoryTab, { ...props, scope: 'folder', ...folderProps })
    expect(screen.queryByTestId('memory-folder-browse')).toBeNull()
  })

  /* ── rev56: экспорт факта в txt/md ── */

  it('экспорт отправляет текст конкретной строки, а не весь список', async () => {
    const onExportItem = vi.fn().mockResolvedValue(null)
    render(MemoryTab, { ...props, onExportItem, exportLabel: STR.ru.memoryToFile })
    const buttons = screen.getAllByTestId('memory-export')
    expect(buttons.length).toBe(items.length)
    await fireEvent.click(buttons[0])
    /* Первая строка списка — id 2: порядок в ленте «свежие сверху». */
    expect(onExportItem).toHaveBeenCalledWith('деплой: tools\\build-release.ps1')
  })

  it('без колбэка экспорта кнопки нет, в busy и при занятом экспорте она не работает', async () => {
    render(MemoryTab, { ...props })
    expect(screen.queryByTestId('memory-export')).toBeNull()
    const onExportItem = vi.fn()
    const { getAllByTestId } = render(MemoryTab, {
      ...props,
      exportBusy: true,
      onExportItem,
      exportLabel: STR.ru.memoryToFile,
      exportingLabel: STR.ru.memoryExporting,
    })
    /* Кнопка своя у каждой строки, поэтому берём первую из списка. */
    const btn = getAllByTestId('memory-export')[0] as HTMLButtonElement
    expect(btn.disabled).toBe(true)
    expect(btn.textContent).toBe(STR.ru.memoryExporting)
    await fireEvent.click(btn)
    expect(onExportItem).not.toHaveBeenCalled()
  })

  it('многострочный факт доезжает до экспорта дословно', async () => {
    const text = '# Роль\nТы ревьюер.\n\n1. Проверяй тесты.\n2. Не молчи.'
    const onExportItem = vi.fn().mockResolvedValue(null)
    render(MemoryTab, {
      ...props,
      items: [{ id: 7, scope: 'folder', text, created_ms: 1_760_000_000_000, dir: 'D:/proj-a' }],
      onExportItem,
      exportLabel: STR.ru.memoryToFile,
    })
    await fireEvent.click(screen.getByTestId('memory-export'))
    expect(onExportItem).toHaveBeenCalledWith(text)
  })
})

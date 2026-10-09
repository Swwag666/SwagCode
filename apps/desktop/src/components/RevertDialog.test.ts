// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen } from '@testing-library/svelte'
import { afterEach, describe, expect, it, vi } from 'vitest'
import RevertDialog from './RevertDialog.svelte'
import { STR } from '../lib/strings'

// globals выключены: размонтируем сами, иначе screen находит дубликаты.
afterEach(cleanup)

describe('RevertDialog: smoke', () => {
  const props = {
    title: STR.ru.revertTitle,
    turnLabel: `${STR.ru.turn} t-1a2b3c4d`,
    sha: '0123456789',
    warning: STR.ru.revertWarning,
    confirmLabel: STR.ru.revertConfirm,
    cancelLabel: STR.ru.revertCancel,
    busyLabel: STR.ru.revertBusy,
  }

  it('рендерит диалог с подписью хода и sha снимка', () => {
    render(RevertDialog, { ...props, onConfirm: vi.fn(), onCancel: vi.fn() })
    const dlg = screen.getByRole('dialog')
    expect(dlg).toBeTruthy()
    // Человек видит, к какому именно снимку вернётся.
    expect(dlg.textContent).toContain('0123456789')
    expect(dlg.textContent).toContain('t-1a2b3c4d')
    // И честное предупреждение о том, чего откат НЕ делает.
    expect(screen.getByText(props.warning)).toBeTruthy()
  })

  it('кнопки возвращают решение родителю', async () => {
    const onConfirm = vi.fn()
    const onCancel = vi.fn()
    render(RevertDialog, { ...props, onConfirm, onCancel })
    await fireEvent.click(screen.getByTestId('revert-confirm'))
    await fireEvent.click(screen.getByText(props.cancelLabel))
    expect(onConfirm).toHaveBeenCalledTimes(1)
    expect(onCancel).toHaveBeenCalledTimes(1)
  })

  it('в работе обе кнопки запрещены и подписаны busy-текстом', async () => {
    const onConfirm = vi.fn()
    const onCancel = vi.fn()
    render(RevertDialog, { ...props, busy: true, onConfirm, onCancel })
    const confirm = screen.getByTestId('revert-confirm') as HTMLButtonElement
    const cancel = screen.getByText(props.cancelLabel) as HTMLButtonElement
    expect(confirm.disabled).toBe(true)
    expect(cancel.disabled).toBe(true)
    expect(confirm.textContent?.trim()).toBe(props.busyLabel)
    // Клик по запрещённой кнопке не рождает второго отката.
    await fireEvent.click(confirm)
    expect(onConfirm).not.toHaveBeenCalled()
  })

  it('без busyLabel показывает обычную подпись', () => {
    const { busyLabel: _omit, ...rest } = props
    render(RevertDialog, { ...rest, busy: true, onConfirm: vi.fn(), onCancel: vi.fn() })
    expect(screen.getByTestId('revert-confirm').textContent?.trim()).toBe(props.confirmLabel)
  })
})

describe('strings: ключи отката есть в обоих языках', () => {
  it('ru и en покрывают подписи диалога', () => {
    for (const key of [
      'revertTitle',
      'revertWarning',
      'revertConfirm',
      'revertCancel',
      'revertBusy',
      'revertButton',
      'revertDone',
      'revertFailed',
    ] as const) {
      expect(typeof STR.ru[key]).toBe('string')
      expect(STR.ru[key].length).toBeGreaterThan(0)
      expect(typeof STR.en[key]).toBe('string')
      expect(STR.en[key].length).toBeGreaterThan(0)
    }
  })
})

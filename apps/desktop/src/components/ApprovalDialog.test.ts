// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen } from '@testing-library/svelte'
import { afterEach, describe, expect, it, vi } from 'vitest'
import ApprovalDialog from './ApprovalDialog.svelte'
import Icon from './Icon.svelte'
import { STR, translate } from '../lib/strings'

// globals выключены, поэтому авто-размонтирования нет: снимаем сами,
// иначе рендеры копятся и screen находит дубликаты из прошлого теста.
afterEach(cleanup)

describe('ApprovalDialog: smoke', () => {
  const props = {
    summary: 'bash {"command":"rm -rf tmp"}',
    title: 'Подтверждение действия',
    hint: 'агент просит опасный инструмент',
    approveLabel: 'Подтвердить',
    denyLabel: 'Отклонить',
  }

  it('рендерит сводку вызова целиком', () => {
    render(ApprovalDialog, { ...props, onRespond: vi.fn() })
    expect(screen.getByRole('dialog')).toBeTruthy()
    expect(screen.getByText(/bash/)).toBeTruthy()
  })

  it('кнопки возвращают решение родителю', async () => {
    const onRespond = vi.fn()
    render(ApprovalDialog, { ...props, onRespond })
    await fireEvent.click(screen.getByText('Подтвердить'))
    await fireEvent.click(screen.getByText('Отклонить'))
    expect(onRespond.mock.calls.map((c) => c[0])).toEqual(['approved', 'denied'])
  })

  /* F-6: с preview диалог показывает саму правку, а не только JSON вызова;
     без preview (bash, fetch_url) остаётся прежняя сводка. */
  it('показывает diff, когда ядро прислало preview', () => {
    render(ApprovalDialog, {
      ...props,
      preview: '@@ -1,3 +1,3 @@\n a\n-b\n+B\n',
      onRespond: vi.fn(),
    })
    expect(screen.getByTestId('diff-preview')).toBeTruthy()
    expect(screen.getByTestId('dp-adds').textContent).toBe('+1')
    expect(screen.getByTestId('dp-dels').textContent).toBe('−1')
    // Сводка остаётся: из неё видно имя инструмента и путь.
    expect(screen.getByText(/bash/)).toBeTruthy()
  })

  it('без preview diff-блок не рисуется', () => {
    render(ApprovalDialog, { ...props, onRespond: vi.fn() })
    expect(screen.queryByTestId('diff-preview')).toBeNull()
  })
})

describe('Icon: smoke', () => {
  it('рисует svg без падений на основных именах', () => {
    for (const name of ['alert', 'brain', 'folder', 'plus', 'close'] as const) {
      const { container } = render(Icon, { name, size: 16 })
      expect(container.querySelector('svg')).toBeTruthy()
    }
  })
})

describe('strings: полнота переводов', () => {
  it('en покрывает все ключи ru', () => {
    const ru = Object.keys(STR.ru)
    const en = Object.keys(STR.en)
    expect(en.sort()).toEqual(ru.sort())
  })
  it('фолбэк возвращает ru при любом языке', () => {
    expect(translate('en', 'newSession')).toBe(STR.en.newSession)
    expect(translate('ru', 'newSession')).toBe(STR.ru.newSession)
  })
})

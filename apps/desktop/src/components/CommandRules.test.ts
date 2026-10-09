// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen } from '@testing-library/svelte'
import { afterEach, describe, expect, it, vi } from 'vitest'
import CommandRules from './CommandRules.svelte'

// globals выключены: размонтируем сами, иначе screen находит дубликаты.
afterEach(cleanup)

const base = {
  title: 'Списки команд',
  desc: 'deny > allow > политика',
  allowLabel: 'Разрешить без диалога',
  denyLabel: 'Запретить всегда',
  allowHint: 'по правилу на строку',
  denyHint: 'regex, регистр не важен',
  saveLabel: 'Сохранить',
  resetLabel: 'Дефолты',
  savedLabel: 'Сохраняю…',
  allow: ['^git status', '^cargo test'],
  deny: ['\\bshutdown\\b'],
  allowDefaults: ['^git status', '^ls'],
  denyDefaults: ['\\bshutdown\\b', 'rm -rf /'],
}

describe('CommandRules (F-3): редактор списков', () => {
  it('показывает действующие правила в двух списках', () => {
    render(CommandRules, { ...base, onSave: vi.fn() })
    const allow = screen.getByLabelText('Разрешить без диалога') as HTMLTextAreaElement
    const deny = screen.getByLabelText('Запретить всегда') as HTMLTextAreaElement
    expect(allow.value).toBe('^git status\n^cargo test')
    expect(deny.value).toBe('\\bshutdown\\b')
    // Счётчики правил видны: человек должен понимать, сколько строк работает.
    expect(screen.getByText('2')).toBeTruthy()
    expect(screen.getByText('1')).toBeTruthy()
  })

  it('сохранение отдаёт родителю чистые строки', async () => {
    const onSave = vi.fn()
    render(CommandRules, { ...base, onSave })
    const allow = screen.getByLabelText('Разрешить без диалога')
    await fireEvent.input(allow, { target: { value: '  ^npm test  \n\n# комментарий\n' } })
    await fireEvent.click(screen.getByText('Сохранить'))
    expect(onSave).toHaveBeenCalledOnce()
    expect(onSave.mock.calls[0][0]).toEqual(['^npm test', '# комментарий'])
    // deny не тронут — уходит как есть.
    expect(onSave.mock.calls[0][1]).toEqual(['\\bshutdown\\b'])
  })

  it('кнопка сброса подставляет дефолты, но не сохраняет их', async () => {
    const onSave = vi.fn()
    render(CommandRules, { ...base, onSave })
    await fireEvent.click(screen.getByText('Дефолты'))
    const allow = screen.getByLabelText('Разрешить без диалога') as HTMLTextAreaElement
    const deny = screen.getByLabelText('Запретить всегда') as HTMLTextAreaElement
    expect(allow.value).toBe('^git status\n^ls')
    expect(deny.value).toBe('\\bshutdown\\b\nrm -rf /')
    expect(onSave).not.toHaveBeenCalled()
  })

  it('показывает статус и блокирует кнопки на время сохранения', () => {
    render(CommandRules, { ...base, busy: true, status: 'битый regex: (oops', onSave: vi.fn() })
    expect(screen.getByRole('status').textContent).toContain('битый regex')
    expect((screen.getByText('Сохраняю…') as HTMLButtonElement).disabled).toBe(true)
  })

  it('пустые списки валидны: правил нет, сохранение уходит с пустыми массивами', async () => {
    const onSave = vi.fn()
    render(CommandRules, { ...base, allow: [], deny: [], onSave })
    const allow = screen.getByLabelText('Разрешить без диалога') as HTMLTextAreaElement
    expect(allow.value).toBe('')
    await fireEvent.click(screen.getByText('Сохранить'))
    expect(onSave.mock.calls[0]).toEqual([[], []])
  })
})

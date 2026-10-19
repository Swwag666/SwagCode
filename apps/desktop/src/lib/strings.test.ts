/* Словарь строк: паритет ru/en и перевод секций настроек.
 *
 * rev61: настройки были наполовину английскими - подписи уведомлений,
 * обслуживания и правил папки писались литералами прямо в разметке, поэтому
 * перевод жил в одном месте, а английский в другом. Эти проверки держат
 * словарь целым: пропущенный en-ключ и вернувшийся английский литерал в ru
 * ловятся тестом, а не глазом. */
import { describe, it, expect } from 'vitest'

import { STR, translate } from './strings'

describe('словарь строк', () => {
  it('ru и en держат один и тот же набор ключей', () => {
    const ru = Object.keys(STR.ru).sort()
    const en = Object.keys(STR.en).sort()
    expect(en).toEqual(ru)
  })

  it('rev61: секции настроек переведены, а не оставлены английскими', () => {
    const localized = [
      'bgEffectsTitle',
      'bgModeTitle',
      'notifTitle',
      'notifSuppressTitle',
      'notifVolumeTitle',
      'notifActionBoth',
      'maintTitle',
      'cleanupMaxSessions',
      'cleanupMaxAge',
      'rulesFolderTitle',
      'rulesGlobalTitle',
      'rulesPromptTitle',
      'rulesTtlTitle',
      'ruleFieldAllow',
      'ruleFieldSkills',
      'skillsVisibleChip',
    ] as const

    for (const key of localized) {
      const ru = STR.ru[key]
      const en = STR.en[key]
      expect(ru, `ru ${key}: должен быть по-русски`).toMatch(/[А-Яа-яЁё]/)
      expect(en, `en ${key}: должен быть по-английски`).toMatch(/[A-Za-z]/)
      expect(ru, `ru ${key}: не должен повторять английский`).not.toBe(en)
    }
  })

  it('ключи с плейсхолдером не теряют его при переводе', () => {
    /* Подстановка идёт через .replace('{msg}', ...): потеря маркера в одном из
       языков оставила бы владельца без причины ошибки. */
    for (const key of ['notifLoadFailed', 'notifSaveFailed', 'notifTestFailed'] as const) {
      expect(STR.ru[key], `ru ${key}`).toContain('{msg}')
      expect(STR.en[key], `en ${key}`).toContain('{msg}')
    }
    expect(STR.ru.rulesResolvedTitle).toContain('{dir}')
    expect(STR.en.rulesResolvedTitle).toContain('{dir}')
  })

  it('translate отдаёт строку своего языка', () => {
    expect(translate('ru', 'notifTest')).toBe('Проверить')
    expect(translate('en', 'notifTest')).toBe('Test')
  })
})
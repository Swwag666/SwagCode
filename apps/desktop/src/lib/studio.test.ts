import { describe, expect, it } from 'vitest'
import { BG_MIME, bgKindFor, hexToRgb, mixHex } from './studio'

describe('hexToRgb', () => {
  it('разбирает hex с решёткой и без', () => {
    expect(hexToRgb('#ff2d44')).toEqual([255, 45, 68])
    expect(hexToRgb('0e9384')).toEqual([14, 147, 132])
  })
  it('мусор возвращает null, а не исключение', () => {
    expect(hexToRgb('красный')).toBeNull()
    expect(hexToRgb('#fff')).toBeNull()
    expect(hexToRgb('')).toBeNull()
  })
})

describe('mixHex', () => {
  it('t=0 и t=1 дают концы отрезка', () => {
    expect(mixHex('#000000', '#ffffff', 0)).toBe('#000000')
    expect(mixHex('#000000', '#ffffff', 1)).toBe('#ffffff')
  })
  it('середина — среднее каналов', () => {
    expect(mixHex('#000000', '#ffffff', 0.5)).toBe('#808080')
  })
  it('невалидный вход не роняет: возвращается a', () => {
    expect(mixHex('мусор', '#ffffff', 0.5)).toBe('мусор')
  })
})

describe('медиафон', () => {
  it('mime по расширению', () => {
    expect(BG_MIME.gif).toBe('image/gif')
    expect(BG_MIME.mp4).toBe('video/mp4')
  })
  it('video только для mp4/webm', () => {
    expect(bgKindFor('a.mp4')).toBe('video')
    expect(bgKindFor('A.WEBM')).toBe('video')
    expect(bgKindFor('eye.gif')).toBe('image')
    expect(bgKindFor('без расширения')).toBe('image')
  })
})

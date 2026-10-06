/**
 * Утилиты студии тем: цвет из hex, смешение цветов, mime медиафона.
 * Чистые функции без DOM — поэтому тестируются в node без jsdom.
 */

/** '#rrggbb' или 'rrggbb' в тройной канал; мусор — null, а не исключение. */
export function hexToRgb(hex: string): [number, number, number] | null {
  const m = /^#?([0-9a-f]{6})$/i.exec(hex.trim())
  if (!m) return null
  const n = parseInt(m[1], 16)
  return [(n >> 16) & 255, (n >> 8) & 255, n & 255]
}

/** Линейная смесь двух hex: t=0 даёт a, t=1 даёт b. Невалидный вход — a. */
export function mixHex(a: string, b: string, t: number): string {
  const ca = hexToRgb(a)
  const cb = hexToRgb(b)
  if (!ca || !cb) return a
  const ch = ca.map((v, i) => Math.round(v + (cb[i] - v) * t))
  return '#' + ch.map((v) => v.toString(16).padStart(2, '0')).join('')
}

/** Mime медиафона по расширению: blob без типа не проиграется в video. */
export const BG_MIME: Record<string, string> = {
  gif: 'image/gif',
  mp4: 'video/mp4',
  webm: 'video/webm',
  png: 'image/png',
  jpg: 'image/jpeg',
  jpeg: 'image/jpeg',
  webp: 'image/webp',
  apng: 'image/apng',
}

/** Видео или картинка: от этого зависит тег на фоне (video против img). */
export function bgKindFor(name: string): 'video' | 'image' {
  const ext = name.split('.').pop()?.toLowerCase() || ''
  return ext === 'mp4' || ext === 'webm' ? 'video' : 'image'
}

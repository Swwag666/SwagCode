<script lang="ts">
  /**
   * Фоновый декоративный слой — wireframe sphere + scan lines + noise (D-015).
   *
   * Референсы: вращающаяся wireframe-сфера (GIF), CRT scan lines,
   * хроматическая аберрация (Tokyo Ghoul glitch), noise/grain.
   *
   * Условия (D-015):
   * 1. Изоляция: canvas в отдельном элементе с contain: strict.
   * 2. Отключаемо: проп `enabled`.
   * 3. Пауза на blur.
   * 4. Бюджет: 30 fps для фона.
   */
  import { onMount } from 'svelte'

  interface Props {
    enabled: boolean
    /** Ускорение вращения при активном стриме. */
    intensity?: number
    /** Режим фона: sphere или matrix. */
    mode?: 'sphere' | 'matrix'
    /** resolved-тема: фон должен совпадать с --bg, иначе тема «не меняется». */
    theme?: 'dark' | 'light' | 'contrast'
  }

  let { enabled, intensity = 1, mode = 'sphere', theme = 'dark' }: Props = $props()

  /** База под trail/очистку: совпадает с фоном темы. */
  const baseFill = $derived(theme === 'light' ? '242, 242, 246' : '10, 10, 13')
  /** В светлой теме декоративный слой почти незаметен — иначе давит на текст. */
  const layerOpacity = $derived(theme === 'light' ? 0.18 : 0.9)
  /** Акцент декора совпадает с акцентом темы: светлая — сине-зелёная целиком. */
  const accentRgb = $derived(
    theme === 'light' ? '14, 147, 132' : theme === 'contrast' ? '255, 59, 82' : '255, 45, 68',
  )

  let canvas: HTMLCanvasElement | undefined = $state()
  // C-5 фикс: rafId/lastFrame/time — обычные let, не $state.
  // Реактивность нужна только visible/пропсам.
  let rafId = 0
  let lastFrame = 0
  let visible = $state(true)
  let time = 0

  const FRAME_INTERVAL = 1000 / 30
  const SPHERE_RADIUS = 0.35 // относительно min(width, height)
  const LAT_LINES = 12
  const LON_LINES = 18
  const POINTS_PER_LINE = 40
  const PARTICLE_COUNT = 60
  const MATRIX_CHARS = '01アイウエオカキクケコサシスセソタチツテトナニヌネノ'
  const MATRIX_FONT_SIZE = 12

  let ctx: CanvasRenderingContext2D | null = null
  let width = 0
  let height = 0

  /** Частицы, летающие вокруг сферы. */
  interface Particle {
    angle: number
    radius: number
    speed: number
    size: number
    alpha: number
    yOffset: number
  }

  let particles: Particle[] = []

  /** Matrix rain: колонки с падающими символами. */
  let matrixColumns: number[] = []

  function initParticles(): void {
    particles = []
    for (let i = 0; i < PARTICLE_COUNT; i++) {
      particles.push({
        angle: Math.random() * Math.PI * 2,
        radius: SPHERE_RADIUS * (1.1 + Math.random() * 0.8),
        speed: 0.002 + Math.random() * 0.006,
        size: 0.5 + Math.random() * 1.5,
        alpha: 0.1 + Math.random() * 0.4,
        yOffset: (Math.random() - 0.5) * 0.6,
      })
    }
  }

  function initMatrix(): void {
    const cols = Math.ceil(width / MATRIX_FONT_SIZE)
    // Колонки стартуют распределёнными по всей высоте: дождь виден сразу,
    // а не после нескольких секунд пустой чёрной подложки.
    const rows = Math.max(1, Math.ceil(height / MATRIX_FONT_SIZE))
    matrixColumns = Array.from({ length: cols }, () => Math.random() * rows)
  }

  // Смена режима пересевает колонки под текущий размер canvas.
  $effect(() => {
    if (mode === 'matrix' && width > 0) initMatrix()
  })

  function resize(): void {
    if (!canvas) return
    const dpr = Math.min(window.devicePixelRatio || 1, 2)
    width = canvas.clientWidth
    height = canvas.clientHeight
    canvas.width = width * dpr
    canvas.height = height * dpr
    ctx = canvas.getContext('2d')
    // setTransform, а не scale: resize вызывается ResizeObserver'ом repeatedly,
    // и накопленный scale уводил всю отрисовку за пределы canvas.
    if (ctx) ctx.setTransform(dpr, 0, 0, dpr, 0, 0)
    initMatrix()
  }

  /**
   * Проекция 3D точки на 2D с перспективой.
   */
  function project(
    x: number,
    y: number,
    z: number,
    cx: number,
    cy: number,
    radius: number
  ): { px: number; py: number; depth: number } {
    const fov = 3.5
    const scale = fov / (fov + z)
    return {
      px: cx + x * radius * scale,
      py: cy + y * radius * scale,
      depth: z,
    }
  }

  /**
   * Вращение точки вокруг оси Y.
   */
  function rotateY(x: number, z: number, angle: number): [number, number] {
    const cos = Math.cos(angle)
    const sin = Math.sin(angle)
    return [x * cos - z * sin, x * sin + z * cos]
  }

  /**
   * Вращение точки вокруг оси X.
   */
  function rotateX(y: number, z: number, angle: number): [number, number] {
    const cos = Math.cos(angle)
    const sin = Math.sin(angle)
    return [y * cos - z * sin, y * sin + z * cos]
  }

  function drawSphere(t: number): void {
    if (!ctx) return

    const cx = width / 2
    const cy = height / 2
    const radius = Math.min(width, height) * SPHERE_RADIUS
    const rotY = t * 0.4 * intensity // вращение ускоряется при стриме
    const rotX = Math.sin(t * 0.2 * intensity) * 0.3

    ctx.lineWidth = 0.5

    // Широты
    for (let i = 0; i <= LAT_LINES; i++) {
      const phi = (i / LAT_LINES) * Math.PI - Math.PI / 2
      const r = Math.cos(phi)
      let y = Math.sin(phi)

      ctx.beginPath()
      let started = false
      for (let j = 0; j <= POINTS_PER_LINE; j++) {
        const theta = (j / POINTS_PER_LINE) * Math.PI * 2
        let x = r * Math.cos(theta)
        let z = r * Math.sin(theta)

        // Вращение
        ;[x, z] = rotateY(x, z, rotY)
        ;[y, z] = rotateX(y, z, rotX)

        const { px, py } = project(x, y, z, cx, cy, radius)

        if (!started) {
          ctx.moveTo(px, py)
          started = true
        } else {
          ctx.lineTo(px, py)
        }
      }
      ctx.strokeStyle = `rgba(${accentRgb}, ${0.3 + Math.sin(t + i) * 0.08})`
      ctx.stroke()
    }

    // Долготы
    for (let i = 0; i < LON_LINES; i++) {
      const theta = (i / LON_LINES) * Math.PI * 2

      ctx.beginPath()
      let started = false
      for (let j = 0; j <= POINTS_PER_LINE; j++) {
        const phi = (j / POINTS_PER_LINE) * Math.PI - Math.PI / 2
        let x = Math.cos(phi) * Math.cos(theta)
        let y = Math.sin(phi)
        let z = Math.cos(phi) * Math.sin(theta)

        ;[x, z] = rotateY(x, z, rotY)
        ;[y, z] = rotateX(y, z, rotX)

        const { px, py } = project(x, y, z, cx, cy, radius)

        if (!started) {
          ctx.moveTo(px, py)
          started = true
        } else {
          ctx.lineTo(px, py)
        }
      }
      ctx.strokeStyle = `rgba(${accentRgb}, ${0.2 + Math.sin(t * 1.3 + i) * 0.06})`
      ctx.stroke()
    }
  }

  function drawScanLines(): void {
    if (!ctx) return
    // Горизонтальные линии каждые 3px — CRT эффект
    ctx.fillStyle = 'rgba(0, 0, 0, 0.06)'
    for (let y = 0; y < height; y += 3) {
      ctx.fillRect(0, y, width, 1)
    }
  }

  function drawNoise(): void {
    if (!ctx) return
    // Лёгкий шум — несколько случайных точек
    const count = 20
    for (let i = 0; i < count; i++) {
      const x = Math.random() * width
      const y = Math.random() * height
      const alpha = Math.random() * 0.03
      ctx.fillStyle = `rgba(255, 255, 255, ${alpha})`
      ctx.fillRect(x, y, 1, 1)
    }
  }

  function drawVignette(): void {
    if (!ctx) return
    // Виньетка: затемнение по краям
    const gradient = ctx.createRadialGradient(
      width / 2, height / 2, Math.min(width, height) * 0.3,
      width / 2, height / 2, Math.max(width, height) * 0.7
    )
    gradient.addColorStop(0, 'rgba(0, 0, 0, 0)')
    gradient.addColorStop(1, 'rgba(0, 0, 0, 0.4)')
    ctx.fillStyle = gradient
    ctx.fillRect(0, 0, width, height)
  }

  function drawParticles(t: number): void {
    if (!ctx) return
    const cx = width / 2
    const cy = height / 2
    const baseRadius = Math.min(width, height)

    for (const p of particles) {
      p.angle += p.speed * intensity
      const x = cx + Math.cos(p.angle) * p.radius * baseRadius
      const y = cy + Math.sin(p.angle) * p.radius * baseRadius * 0.4 + p.yOffset * baseRadius * 0.3
      const pulse = 0.5 + Math.sin(t * 2 + p.angle * 3) * 0.5

      ctx.beginPath()
      ctx.arc(x, y, p.size * (0.8 + pulse * 0.4), 0, Math.PI * 2)
      ctx.fillStyle = `rgba(${accentRgb}, ${p.alpha * pulse})`
      ctx.fill()
    }
  }

  function drawMatrix(): void {
    if (!ctx) return
    // Защита от пустого набора колонок (resize до layout): иначе режим
    // выглядит как «чёрный прямоугольник без дождя».
    if (matrixColumns.length === 0 && width > 0) initMatrix()
    // N-6 фикс: trail-очистка делается в draw(), здесь не дублируем

    ctx.font = `${MATRIX_FONT_SIZE}px monospace`

    for (let i = 0; i < matrixColumns.length; i++) {
      const char = MATRIX_CHARS[Math.floor(Math.random() * MATRIX_CHARS.length)]
      const x = i * MATRIX_FONT_SIZE
      const y = matrixColumns[i] * MATRIX_FONT_SIZE

      // Голова колонки — ярче
      ctx.fillStyle = `rgba(${accentRgb}, ${0.75 + Math.random() * 0.25})`
      ctx.fillText(char, x, y)

      // Хвост — тусклее
      if (y > MATRIX_FONT_SIZE) {
        ctx.fillStyle = `rgba(${accentRgb}, 0.35)`
        ctx.fillText(
          MATRIX_CHARS[Math.floor(Math.random() * MATRIX_CHARS.length)],
          x,
          y - MATRIX_FONT_SIZE
        )
      }

      // Сброс колонки когда ушла за экран
      if (y > height + 100 && Math.random() > 0.975) {
        matrixColumns[i] = 0
      }
      matrixColumns[i] += 0.5 * intensity
    }
  }

  function draw(timestamp: number): void {
    // N-7 фикс: перепланируем кадр даже при раннем return — если ctx
    // станет доступен после resize(), анимация возобновится
    if (!enabled || !visible) return
    if (!ctx) {
      rafId = requestAnimationFrame(draw)
      return
    }

    if (timestamp - lastFrame < FRAME_INTERVAL) {
      rafId = requestAnimationFrame(draw)
      return
    }
    // N-5 фикс: реальный dt из timestamp, а не хардкод 0.016
    const dt = (timestamp - lastFrame) / 1000
    lastFrame = timestamp
    time += dt

    // Очистка
    if (mode === 'matrix') {
      // Matrix: полупрозрачная очистка для trail
      ctx.fillStyle = `rgba(${baseFill}, 0.12)`
      ctx.fillRect(0, 0, width, height)
      drawMatrix()
    } else {
      // Sphere: полная очистка
      ctx.fillStyle = `rgba(${baseFill}, 1)`
      ctx.fillRect(0, 0, width, height)
      drawSphere(time)
      drawParticles(time)
      drawScanLines()
      drawNoise()
      drawVignette()
    }

    rafId = requestAnimationFrame(draw)
  }

  function start(): void {
    cancelAnimationFrame(rafId)
    rafId = requestAnimationFrame(draw)
  }

  function stop(): void {
    cancelAnimationFrame(rafId)
    // W-3 фикс: очищаем canvas при остановке — последний кадр не висит под UI
    if (ctx) {
      ctx.clearRect(0, 0, width, height)
    }
  }

  onMount(() => {
    if (!canvas) return
    resize()
    initParticles()

    const ro = new ResizeObserver(() => resize())
    ro.observe(canvas)

    const onFocus = () => {
      visible = true
      if (enabled) start()
    }
    const onBlur = () => {
      visible = false
      stop()
    }
    // W-3 фикс: обработка visibilitychange (минимизация не всегда шлёт blur)
    const onVisibility = () => {
      if (document.hidden) {
        visible = false
        stop()
      } else if (enabled) {
        visible = true
        start()
      }
    }
    window.addEventListener('focus', onFocus)
    window.addEventListener('blur', onBlur)
    document.addEventListener('visibilitychange', onVisibility)

    // W-3 фикс: стартуем только если окно в фокусе И видимо
    visible = document.hasFocus() && !document.hidden
    if (enabled && visible) start()

    return () => {
      ro.disconnect()
      window.removeEventListener('focus', onFocus)
      window.removeEventListener('blur', onBlur)
      document.removeEventListener('visibilitychange', onVisibility)
      stop()
    }
  })

  $effect(() => {
    if (enabled && visible) {
      start()
    } else {
      stop()
    }
  })
</script>

<canvas class="bgfx" bind:this={canvas} style:opacity={layerOpacity} aria-hidden="true"></canvas>

<style>
  .bgfx {
    position: fixed;
    inset: 0;
    z-index: 0;
    pointer-events: none;
    contain: strict;
    opacity: 0.7;
  }
</style>

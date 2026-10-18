<script lang="ts">
  /**
   * Виртуализированная транскрипция.
   *
   * Три правила, без которых бюджет fps не сходится (DECISIONS.md §2):
   * 1. В DOM только видимые элементы плюс overscan — никогда весь список.
   * 2. Высота задаётся распорками, реальные высоты измеряются и кэшируются.
   * 3. Автопрокрутка вниз только если пользователь уже у низа.
   */
  import { onMount, tick } from 'svelte'
  import Icon from './Icon.svelte'
  import { renderMarkdown } from '../lib/markdown'
  import {
    ESTIMATED_ITEM_HEIGHT,
    OVERSCAN,
  } from '../lib/virtual'
  import type { TranscriptItem } from '../lib/transcript'

  interface Props {
    items: TranscriptItem[]
    /** Монотонный счётчик изменений: сигнал пересчитать окно. */
    revision: number
    /** Максимум элементов в DOM — защита от pathological случая. */
    maxRendered?: number
    bookmarks?: Set<string>
    onToggleBookmark?: (key: string) => void
    /** Показ мыслей: 'live' — раскрыты, пока размышление стримится,
        'always' — завершённые строки мыслей тоже раскрыты по умолчанию. */
    showThoughts?: 'live' | 'always'
    /** Монотонный счётчик «чат открылся заново» (смена сессии, загрузка
        истории). По нему список принудительно уезжает к низу: scrollTop
        иначе переживает смену сессии и новый чат открывается с дырой. */
    scrollNonce?: number
  }

  let {
    items,
    revision,
    maxRendered = 200,
    bookmarks = new Set(),
    onToggleBookmark,
    showThoughts = 'live',
    scrollNonce = 0,
  }: Props = $props()

  let scroller: HTMLDivElement | undefined = $state()
  let scrollTop = $state(0)
  let viewportHeight = $state(600)
  let stickToBottom = $state(true)

  /* Чат открылся заново (смена сессии, догрузка истории): после первого тика
     список принудительно встаёт на самый низ и возвращает прилипание.
     Старый scrollTop прошлой сессии иначе открывает новый чат «с дырой». */
  let pendingBottom = $state(true)

  $effect(() => {
    void scrollNonce
    if (scrollNonce > 0) pendingBottom = true
  })

  /** Кэш измеренных высот: ключ строки → px. Ключи стабильны внутри сессии,
      поэтому кэш переживает пересборки derived-массива. */
  const heights = new Map<string, number>()
  /** Узлы строк по ключу: по ним снимаем измеренную высоту. */
  let rowNodes = new Map<string, HTMLElement>()
  /** Версия кэша высот: растёт от ResizeObserver строк, чтобы derived
      окна и распорок пересчитывались на живом росте текста, а не только
      на батчах шины. */
  let heightsVersion = $state(0)
  let rowRO: ResizeObserver | null = null

  // C-2 фикс: сбрасываем кэш высот только когда набор ДЕЙСТВИТЕЛЬНО новый
  // (другая сессия, очистка, поиск), а не на каждую пересборку derived-массива.
  let lastFirstKey: string | null = null
  $effect(() => {
    const first = items[0]?.key ?? null
    if (first !== lastFirstKey) {
      lastFirstKey = first
      heights.clear()
      rowNodes.clear()
    }
  })

  /** Строка окна рендера: индекс элемента плюс готовый ключ для each. */
  interface WinRow {
    i: number
    item: TranscriptItem
    key: string
  }

  interface VWin {
    start: number
    end: number
    /** Распорка НАД окном (нерендеренная верхняя часть истории), px. */
    offsetTop: number
    /** Распорка ПОД окном, px. */
    bottomPx: number
    totalHeight: number
    rows: WinRow[]
  }

  /**
   * Окно виртуализации сверху вниз, с гарантией покрытия вьюпорта.
   *
   * Главная болезнь прошлой версии: старт окна искался по ОЦЕНОЧНЫМ высотам,
   * а измеренные строки оказывались ниже оценок - и над окном повисала дыра
   * в распорку. Здесь после построения окна оно добивается вверх, пока
   * суммарная высота строк не покроет вьюпорт: дыра конструктивно невозможна,
   * а скролл остаётся обычным (колесо, скроллбар и клавиши не инвертированы).
   */
  let win: VWin = $derived.by(() => {
    void revision
    void items.length
    void heightsVersion
    const n = items.length
    if (n <= 0) {
      return { start: 0, end: 0, offsetTop: 0, bottomPx: 0, totalHeight: 0, rows: [] }
    }
    const heightOf = (i: number): number => {
      const it = items[i]
      const h = it ? heights.get(it.key) : undefined
      return h ?? ESTIMATED_ITEM_HEIGHT
    }
    let total = 0
    let start = 0
    let offsetTop = 0
    let found = false
    for (let i = 0; i < n; i++) {
      const h = heightOf(i)
      if (!found && scrollTop < total + h) {
        start = i
        offsetTop = total
        found = true
      }
      total += h
    }
    if (!found) {
      start = n - 1
      offsetTop = total - heightOf(start)
    }
    const rawStart = start
    start = Math.max(0, start - OVERSCAN)
    for (let i = start; i < rawStart; i++) offsetTop -= heightOf(i)
    let acc = offsetTop
    let end = start
    const bottom = scrollTop + viewportHeight
    while (end < n && acc < bottom) {
      acc += heightOf(end)
      end++
    }
    end = Math.min(n, end + OVERSCAN)
    // Покрытие: добиваем окно вверх, пока измеренные/оценочные высоты строк
    // не заполнят вьюпорт целиком. Иначе над окном светится распорка-дыра.
    let rendered = 0
    for (let i = start; i < end; i++) rendered += heightOf(i)
    while (start > 0 && rendered < viewportHeight) {
      start--
      const h = heightOf(start)
      rendered += h
      offsetTop -= h
    }
    let rows: WinRow[] = []
    for (let i = start; i < end; i++) {
      const item = items[i]
      if (item) rows.push({ i, item, key: `${item.key}-${item.ver ?? 0}` })
    }
    if (rows.length > maxRendered) {
      // Потолок DOM: режем СВЕРХУ нельзя - отрежем низ экрана; режем низ
      // только в патологическом случае, когда окно и так огромно.
      rows = rows.slice(rows.length - maxRendered)
      start = rows[0].i
      offsetTop = 0
      for (let i = 0; i < start; i++) offsetTop += heightOf(i)
      end = rows[rows.length - 1].i + 1
    }
    rendered = 0
    for (const r of rows) rendered += heightOf(r.i)
    const bottomPx = Math.max(0, total - offsetTop - rendered)
    return { start, end, offsetTop, bottomPx, totalHeight: total, rows }
  })

  function onScroll(e: Event): void {
    const el = e.currentTarget as HTMLDivElement
    scrollTop = el.scrollTop
    viewportHeight = el.clientHeight
    // Прилипание считаем по РЕАЛЬНОЙ высоте DOM: во время стрима оценка
    // отстаёт от отрисовки, и список «откидывало вверх».
    stickToBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 64
  }

  /** Снять реальные высоты отрендеренных строк в кэш. */
  function measure(): void {
    for (const r of win.rows) {
      const node = rowNodes.get(r.item.key)
      if (!node) continue
      const measured = node.getBoundingClientRect().height
      if (measured <= 0) continue
      const prev = heights.get(r.item.key)
      if (prev !== undefined && Math.abs(prev - measured) < 1) continue
      heights.set(r.item.key, measured)
      heightsVersion++
    }
  }

  /**
   * Action-регистрация узла строки.
   *
   * Через action, а не `bind:this={(n) => ...}`: Svelte принимает в bind:this
   * только идентификатор, обращение к свойству или пару {get,set}, но не
   * произвольный колбэк. Заодно destroy гарантирует чистку кэша при уходе
   * строки из виртуального окна — иначе map рос бы бесконечно.
   */
  function trackRow(node: HTMLElement, key: string): { update: (k: string) => void; destroy: () => void } {
    let current = key
    rowNodes.set(current, node)
    rowRO?.observe(node)
    return {
      update(next: string) {
        if (next === current) return
        rowNodes.delete(current)
        current = next
        rowNodes.set(current, node)
      },
      destroy() {
        rowRO?.unobserve(node)
        rowNodes.delete(current)
      },
    }
  }

  function labelOf(kind: string): string {
    switch (kind) {
      case 'user':
        return 'вы'
      case 'assistant':
        return 'агент'
      case 'reasoning':
        return 'думает'
      case 'tool_call':
        return 'вызов'
      case 'tool_result':
        return 'результат'
      case 'approval':
        return 'подтвердите'
      case 'error':
        return 'ошибка'
      default:
        return 'статус'
    }
  }

  /* Разворот одного сообщения на весь экран: его можно растянуть и читать
     крупно, не меняя масштаб всего интерфейса. */
  let expanded = $state<TranscriptItem | null>(null)
  let readerScale = $state(1)

  /* Мысли свёрнуты по-умолчанию, как в DeepSeek: ответ виден сразу,
     размышления — за кликом. Ключи элементов, которые раскрыли вручную. */
  let openThoughts = $state<Set<string>>(new Set())

  function toggleThoughts(key: string): void {
    const next = new Set(openThoughts)
    if (next.has(key)) next.delete(key)
    else next.add(key)
    openThoughts = next
  }

  /* Раскрыта ли строка мыслей: пока размышление стримится — раскрыта всегда
     (мысли читаются живьём, настройка show_thoughts='live'). Завершённая
     строка при 'live' свёрнута до клика, при 'always' раскрыта по умолчанию.
     Множество openThoughts в фазе «раскрыта по умолчанию» означает
     «переключили вручную», поэтому один клик тумблера честен в обеих фазах. */
  function thoughtsOpen(item: TranscriptItem): boolean {
    const openByDefault = !item.done || showThoughts === 'always'
    return openByDefault ? !openThoughts.has(item.key) : openThoughts.has(item.key)
  }

  function readerZoomIn(): void {
    readerScale = Math.min(2.5, Math.round((readerScale + 0.1) * 10) / 10)
  }
  function readerZoomOut(): void {
    readerScale = Math.max(0.7, Math.round((readerScale - 0.1) * 10) / 10)
  }
  function readerZoomReset(): void {
    readerScale = 1
  }

  $effect(() => {
    if (!expanded) return
    const onKey = (e: KeyboardEvent): void => {
      if (e.key === 'Escape') expanded = null
      if ((e.ctrlKey || e.metaKey) && (e.key === '+' || e.key === '=')) { e.preventDefault(); readerZoomIn() }
      if ((e.ctrlKey || e.metaKey) && e.key === '-') { e.preventDefault(); readerZoomOut() }
      if ((e.ctrlKey || e.metaKey) && e.key === '0') { e.preventDefault(); readerZoomReset() }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  })

  // Новый батч → измерить высоты и, если нужно, прижать к низу.
  $effect(() => {
    void revision
    void items.length
    if (!scroller) return
    tick().then(() => {
      measure()
      if (!scroller) return
      if (pendingBottom) {
        // Чат открылся заново: встаём на самый низ и возвращаем прилипание,
        // даже если пользователь в прошлой сессии уехал читать историю.
        scroller.scrollTop = scroller.scrollHeight
        scrollTop = scroller.scrollTop
        stickToBottom = true
        pendingBottom = false
      } else if (stickToBottom) {
        scroller.scrollTop = scroller.scrollHeight
        scrollTop = scroller.scrollTop
      }
    })
  })

  onMount(() => {
    if (!scroller) return
    viewportHeight = scroller.clientHeight
    const ro = new ResizeObserver(() => {
      if (!scroller) return
      viewportHeight = scroller.clientHeight
      if (stickToBottom) {
        scroller.scrollTop = scroller.scrollHeight
        scrollTop = scroller.scrollTop
      }
    })
    ro.observe(scroller)

    /* Строки растут на живом стриме: свой ResizeObserver меряет их сам,
       поэтому окно виртуализации и распорки не отстают от текста и ничего
       не обрезается по старой высоте. */
    rowRO = new ResizeObserver(() => {
      measure()
      if (stickToBottom && scroller) {
        scroller.scrollTop = scroller.scrollHeight
        scrollTop = scroller.scrollTop
      }
    })

    // W-10 фикс: делегированный слушатель для кнопки "копировать"
    const onClick = (e: MouseEvent) => {
      const btn = (e.target as HTMLElement).closest('[data-copy]')
      if (!btn) return
      const codeBlock = btn.parentElement?.querySelector('code')
      if (!codeBlock) return
      navigator.clipboard.writeText(codeBlock.textContent || '').then(() => {
        btn.textContent = 'скопировано'
        setTimeout(() => (btn.textContent = 'копировать'), 1500)
      })
    }
    scroller.addEventListener('click', onClick)

    return () => {
      ro.disconnect()
      rowRO?.disconnect()
      rowRO = null
      scroller?.removeEventListener('click', onClick)
    }
  })
</script>

<div class="wrap">
  <div class="scroller" bind:this={scroller} onscroll={onScroll}>
    <!-- Верхняя распорка задаёт позицию видимого окна в полном списке. -->
    <div class="spacer" style="height: {win.offsetTop}px"></div>

    {#each win.rows as r (r.key)}
      {@const item = r.item}
    {#if item}
      <div
        class="row row-{item.kind}"
        class:row-failed={item.ok === false}
        use:trackRow={item.key}
        data-key={item.key}
      >
        <div class="row-head">
          {#if item.kind === 'error' || item.ok === false}
            <span class="row-state state-err" title="ход завершился ошибкой">
              <Icon name="alert" size={13} />
            </span>
          {:else if item.kind === 'reasoning'}
            <span class="row-state state-think" title="размышления модели">
              <Icon name="brain" size={13} />
            </span>
          {:else if item.kind === 'tool_call'}
            <span class="row-state state-tool" title="вызов инструмента">
              <Icon name="wrench" size={12} />
            </span>
          {/if}
          <span class="badge">{labelOf(item.kind)}</span>
          {#if item.kind === 'reasoning'}
            {@const isOpen = thoughtsOpen(item)}
            <button
              class="thoughts-toggle"
              aria-expanded={isOpen}
              onclick={() => toggleThoughts(item.key)}
              title={isOpen ? 'свернуть мысли' : 'раскрыть мысли'}
            >
              <Icon name="chevron-down" size={11} />
              {isOpen ? 'свернуть' : 'раскрыть'}
            </button>
          {/if}
          {#if item.tool}<span class="tool">{item.tool}</span>{/if}
          {#if item.elapsedMs !== undefined}<span class="elapsed">{item.elapsedMs} мс</span>{/if}
          {#if item.ok === false}<span class="failed">ошибка</span>{/if}
          {#if !item.done}<span class="live-dot" aria-hidden="true"></span>{/if}
          <button
            class="bookmark-btn"
            class:active={bookmarks.has(item.key)}
            onclick={() => onToggleBookmark?.(item.key)}
            title="закладка">
            <Icon name={bookmarks.has(item.key) ? 'star' : 'star-o'} size={13} />
          </button>
          <button
            class="expand-btn"
            onclick={() => { expanded = item; readerScale = 1 }}
            title="развернуть на весь экран"
            aria-label="развернуть на весь экран">
            <Icon name="expand" size={13} />
          </button>
        </div>
        <!-- Мысли видны ЖИВЬЁМ, пока модель рассуждает (стрим не завершён):
             «думает» читается в момент рассуждения, а не после хода.
             Готовая строка свёрнута, пока её не раскрыли вручную. Множество
             openThoughts до финиша работает как «свернули вручную», после —
             как «раскрыли вручную», поэтому один клик тумблера честен в
             обоих состояниях. -->
        {#if item.kind !== 'reasoning' || thoughtsOpen(item)}
          <div class="row-body">
            {#if item.kind === 'assistant' || item.kind === 'reasoning'}
              <!-- Маркдаун рендерится ЖИВЬЁМ на каждом батче: «расшифровка»
                   ответа больше не запаздывает до конца хода. -->
              {@html renderMarkdown(item.text || '')}
            {:else}
              {item.text || '\u00a0'}
            {/if}
          </div>
        {/if}
      </div>
    {/if}
    {/each}

    <!-- Нижняя распорка сохраняет полную высоту скролла. -->
    <div class="spacer" style="height: {win.bottomPx}px"></div>
  </div>
</div>

{#if expanded}
  <div
    class="reader-overlay"
    onclick={() => (expanded = null)}
    onkeydown={(e) => { if (e.key === 'Escape') expanded = null }}
    role="presentation"
  >
    <div
      class="reader"
      onclick={(e) => e.stopPropagation()}
      onkeydown={(e) => { if (e.key === 'Escape') expanded = null }}
      role="dialog"
      aria-label="сообщение целиком"
      tabindex="-1"
      style:--reader-scale={readerScale}
    >
      <div class="reader-head">
        {#if expanded.kind === 'error' || expanded.ok === false}
          <span class="row-state state-err" title="ход завершился ошибкой">
            <Icon name="alert" size={13} />
          </span>
        {:else if expanded.kind === 'reasoning'}
          <span class="row-state state-think" title="размышления модели">
            <Icon name="brain" size={13} />
          </span>
        {/if}
        <span class="badge">{labelOf(expanded.kind)}</span>
        {#if expanded.tool}<span class="tool">{expanded.tool}</span>{/if}
        {#if expanded.elapsedMs !== undefined}<span class="elapsed">{expanded.elapsedMs} мс</span>{/if}
        <div class="reader-tools">
          <button class="reader-btn" onclick={readerZoomOut} aria-label="уменьшить">−</button>
          <button class="reader-btn reader-value" onclick={readerZoomReset} title="сбросить">{Math.round(readerScale * 100)}%</button>
          <button class="reader-btn" onclick={readerZoomIn} aria-label="увеличить">+</button>
          <button class="reader-btn reader-close" onclick={() => (expanded = null)} aria-label="закрыть"><Icon name="close" size={12} /></button>
        </div>
      </div>
      <div class="reader-body row-body">
        {#if expanded.kind === 'assistant' || expanded.kind === 'reasoning'}
          {@html renderMarkdown(expanded.text || '')}
        {:else}
          <pre class="reader-raw">{expanded.text || ''}</pre>
        {/if}
      </div>
    </div>
  </div>
{/if}

<style>
  .wrap {
    position: relative;
    height: 100%;
  }

  .scroller {
    height: 100%;
    overflow-y: auto;
    overflow-x: hidden;
    /* Контент строк не влияет на layout родителя: браузер может пропускать
       пересчёт всей страницы при изменении одной строки. */
    /* W-17 фикс: layout paint style вместо strict — не обнуляет вклад
       содержимого в собственный размер, менее хрупко */
    contain: layout paint style;
    position: relative;
  }

  .spacer {
    pointer-events: none;
  }

  .row {
    padding: 8px 14px;
    border-bottom: 1px solid var(--border);
    /* N-4 фикс: убрали row-appear — при виртуализации анимация проигрывается
       для каждой строки входящей в окно при скролле, что заметно и раздражает.
       contain: content тоже убран: paint-containment резал растущие на
       стриме строки по старой высоте. */
  }

  .row-head {
    display: flex;
    gap: 8px;
    align-items: center;
    margin-bottom: 4px;
    font-size: 11px;
    color: var(--text-dim);
  }

  .badge {
    text-transform: uppercase;
    letter-spacing: 0.06em;
    font-weight: 600;
  }

  .row-reasoning {
    color: var(--text-dim);
    font-style: italic;
  }

  /* Кнопка раскрытия мыслей:_chevron поворачивается, свечение подсказывает
     кликабельность. Ответ это не трогает — он виден всегда. */
  .thoughts-toggle {
    background: none;
    border: none;
    padding: 0;
    cursor: pointer;
    display: inline-flex;
    align-items: center;
    gap: 4px;
    color: var(--text-faint);
    font: inherit;
    text-transform: uppercase;
    letter-spacing: 0.06em;
    font-weight: 600;
    transition: color 0.15s ease;
  }
  .thoughts-toggle:hover {
    color: var(--accent);
    text-shadow: 0 0 8px rgba(var(--accent-rgb), calc(0.35 * var(--glow-k)));
  }
  .thoughts-toggle :global(.icon) {
    transition: transform 0.15s ease;
  }
  .thoughts-toggle[aria-expanded='true'] :global(.icon) {
    transform: rotate(180deg);
  }

  /* Реплика человека — не серая простыня, а акцентная плашка в стиле
     интерфейса: скошенный уголок, тинт акцента, внутреннее свечение. */
  .row-user {
    background: linear-gradient(
      135deg,
      rgba(var(--accent-rgb), calc(0.12 * var(--glow-k))),
      rgba(var(--accent-rgb), calc(0.03 * var(--glow-k))) 70%
    );
    border-left: 3px solid var(--accent);
    border-radius: 0 10px 10px 0;
    box-shadow: inset 0 0 22px rgba(var(--accent-rgb), calc(0.06 * var(--glow-k)));
  }

  .row-user .badge {
    color: var(--accent);
    text-shadow: 0 0 8px rgba(var(--accent-rgb), calc(0.45 * var(--glow-k)));
  }

  .row-user .row-body {
    color: var(--text);
  }

  .row-error {
    border-left: 3px solid var(--err);
    background: rgba(var(--err-rgb), 0.05);
    animation: error-glitch 0.5s ease-out;
  }

  /* Любая аварийная строка (error или ok=false у инструмента):
     мигающий восклицательный знак и волна цвета, проходящая по тексту. */
  .row-failed {
    border-left: 3px solid var(--err);
  }

  .row-error .row-body,
  .row-failed .row-body {
    border-radius: 6px;
    background-image: linear-gradient(
      100deg,
      transparent 32%,
      rgba(var(--err-rgb), 0.1) 45%,
      rgba(var(--err-rgb), 0.22) 50%,
      rgba(var(--err-rgb), 0.1) 55%,
      transparent 68%
    );
    background-size: 300% 100%;
    background-repeat: no-repeat;
    animation: err-wave 2.8s linear infinite;
  }

  @keyframes err-wave {
    from {
      background-position: 130% 0;
    }
    to {
      background-position: -130% 0;
    }
  }

  .row-state {
    flex: none;
    display: inline-flex;
    align-items: center;
  }

  .state-err {
    color: var(--err);
    animation: state-blink 0.9s steps(2, jump-none) infinite;
  }

  @keyframes state-blink {
    0%,
    100% {
      opacity: 1;
      filter: drop-shadow(0 0 6px rgba(var(--err-rgb), 0.85));
    }
    50% {
      opacity: 0.25;
      filter: none;
    }
  }

  /* Мысли: серый текст с медленным переливом акцента текущей темы. */
  .state-think {
    color: var(--text-dim);
  }

  .row-reasoning .badge {
    background-image: linear-gradient(
      90deg,
      var(--text-faint) 20%,
      rgba(var(--accent-rgb), calc(0.95 * var(--glow-k))) 50%,
      var(--text-faint) 80%
    );
    background-size: 220% 100%;
    background-clip: text;
    -webkit-background-clip: text;
    color: transparent;
    animation: think-shimmer 3.6s linear infinite;
  }

  @keyframes think-shimmer {
    from {
      background-position: 210% 0;
    }
    to {
      background-position: -10% 0;
    }
  }

  .row-reasoning .row-body {
    animation: think-sheen 4.2s ease-in-out infinite;
  }

  @keyframes think-sheen {
    0%,
    100% {
      text-shadow: none;
    }
    50% {
      text-shadow: 0 0 10px rgba(var(--accent-rgb), calc(0.35 * var(--glow-k)));
    }
  }

  .state-tool {
    color: var(--text-dim);
    animation: tool-breathe 3s ease-in-out infinite;
  }

  @keyframes tool-breathe {
    0%,
    100% {
      opacity: 0.65;
    }
    50% {
      opacity: 1;
    }
  }

  @keyframes error-glitch {
    0% { transform: translateX(0); }
    20% { transform: translateX(-2px); }
    40% { transform: translateX(2px); }
    60% { transform: translateX(-1px); }
    80% { transform: translateX(1px); }
    100% { transform: translateX(0); }
  }

  .row-error .badge,
  .failed {
    color: var(--err);
    text-shadow: 0 0 8px rgba(var(--err-rgb), 0.4);
  }

  .row-approval {
    border-left: 3px solid var(--accent);
    background: rgba(var(--accent-rgb), calc(0.03 * var(--glow-k)));
    box-shadow: inset 0 0 12px rgba(var(--accent-rgb), calc(0.06 * var(--glow-k)));
  }

  .row-approval .badge {
    color: var(--accent);
  }

  /* Живой ход: тихая пульсирующая точка вместо красной «волны». */
  .live-dot {
    width: 6px;
    height: 6px;
    border-radius: 50%;
    background: var(--accent);
    box-shadow: 0 0 8px rgba(var(--accent-rgb), calc(0.6 * var(--glow-k)));
    animation: live-pulse 1.2s ease-in-out infinite;
  }

  @keyframes live-pulse {
    0%, 100% { opacity: 0.35; transform: scale(0.8); }
    50% { opacity: 1; transform: scale(1.15); }
  }

  .bookmark-btn {
    margin-left: auto;
    background: none;
    border: none;
    color: var(--text-faint);
    cursor: pointer;
    font-size: 12px;
    padding: 0 4px;
    opacity: 0;
    transition: opacity 0.15s;
  }

  .row:hover .bookmark-btn {
    opacity: 1;
  }

  .bookmark-btn.active {
    color: var(--warn);
    opacity: 1;
  }

  .expand-btn {
    background: none;
    border: none;
    color: var(--text-faint);
    cursor: pointer;
    font-size: 12px;
    padding: 0 4px;
    opacity: 0;
    transition: opacity 0.15s, color 0.15s, transform 0.15s;
  }

  .row:hover .expand-btn {
    opacity: 1;
  }

  .expand-btn:hover {
    color: var(--accent);
    transform: scale(1.2);
    text-shadow: 0 0 8px var(--accent-glow);
  }

  .tool {
    font-family: var(--mono);
    color: var(--accent);
  }

  .elapsed {
    font-family: var(--mono);
  }

  .row-body {
    word-break: break-word;
    font-family: var(--mono);
    /* Управляется из Settings → Font size (переменная на :root). */
    font-size: var(--content-font-size, 13px);
    line-height: 1.6;
  }

  /* Markdown */
  .row-body :global(h1),
  .row-body :global(h2),
  .row-body :global(h3) {
    color: var(--text);
    margin: 8px 0 4px;
    font-family: var(--sans);
  }

  .row-body :global(h1) { font-size: 18px; }
  .row-body :global(h2) { font-size: 15px; }
  .row-body :global(h3) { font-size: 13px; }

  .row-body :global(p) { margin: 4px 0; }

  .row-body :global(ul),
  .row-body :global(ol) {
    margin: 4px 0;
    padding-left: 20px;
  }

  .row-body :global(code:not(.hljs)) {
    background: var(--bg-elevated);
    padding: 1px 4px;
    border-radius: 3px;
    font-size: 12px;
    color: var(--accent);
  }

  .row-body :global(.code-block-wrap) {
    position: relative;
    margin: 6px 0;
  }

  .row-body :global(.copy-btn) {
    position: absolute;
    top: 6px;
    right: 6px;
    background: var(--bg-elevated);
    border: 1px solid var(--border);
    border-radius: 4px;
    color: var(--text-dim);
    font-family: var(--mono);
    font-size: 10px;
    padding: 2px 8px;
    cursor: pointer;
    opacity: 0;
    transition: opacity 0.15s;
    z-index: 1;
  }

  .row-body :global(.code-block-wrap:hover .copy-btn) {
    opacity: 1;
  }

  .row-body :global(.copy-btn:hover) {
    color: var(--accent);
    border-color: var(--accent);
  }

  .row-body :global(.code-block) {
    background: var(--bg);
    border: 1px solid var(--border);
    border-radius: 6px;
    padding: 10px 12px;
    margin: 6px 0;
    overflow-x: auto;
    font-size: 12px;
    line-height: 1.4;
  }

  .row-body :global(.code-block.streaming) {
    border-color: var(--accent-dim);
  }

  .row-body :global(.hljs) {
    background: transparent;
    color: var(--text);
  }

  .row-body :global(.hljs-keyword) { color: #ff7b72; }
  .row-body :global(.hljs-string) { color: #a5d6ff; }
  .row-body :global(.hljs-comment) { color: #8b949e; font-style: italic; }
  .row-body :global(.hljs-function) { color: #d2a8ff; }
  .row-body :global(.hljs-number) { color: #79c0ff; }
  .row-body :global(.hljs-title) { color: #d2a8ff; }
  .row-body :global(.hljs-built_in) { color: #ffa657; }
  .row-body :global(.hljs-literal) { color: #79c0ff; }
  .row-body :global(.hljs-type) { color: #ffa657; }
  .row-body :global(.hljs-attr) { color: #79c0ff; }

  .row-body :global(.cursor) {
    color: var(--accent);
    animation: blink 0.8s step-end infinite;
  }

  @keyframes blink {
    0%, 100% { opacity: 1; }
    50% { opacity: 0; }
  }

  /* ── Разворот сообщения на весь экран ── */
  .reader-overlay {
    position: fixed;
    inset: 0;
    z-index: 900;
    background: var(--surface-overlay);
    backdrop-filter: blur(4px);
    display: flex;
    align-items: center;
    justify-content: center;
    animation: reader-fade 0.16s ease-out;
  }

  @keyframes reader-fade {
    from { opacity: 0; }
    to { opacity: 1; }
  }

  .reader {
    width: min(1100px, 92vw);
    height: 86vh;
    display: flex;
    flex-direction: column;
    background: var(--bg-panel);
    border: 1px solid var(--border-strong);
    border-radius: 12px;
    box-shadow: var(--shadow-strong);
    overflow: hidden;
    animation: reader-in 0.18s ease-out;
  }

  @keyframes reader-in {
    from { opacity: 0; transform: scale(0.97); }
    to { opacity: 1; transform: scale(1); }
  }

  .reader-head {
    display: flex;
    align-items: center;
    gap: 10px;
    padding: 10px 14px;
    border-bottom: 1px solid var(--border);
    background: var(--surface-inset);
    font-family: var(--mono);
    font-size: 11px;
    color: var(--text-dim);
  }

  .reader-tools {
    margin-left: auto;
    display: flex;
    align-items: center;
    gap: 4px;
  }

  .reader-btn {
    background: none;
    border: 1px solid var(--border);
    border-radius: 6px;
    color: var(--text-dim);
    font-family: var(--mono);
    font-size: 11px;
    padding: 3px 9px;
    cursor: pointer;
    transition: all 0.15s;
  }

  .reader-btn:hover {
    color: var(--accent);
    border-color: var(--accent-dim);
    box-shadow: 0 0 10px rgba(var(--accent-rgb), calc(0.15 * var(--glow-k)));
  }

  .reader-value {
    min-width: 48px;
    color: var(--text);
  }

  .reader-close:hover {
    color: #fff;
    background: var(--accent);
    border-color: var(--accent);
  }

  .reader-body {
    flex: 1;
    overflow: auto;
    padding: 20px 24px;
    /* Свой масштаб поверх глобального размера шрифта — «растянуть конкретно». */
    font-size: calc(var(--content-font-size, 13px) * var(--reader-scale, 1));
    line-height: 1.65;
  }

  .reader-raw {
    margin: 0;
    white-space: pre-wrap;
    word-break: break-word;
    font-family: var(--mono);
    font-size: inherit;
    color: var(--text);
  }
</style>

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
  import { renderMarkdown, renderStreaming } from '../lib/markdown'
  import {
    clampWindow,
    computeWindow,
    ESTIMATED_ITEM_HEIGHT,
    recordHeight,
    type HeightCache,
    type VirtualWindow,
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
  }

  let { items, revision, maxRendered = 200, bookmarks = new Set(), onToggleBookmark }: Props = $props()

  let scroller: HTMLDivElement | undefined = $state()
  let minimapVisible = $state(false)
  let minimapTop = $state(0)
  let minimapHeight = $state(0)
  let scrollTop = $state(0)
  let viewportHeight = $state(600)
  let stickToBottom = $state(true)

  const heights: HeightCache = new Map()
  /** Узлы строк по ключу: по ним снимаем измеренную высоту. */
  let rowNodes = new Map<string, HTMLElement>()

  // C-2 фикс: сбрасываем кэш высот только когда набор ДЕЙСТВИТЕЛЬНО новый
  // (другая сессия, очистка, поиск), а не на каждую пересборку derived-массива:
  // иначе totalHeight схлопывался до оценок и список дёргался вверх.
  let lastFirstKey: string | null = null
  $effect(() => {
    const first = items[0]?.key ?? null
    if (first !== lastFirstKey) {
      lastFirstKey = first
      heights.clear()
      rowNodes.clear()
    }
  })

  let win: VirtualWindow = $derived.by(() => {
    // revision — триггер пересчёта: массив items мутируется моделью напрямую.
    void revision
    void items.length
    return clampWindow(computeWindow(items.length, scrollTop, viewportHeight, heights), maxRendered)
  })

  /**
   * Высота нижней распорки.
   *
   * Считаем из тех же измеренных/оценённых высот, а не из DOM: чтение
   * offsetHeight здесь дало бы принудительный layout на каждый кадр.
   */
  let bottomSpacer = $derived.by(() => {
    let rendered = 0
    for (const i of win.indices) {
      rendered += heights.get(i) ?? ESTIMATED_ITEM_HEIGHT
    }
    return Math.max(0, win.totalHeight - win.offsetTop - rendered)
  })

  function onScroll(e: Event): void {
    const el = e.currentTarget as HTMLDivElement
    scrollTop = el.scrollTop
    viewportHeight = el.clientHeight
    // Прилипание считаем по РЕАЛЬНОЙ высоте DOM, а не по оценке виртуализатора:
    // во время стрима оценка отстаёт от отрисовки, и список «откидывало вверх».
    stickToBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 64

    // Мини-карта
    if (win.totalHeight > 0) {
      const ratio = el.clientHeight / win.totalHeight
      minimapHeight = Math.max(20, ratio * 100)
      minimapTop = (el.scrollTop / win.totalHeight) * 100
      minimapVisible = ratio < 0.95 // показываем только если есть скролл
    }
  }

  /** Снять реальные высоты отрендеренных строк в кэш. */
  function measure(): void {
    for (const i of win.indices) {
      const item = items[i]
      if (!item) continue
      const node = rowNodes.get(item.key)
      if (!node) continue
      recordHeight(heights, i, node.getBoundingClientRect().height)
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
    return {
      update(next: string) {
        if (next === current) return
        rowNodes.delete(current)
        current = next
        rowNodes.set(current, node)
      },
      destroy() {
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
      if (stickToBottom && scroller) {
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
      scroller?.removeEventListener('click', onClick)
    }
  })
</script>

<div class="scroller" bind:this={scroller} onscroll={onScroll}>
  <!-- Мини-карта скролла -->
  {#if minimapVisible}
    <div class="minimap">
      <div class="minimap-thumb" style="top: {minimapTop}%; height: {minimapHeight}%"></div>
    </div>
  {/if}
  <!-- Верхняя распорка задаёт позицию видимого окна в полном списке. -->
  <div class="spacer" style="height: {win.offsetTop}px"></div>

  {#each win.indices as i (items[i]?.key ?? i)}
    {@const item = items[i]}
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
            <button
              class="thoughts-toggle"
              aria-expanded={openThoughts.has(item.key)}
              onclick={() => toggleThoughts(item.key)}
              title={openThoughts.has(item.key) ? 'свернуть мысли' : 'раскрыть мысли'}
            >
              <Icon name="chevron-down" size={11} />
              {openThoughts.has(item.key) ? 'свернуть' : 'раскрыть'}
            </button>
          {/if}
          {#if item.tool}<span class="tool">{item.tool}</span>{/if}
          {#if item.elapsedMs !== undefined}<span class="elapsed">{item.elapsedMs} мс</span>{/if}
          {#if item.ok === false}<span class="failed">ошибка</span>{/if}
          {#if !item.done}<span class="streaming">▁▂▃▅▇▅▃▂▁</span>{/if}
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
        <!-- Мысли свёрнуты, пока их не раскрыли: ответ читается без
             простыни размышлений, а стрим «думает» виден по волне в шапке. -->
        {#if item.kind !== 'reasoning' || openThoughts.has(item.key)}
          <div class="row-body">
            {#if item.kind === 'assistant' || item.kind === 'reasoning'}
              {#if item.done}
                {@html renderMarkdown(item.text || '')}
              {:else}
                {@html renderStreaming(item.text || '')}
              {/if}
            {:else}
              {item.text || '\u00a0'}
            {/if}
          </div>
        {/if}
      </div>
    {/if}
  {/each}

  <!-- Нижняя распорка сохраняет полную высоту скролла. -->
  <div class="spacer" style="height: {bottomSpacer}px"></div>
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

  .minimap {
    position: sticky;
    top: 0;
    right: 2px;
    width: 4px;
    height: 100%;
    background: rgba(255, 255, 255, 0.03);
    border-radius: 2px;
    pointer-events: none;
    z-index: 10;
    margin-left: auto;
  }

  .minimap-thumb {
    position: absolute;
    left: 0;
    width: 100%;
    background: var(--accent-dim);
    border-radius: 2px;
    transition: top 0.1s ease-out;
  }

  .spacer {
    pointer-events: none;
  }

  .row {
    padding: 8px 14px;
    border-bottom: 1px solid var(--border);
    /* Изолируем перерисовку строки от остального документа. */
    contain: content;
    /* N-4 фикс: убрали row-appear — при виртуализации анимация проигрывается
       для каждой строки входящей в окно при скролле, что заметно и раздражает */
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

  .row-user {
    background: var(--bg-panel);
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

  .streaming {
    color: var(--accent);
    font-size: 9px;
    letter-spacing: -1px;
    animation: wave 1.2s ease-in-out infinite;
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

  @keyframes wave {
    0%, 100% { opacity: 0.4; transform: scaleY(0.8); }
    50% { opacity: 1; transform: scaleY(1.2); }
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

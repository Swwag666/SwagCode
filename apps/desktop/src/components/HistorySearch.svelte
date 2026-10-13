<script lang="ts">
  /* F-10: результаты полнотекстового поиска по истории (палитра → «/search»).

     Панель устроена как Palette: свой overlay, свой Escape, свой фокус. Данные
     приносит App (invoke search_messages), компонент лишь рисует их и сообщает
     о выборе — решений в нём нет, как и во всём фронтенде (D-011: логика в
     ядре). Сниппет рисуется текстом через parseSnippet, а не {@html}:
     содержимое сообщений недоверенное. */
  import { hitTitle, parseSnippet } from '../lib/search'
  import type { StrKey } from '../lib/strings'

  /** Совпадает с MessageHit в crates/core/src/store.rs. */
  export interface MessageHitDto {
    session_id: string
    session_title: string
    ord: number
    role: string
    snippet: string
    rank: number
  }

  interface Props {
    /** Открыта ли панель. Закрывается изнутри (Escape, клик по фону). */
    open: boolean
    hits: MessageHitDto[]
    /** Текст ошибки ядра (пустой запрос, слишком длинный) — показываем как есть. */
    error: string
    busy: boolean
    /** Тот же предел, что App передаёт в команду: по нему виден обрезок выдачи. */
    limit: number
    t: (key: StrKey) => string
    /** Запрос из поля ввода (уже с задержкой) — App ходит им в ядро. */
    onquery: (query: string) => void
    onpick: (hit: MessageHitDto) => void
  }

  let {
    open = $bindable(false),
    hits,
    error,
    busy,
    limit,
    t,
    onquery,
    onpick,
  }: Props = $props()

  let query = $state('')
  let idx = $state(0)
  let inputEl = $state<HTMLInputElement | null>(null)

  /* Задержка перед запросом: FTS5 отвечает за единицы миллисекунд, но ходить в
     ядро на каждое нажатие значит плодить гонки, где поздний ответ перекрывает
     ранний. Гонки всё равно дожимает App счётчиком поколений — задержка здесь
     не единственная защита. */
  $effect(() => {
    // Закрытая панель не спрашивает ничего: иначе первый же рендер App ушёл бы
    // в ядро с пустым запросом. При открытии query сбрасывается эффектом ниже,
    // и этот эффект отрабатывает уже по-настоящему.
    if (!open) return
    const q = query
    if (!q.trim()) {
      onquery('')
      return
    }
    const id = setTimeout(() => onquery(q), 220)
    return () => clearTimeout(id)
  })

  /* Открыли — поле пустое и фокус в нём: панель запускают из палитры, значит
     человек собирается печатать сразу. */
  $effect(() => {
    if (!open) return
    query = ''
    idx = 0
    const id = setTimeout(() => inputEl?.focus(), 50)
    return () => clearTimeout(id)
  })

  const status = $derived(
    error
      ? error
      : busy
        ? t('searchBusy')
        : !query.trim()
          ? t('searchHint')
          : hits.length === 0
            ? t('searchEmpty')
            : `${hits.length} ${t('searchFound')}`,
  )
  /* Выдача упирается в предел — честно говорим, что совпадений может быть
     больше: молчаливый обрезок выглядит как «больше ничего нет». */
  const truncated = $derived(!error && !busy && hits.length >= limit && limit > 0)

  function close(): void {
    open = false
  }

  function pick(h: MessageHitDto): void {
    onpick(h)
    open = false
  }

  function onKey(e: KeyboardEvent): void {
    if (e.key === 'Escape') {
      e.preventDefault()
      e.stopPropagation()
      close()
      return
    }
    if (e.key === 'ArrowDown') {
      e.preventDefault()
      idx = Math.min(idx + 1, Math.max(hits.length - 1, 0))
      return
    }
    if (e.key === 'ArrowUp') {
      e.preventDefault()
      idx = Math.max(idx - 1, 0)
      return
    }
    if (e.key === 'Enter') {
      e.preventDefault()
      const h = hits[idx]
      // Выделение могло устареть, если выдача сократилась: Enter не обязан
      // открывать случайную сессию.
      if (h) pick(h)
    }
  }
</script>

{#if open}
  <div class="hsearch-overlay" onclick={close} role="presentation">
    <div
      class="hsearch"
      role="dialog"
      aria-label={t('searchTitle')}
      tabindex="-1"
      onclick={(e) => e.stopPropagation()}
      onkeydown={(e) => {
        if (e.key === 'Escape') close()
      }}
    >
      <div class="hsearch-head">
        <span class="hsearch-title">{t('searchTitle')}</span>
        <span class="hsearch-note">{t('searchNote')}</span>
      </div>
      <input
        bind:this={inputEl}
        class="hsearch-input"
        bind:value={query}
        onkeydown={onKey}
        placeholder={t('searchPlaceholder')}
        spellcheck="false"
        autocomplete="off"
      />
      <div class="hsearch-status" class:err={!!error}>
        {status}{#if truncated}&nbsp;· {t('searchTruncated')}{/if}
      </div>
      <ul class="hsearch-list">
        {#each hits as h, i (`${h.session_id}:${h.ord}`)}
          <li>
            <button
              type="button"
              class="hsearch-item"
              class:sel={i === idx}
              onclick={() => pick(h)}
              onmouseenter={() => (idx = i)}
            >
              <span class="hsearch-where">
                <span class="hsearch-role">{h.role}</span>
                <span class="hsearch-ses">{hitTitle(h.session_title, h.session_id)}</span>
                <span class="hsearch-ord">#{h.ord}</span>
              </span>
              <span class="hsearch-snip">
                {#each parseSnippet(h.snippet) as p, k (k)}
                  {#if p.hit}<mark>{p.text}</mark>{:else}{p.text}{/if}
                {/each}
              </span>
            </button>
          </li>
        {/each}
      </ul>
    </div>
  </div>
{/if}

<style>
  /* Те же токены темы, что у Palette: панель наследует тёмную, светлую и
     контрастную темы без собственных цветов. */
  .hsearch-overlay {
    position: fixed;
    inset: 0;
    z-index: 520;
    display: flex;
    align-items: flex-start;
    justify-content: center;
    padding-top: 12vh;
    background: var(--surface-overlay);
    backdrop-filter: blur(2px);
  }
  .hsearch {
    width: min(760px, 92vw);
    max-height: 70vh;
    display: flex;
    flex-direction: column;
    background: var(--bg-elevated);
    border: 1px solid var(--border);
    border-radius: 10px;
    box-shadow: var(--shadow-strong);
    overflow: hidden;
  }
  .hsearch-head {
    display: flex;
    align-items: baseline;
    gap: 10px;
    padding: 10px 14px 0;
  }
  .hsearch-title {
    font-size: 13px;
    font-weight: 600;
    color: var(--text);
    letter-spacing: 0.04em;
  }
  .hsearch-note {
    font-size: 11px;
    color: var(--text-faint);
  }
  .hsearch-input {
    margin: 8px 14px;
    padding: 8px 10px;
    background: transparent;
    border: 1px solid var(--border);
    border-radius: 6px;
    color: var(--text);
    font-family: var(--mono);
    font-size: 13px;
    outline: none;
  }
  .hsearch-input:focus {
    border-color: var(--accent);
  }
  .hsearch-status {
    padding: 0 14px 8px;
    font-size: 11px;
    color: var(--text-faint);
    font-family: var(--mono);
  }
  .hsearch-status.err {
    color: var(--danger, #ff6b6b);
  }
  .hsearch-list {
    list-style: none;
    margin: 0;
    padding: 0 6px 8px;
    overflow-y: auto;
  }
  .hsearch-item {
    width: 100%;
    display: flex;
    flex-direction: column;
    gap: 3px;
    padding: 7px 9px;
    background: transparent;
    border: none;
    border-left: 2px solid transparent;
    border-radius: 6px;
    text-align: left;
    cursor: pointer;
    color: var(--text);
  }
  .hsearch-item.sel {
    background: rgba(var(--accent-rgb), calc(0.1 * var(--glow-k)));
    border-left-color: var(--accent);
  }
  .hsearch-where {
    display: flex;
    align-items: center;
    gap: 8px;
    font-size: 11px;
    color: var(--text-faint);
    font-family: var(--mono);
  }
  .hsearch-role {
    padding: 0 5px;
    border: 1px solid var(--border);
    border-radius: 4px;
    color: var(--text-dim);
  }
  .hsearch-ses {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    max-width: 46ch;
  }
  .hsearch-ord {
    margin-left: auto;
    color: var(--text-faint);
  }
  .hsearch-snip {
    font-size: 12px;
    line-height: 1.45;
    color: var(--text-dim);
    white-space: pre-wrap;
    word-break: break-word;
  }
  .hsearch-snip mark {
    background: rgba(var(--accent-rgb), calc(0.22 * var(--glow-k)));
    color: var(--text);
    border-radius: 3px;
    padding: 0 1px;
  }
</style>

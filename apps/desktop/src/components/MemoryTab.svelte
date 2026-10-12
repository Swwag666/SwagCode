<script lang="ts">
  /**
   * F-7: вкладка памяти приложения.
   *
   * Компонент не знает ни про Tauri, ни про базу: родитель передаёт строки и
   * забирает действия через колбэки (тот же приём, что в CommandRules),
   * поэтому вкладка тестируется без ядра. Одна и та же память видна и
   * человеку, и агенту — инструменты `memory_*` пишут в ту же таблицу.
   */

  /** Строка памяти из ядра (`core::store::MemoryEntry`). */
  export interface MemoryItem {
    id: number
    scope: string
    text: string
    created_ms: number
    session_id?: string | null
  }

  interface Props {
    title: string
    desc: string
    globalLabel: string
    sessionLabel: string
    globalHint: string
    sessionHint: string
    placeholder: string
    addLabel: string
    forgetLabel: string
    emptyLabel: string
    items: MemoryItem[]
    scope: 'global' | 'session'
    busy?: boolean
    status?: string | null
    onScope: (scope: 'global' | 'session') => void
    onAdd: (text: string) => void
    onForget: (id: number) => void
  }

  let {
    title,
    desc,
    globalLabel,
    sessionLabel,
    globalHint,
    sessionHint,
    placeholder,
    addLabel,
    forgetLabel,
    emptyLabel,
    items,
    scope,
    busy = false,
    status = null,
    onScope,
    onAdd,
    onForget,
  }: Props = $props()

  let draft = $state('')

  /** Пустой черновик не отправляем: ядро всё равно отвергнет, а кнопка
   * «живая» только когда есть что сохранить. */
  const canAdd = $derived(draft.trim().length > 0 && !busy)

  function submit(): void {
    const text = draft.trim()
    if (!text || busy) return
    onAdd(text)
    draft = ''
  }

  /** Короткая метка времени: список читается как лента, а не как журнал. */
  function when(ms: number): string {
    if (!ms) return ''
    return new Date(ms).toLocaleString()
  }
</script>

<section class="memory-tab">
  <h3>{title}</h3>
  <p class="mem-desc">{desc}</p>

  <div class="mem-scopes" role="group">
    <button
      type="button"
      class="mem-scope"
      class:active={scope === 'global'}
      data-testid="scope-global"
      onclick={() => onScope('global')}>{globalLabel}</button
    >
    <button
      type="button"
      class="mem-scope"
      class:active={scope === 'session'}
      data-testid="scope-session"
      onclick={() => onScope('session')}>{sessionLabel}</button
    >
  </div>
  <p class="mem-hint">{scope === 'global' ? globalHint : sessionHint}</p>

  <div class="mem-add">
    <textarea
      class="mem-input"
      rows="2"
      data-testid="memory-input"
      placeholder={placeholder}
      bind:value={draft}
      onkeydown={(e) => {
        /* Ctrl+Enter — сохранить: память добавляют одной рукой, не отрываясь
           от чтения. Enter остаётся переводом строки. */
        if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) {
          e.preventDefault()
          submit()
        }
      }}
    ></textarea>
    <button
      type="button"
      class="btn primary"
      data-testid="memory-add"
      disabled={!canAdd}
      onclick={() => submit()}>{addLabel}</button
    >
  </div>

  {#if status}<p class="mem-status" data-testid="memory-status">{status}</p>{/if}

  {#if items.length === 0}
    <p class="mem-empty" data-testid="memory-empty">{emptyLabel}</p>
  {:else}
    <ul class="mem-list" data-testid="memory-list">
      {#each items as it (it.id)}
        <li class="mem-item">
          <div class="mem-text">{it.text}</div>
          <div class="mem-meta">
            <span class="mem-id">#{it.id}</span>
            {#if when(it.created_ms)}<span class="mem-when">{when(it.created_ms)}</span>{/if}
            <button
              type="button"
              class="mem-forget"
              data-testid="memory-forget"
              disabled={busy}
              title={forgetLabel}
              onclick={() => {
                if (!busy) onForget(it.id)
              }}>{forgetLabel}</button
            >
          </div>
        </li>
      {/each}
    </ul>
  {/if}
</section>

<style>
  .memory-tab {
    display: flex;
    flex-direction: column;
    gap: 10px;
  }
  .memory-tab h3 {
    margin: 0;
    font-size: 15px;
  }
  .mem-desc {
    margin: 0;
    color: var(--text-dim);
    font-size: 12px;
    line-height: 1.5;
  }
  .mem-scopes {
    display: flex;
    gap: 6px;
  }
  .mem-scope {
    padding: 4px 10px;
    border: 1px solid var(--border);
    border-radius: 999px;
    background: transparent;
    color: var(--text-dim);
    font-size: 12px;
    cursor: pointer;
  }
  .mem-scope.active {
    border-color: var(--accent);
    color: var(--accent);
    background: rgba(var(--accent-rgb), 0.08);
  }
  .mem-hint {
    margin: 0;
    color: var(--text-faint);
    font-size: 11.5px;
    line-height: 1.45;
  }
  .mem-add {
    display: flex;
    gap: 8px;
    align-items: flex-start;
  }
  .mem-input {
    flex: 1;
    resize: vertical;
    min-height: 44px;
    padding: 7px 9px;
    border: 1px solid var(--border);
    border-radius: 7px;
    background: var(--bg);
    color: var(--text);
    font-family: inherit;
    font-size: 12.5px;
    line-height: 1.45;
  }
  .mem-status {
    margin: 0;
    font-size: 12px;
    color: var(--accent);
  }
  .mem-empty {
    margin: 0;
    color: var(--text-faint);
    font-size: 12px;
  }
  .mem-list {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: 6px;
    max-height: 320px;
    overflow: auto;
  }
  .mem-item {
    border: 1px solid var(--border);
    border-radius: 7px;
    padding: 7px 9px;
    display: flex;
    flex-direction: column;
    gap: 5px;
  }
  .mem-text {
    font-size: 12.5px;
    line-height: 1.45;
    white-space: pre-wrap;
    word-break: break-word;
  }
  .mem-meta {
    display: flex;
    gap: 8px;
    align-items: center;
    color: var(--text-faint);
    font-size: 11px;
  }
  .mem-when {
    flex: 1;
  }
  .mem-forget {
    border: 1px solid var(--border);
    border-radius: 6px;
    background: transparent;
    color: var(--err);
    font-size: 11px;
    padding: 2px 8px;
    cursor: pointer;
  }
  .mem-forget:disabled {
    opacity: 0.5;
    cursor: default;
  }
</style>

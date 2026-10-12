<script lang="ts">
  /**
   * F-8: редактор хуков (pre_turn / post_turn / pre_tool).
   *
   * Компонент не знает ни про Tauri, ни про prefs: родитель передаёт текст
   * конфигурации и забирает его обратно через onSave. Поэтому редактор
   * тестируется без ядра (см. HooksEditor.test.ts), а разбор, проверка и
   * отбор по regex живут в core::hooks — UI здесь только текст и кнопки.
   *
   * Конфигурация — JSON, а не набор полей: три события с разными формами
   * (строки против объектов с regex) в плоском редакторе выглядели бы как
   * три несвязанных списка, а владелец копирует эти куски из документации
   * целиком. Локальная проверка JSON нужна только чтобы не гонять заведомо
   * битый текст на бэкенд: решает всё равно `hooks_save`.
   */
  interface Props {
    title: string
    desc: string
    /** Подсказка про контракт окружения и коды возврата. */
    hint: string
    saveLabel: string
    savedLabel: string
    /** Текст конфигурации из prefs (JSON или пусто = хуков нет). */
    raw: string
    /** Сколько хуков сохранено по событиям. */
    counts: { pre_turn: number; post_turn: number; pre_tool: number }
    timeoutSecs: number
    maxPerEvent: number
    busy?: boolean
    status?: string | null
    /** Ошибка сохранения от бэкенда (или разбора сохранённого текста). */
    error?: string | null
    onSave: (raw: string) => void
  }

  let {
    title,
    desc,
    hint,
    saveLabel,
    savedLabel,
    raw,
    counts,
    timeoutSecs,
    maxPerEvent,
    busy = false,
    status = null,
    error = null,
    onSave,
  }: Props = $props()

  // Текст заполняется эффектом из пропа: инициализация значением пропа сняла
  // бы только первый кадр и потеряла обновления (перечитывание prefs).
  let text = $state('')
  $effect(() => {
    text = raw
  })

  /** Локальная проверка: пустой текст валиден (хуков нет), иначе JSON. */
  let jsonError = $derived.by(() => {
    const t = text.trim()
    if (t.length === 0) return ''
    try {
      JSON.parse(t)
      return ''
    } catch (e) {
      return String(e instanceof Error ? e.message : e)
    }
  })

  let canSave = $derived(!busy)
  let total = $derived(counts.pre_turn + counts.post_turn + counts.pre_tool)

  function save(): void {
    // Защита от двойного сохранения: disabled-атрибут не мешает
    // программному клику, а два сохранения подряд дают гонку статусов.
    if (busy) return
    onSave(text.trim())
  }
</script>

<div class="hooks" aria-label={title}>
  <div class="hooks-head">
    <span class="hooks-title">{title}</span>
    <span class="hooks-desc">{desc}</span>
  </div>

  <div class="hooks-counts" data-testid="hooks-counts">
    <span class="hooks-chip">pre_turn <b>{counts.pre_turn}</b></span>
    <span class="hooks-chip">post_turn <b>{counts.post_turn}</b></span>
    <span class="hooks-chip">pre_tool <b>{counts.pre_tool}</b></span>
    <span class="hooks-chip dim">
      {total} · {timeoutSecs}s · ≤{maxPerEvent}
    </span>
  </div>

  <textarea
    id="hooks-json"
    class="hooks-text"
    rows="10"
    spellcheck="false"
    aria-label={title}
    data-testid="hooks-text"
    bind:value={text}
  ></textarea>

  {#if jsonError}
    <span class="hooks-hint bad" data-testid="hooks-json-error">JSON: {jsonError}</span>
  {:else}
    <span class="hooks-hint">{hint}</span>
  {/if}

  {#if error}<span class="hooks-error" role="alert" data-testid="hooks-error">{error}</span>{/if}

  <div class="hooks-actions">
    {#if status}<span class="hooks-status" role="status" data-testid="hooks-status">{status}</span>{/if}
    <button class="appearance-btn primary" data-testid="hooks-save" onclick={save} disabled={!canSave}>
      {busy ? savedLabel : saveLabel}
    </button>
  </div>
</div>

<style>
  .hooks {
    display: flex;
    flex-direction: column;
    gap: 10px;
    width: 100%;
  }
  .hooks-head {
    display: flex;
    flex-direction: column;
    gap: 3px;
  }
  .hooks-title {
    font-size: 13px;
    color: var(--text);
    letter-spacing: 0.02em;
  }
  .hooks-desc {
    font-size: 11.5px;
    color: var(--text-dim);
    line-height: 1.5;
  }
  .hooks-counts {
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
  }
  .hooks-chip {
    font-family: var(--mono, ui-monospace), monospace;
    font-size: 10.5px;
    padding: 2px 8px;
    border-radius: 999px;
    border: 1px solid var(--border);
    color: var(--text-dim);
  }
  .hooks-chip b {
    color: var(--text);
    font-weight: 600;
  }
  .hooks-chip.dim {
    opacity: 0.75;
  }
  .hooks-text {
    width: 100%;
    resize: vertical;
    background: var(--bg);
    border: 1px solid var(--border);
    border-radius: 8px;
    color: var(--text);
    font-family: var(--mono, ui-monospace), monospace;
    font-size: 11.5px;
    line-height: 1.55;
    padding: 8px 10px;
  }
  .hooks-text:focus {
    outline: none;
    border-color: rgba(var(--accent-rgb), 0.55);
  }
  .hooks-hint {
    font-size: 10.5px;
    color: var(--text-dim);
    line-height: 1.5;
  }
  .hooks-hint.bad {
    color: var(--err);
  }
  .hooks-error {
    font-size: 11px;
    color: var(--err);
    line-height: 1.5;
  }
  .hooks-actions {
    display: flex;
    align-items: center;
    justify-content: flex-end;
    gap: 10px;
  }
  .hooks-status {
    margin-right: auto;
    font-size: 11.5px;
    color: var(--text-dim);
  }
  .appearance-btn.primary {
    border-color: rgba(var(--accent-rgb), 0.6);
    color: var(--accent);
  }
</style>

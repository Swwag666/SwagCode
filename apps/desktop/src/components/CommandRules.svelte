<script lang="ts">
  /**
   * F-3: редактор allow/deny-списков команд.
   *
   * Компонент не знает ни про Tauri, ни про prefs: родитель передаёт строки
   * правил и забирает их обратно через onSave. Поэтому список тестируется
   * без ядра (см. CommandRules.test.ts), а вся политика живёт в core::rules
   * — UI здесь только текст и кнопки.
   *
   * Семантика (напоминание в подсказках): deny проверяется первым и
   * отменяет вызов без диалога, allow исполняет без диалога, иначе решает
   * политика подтверждений. Пустой список = правил нет.
   */
  interface Props {
    title: string
    desc: string
    allowLabel: string
    denyLabel: string
    allowHint: string
    denyHint: string
    saveLabel: string
    resetLabel: string
    savedLabel: string
    allow: string[]
    deny: string[]
    allowDefaults: string[]
    denyDefaults: string[]
    busy?: boolean
    status?: string | null
    onSave: (allow: string[], deny: string[]) => void
  }

  let {
    title,
    desc,
    allowLabel,
    denyLabel,
    allowHint,
    denyHint,
    saveLabel,
    resetLabel,
    savedLabel,
    allow,
    deny,
    allowDefaults,
    denyDefaults,
    busy = false,
    status = null,
    onSave,
  }: Props = $props()

  // Текст заполняется эффектом из пропсов (ниже): инициализировать здесь
  // значением пропа значит снять только первый кадр и потерять обновления.
  let allowText = $state('')
  let denyText = $state('')

  // Родитель может перечитать prefs (сброс, ошибка сохранения) — текст
  // следует за пропсами. Зависимость только от массива правил: ввод
  // пользователя эффект не перезапускает и не затирает.
  $effect(() => {
    allowText = allow.join('\n')
  })
  $effect(() => {
    denyText = deny.join('\n')
  })

  /** Строки из textarea: trim, без пустых. Комментарии `#` сохраняем —
   * движок их пропускает, а человек видит пояснение. */
  function lines(text: string): string[] {
    return text
      .split('\n')
      .map((l) => l.trim())
      .filter((l) => l.length > 0)
  }

  let allowCount = $derived(lines(allowText).length)
  let denyCount = $derived(lines(denyText).length)

  function save(): void {
    onSave(lines(allowText), lines(denyText))
  }

  function reset(): void {
    allowText = allowDefaults.join('\n')
    denyText = denyDefaults.join('\n')
  }
</script>

<div class="cmd-rules" aria-label={title}>
  <div class="cmd-rules-head">
    <span class="cmd-rules-title">{title}</span>
    <span class="cmd-rules-desc">{desc}</span>
  </div>

  <div class="cmd-rules-cols">
    <div class="cmd-rules-col">
      <label class="cmd-rules-label allow" for="cmd-allow">
        {allowLabel} <span class="cmd-rules-count">{allowCount}</span>
      </label>
      <textarea
        id="cmd-allow"
        class="cmd-rules-text"
        rows="8"
        spellcheck="false"
        aria-label={allowLabel}
        bind:value={allowText}
      ></textarea>
      <span class="cmd-rules-hint">{allowHint}</span>
    </div>

    <div class="cmd-rules-col">
      <label class="cmd-rules-label deny" for="cmd-deny">
        {denyLabel} <span class="cmd-rules-count">{denyCount}</span>
      </label>
      <textarea
        id="cmd-deny"
        class="cmd-rules-text deny"
        rows="8"
        spellcheck="false"
        aria-label={denyLabel}
        bind:value={denyText}
      ></textarea>
      <span class="cmd-rules-hint">{denyHint}</span>
    </div>
  </div>

  <div class="cmd-rules-actions">
    {#if status}<span class="cmd-rules-status" role="status">{status}</span>{/if}
    <button class="appearance-btn" onclick={reset} disabled={busy}>{resetLabel}</button>
    <button class="appearance-btn primary" onclick={save} disabled={busy}>
      {busy ? savedLabel : saveLabel}
    </button>
  </div>
</div>

<style>
  .cmd-rules {
    display: flex;
    flex-direction: column;
    gap: 10px;
    width: 100%;
  }
  .cmd-rules-head {
    display: flex;
    flex-direction: column;
    gap: 3px;
  }
  .cmd-rules-title {
    font-size: 13px;
    color: var(--text);
    letter-spacing: 0.02em;
  }
  .cmd-rules-desc {
    font-size: 11.5px;
    color: var(--text-dim);
    line-height: 1.5;
  }
  .cmd-rules-cols {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: 12px;
  }
  @media (max-width: 720px) {
    .cmd-rules-cols {
      grid-template-columns: 1fr;
    }
  }
  .cmd-rules-col {
    display: flex;
    flex-direction: column;
    gap: 5px;
    min-width: 0;
  }
  .cmd-rules-label {
    font-size: 11.5px;
    text-transform: uppercase;
    letter-spacing: 0.06em;
    color: var(--text-dim);
    display: flex;
    align-items: center;
    gap: 6px;
  }
  .cmd-rules-label.allow {
    color: var(--accent);
  }
  .cmd-rules-label.deny {
    color: var(--err);
  }
  .cmd-rules-count {
    font-size: 10.5px;
    padding: 1px 6px;
    border-radius: 999px;
    border: 1px solid var(--border);
    color: var(--text-dim);
  }
  .cmd-rules-text {
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
  .cmd-rules-text:focus {
    outline: none;
    border-color: rgba(var(--accent-rgb), 0.55);
  }
  .cmd-rules-text.deny:focus {
    border-color: rgba(var(--err-rgb), 0.55);
  }
  .cmd-rules-hint {
    font-size: 10.5px;
    color: var(--text-dim);
    line-height: 1.5;
  }
  .cmd-rules-actions {
    display: flex;
    align-items: center;
    justify-content: flex-end;
    gap: 10px;
  }
  .cmd-rules-status {
    margin-right: auto;
    font-size: 11.5px;
    color: var(--text-dim);
  }
  .appearance-btn.primary {
    border-color: rgba(var(--accent-rgb), 0.6);
    color: var(--accent);
  }
</style>

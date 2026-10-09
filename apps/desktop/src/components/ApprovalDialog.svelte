<script lang="ts">
  /**
   * Диалог подтверждения опасного действия агента.
   *
   * Ядро спрашивает событием approval_required, решение уходит командой
   * respond_approval. Сам компонент ядро не зовёт: родитель передаёт
   * onRespond, поэтому диалог тестируется без Tauri и без сети.
   */
  import Icon from './Icon.svelte'
  import DiffPreview from './DiffPreview.svelte'

  interface Props {
    summary: string
    title: string
    hint: string
    approveLabel: string
    denyLabel: string
    /**
     * F-6: unified-diff будущей правки из ядра. Есть только у файловых
     * инструментов: у bash/fetch_url показывать нечего, там решает summary.
     */
    preview?: string
    /** Подпись обрезки diff ({n} — сколько строк не влезло в DOM). */
    diffHiddenLabel?: string
    onRespond: (decision: 'approved' | 'denied') => void
  }

  let {
    summary,
    title,
    hint,
    approveLabel,
    denyLabel,
    preview = '',
    diffHiddenLabel = '',
    onRespond,
  }: Props = $props()
</script>

<div class="approval-overlay" role="presentation">
  <div class="approval-dialog" role="dialog" aria-label={title} tabindex="-1">
    <div class="approval-head">
      <Icon name="alert" size={16} />
      <h2>{title}</h2>
    </div>
    <p class="approval-hint">{hint}</p>
    <pre class="approval-summary">{summary}</pre>
    {#if preview}
      <!-- Что именно произойдёт с файлом — до того как человек решит. -->
      <DiffPreview text={preview} hiddenLabel={diffHiddenLabel} />
    {/if}
    <div class="approval-actions">
      <button class="approval-btn deny" onclick={() => onRespond('denied')}>{denyLabel}</button>
      <button class="approval-btn approve" onclick={() => onRespond('approved')}>{approveLabel}</button>
    </div>
  </div>
</div>

<style>
  /* Поверх всего: решение необратимо, диалог не должен тонуть под панелями. */
  .approval-overlay {
    position: fixed;
    inset: 0;
    z-index: 1200;
    background: rgba(0, 0, 0, 0.55);
    display: flex;
    align-items: center;
    justify-content: center;
    backdrop-filter: blur(3px);
  }
  .approval-dialog {
    width: min(560px, calc(100vw - 48px));
    background: var(--bg-panel);
    border: 1px solid rgba(var(--err-rgb), 0.45);
    border-radius: 12px;
    padding: 18px 20px;
    box-shadow: 0 18px 60px rgba(0, 0, 0, 0.5), 0 0 24px rgba(var(--err-rgb), 0.18);
    animation: content-in 0.16s ease-out;
  }
  .approval-head {
    display: flex;
    align-items: center;
    gap: 8px;
    color: var(--err);
  }
  .approval-head h2 {
    font-size: 15px;
    margin: 0;
    letter-spacing: 0.04em;
    text-transform: uppercase;
  }
  .approval-hint {
    margin: 10px 0 0;
    color: var(--text-dim);
    font-size: 12px;
    line-height: 1.5;
  }
  .approval-summary {
    margin: 12px 0 0;
    padding: 10px 12px;
    max-height: 220px;
    overflow: auto;
    background: var(--bg);
    border: 1px solid var(--border);
    border-radius: 8px;
    font-size: 12px;
    white-space: pre-wrap;
    word-break: break-all;
  }
  .approval-actions {
    display: flex;
    justify-content: flex-end;
    gap: 10px;
    margin-top: 14px;
  }
  .approval-btn {
    padding: 7px 16px;
    border-radius: 8px;
    font-size: 12.5px;
    cursor: pointer;
    border: 1px solid var(--border);
    background: transparent;
    color: var(--text);
    transition: transform 0.12s ease, box-shadow 0.12s ease, border-color 0.12s ease,
      color 0.12s ease;
  }
  .approval-btn:hover {
    transform: translateY(-1px);
  }
  .approval-btn:active {
    transform: translateY(0) scale(0.97);
  }
  .approval-btn.approve {
    border-color: rgba(var(--accent-rgb), calc(0.6 * var(--glow-k)));
    background: rgba(var(--accent-rgb), calc(0.16 * var(--glow-k)));
    color: var(--accent);
  }
  .approval-btn.approve:hover {
    box-shadow: 0 0 18px rgba(var(--accent-rgb), calc(0.35 * var(--glow-k)));
  }
  .approval-btn.deny:hover {
    border-color: rgba(var(--err-rgb), 0.6);
    color: var(--err);
    box-shadow: 0 0 18px rgba(var(--err-rgb), 0.25);
  }
</style>

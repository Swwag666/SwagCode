<script lang="ts">
  /**
   * Диалог подтверждения отката хода к git-снимку (F-5).
   *
   * Откат переписывает файлы в рабочей директории — действие заметно
   * необратимое (собственные правки, сделанные после хода, уйдут вместе с
   * агентскими), поэтому без подтверждения нельзя. Ядро компонент не зовёт:
   * родитель передаёт onConfirm/onCancel, поэтому диалог тестируется без
   * Tauri и без git.
   */
  import Icon from './Icon.svelte'

  interface Props {
    title: string
    /** Подпись хода, который откатываем. */
    turnLabel: string
    /** Короткий sha снимка: человек видит, к чему именно вернётся. */
    sha: string
    /** Что откат делает и чего НЕ делает (файлы, созданные после снимка). */
    warning: string
    confirmLabel: string
    cancelLabel: string
    busyLabel?: string
    busy?: boolean
    onConfirm: () => void
    onCancel: () => void
  }

  let {
    title,
    turnLabel,
    sha,
    warning,
    confirmLabel,
    cancelLabel,
    busyLabel = '',
    busy = false,
    onConfirm,
    onCancel,
  }: Props = $props()
</script>

<div class="revert-overlay" role="presentation">
  <div class="revert-dialog" role="dialog" aria-label={title} tabindex="-1">
    <div class="revert-head">
      <Icon name="restore" size={16} />
      <h2>{title}</h2>
    </div>
    <p class="revert-turn">
      {turnLabel}
      <span class="revert-sha" title={sha}>{sha}</span>
    </p>
    <p class="revert-warning">{warning}</p>
    <div class="revert-actions">
      <!-- busy-страховка в обработчике, а не только в `disabled`: второй
           откат не должен начинаться ни из какого окружения. -->
      <button class="revert-btn" onclick={() => { if (!busy) onCancel() }} disabled={busy}>{cancelLabel}</button>
      <button class="revert-btn danger" onclick={() => { if (!busy) onConfirm() }} disabled={busy} data-testid="revert-confirm">
        {busy && busyLabel ? busyLabel : confirmLabel}
      </button>
    </div>
  </div>
</div>

<style>
  /* Поверх панелей, но ниже диалога подтверждения инструмента: откат —
     решение человека, оно не должно тонуть под траекторией. */
  .revert-overlay {
    position: fixed;
    inset: 0;
    z-index: 1100;
    background: rgba(0, 0, 0, 0.5);
    display: flex;
    align-items: center;
    justify-content: center;
    backdrop-filter: blur(2px);
  }
  .revert-dialog {
    width: min(520px, calc(100vw - 48px));
    background: var(--bg-panel);
    border: 1px solid var(--border);
    border-radius: 12px;
    padding: 18px 20px;
    box-shadow: 0 18px 60px rgba(0, 0, 0, 0.5);
    animation: content-in 0.16s ease-out;
  }
  .revert-head {
    display: flex;
    align-items: center;
    gap: 8px;
    color: var(--accent);
  }
  .revert-head h2 {
    font-size: 15px;
    margin: 0;
    letter-spacing: 0.04em;
    text-transform: uppercase;
  }
  .revert-turn {
    margin: 12px 0 0;
    font-size: 12.5px;
    color: var(--text);
    display: flex;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
  }
  .revert-sha {
    font-family: var(--font-mono, monospace);
    font-size: 11.5px;
    color: var(--text-dim);
    background: var(--bg);
    border: 1px solid var(--border);
    border-radius: 6px;
    padding: 1px 6px;
  }
  .revert-warning {
    margin: 10px 0 0;
    color: var(--text-dim);
    font-size: 12px;
    line-height: 1.55;
  }
  .revert-actions {
    display: flex;
    justify-content: flex-end;
    gap: 10px;
    margin-top: 16px;
  }
  .revert-btn {
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
  .revert-btn:hover:not(:disabled) {
    transform: translateY(-1px);
  }
  .revert-btn:active:not(:disabled) {
    transform: translateY(0) scale(0.97);
  }
  .revert-btn:disabled {
    opacity: 0.55;
    cursor: progress;
  }
  .revert-btn.danger {
    border-color: rgba(var(--err-rgb), 0.55);
    color: var(--err);
    background: rgba(var(--err-rgb), 0.1);
  }
  .revert-btn.danger:hover:not(:disabled) {
    box-shadow: 0 0 18px rgba(var(--err-rgb), 0.25);
  }
</style>

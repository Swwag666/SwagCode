<script lang="ts">
  /**
   * Unified-diff будущей правки в диалоге подтверждения (F-6).
   *
   * Текст приходит из ядра (`fsx::format_unified_preview`): одна строка на
   * строку diff, первый символ — маркер (`+`, `-`, ` `), регионы разделены
   * заголовком `@@ -a,b +c,d @@`. Компонент только красит и режет по числу
   * строк: пересчитывать diff на клиенте значит получить вторую реализацию
   * того же алгоритма, которая однажды разойдётся с ядром.
   */

  interface Props {
    text: string
    /** Потолок строк в DOM: 64 КБ diff — это тысячи узлов, UI вешать нельзя. */
    maxLines?: number
    /** Подпись под обрезанным списком ({n} — сколько строк не показано). */
    hiddenLabel?: string
  }

  let { text, maxLines = 600, hiddenLabel = '' }: Props = $props()

  type RowKind = 'add' | 'del' | 'ctx' | 'hunk'
  interface Row {
    kind: RowKind
    text: string
  }

  const rows = $derived.by<Row[]>(() => {
    const lines = text.split('\n')
    // Завершающий перевод строки даёт пустой хвост — строкой diff он не является.
    if (lines.length > 0 && lines[lines.length - 1] === '') lines.pop()
    return lines.map((l) => {
      const kind: RowKind = l.startsWith('@@')
        ? 'hunk'
        : l.startsWith('+')
          ? 'add'
          : l.startsWith('-')
            ? 'del'
            : 'ctx'
      return { kind, text: kind === 'hunk' || kind === 'ctx' ? l.replace(/^ /, '') : l.slice(1) }
    })
  })

  const shown = $derived(rows.slice(0, maxLines))
  const hidden = $derived(rows.length - shown.length)
  const adds = $derived(rows.filter((r) => r.kind === 'add').length)
  const dels = $derived(rows.filter((r) => r.kind === 'del').length)

  const marker = (k: RowKind): string =>
    k === 'add' ? '+' : k === 'del' ? '\u2212' : k === 'hunk' ? '' : ' '
</script>

<div class="diff-preview" data-testid="diff-preview">
  <div class="dp-head">
    <span class="dp-stat adds" data-testid="dp-adds">+{adds}</span>
    <span class="dp-stat dels" data-testid="dp-dels">−{dels}</span>
  </div>
  <div class="dp-body">
    {#each shown as row, i (i)}
      <div class="dp-line {row.kind}">
        <span class="dp-marker">{marker(row.kind)}</span>
        <span class="dp-text">{row.text}</span>
      </div>
    {/each}
    {#if hidden > 0 && hiddenLabel}
      <div class="dp-hidden" data-testid="dp-hidden">{hiddenLabel.replace('{n}', String(hidden))}</div>
    {/if}
  </div>
</div>

<style>
  .diff-preview {
    margin-top: 12px;
    border: 1px solid var(--border);
    border-radius: 8px;
    background: var(--bg);
    overflow: hidden;
  }
  .dp-head {
    display: flex;
    gap: 10px;
    padding: 5px 10px;
    border-bottom: 1px solid var(--border);
    background: var(--bg-elevated, rgba(255, 255, 255, 0.03));
    font-family: var(--mono, monospace);
    font-size: 11px;
  }
  .dp-stat.adds {
    color: var(--ok);
  }
  .dp-stat.dels {
    color: var(--err);
  }
  .dp-body {
    max-height: 260px;
    overflow: auto;
    font-family: var(--mono, monospace);
    font-size: 11.5px;
    line-height: 1.5;
  }
  .dp-line {
    display: flex;
    gap: 6px;
    padding: 0 8px;
    white-space: pre;
  }
  .dp-line.add {
    background: rgba(63, 185, 80, 0.09);
    color: var(--ok);
  }
  .dp-line.del {
    background: rgba(255, 45, 68, 0.09);
    color: var(--err);
  }
  .dp-line.ctx {
    color: var(--text-dim);
  }
  .dp-line.hunk {
    color: var(--accent);
    background: rgba(var(--accent-rgb), 0.07);
    padding-top: 2px;
    padding-bottom: 2px;
  }
  .dp-marker {
    flex: none;
    width: 10px;
    user-select: none;
  }
  .dp-text {
    white-space: pre-wrap;
    word-break: break-word;
  }
  .dp-hidden {
    padding: 6px 10px;
    color: var(--text-faint);
    font-size: 11px;
    border-top: 1px dashed var(--border);
  }
</style>

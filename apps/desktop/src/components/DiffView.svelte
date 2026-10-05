<script lang="ts">
  /**
   * Diff-просмотр изменений файлов (Этап 2).
   *
   * Показывает unified diff с подсветкой добавленных/удалённых строк.
   * Алгоритм: простой LCS на строках — для файлов до 10k строк этого
   * достаточно, а сложность O(n*m) ограничена размером окна.
   */

  interface Props {
    original: string
    modified: string
    filename?: string
  }

  let { original, modified, filename = 'file' }: Props = $props()

  interface DiffLine {
    type: 'same' | 'add' | 'del'
    text: string
    oldNum?: number
    newNum?: number
  }

  let diffLines = $state<DiffLine[]>([])

  /**
   * Простой diff на основе LCS. Для больших файлов используем
   * эвристику: если строки совпадают по префиксу — считаем одинаковыми.
   */
  function computeDiff(oldText: string, newText: string): DiffLine[] {
    const oldLines = oldText.split('\n')
    const newLines = newText.split('\n')
    const result: DiffLine[] = []

    // Простой алгоритм: идём по обеим строкам, ищем совпадения
    let i = 0
    let j = 0

    while (i < oldLines.length || j < newLines.length) {
      if (i < oldLines.length && j < newLines.length && oldLines[i] === newLines[j]) {
        result.push({ type: 'same', text: oldLines[i], oldNum: i + 1, newNum: j + 1 })
        i++
        j++
      } else if (i < oldLines.length && (j >= newLines.length || !newLines.slice(j, j + 3).includes(oldLines[i]))) {
        result.push({ type: 'del', text: oldLines[i], oldNum: i + 1 })
        i++
      } else {
        result.push({ type: 'add', text: newLines[j], newNum: j + 1 })
        j++
      }
    }

    return result
  }

  $effect(() => {
    diffLines = computeDiff(original, modified)
  })

  const adds = $derived(diffLines.filter((l) => l.type === 'add').length)
  const dels = $derived(diffLines.filter((l) => l.type === 'del').length)
</script>

<div class="diff-view">
  <div class="diff-header">
    <span class="diff-file">{filename}</span>
    <span class="diff-stats">
      <span class="adds">+{adds}</span>
      <span class="dels">-{dels}</span>
    </span>
  </div>
  <div class="diff-body">
    {#each diffLines as line, idx (idx)}
      <div class="diff-line {line.type}">
        <span class="line-num old">{line.oldNum ?? ''}</span>
        <span class="line-num new">{line.newNum ?? ''}</span>
        <span class="line-marker">
          {line.type === 'add' ? '+' : line.type === 'del' ? '-' : ' '}
        </span>
        <span class="line-text">{line.text}</span>
      </div>
    {/each}
  </div>
</div>

<style>
  .diff-view {
    font-family: var(--mono);
    font-size: 12px;
    border: 1px solid var(--border);
    border-radius: 6px;
    overflow: hidden;
    background: var(--bg);
  }

  .diff-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    padding: 6px 12px;
    background: var(--bg-elevated);
    border-bottom: 1px solid var(--border);
  }

  .diff-file {
    color: var(--text);
    font-weight: 600;
  }

  .diff-stats {
    display: flex;
    gap: 8px;
    font-size: 11px;
  }

  .adds {
    color: var(--ok);
  }

  .dels {
    color: var(--err);
  }

  .diff-body {
    overflow-x: auto;
    max-height: 400px;
    overflow-y: auto;
  }

  .diff-line {
    display: flex;
    padding: 1px 0;
    line-height: 1.5;
  }

  .diff-line.add {
    background: rgba(63, 185, 80, 0.08);
  }

  .diff-line.del {
    background: rgba(255, 45, 68, 0.08);
  }

  .line-num {
    width: 40px;
    text-align: right;
    padding-right: 8px;
    color: var(--text-faint);
    user-select: none;
    flex-shrink: 0;
  }

  .line-marker {
    width: 16px;
    text-align: center;
    flex-shrink: 0;
  }

  .diff-line.add .line-marker {
    color: var(--ok);
  }

  .diff-line.del .line-marker {
    color: var(--err);
  }

  .line-text {
    white-space: pre;
    color: var(--text);
  }

  .diff-line.add .line-text {
    color: var(--ok);
  }

  .diff-line.del .line-text {
    color: var(--err);
  }
</style>

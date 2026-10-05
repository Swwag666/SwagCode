<script lang="ts">
  /**
   * Терминал / command runner (Этап 2, упрощённый).
   *
   * Запускает команды в рабочей директории и показывает вывод.
   * Полноценный PTY с ANSI-парсером — следующий шаг (crates/pty).
   * Здесь: stdin не интерактивный, вывод стримится построчно.
   */
  import { invoke } from '@tauri-apps/api/core'

  interface TermLine {
    text: string
    kind: 'cmd' | 'out' | 'err' | 'info'
    html?: string
  }

  interface Props {
    cwd: string
  }

  let { cwd }: Props = $props()

  let lines = $state<TermLine[]>([])
  let input = $state('')
  let running = $state(false)
  let history = $state<string[]>(JSON.parse(localStorage.getItem('swagcod-term-history') || '[]'))
  let historyIdx = $state(-1)
  let outputEl: HTMLDivElement | undefined = $state()

  // Сохраняем историю команд
  $effect(() => {
    localStorage.setItem('swagcod-term-history', JSON.stringify(history.slice(-50)))
  })

  // W-4 фикс: автопрокрутка читает lines.length чтобы эффект срабатывал
  $effect(() => {
    void lines.length // зависимость от количества строк
    if (outputEl) {
      outputEl.scrollTop = outputEl.scrollHeight
    }
  })

  /**
   * Парсинг ANSI escape-последовательностей в HTML.
   * Поддерживает: цвета (30-37, 90-97), жирный (1), сброс (0).
   */
  function ansiToHtml(text: string): string {
    const colors: Record<string, string> = {
      '30': '#484f58', '31': '#ff7b72', '32': '#3fb950', '33': '#d29922',
      '34': '#58a6ff', '35': '#bc8cff', '36': '#39c5cf', '37': '#c9d1d9',
      '90': '#8b949e', '91': '#ff7b72', '92': '#3fb950', '93': '#d29922',
      '94': '#58a6ff', '95': '#bc8cff', '96': '#39c5cf', '97': '#f0f6fc',
    }

    let html = ''
    let currentColor = ''
    let bold = false
    // eslint-disable-next-line no-control-regex
    const regex = /\x1b\[([0-9;]*)m/g
    let lastIndex = 0
    let match: RegExpExecArray | null

    while ((match = regex.exec(text)) !== null) {
      // Текст до escape-последовательности
      const plain = text.slice(lastIndex, match.index)
      if (plain) {
        html += wrapSpan(escapeHtml(plain), currentColor, bold)
      }
      lastIndex = regex.lastIndex

      // Парсим коды
      const codes = match[1].split(';')
      for (const code of codes) {
        if (code === '0' || code === '') {
          currentColor = ''
          bold = false
        } else if (code === '1') {
          bold = true
        } else if (colors[code]) {
          currentColor = colors[code]
        }
      }
    }

    // Оставшийся текст
    const remaining = text.slice(lastIndex)
    if (remaining) {
      html += wrapSpan(escapeHtml(remaining), currentColor, bold)
    }

    return html || escapeHtml(text)
  }

  function wrapSpan(text: string, color: string, bold: boolean): string {
    if (!color && !bold) return text
    const style = [color ? `color:${color}` : '', bold ? 'font-weight:700' : '']
      .filter(Boolean)
      .join(';')
    return `<span style="${style}">${text}</span>`
  }

  function escapeHtml(s: string): string {
    return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
  }

  /**
   * Tab-completion: дополняет команду из истории.
   * Ищет последнюю команду начинающуюся с текущего ввода.
   */
  function autoComplete(): void {
    if (!input.trim() || history.length === 0) return
    const prefix = input.trim()
    // Ищем с конца истории
    for (let i = history.length - 1; i >= 0; i--) {
      if (history[i].startsWith(prefix) && history[i] !== input) {
        input = history[i]
        return
      }
    }
  }

  function addLine(text: string, kind: TermLine['kind']): void {
    const html = kind === 'out' || kind === 'err' ? ansiToHtml(text) : undefined
    lines = [...lines, { text, kind, html }]
    // Лимит: не больше 500 строк в DOM
    if (lines.length > 500) {
      lines = lines.slice(-400)
    }
  }

  async function runCommand(): Promise<void> {
    const cmd = input.trim()
    if (!cmd || running) return

    input = ''
    history.push(cmd)
    historyIdx = -1
    running = true

    addLine(`$ ${cmd}`, 'cmd')

    try {
      const result = await invoke<{ stdout: string; stderr: string; code: number }>(
        'run_command',
        { command: cmd, cwd }
      )
      if (result.stdout) {
        for (const line of result.stdout.split('\n')) {
          addLine(line, 'out')
        }
      }
      if (result.stderr) {
        for (const line of result.stderr.split('\n')) {
          addLine(line, 'err')
        }
      }
      addLine(`exit: ${result.code}`, result.code === 0 ? 'info' : 'err')
    } catch (err) {
      addLine(`ошибка: ${err}`, 'err')
    } finally {
      running = false
    }
  }

  function onKeydown(e: KeyboardEvent): void {
    if (e.key === 'Enter') {
      e.preventDefault()
      runCommand()
    } else if (e.key === 'Tab') {
      e.preventDefault()
      autoComplete()
    } else if (e.key === 'ArrowUp') {
      e.preventDefault()
      if (history.length > 0) {
        historyIdx = historyIdx === -1 ? history.length - 1 : Math.max(0, historyIdx - 1)
        input = history[historyIdx]
      }
    } else if (e.key === 'ArrowDown') {
      e.preventDefault()
      if (historyIdx >= 0) {
        historyIdx++
        if (historyIdx >= history.length) {
          historyIdx = -1
          input = ''
        } else {
          input = history[historyIdx]
        }
      }
    }
  }
</script>

<div class="terminal">
  <div class="term-header">
    <span class="term-title">терминал</span>
    <span class="term-cwd">{cwd.split(/[/\\]/).pop()}</span>
  </div>
  <div class="term-output" bind:this={outputEl}>
    {#each lines as line, i (i)}
      {#if line.html}
        <div class="term-line {line.kind}">{@html line.html}</div>
      {:else}
        <div class="term-line {line.kind}">{line.text}</div>
      {/if}
    {/each}
    {#if running}
      <div class="term-line info">выполняется…</div>
    {/if}
  </div>
  <div class="term-input-row">
    <span class="term-prompt">$</span>
    <input
      type="text"
      class="term-input"
      bind:value={input}
      onkeydown={onKeydown}
      placeholder="команда…"
      disabled={running}
      spellcheck="false"
    />
  </div>
</div>

<style>
  .terminal {
    display: flex;
    flex-direction: column;
    height: 100%;
    background: var(--bg);
    border-top: 1px solid var(--border);
    font-family: var(--mono);
    font-size: 12px;
  }

  .term-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    padding: 6px 12px;
    border-bottom: 1px solid var(--border);
    background: rgba(20, 20, 25, 0.95);
  }

  .term-title {
    color: var(--text-dim);
    text-transform: uppercase;
    letter-spacing: 0.08em;
    font-size: 10px;
  }

  .term-cwd {
    color: var(--accent);
    font-size: 11px;
  }

  .term-output {
    flex: 1;
    overflow-y: auto;
    padding: 8px 12px;
    min-height: 0;
  }

  .term-line {
    white-space: pre-wrap;
    word-break: break-all;
    line-height: 1.4;
  }

  .term-line.cmd {
    color: var(--accent);
    font-weight: 600;
  }

  .term-line.out {
    color: var(--text);
  }

  .term-line.err {
    color: var(--err);
  }

  .term-line.info {
    color: var(--text-dim);
    font-style: italic;
  }

  .term-input-row {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 8px 12px;
    border-top: 1px solid var(--border);
    background: rgba(20, 20, 25, 0.95);
  }

  .term-prompt {
    color: var(--accent);
    font-weight: 700;
  }

  .term-input {
    flex: 1;
    background: transparent;
    border: none;
    color: var(--text);
    font-family: var(--mono);
    font-size: 12px;
    outline: none;
  }

  .term-input::placeholder {
    color: var(--text-faint);
  }
</style>

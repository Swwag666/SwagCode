<script lang="ts">
  /**
   * B-2: терминал на настоящем PTY. События pty_output приходят уже
   * разобранными ANSI-парсером из Rust: компонент только рисует операции.
   * Это лог-вью потока (цвета SGR есть, экранной модели нет) — полноэкранные
   * приложения (vim, top) будут линейным текстом, что на этом этапе
   * осознанно допустимо.
   */
  import { onDestroy, onMount } from 'svelte'
  import { invoke } from '@tauri-apps/api/core'
  import { bus, type PtyOp } from '../lib/bus'
  import { STR } from '../lib/strings'
  import Icon from './Icon.svelte'

  const {
    sessionId,
    lang = 'ru',
  }: {
    sessionId: string | null
    lang?: 'ru' | 'en'
  } = $props()

  const t = $derived(STR[lang])

  let ptyId = $state<string | null>(null)
  let starting = $state(false)
  let err = $state('')
  let exitCode = $state<number | null>(null)
  let input = $state('')
  let html = $state('')
  let stick = $state(true)
  let out = $state<HTMLPreElement>()
  let unsub: (() => void) | null = null

  // Палитра SGR: 0-7 обычные, 8-15 яркие (порядок ANSI).
  const PALETTE = [
    '#5f8fd9', '#55b06a', '#d9a441', '#c9885a',
    '#b57edc', '#4db6ac', '#e05555', '#c8c8c8',
    '#7fb2f5', '#6fd98a', '#f5c86a', '#e8a07f',
    '#d9a6f5', '#6fd9cd', '#ff7a7a', '#ffffff',
  ]

  let fg: number | null = null
  let bold = false
  let dim = false

  function esc(s: string): string {
    return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
  }

  function spanStyle(): string {
    const parts: string[] = []
    if (fg !== null) parts.push(`color:${PALETTE[fg] ?? '#c8c8c8'}`)
    if (bold) parts.push('font-weight:700')
    if (dim) parts.push('opacity:.55')
    return parts.join(';')
  }

  function applyOps(ops: PtyOp[]): void {
    let buf = ''
    for (const op of ops) {
      if (op.t === 'text') {
        const s = esc(op.s)
        const st = spanStyle()
        buf += st ? `<span style="${st}">${s}</span>` : s
      } else if (op.t === 'sgr') {
        if (op.reset) {
          fg = null
          bold = false
          dim = false
        }
        if (op.fg !== null) fg = op.fg
        if (op.bold !== null) bold = op.bold
        if (op.dim !== null) dim = op.dim
      }
      // csi/osc лог-вью игнорирует: эмулятор экрана придёт позже.
    }
    if (!buf) return
    html += buf
    // Потолок: длинный бурный билд не должен распухать в WebView.
    if (html.length > 400_000) {
      const cut = html.indexOf('\n', html.length - 200_000)
      html = cut > 0 ? html.slice(cut + 1) : html.slice(-200_000)
    }
    if (stick) {
      requestAnimationFrame(() => {
        if (out) out.scrollTop = out.scrollHeight
      })
    }
  }

  async function start(): Promise<void> {
    if (!sessionId || starting) return
    starting = true
    err = ''
    exitCode = null
    html = ''
    fg = null
    bold = false
    dim = false
    try {
      ptyId = await invoke<string>('pty_spawn', { sessionId, shell: null })
    } catch (e) {
      err = String(e)
    } finally {
      starting = false
    }
  }

  async function stop(): Promise<void> {
    if (!ptyId) return
    const id = ptyId
    ptyId = null
    try {
      await invoke('pty_kill', { ptyId: id })
    } catch {
      /* игнор: сессия могла уже закрыться */
    }
  }

  function send(): void {
    if (!ptyId) return
    const text = input
    input = ''
    invoke('pty_write', { ptyId, text: text + '\r' }).catch(() => {})
  }

  // Прокрутка решает всё: у дна — прилипаем, выше дна — отвязываемся.
  function onScroll(): void {
    if (out) stick = out.scrollHeight - out.scrollTop - out.clientHeight < 40
  }

  onMount(() => {
    unsub = bus.subscribe((batch) => {
      for (const ev of batch) {
        const k = ev.kind
        if (k.kind === 'pty_output' && k.data.pty === ptyId) applyOps(k.data.ops)
        else if (k.kind === 'pty_exit' && k.data.pty === ptyId) {
          exitCode = k.data.code
          ptyId = null
        }
      }
    })
  })

  onDestroy(() => {
    unsub?.()
    if (ptyId) {
      const id = ptyId
      ptyId = null
      invoke('pty_kill', { ptyId: id }).catch(() => {})
    }
  })

  // Панель изменилась — пересчитать cols/rows и сказать PTY.
  $effect(() => {
    const el = out
    const pid = ptyId
    if (!el || !pid) return
    const push = () => {
      const cols = Math.max(20, Math.floor(el.clientWidth / 7.6))
      const rows = Math.max(6, Math.floor(el.clientHeight / 17))
      invoke('pty_resize', { ptyId: pid, cols, rows }).catch(() => {})
    }
    const ro = new ResizeObserver(push)
    ro.observe(el)
    push()
    return () => ro.disconnect()
  })
</script>

<div class="term">
  <div class="term-bar">
    {#if ptyId}
      <span class="term-live"><span class="live-dot"></span>{t.terminalLive}</span>
      <button class="mini-btn" onclick={stop}>
        <Icon name="close" size={13} />
        {t.stopTerminal}
      </button>
    {:else if exitCode !== null}
      <span class="term-exit">{t.ptyExited} ({exitCode})</span>
      <button class="mini-btn" onclick={start} disabled={!sessionId}>
        <Icon name="external" size={13} />
        {t.restartTerminal}
      </button>
    {:else}
      <button class="mini-btn" onclick={start} disabled={starting || !sessionId}>
        <Icon name="external" size={13} />
        {starting ? t.startingTerminal : t.startTerminal}
      </button>
      {#if !sessionId}<span class="term-hint">{t.noSession}</span>{/if}
    {/if}
    {#if err}<span class="term-err">{err}</span>{/if}
  </div>

  <pre
    class="term-out"
    bind:this={out}
    role="log"
    aria-live="polite"
    onscroll={onScroll}>{@html html}</pre>

  <div class="term-input-row">
    <span class="term-prompt">&gt;</span>
    <input
      class="term-input"
      bind:value={input}
      onkeydown={(e) => {
        if (e.key === 'Enter') send()
      }}
      placeholder={ptyId ? t.termInputPlaceholder : t.noTerminal}
      disabled={!ptyId}
      aria-label={t.termInputLabel}
    />
  </div>
</div>

<style>
  .term {
    flex: 1;
    min-height: 0;
    display: flex;
    flex-direction: column;
    gap: 8px;
  }
  .term-bar {
    flex: none;
    display: flex;
    align-items: center;
    gap: 10px;
    font-size: 11px;
    color: var(--text-dim);
  }
  .term-live {
    display: flex;
    align-items: center;
    gap: 6px;
  }
  .live-dot {
    width: 6px;
    height: 6px;
    border-radius: 50%;
    background: var(--ok);
    animation: term-pulse 1.4s ease-in-out infinite;
  }
  @keyframes term-pulse {
    0%, 100% { opacity: 0.4; }
    50% { opacity: 1; }
  }
  .term-exit {
    color: var(--text-dim);
  }
  .term-err {
    color: var(--err);
    font-size: 11px;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    max-width: 420px;
  }
  .term-hint {
    font-size: 11px;
    color: var(--text-faint);
  }
  .term-out {
    flex: 1;
    min-height: 0;
    margin: 0;
    overflow: auto;
    font-family: var(--mono);
    font-size: 12.5px;
    line-height: 17px;
    white-space: pre-wrap;
    word-break: break-word;
    color: var(--text);
    background: var(--surface-chrome);
    border: 1px solid var(--border);
    border-radius: 10px;
    padding: 10px 12px;
  }
  .term-input-row {
    flex: none;
    display: flex;
    align-items: center;
    gap: 8px;
    border: 1px solid var(--border);
    border-radius: 10px;
    padding: 7px 12px;
    background: var(--surface-chrome);
  }
  .term-prompt {
    color: var(--accent);
    font-family: var(--mono);
    font-size: 13px;
    flex: none;
  }
  .term-input {
    flex: 1;
    min-width: 0;
    border: none;
    outline: none;
    background: transparent;
    color: var(--text);
    font-family: var(--mono);
    font-size: 13px;
  }
  .term-input:disabled {
    color: var(--text-faint);
  }
  .mini-btn {
    display: inline-flex;
    align-items: center;
    gap: 5px;
    padding: 4px 10px;
    border: 1px solid var(--border);
    border-radius: 6px;
    background: none;
    color: var(--text-dim);
    font-size: 11px;
    cursor: pointer;
    transition: all 0.15s;
  }
  .mini-btn:hover:not(:disabled) {
    color: var(--accent);
    border-color: var(--accent-dim);
  }
  .mini-btn:disabled {
    opacity: 0.5;
    cursor: default;
  }
</style>

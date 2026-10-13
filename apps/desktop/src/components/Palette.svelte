<script lang="ts">
  import { filterCommands, type PaletteCommand, type Translate } from '../lib/palette'

  /* F-10: палитра команд уехала из App.svelte в компонент. Запрос и
     выделенная строка — локальное состояние, команды и перевод приходят
     сверху, поэтому компонент рендерится и тестируется без всего приложения.
     Поведение прежнее: Enter исполняет выделенную команду, клик мимо и
     Escape закрывают, стрелки двигают выделение. */
  interface Props {
    open: boolean
    commands: PaletteCommand[]
    t: Translate
    onclose?: () => void
  }

  let { open = $bindable(false), commands, t, onclose }: Props = $props()

  let query = $state('')
  let idx = $state(0)
  let inputEl: HTMLInputElement | undefined = $state()

  const filtered = $derived(filterCommands(commands, query, t))

  function close(): void {
    open = false
    query = ''
    idx = 0
    onclose?.()
  }

  function pick(c: PaletteCommand): void {
    c.action()
    close()
  }

  /* Открытие сбрасывает запрос и ставит фокус в поле. Раньше это делал App
     через document.querySelector('.palette-input') с задержкой 50 мс из двух
     мест (Ctrl+P и «/» в пустом вводе); эффект живёт в компоненте, поэтому
     глобальный поиск по DOM больше не нужен, а задержка осталась та же —
     оверлей ещё анимируется. */
  $effect(() => {
    if (!open) return
    query = ''
    idx = 0
    const timer = setTimeout(() => inputEl?.focus(), 50)
    return () => clearTimeout(timer)
  })
</script>

{#if open}
  <div class="palette-overlay" onclick={close} role="presentation">
    <div
      class="palette"
      onclick={(e) => e.stopPropagation()}
      onkeydown={(e) => { if (e.key === 'Escape') close() }}
      role="dialog"
      aria-label="палитра команд"
      tabindex="-1"
    >
      <input
        type="text"
        class="palette-input"
        placeholder={t('palPlaceholder')}
        bind:value={query}
        bind:this={inputEl}
        oninput={() => (idx = 0)}
        onkeydown={(e) => {
          if (e.key === 'ArrowDown') { e.preventDefault(); idx = Math.min(idx + 1, filtered.length - 1) }
          else if (e.key === 'ArrowUp') { e.preventDefault(); idx = Math.max(idx - 1, 0) }
          else if (e.key === 'Enter' && filtered[idx]) {
            pick(filtered[idx])
          }
        }}
        spellcheck="false"
      />
      <div class="palette-list" role="listbox">
        {#each filtered as cmd, i (cmd.slash)}
          {#if i === 0 || filtered[i - 1].group !== cmd.group}
            <div class="palette-group">{t(cmd.group)}</div>
          {/if}
          <div
            class="palette-item"
            class:selected={i === idx}
            onclick={() => pick(cmd)}
            onmouseenter={() => (idx = i)}
            onkeydown={(e) => {
              if (e.key === 'Enter' || e.key === ' ') {
                e.preventDefault()
                pick(cmd)
              }
            }}
            role="option"
            tabindex="0"
            aria-selected={i === idx}
          >
            <span class="palette-cmd">{cmd.slash}</span>
            <span class="palette-desc">{t(cmd.desc)}</span>
          </div>
        {/each}
        {#if filtered.length === 0}
          <div class="palette-empty">{t('palEmpty')}</div>
        {/if}
      </div>
    </div>
  </div>
{/if}

<style>
.palette-overlay {
  position: fixed;
  inset: 0;
  z-index: 500;
  background: var(--surface-overlay);
  display: flex;
  align-items: flex-start;
  justify-content: center;
  padding-top: 15vh;
  transition: background 0.15s;
}

.palette-overlay:nth-child(odd):hover {
  background: rgba(0, 0, 0, 0.75);
  backdrop-filter: blur(8px);
}

.palette-overlay:nth-child(even):hover {
  background: rgba(0, 0, 0, 0.7);
  backdrop-filter: blur(6px);
}

.palette-overlay:hover {
  background: rgba(0, 0, 0, 0.6);
}

.palette {
  width: 400px;
  max-width: 90vw;
  background: var(--bg-elevated);
  border: 1px solid var(--border);
  border-radius: 10px;
  overflow: hidden;
  box-shadow: var(--shadow-strong);
  animation: palette-in 0.2s ease-out;
  transition: all 0.15s;
}

.palette:hover {
  border-color: var(--accent-dim);
  box-shadow: 0 16px 56px rgba(var(--accent-rgb), calc(0.15 * var(--glow-k)));
  transform: scale(1.01);
}

@keyframes palette-in {
  from {
    opacity: 0;
    transform: scale(0.95) translateY(-10px);
  }
  to {
    opacity: 1;
    transform: scale(1) translateY(0);
  }
}

.palette-input {
  width: 100%;
  padding: 12px 16px;
  background: transparent;
  border: none;
  border-bottom: 1px solid var(--border);
  color: var(--text);
  font-family: var(--mono);
  font-size: 14px;
  outline: none;
  transition: all 0.15s;
}

.palette-input:focus {
  border-bottom-color: var(--accent);
  box-shadow: 0 1px 0 0 var(--accent);
}

.palette-input:hover {
  background: rgba(var(--accent-rgb), calc(0.01 * var(--glow-k)));
  border-bottom-color: var(--accent-dim);
}

.palette-list {
  max-height: 300px;
  overflow-y: auto;
  padding: 4px;
  transition: background 0.15s;
}

.palette-list:nth-child(odd):hover {
  background: rgba(var(--accent-rgb), calc(0.04 * var(--glow-k)));
  box-shadow: inset 0 0 20px rgba(var(--accent-rgb), calc(0.06 * var(--glow-k)));
  border-radius: 8px;
}

.palette-list:nth-child(even):hover {
  background: rgba(255, 255, 255, 0.04);
  box-shadow: inset 0 0 16px rgba(255, 255, 255, 0.04);
  border-radius: 6px;
}

.palette-list:hover {
  background: rgba(255, 255, 255, 0.01);
}

.palette-item {
  padding: 8px 12px;
  border-radius: 6px;
  cursor: pointer;
  font-family: var(--mono);
  font-size: 12px;
  color: var(--text);
  transition: all 0.15s;
}

.palette-item:nth-child(odd):hover {
  background: rgba(var(--accent-rgb), calc(0.15 * var(--glow-k)));
  transform: translateX(8px);
  box-shadow: 0 0 20px rgba(var(--accent-rgb), calc(0.2 * var(--glow-k)));
}

.palette-item:nth-child(even):hover {
  background: rgba(var(--accent-rgb), calc(0.12 * var(--glow-k)));
  transform: translateX(6px);
  box-shadow: 0 0 16px rgba(var(--accent-rgb), calc(0.18 * var(--glow-k)));
}

.palette-item:hover {
  background: rgba(var(--accent-rgb), calc(0.08 * var(--glow-k)));
  transform: translateX(4px);
  box-shadow: 0 0 12px rgba(var(--accent-rgb), calc(0.15 * var(--glow-k)));
}

.palette-item.selected {
  background: rgba(var(--accent-rgb), calc(0.1 * var(--glow-k)));
  color: var(--accent);
  border-left: 2px solid var(--accent);
  box-shadow: 0 0 12px rgba(var(--accent-rgb), calc(0.1 * var(--glow-k)));
}

.palette-item.selected:nth-child(odd):hover {
  background: rgba(var(--accent-rgb), calc(0.2 * var(--glow-k)));
  box-shadow: 0 0 20px rgba(var(--accent-rgb), calc(0.25 * var(--glow-k)));
}

.palette-item.selected:nth-child(even):hover {
  background: rgba(var(--accent-rgb), calc(0.18 * var(--glow-k)));
  box-shadow: 0 0 16px rgba(var(--accent-rgb), calc(0.22 * var(--glow-k)));
}

.palette-item.selected:hover {
  background: rgba(var(--accent-rgb), calc(0.15 * var(--glow-k)));
  box-shadow: 0 0 16px rgba(var(--accent-rgb), calc(0.2 * var(--glow-k)));
}

.palette-empty {
  padding: 16px;
  text-align: center;
  color: var(--text-faint);
  font-size: 12px;
  transition: color 0.15s;
}

/* F-2: группы и пара «slash-имя + описание» как в DSH-референсе. */
.palette-group {
  padding: 8px 12px 2px;
  font-size: 10px;
  letter-spacing: 0.08em;
  text-transform: uppercase;
  color: var(--text-faint);
  user-select: none;
}

.palette-item {
  display: flex;
  align-items: baseline;
}

.palette-cmd {
  font-weight: 600;
  margin-right: 10px;
  white-space: nowrap;
  color: var(--text);
}

.palette-desc {
  color: var(--text-faint);
  font-size: 11px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.palette-empty:nth-child(odd):hover {
  color: var(--text);
  text-shadow: 0 0 8px rgba(255, 255, 255, 0.1);
}

.palette-empty:nth-child(even):hover {
  color: var(--text-dim);
  text-shadow: 0 0 6px rgba(255, 255, 255, 0.08);
}

.palette-empty:hover {
  color: var(--text-dim);
}
</style>

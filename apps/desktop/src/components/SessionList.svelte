<script lang="ts">
  /**
   * Список сессий (мульти-сессии, Этап 2).
   *
   * Показывает активные сессии с статусом и количеством ходов.
   * Переключение по клику, создание новой — кнопка «+».
   */
  import { invoke } from '@tauri-apps/api/core'
  import Icon from './Icon.svelte'

  interface SessionInfo {
    id: string
    title: string
    cwd: string
    model: string
    status: string
    turns: number
    est_context_tokens: number
    last_ok: boolean | null
  }

  interface Props {
    activeId: string | null
    onSelect: (id: string) => void
    onCreate: () => void
    /** Сигнал обновления из App: растёт только на ходовых событиях шины.
        Таймера больше нет — список не дёргается и не жрёт инвоки впустую. */
    refreshTick?: number
    /** Живой статус из событий шины: мозг/галочка/треугольник без опроса. */
    liveStates?: Record<string, 'running' | 'ok' | 'fail'>
  }

  let { activeId, onSelect, onCreate, refreshTick = 0, liveStates = {} }: Props = $props()

  let sessions = $state<SessionInfo[]>([])
  let contextMenu = $state<{ x: number; y: number; session: SessionInfo } | null>(null)

  function onContextMenu(e: MouseEvent, s: SessionInfo): void {
    e.preventDefault()
    contextMenu = { x: e.clientX, y: e.clientY, session: s }
  }

  function closeContextMenu(): void {
    contextMenu = null
  }

  function deleteSession(): void {
    // TODO: invoke delete_session
    closeContextMenu()
  }

  async function refresh(): Promise<void> {
    try {
      sessions = await invoke<SessionInfo[]>('list_sessions')
    } catch {
      sessions = []
    }
  }

  /** Сессия занята: живой статус из шины важнее опросного статуса ядра. */
  function isBusy(s: SessionInfo): boolean {
    return liveStates[s.id] === 'running' || s.status === 'running' || s.status === 'waiting_approval'
  }

  /** Последний ход упал или остановлен пользователем. */
  function isFailed(s: SessionInfo): boolean {
    return liveStates[s.id] === 'fail' || s.status === 'failed' || (s.last_ok === false && liveStates[s.id] !== 'ok')
  }

  /** Ходы были и последний завершился успешно. */
  function isDone(s: SessionInfo): boolean {
    return liveStates[s.id] === 'ok' || (!isBusy(s) && !isFailed(s) && s.last_ok === true)
  }

  // Обновляем при монтировании и по сигналу ходовых событий из App.
  $effect(() => {
    void refreshTick
    refresh()
  })
</script>

<div class="sessions">
  <div class="sessions-header">
    <span class="sessions-title">сессии</span>
    <button class="sessions-add" onclick={onCreate} title="новая сессия">+</button>
  </div>
  <div class="sessions-list" role="listbox" aria-label="сессии">
    {#each sessions as s (s.id)}
      <div
        class="session-item"
        class:active={s.id === activeId}
        onclick={() => onSelect(s.id)}
        oncontextmenu={(e) => onContextMenu(e, s)}
        onkeydown={(e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); onSelect(s.id) } }}
        role="option"
        tabindex="0"
        aria-selected={s.id === activeId}
      >
        {#if isBusy(s)}
          <span class="session-state state-run" title="нейронка работает">
            <Icon name="brain" size={13} />
          </span>
        {:else if isFailed(s)}
          <span class="session-state state-err" title="последний ход завершился ошибкой">
            <Icon name="alert" size={13} />
          </span>
        {:else if isDone(s)}
          <span class="session-state state-ok" title="ход завершён успешно">
            <Icon name="check" size={13} />
          </span>
        {:else}
          <span class="session-dot" title="ходов ещё не было"></span>
        {/if}
        <div class="session-info">
          <span class="session-name">
            {s.title || s.id.slice(0, 8)}
            <span class="session-id">#{s.id.slice(0, 4)}</span>
          </span>
          <span class="session-meta">{s.turns} ходов · {s.model.split('-').pop()}</span>
        </div>
      </div>
    {/each}
    {#if sessions.length === 0}
      <div class="sessions-empty">нет сессий</div>
    {/if}
  </div>
</div>

{#if contextMenu}
  <div
    class="session-context-menu"
    style="left: {contextMenu.x}px; top: {contextMenu.y}px"
    onclick={closeContextMenu}
    onkeydown={(e) => { if (e.key === 'Escape') closeContextMenu() }}
    role="menu"
    tabindex="-1"
  >
    <button onclick={deleteSession} role="menuitem">удалить сессию</button>
  </div>
{/if}

<style>
  .sessions {
    display: flex;
    flex-direction: column;
    border-bottom: 1px solid var(--border);
    max-height: 200px;
  }

  .sessions-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    padding: 6px 12px;
    border-bottom: 1px solid var(--border);
    transition: background 0.15s;
  }

  .sessions-header:hover {
    background: rgba(255, 255, 255, 0.02);
  }

  .sessions-title {
    color: var(--text-dim);
    text-transform: uppercase;
    letter-spacing: 0.08em;
    font-size: 10px;
    font-family: var(--mono);
    transition: color 0.15s;
  }

  .sessions-header:hover .sessions-title {
    color: var(--text);
  }

  .sessions-add {
    background: none;
    border: 1px solid var(--border);
    border-radius: 3px;
    color: var(--text-dim);
    width: 18px;
    height: 18px;
    display: flex;
    align-items: center;
    justify-content: center;
    cursor: pointer;
    font-size: 12px;
    line-height: 1;
    transition: all 0.15s;
  }

  .sessions-add:hover {
    color: var(--accent);
    border-color: var(--accent);
    background: rgba(var(--accent-rgb), calc(0.08 * var(--glow-k)));
    transform: scale(1.1) rotate(90deg);
  }

  .sessions-list {
    overflow-y: auto;
    flex: 1;
    transition: background 0.15s;
  }

  .sessions-list:hover {
    background: rgba(255, 255, 255, 0.01);
  }

  .session-item {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 6px 12px;
    cursor: pointer;
    transition: all 0.15s;
    animation: session-in 0.2s ease-out;
  }

  @keyframes session-in {
    from {
      opacity: 0;
      transform: translateX(-4px);
    }
    to {
      opacity: 1;
      transform: translateX(0);
    }
  }

  .session-item:nth-child(odd):hover {
    background: rgba(var(--accent-rgb), calc(0.02 * var(--glow-k)));
  }

  .session-item:nth-child(even):hover {
    background: rgba(255, 255, 255, 0.02);
  }

  .session-item:nth-child(odd):hover {
    background: rgba(var(--accent-rgb), calc(0.06 * var(--glow-k)));
    box-shadow: 0 0 20px rgba(var(--accent-rgb), calc(0.1 * var(--glow-k)));
    transform: translateX(4px);
  }

  .session-item:nth-child(even):hover {
    background: rgba(255, 255, 255, 0.06);
    box-shadow: 0 0 16px rgba(255, 255, 255, 0.08);
    transform: translateX(3px);
  }

  .session-item:hover {
    background: var(--bg-hover);
    box-shadow: 0 0 12px rgba(255, 255, 255, 0.02);
  }

  .session-item.active {
    background: rgba(var(--accent-rgb), calc(0.08 * var(--glow-k)));
    border-left: 3px solid var(--accent);
    box-shadow: inset 0 0 12px rgba(var(--accent-rgb), calc(0.05 * var(--glow-k))), 0 0 16px rgba(var(--accent-rgb), calc(0.1 * var(--glow-k)));
  }

  .session-item.active:nth-child(odd):hover {
    box-shadow: inset 0 0 24px rgba(var(--accent-rgb), calc(0.15 * var(--glow-k))), 0 0 32px rgba(var(--accent-rgb), calc(0.2 * var(--glow-k)));
    transform: translateX(6px);
  }

  .session-item.active:nth-child(even):hover {
    box-shadow: inset 0 0 20px rgba(var(--accent-rgb), calc(0.12 * var(--glow-k))), 0 0 24px rgba(var(--accent-rgb), calc(0.18 * var(--glow-k)));
    transform: translateX(5px);
  }

  .session-item.active:hover {
    box-shadow: inset 0 0 16px rgba(var(--accent-rgb), calc(0.08 * var(--glow-k))), 0 0 20px rgba(var(--accent-rgb), calc(0.15 * var(--glow-k)));
  }

  .session-item.active .session-name:nth-child(odd):hover {
    color: var(--accent-hover);
    text-shadow: 0 0 20px var(--accent-glow);
    transform: scale(1.03);
  }

  .session-item.active .session-name:nth-child(even):hover {
    color: var(--accent);
    text-shadow: 0 0 16px var(--accent-glow);
    transform: scale(1.02);
  }

  .session-item.active .session-name {
    color: var(--accent);
    font-weight: 500;
  }

  .session-item.active .session-dot:nth-child(odd):hover {
    box-shadow: 0 0 24px currentColor;
    transform: scale(1.8);
  }

  .session-item.active .session-dot:nth-child(even):hover {
    box-shadow: 0 0 20px currentColor;
    transform: scale(1.6);
  }

  .session-item.active .session-dot {
    box-shadow: 0 0 6px currentColor;
  }

  .session-item.active .session-info:nth-child(odd):hover {
    transform: translateX(8px);
    gap: 6px;
  }

  .session-item.active .session-info:nth-child(even):hover {
    transform: translateX(6px);
    gap: 4px;
  }

  .session-item.active .session-info {
    gap: 2px;
  }

  .session-item.active .session-meta:nth-child(odd):hover {
    transform: translateX(8px);
    color: var(--text);
    text-shadow: 0 0 8px rgba(255, 255, 255, 0.1);
  }

  .session-item.active .session-meta:nth-child(even):hover {
    transform: translateX(6px);
    color: var(--text-dim);
    text-shadow: 0 0 6px rgba(255, 255, 255, 0.08);
  }

  .session-item.active .session-meta {
    color: var(--text-dim);
  }

  .session-dot {
    width: 6px;
    height: 6px;
    border-radius: 50%;
    flex-shrink: 0;
    background: var(--text-faint);
    opacity: 0.6;
    transition: all 0.15s;
  }

  .session-item:hover .session-dot {
    opacity: 1;
    box-shadow: 0 0 10px currentColor;
  }

  /* Честный индикатор состояния: мозг (работа), мигающий треугольник
     (ошибка), мигающая галочка (готово). Никаких «всегда зелёных точек». */
  .session-state {
    flex: none;
    display: inline-flex;
    align-items: center;
    justify-content: center;
    width: 15px;
    height: 15px;
  }

  .state-run {
    color: var(--accent);
    filter: drop-shadow(0 0 5px rgba(var(--accent-rgb), calc(0.55 * var(--glow-k))));
  }

  .state-err {
    color: var(--err);
    animation: state-blink 0.9s steps(2, jump-none) infinite;
  }

  .state-ok {
    color: var(--ok);
    animation: state-blink-soft 1.8s ease-in-out infinite;
  }

  @keyframes state-blink {
    0%,
    100% {
      opacity: 1;
      filter: drop-shadow(0 0 6px rgba(var(--err-rgb), 0.85));
    }
    50% {
      opacity: 0.25;
      filter: none;
    }
  }

  @keyframes state-blink-soft {
    0%,
    100% {
      opacity: 1;
      filter: drop-shadow(0 0 5px rgba(63, 185, 80, 0.7));
    }
    50% {
      opacity: 0.45;
      filter: none;
    }
  }

  .session-info:nth-child(odd):hover {
    transform: translateX(5px);
    gap: 4px;
  }

  .session-info:nth-child(even):hover {
    transform: translateX(4px);
    gap: 3px;
  }

  .session-info {
    display: flex;
    flex-direction: column;
    gap: 1px;
    min-width: 0;
    transition: all 0.15s;
  }

  .session-item:hover .session-info {
    gap: 2px;
    transform: translateX(2px);
  }

  .session-item:hover .session-info {
    transform: translateX(2px);
  }

  .session-name {
    font-family: var(--mono);
    font-size: 11px;
    color: var(--text);
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
    transition: color 0.15s;
  }

  /* Короткий id сессии: две сессии с одинаковым cwd визуально различимы,
     утечку чужого чата в свою сессию видно сразу. */
  .session-id {
    margin-left: 6px;
    color: var(--text-faint);
    font-size: 9px;
    letter-spacing: 0.06em;
  }

  .session-name:nth-child(odd):hover {
    color: var(--accent-hover);
    text-shadow: 0 0 16px var(--accent-glow);
    transform: scale(1.02);
  }

  .session-name:nth-child(even):hover {
    color: var(--accent);
    text-shadow: 0 0 12px var(--accent-glow);
    transform: scale(1.01);
  }

  .session-item:hover .session-name {
    color: var(--accent);
    text-shadow: 0 0 8px var(--accent-glow);
  }

  .session-meta:nth-child(odd):hover {
    transform: translateX(5px);
    color: var(--text-dim);
    text-shadow: 0 0 8px rgba(255, 255, 255, 0.1);
  }

  .session-meta:nth-child(even):hover {
    transform: translateX(4px);
    color: var(--text-faint);
    text-shadow: 0 0 6px rgba(255, 255, 255, 0.08);
  }

  .session-meta {
    font-size: 9px;
    color: var(--text-faint);
    font-family: var(--mono);
    transition: all 0.15s;
  }

  .session-item:hover .session-meta {
    color: var(--text-dim);
    transform: translateX(2px);
  }

  .session-item:hover .session-meta {
    color: var(--text-dim);
  }

  .sessions-empty {
    padding: 12px;
    color: var(--text-faint);
    font-size: 11px;
    text-align: center;
    font-family: var(--mono);
    transition: color 0.15s;
  }

  .sessions-empty:nth-child(odd):hover {
    color: var(--text);
    text-shadow: 0 0 8px rgba(255, 255, 255, 0.1);
    transform: scale(1.02);
  }

  .sessions-empty:nth-child(even):hover {
    color: var(--text-dim);
    text-shadow: 0 0 6px rgba(255, 255, 255, 0.08);
    transform: scale(1.01);
  }

  .sessions-empty:hover {
    color: var(--text-dim);
  }

  .session-context-menu {
    position: fixed;
    z-index: 1000;
    background: var(--bg-elevated);
    border: 1px solid var(--border);
    border-radius: 6px;
    padding: 4px;
    box-shadow: 0 8px 24px rgba(0, 0, 0, 0.4);
    min-width: 150px;
    animation: context-menu-in 0.15s ease-out;
    transition: all 0.15s;
  }

  .session-context-menu:nth-child(odd):hover {
    box-shadow: 0 8px 40px rgba(var(--accent-rgb), calc(0.25 * var(--glow-k)));
    border-color: var(--accent);
    transform: scale(1.03);
  }

  .session-context-menu:nth-child(even):hover {
    box-shadow: 0 8px 36px rgba(var(--accent-rgb), calc(0.2 * var(--glow-k)));
    border-color: var(--accent-dim);
    transform: scale(1.02);
  }

  .session-context-menu:hover {
    box-shadow: 0 8px 32px rgba(var(--accent-rgb), calc(0.1 * var(--glow-k)));
    border-color: var(--accent-dim);
  }

  @keyframes context-menu-in {
    from {
      opacity: 0;
      transform: scale(0.95);
    }
    to {
      opacity: 1;
      transform: scale(1);
    }
  }

  .session-context-menu button {
    width: 100%;
    padding: 8px 12px;
    background: none;
    border: none;
    border-radius: 4px;
    color: var(--text);
    font-family: var(--mono);
    font-size: 12px;
    cursor: pointer;
    text-align: left;
    transition: all 0.15s;
  }

  .session-context-menu button:nth-child(odd):hover {
    background: rgba(var(--accent-rgb), calc(0.25 * var(--glow-k)));
    color: var(--accent-hover);
    transform: translateX(8px) scale(1.05);
    box-shadow: 0 0 24px rgba(var(--accent-rgb), calc(0.25 * var(--glow-k)));
  }

  .session-context-menu button:nth-child(even):hover {
    background: rgba(255, 255, 255, 0.25);
    color: var(--accent);
    transform: translateX(6px) scale(1.03);
    box-shadow: 0 0 20px rgba(255, 255, 255, 0.2);
  }

  .session-context-menu button:hover {
    background: rgba(var(--accent-rgb), calc(0.1 * var(--glow-k)));
    color: var(--accent);
    transform: translateX(2px);
  }
</style>

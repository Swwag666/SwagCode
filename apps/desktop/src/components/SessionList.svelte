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
    last_activity_ms: number
  }

  interface Props {
    activeId: string | null
    onSelect: (id: string) => void
    /** Сигнал обновления из App: растёт только на ходовых событиях шины.
        Таймера больше нет — список не дёргается и не жрёт инвоки впустую. */
    refreshTick?: number
    /** Живой статус из событий шины: мозг/галочка/треугольник без опроса. */
    liveStates?: Record<string, 'running' | 'ok' | 'fail'>
    /** Группировка как в DeepSeek: по рабочим папкам или единым списком. */
    groupMode?: 'workspace' | 'list'
    /** Порядок: ручной (порядок создания) или по последней активности. */
    orderMode?: 'manual' | 'updated'
    /** Строка поиска из шапки Workspaces. */
    filter?: string
    /** Малый плюс на папке: новая сессия именно в этой директории. */
    onCreateIn?: (cwd: string) => void
  }

  let {
    activeId,
    onSelect,
    refreshTick = 0,
    liveStates = {},
    groupMode = 'workspace',
    orderMode = 'updated',
    filter = '',
    onCreateIn,
  }: Props = $props()

  let sessions = $state<SessionInfo[]>([])
  let contextMenu = $state<{ x: number; y: number; session: SessionInfo } | null>(null)

  function onContextMenu(e: MouseEvent, s: SessionInfo): void {
    e.preventDefault()
    contextMenu = { x: e.clientX, y: e.clientY, session: s }
  }

  function closeContextMenu(): void {
    contextMenu = null
  }

  async function deleteSession(): Promise<void> {
    const target = contextMenu?.session.id
    closeContextMenu()
    if (!target) return
    try {
      await invoke('delete_session', { sessionId: target })
    } catch (e) {
      console.error('delete_session', e)
    }
    await refresh()
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

  /* Группировка по рабочим папкам, как в референсе: папка = категория,
     внутри — сессии с возрастом последней активности. */
  const GROUP_LIMIT = 5
  let collapsedGroups = $state<Set<string>>(new Set())
  let shownAllGroups = $state<Set<string>>(new Set())

  function normKey(cwd: string): string {
    return cwd.replace(/[\\/]+$/, '').toLowerCase()
  }

  function groupLabel(cwd: string): string {
    const clean = cwd.replace(/[\\/]+$/, '')
    const parts = clean.split(/[\\/]/).filter(Boolean)
    return parts.length > 0 ? parts[parts.length - 1] : clean
  }

  /** Возраст как в референсе: 14min / 21h / 7d, свежее минуты — «now». */
  function formatAge(ms: number): string {
    const min = Math.floor((Date.now() - ms) / 60000)
    if (min < 1) return 'now'
    if (min < 60) return `${min}min`
    const h = Math.floor(min / 60)
    if (h < 24) return `${h}h`
    return `${Math.floor(h / 24)}d`
  }

  let filtered = $derived.by(() => {
    const q = filter.trim().toLowerCase()
    const base = q
      ? sessions.filter((s) => `${s.title} ${s.cwd}`.toLowerCase().includes(q))
      : sessions.slice()
    if (orderMode === 'updated') base.sort((a, b) => b.last_activity_ms - a.last_activity_ms)
    return base
  })

  interface Group {
    key: string
    label: string
    cwd: string
    sessions: SessionInfo[]
    activity: number
  }

  let groups = $derived.by<Group[]>(() => {
    const map = new Map<string, Group>()
    for (const s of filtered) {
      const key = normKey(s.cwd)
      let g = map.get(key)
      if (!g) {
        g = { key, label: groupLabel(s.cwd), cwd: s.cwd, sessions: [], activity: 0 }
        map.set(key, g)
      }
      g.sessions.push(s)
      if (s.last_activity_ms > g.activity) g.activity = s.last_activity_ms
    }
    const list = [...map.values()]
    if (orderMode === 'updated') list.sort((a, b) => b.activity - a.activity)
    return list
  })

  function toggleGroup(key: string): void {
    const next = new Set(collapsedGroups)
    if (next.has(key)) next.delete(key)
    else next.add(key)
    collapsedGroups = next
  }

  function showAllInGroup(key: string): void {
    const next = new Set(shownAllGroups)
    next.add(key)
    shownAllGroups = next
  }

  // Обновляем при монтировании и по сигналу ходовых событий из App.
  $effect(() => {
    void refreshTick
    refresh()
  })
</script>

<!-- Строка сессии общая для группированного и плоского режимов. -->
{#snippet sessionRow(s: SessionInfo)}
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
      </span>
      <span class="session-meta">{s.turns} ходов · {s.model.split('-').pop()}</span>
    </div>
    <span class="session-age" title={new Date(s.last_activity_ms).toLocaleString()}>
      {formatAge(s.last_activity_ms)}
    </span>
  </div>
{/snippet}

<div class="sessions">
  <div class="sessions-list" role="listbox" aria-label="сессии">
    {#if groupMode === 'workspace'}
      {#each groups as g (g.key)}
        <div class="ws-group">
          <div
            class="ws-group-head"
            role="button"
            tabindex="0"
            onclick={() => toggleGroup(g.key)}
            onkeydown={(e) => { if (e.key === 'Enter') toggleGroup(g.key) }}
            title={g.cwd}
          >
            <Icon name={collapsedGroups.has(g.key) ? 'folder' : 'folder-open'} size={13} />
            <span class="ws-group-name">{g.label}</span>
            <button
              class="ws-group-add"
              title="новая сессия в {g.label}"
              aria-label="новая сессия в {g.label}"
              onclick={(e) => {
                e.stopPropagation()
                onCreateIn?.(g.cwd)
              }}>
              <Icon name="plus" size={11} />
            </button>
          </div>
          {#if !collapsedGroups.has(g.key)}
            {#each shownAllGroups.has(g.key) ? g.sessions : g.sessions.slice(0, GROUP_LIMIT) as s (s.id)}
              {@render sessionRow(s)}
            {/each}
            {#if g.sessions.length > GROUP_LIMIT && !shownAllGroups.has(g.key)}
              <button class="ws-more" onclick={() => showAllInGroup(g.key)}>
                показать ещё {g.sessions.length - GROUP_LIMIT} сессий
              </button>
            {/if}
          {/if}
        </div>
      {/each}
    {:else}
      {#each filtered as s (s.id)}
        {@render sessionRow(s)}
      {/each}
    {/if}
    {#if filtered.length === 0}
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
  }

  /* Группа-папка как в референсе: заголовок с иконкой и своим плюсом,
     внутри — сессии категории. */
  .ws-group-head {
    display: flex;
    align-items: center;
    gap: 7px;
    padding: 7px 10px 4px;
    cursor: pointer;
    color: var(--text-dim);
    transition: color 0.15s;
  }

  .ws-group-head:hover {
    color: var(--text);
  }

  .ws-group-name {
    flex: 1;
    min-width: 0;
    font-size: 11px;
    font-family: var(--mono);
    letter-spacing: 0.04em;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .ws-group-add {
    background: none;
    border: none;
    color: var(--text-faint);
    cursor: pointer;
    padding: 2px;
    border-radius: 4px;
    opacity: 0;
    display: flex;
    align-items: center;
    transition: all 0.15s;
  }

  .ws-group-head:hover .ws-group-add {
    opacity: 1;
  }

  .ws-group-add:hover {
    color: var(--accent);
    background: rgba(var(--accent-rgb), calc(0.1 * var(--glow-k)));
    box-shadow: 0 0 10px rgba(var(--accent-rgb), calc(0.2 * var(--glow-k)));
  }

  .ws-more {
    background: none;
    border: none;
    color: var(--text-faint);
    font-size: 10px;
    font-family: var(--mono);
    padding: 4px 12px 6px 30px;
    cursor: pointer;
    text-align: left;
    transition: color 0.15s;
  }

  .ws-more:hover {
    color: var(--accent);
  }

  .session-age {
    margin-left: auto;
    color: var(--text-faint);
    font-size: 9px;
    font-family: var(--mono);
    flex-shrink: 0;
  }

  .session-item:hover .session-age {
    color: var(--text-dim);
  }

  .sessions-list {
    overflow-y: auto;
    /* Горизонтального ползунка в списке сессий не бывает вообще: длинные
        пути уходят в многоточие, а не растягивают строку. */
    overflow-x: hidden;
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
    animation: run-pulse 1.4s ease-in-out infinite;
  }

  /* Мозг за работой: импульсы бегут по контурам — штрихи стекают по путям
     SVG, как нейроны по извилинам. Просили в первом чате. */
  .state-run :global(path),
  .state-run :global(circle) {
    stroke-dasharray: 5 3;
    animation: neuron-flow 1.1s linear infinite;
  }

  @keyframes neuron-flow {
    to {
      stroke-dashoffset: -16;
    }
  }

  @keyframes run-pulse {
    0%,
    100% {
      transform: scale(1);
    }
    50% {
      transform: scale(1.12);
    }
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
    flex: 1;
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

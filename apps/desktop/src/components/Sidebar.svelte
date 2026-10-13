<script lang="ts">
  /**
   * F-10: левая панель (бренд, новая сессия, список сессий с фильтром и
   * группировкой, рабочие области, статусная строка, кнопка настроек) уехала
   * из App.svelte в компонент.
   *
   * Компонент презентационный: состояние живёт в App и приходит пропсами,
   * действия уходят колбэками, а шесть флагов, которые панель переключает
   * сама (сворачивание, фильтр, поповер группировки, области, настройки),
   * объявлены $bindable — тот же приём, что в Palette и SettingsTabs.
   * Ни invoke, ни шины здесь нет, поэтому разметка переехала дословно.
   *
   * `logoGif` компонент импортирует сам (как Splash.svelte): это ассет, а не
   * состояние приложения.
   */
  import Icon from './Icon.svelte'
  import SessionList from './SessionList.svelte'
  import logoGif from '../assets/logo.gif'
  import { type StrKey } from '../lib/strings'
  import { type TranscriptItem } from '../lib/transcript'

  interface Props {
    /* ── шапка: бренд, сворачивание, новая сессия ── */
    t: (key: StrKey) => string
    sidebarCollapsed: boolean
    creatingSession: boolean
    createNewSession: (cwdOverride?: string) => Promise<void>

    /* ── список сессий: фильтр, группировка, порядок ── */
    sessionFilterOpen: boolean
    sessionFilter: string
    groupPop: boolean
    groupMode: 'workspace' | 'list'
    orderMode: 'manual' | 'updated'
    setGroupMode: (m: 'workspace' | 'list') => void
    setOrderMode: (m: 'manual' | 'updated') => void
    currentSession: string | null
    sessionRefreshTick: number
    liveStates: Record<string, 'running' | 'ok' | 'fail'>
    switchSession: (id: string) => void

    /* ── рабочие области ── */
    showWorkspaces: boolean
    workspaces: { cwd: string; id: string }[]
    loadWorkspaces: () => Promise<void>
    openWorkspace: (path: string, sessionId: string) => void
    copyWorkspace: (path: string) => Promise<void>
    browseFolder: () => Promise<void>
    browseError: string
    openActiveFolder: () => Promise<void>

    /* ── нижняя статусная строка и вход в настройки ── */
    thinking: boolean
    items: TranscriptItem[]
    turnsCount: number
    eventsTotal: number
    connected: boolean
    tauriAvailable: boolean
    logNote: string
    showSettings: boolean
  }

  /* Флаги, которые панель меняет сама, — $bindable: App видит новое значение
     сразу (сворачивание сайдбара нужно и SettingsTabs, и шапке контента).
     Остальное приходит только сверху. */
  let {
    /* шапка */
    t,
    sidebarCollapsed = $bindable(),
    creatingSession,
    createNewSession,

    /* список сессий */
    sessionFilterOpen = $bindable(),
    sessionFilter = $bindable(),
    groupPop = $bindable(),
    groupMode,
    orderMode,
    setGroupMode,
    setOrderMode,
    currentSession,
    sessionRefreshTick,
    liveStates,
    switchSession,

    /* рабочие области */
    showWorkspaces = $bindable(),
    workspaces,
    loadWorkspaces,
    openWorkspace,
    copyWorkspace,
    browseFolder,
    browseError,
    openActiveFolder,

    /* статус и настройки */
    thinking,
    items,
    turnsCount,
    eventsTotal,
    connected,
    tauriAvailable,
    logNote,
    showSettings = $bindable(),
  }: Props = $props()
</script>

<aside class="sidebar" aria-hidden={sidebarCollapsed}>
  <div class="sidebar-header" data-tauri-drag-region>
    <div class="brand" data-tauri-drag-region>
      <div class="eye" aria-hidden="true">
        <!-- Чёрный фон кадра вырезаем luminance-маской, а не blend-mode:
             blend ломается от filter/zoom на предках, маска — нет.
             Альфа после маски повторяет форму глаза, поэтому drop-shadow
             светит по контуру зрачка, а не прямоугольником. -->
        <span
          class="eye-img"
          style="-webkit-mask-image: url('{logoGif}'); mask-image: url('{logoGif}'); -webkit-mask-mode: luminance; mask-mode: luminance; -webkit-mask-size: contain; mask-size: contain; -webkit-mask-repeat: no-repeat; mask-repeat: no-repeat;"
        ></span>
      </div>
      <span class="name">SwagCod</span>
      <button
        class="brand-collapse"
        onclick={() => (sidebarCollapsed = true)}
        title={t('collapseSidebar')}
        aria-label={t('collapseSidebar')}
      ><Icon name="panel" size={14} /></button>
    </div>
    <button class="new-session-btn" onclick={() => void createNewSession()} disabled={creatingSession} title={t('newSession')}>
      {#if creatingSession}
        <span class="spinner"></span>
      {:else}
        <Icon name="plus" size={14} />
      {/if}
      {t('newSession')}
    </button>
  </div>

  <div class="sidebar-section">
    <div class="section-header">
      <span>{t('workspaces')}</span>
      <div class="section-actions">
        <button
          title={t('filterSessions')}
          aria-label={t('filterSessions')}
          aria-pressed={sessionFilterOpen}
          onclick={() => (sessionFilterOpen = !sessionFilterOpen)}
        ><Icon name="search" size={13} /></button>
        <button
          title={t('groupOrder')}
          aria-label={t('groupOrder')}
          aria-expanded={groupPop}
          onclick={() => (groupPop = !groupPop)}
        ><Icon name="sliders" size={13} /></button>
        <button
          title={t('wsTitle')}
          aria-label={t('wsTitle')}
          aria-expanded={showWorkspaces}
          onclick={() => {
            showWorkspaces = !showWorkspaces
            if (showWorkspaces) void loadWorkspaces()
          }}
        ><Icon name="folder" size={13} /></button>
        <button
          title={t('openFolder')}
          aria-label={t('openFolder')}
          onclick={() => void openActiveFolder()}
        ><Icon name="external" size={13} /></button>
      </div>
    </div>
    {#if groupPop}
      <!-- Наполнение второго значка из референса: группировка и порядок. -->
      <div class="group-pop" role="dialog" aria-label={t('groupOrder')}>
        <div class="gp-cap">{t('groupBy')}</div>
        <button
          class="gp-row"
          class:on={groupMode === 'workspace'}
          onclick={() => setGroupMode('workspace')}
        >
          <span>{t('groupWs')}</span>
          {#if groupMode === 'workspace'}<Icon name="check" size={12} />{/if}
        </button>
        <button class="gp-row" class:on={groupMode === 'list'} onclick={() => setGroupMode('list')}>
          <span>{t('groupList')}</span>
          {#if groupMode === 'list'}<Icon name="check" size={12} />{/if}
        </button>
        <div class="gp-cap">{t('orderBy')}</div>
        <button class="gp-row" class:on={orderMode === 'manual'} onclick={() => setOrderMode('manual')}>
          <span>{t('orderManual')}</span>
          {#if orderMode === 'manual'}<Icon name="check" size={12} />{/if}
        </button>
        <button class="gp-row" class:on={orderMode === 'updated'} onclick={() => setOrderMode('updated')}>
          <span>{t('orderUpdated')}</span>
          {#if orderMode === 'updated'}<Icon name="check" size={12} />{/if}
        </button>
      </div>
    {/if}
    {#if sessionFilterOpen}
      <div class="session-filter">
        <input
          type="text"
          class="search-input"
          placeholder={t('filterPlaceholder')}
          bind:value={sessionFilter}
          onkeydown={(e) => {
            if (e.key === 'Escape') {
              sessionFilterOpen = false
              sessionFilter = ''
            }
          }}
        />
      </div>
    {/if}
    {#if showWorkspaces}
      <div class="ws-popover" role="dialog" aria-label={t('wsTitle')}>
        <div class="ws-title">{t('wsTitle')}</div>
        {#if workspaces.length === 0}
          <div class="ws-empty">{t('wsEmpty')}</div>
        {:else}
          {#each workspaces as w (w.id)}
            <div class="ws-row">
              <Icon name="folder" size={12} />
              <span class="ws-path" title={w.cwd}>{w.cwd}</span>
              <button
                class="ws-act"
                title={t('wsOpen')}
                aria-label={t('wsOpen')}
                onclick={() => openWorkspace(w.cwd, w.id)}
              ><Icon name="expand" size={11} /></button>
              <button
                class="ws-act"
                title={t('wsCopy')}
                aria-label={t('wsCopy')}
                onclick={() => copyWorkspace(w.cwd)}
              ><Icon name="clipboard" size={11} /></button>
            </div>
          {/each}
        {/if}
        <div class="ws-browse-row">
          <button class="ws-browse" onclick={() => void browseFolder()} title={t('browse')}>
            <Icon name="folder" size={12} /> {t('browse')}
          </button>
          {#if browseError}
            <span class="ws-err">{browseError}</span>
          {/if}
        </div>
      </div>
    {/if}
    <SessionList
      activeId={currentSession}
      onSelect={switchSession}
      refreshTick={sessionRefreshTick}
      liveStates={liveStates}
      groupMode={groupMode}
      orderMode={orderMode}
      filter={sessionFilter}
      onCreateIn={(cwd) => void createNewSession(cwd)}
    />
  </div>

  <div class="sidebar-footer">
    {#if thinking}
      <div class="waveform" aria-hidden="true">
        {#each Array(20) as _, i (i)}
          <span class="wave-bar" style="animation-delay: {i * 0.05}s"></span>
        {/each}
      </div>
    {/if}
    <div class="sidebar-stats">
      <div class="stat">
        <span class="stat-value">{eventsTotal}</span>
        <span class="stat-label">events</span>
      </div>
      <div class="stat">
        <span class="stat-value">{turnsCount}</span>
        <span class="stat-label">turns</span>
      </div>
      <div class="stat">
        <span class="stat-value">{items.length}</span>
        <span class="stat-label">items</span>
      </div>
    </div>
    <div class="status-line">
      <span class:ok={connected} class:warn={!connected}>
        {connected ? '●' : '○'} {connected ? t('connected') : tauriAvailable ? t('connecting') : t('browser')}
      </span>
      {#if logNote}
        <span class="log-note" title={logNote}>{logNote}</span>
      {/if}
    </div>
    <button class="settings-btn" onclick={() => (showSettings = !showSettings)}>
      <Icon name="gear" size={13} /> {t('settings')}
    </button>
  </div>
</aside>

<style>
.sidebar {
    display: flex;
    flex-direction: column;
    background: var(--surface-sidebar);
    border-right: 1px solid var(--border);
    min-height: 0;
    animation: sidebar-in 0.3s ease-out;
    transition: all 0.15s;
  }

.sidebar:nth-child(odd):hover {
    background: rgba(var(--accent-rgb), calc(0.008 * var(--glow-k)));
    box-shadow: inset 0 0 20px rgba(var(--accent-rgb), calc(0.03 * var(--glow-k)));
  }

.sidebar:nth-child(even):hover {
    background: rgba(255, 255, 255, 0.008);
    box-shadow: inset 0 0 16px rgba(255, 255, 255, 0.02);
  }

.sidebar:hover {
    background: rgba(var(--accent-rgb), calc(0.005 * var(--glow-k)));
    box-shadow: inset 0 0 16px rgba(var(--accent-rgb), calc(0.02 * var(--glow-k)));
  }

@keyframes sidebar-in {
    from {
      opacity: 0;
      transform: translateX(-10px);
    }
    to {
      opacity: 1;
      transform: translateX(0);
    }
  }

.sidebar-header {
    padding: 16px;
    border-bottom: 1px solid var(--border);
    transition: background 0.15s;
  }

.sidebar-header:nth-child(odd):hover {
    background: rgba(var(--accent-rgb), calc(0.04 * var(--glow-k)));
    box-shadow: 0 0 24px rgba(var(--accent-rgb), calc(0.08 * var(--glow-k)));
    border-bottom-color: var(--accent-dim);
    transform: translateY(-2px);
  }

.sidebar-header:nth-child(even):hover {
    background: rgba(var(--accent-rgb), calc(0.03 * var(--glow-k)));
    box-shadow: 0 0 20px rgba(var(--accent-rgb), calc(0.06 * var(--glow-k)));
    border-bottom-color: var(--accent-dim);
    transform: translateY(-1px);
  }

.sidebar-header:hover {
    background: rgba(var(--accent-rgb), calc(0.01 * var(--glow-k)));
    box-shadow: 0 0 16px rgba(var(--accent-rgb), calc(0.03 * var(--glow-k)));
  }

.brand {
    display: flex;
    align-items: center;
    gap: 8px;
    margin-bottom: 12px;
    transition: all 0.15s;
  }

.brand:nth-child(odd):hover {
    gap: 16px;
    background: rgba(var(--accent-rgb), calc(0.02 * var(--glow-k)));
    box-shadow: 0 0 16px rgba(var(--accent-rgb), calc(0.04 * var(--glow-k)));
    transform: translateY(-2px);
  }

.brand:nth-child(even):hover {
    gap: 14px;
    background: rgba(255, 255, 255, 0.02);
    box-shadow: 0 0 12px rgba(255, 255, 255, 0.03);
    transform: translateY(-1px);
  }

.brand:hover {
    gap: 10px;
  }

.eye {
    color: var(--accent);
    display: flex;
    transition: all 0.15s;
  }

/* Глаз логотипа: сплошной акцентный прямоугольник, из которого luminance-
     маска гифки вырезает форму зрачка (чёрный фон кадра исчезает). Маска,
     а не blend-mode: blend ломается от filter/zoom на предках. */
  .eye-img {
    display: block;
    width: 26px;
    height: 21px;
    background: var(--accent);
    filter: drop-shadow(0 0 6px var(--accent-glow));
  }

.brand:hover .eye {
    transform: scale(1.1);
    filter: drop-shadow(0 0 8px var(--accent-glow));
  }

.name {
    font-family: var(--mono);
    font-size: 14px;
    font-weight: 600;
    color: var(--text);
    transition: all 0.15s;
  }

.name:nth-child(odd):hover {
    color: var(--accent-hover);
    text-shadow: 0 0 16px var(--accent-glow);
    transform: scale(1.02);
  }

.name:nth-child(even):hover {
    color: var(--accent);
    text-shadow: 0 0 12px var(--accent-glow);
    transform: scale(1.01);
  }

.brand:hover .name {
    color: var(--accent);
    text-shadow: 0 0 8px var(--accent-glow);
  }

.new-session-btn {
    width: 100%;
    padding: 10px 14px;
    background: rgba(var(--accent-rgb), calc(0.08 * var(--glow-k)));
    border: 1px solid var(--accent-dim);
    border-radius: 8px;
    color: var(--text);
    font-family: var(--mono);
    font-size: 12px;
    cursor: pointer;
    display: flex;
    align-items: center;
    gap: 8px;
    transition: all 0.2s;
  }

.new-session-btn:nth-child(odd):hover {
    background: rgba(var(--accent-rgb), calc(0.25 * var(--glow-k)));
    box-shadow: 0 0 32px rgba(var(--accent-rgb), calc(0.45 * var(--glow-k)));
    transform: translateY(-4px) scale(1.02);
  }

.new-session-btn:nth-child(even):hover {
    background: rgba(var(--accent-rgb), calc(0.2 * var(--glow-k)));
    box-shadow: 0 0 24px rgba(var(--accent-rgb), calc(0.4 * var(--glow-k)));
    transform: translateY(-3px) scale(1.01);
  }

.new-session-btn:hover {
    background: rgba(var(--accent-rgb), calc(0.15 * var(--glow-k)));
    border-color: var(--accent);
    box-shadow: 0 0 20px rgba(var(--accent-rgb), calc(0.3 * var(--glow-k)));
    transform: translateY(-2px);
  }

.new-session-btn:active {
    transform: translateY(0);
    box-shadow: 0 0 12px rgba(var(--accent-rgb), calc(0.2 * var(--glow-k)));
  }

.new-session-btn:disabled {
    opacity: 0.6;
    cursor: not-allowed;
  }

.spinner {
    width: 14px;
    height: 14px;
    border: 2px solid var(--border);
    border-top-color: var(--accent);
    border-radius: 50%;
    animation: spin 0.8s linear infinite;
    transition: all 0.15s;
  }

.spinner:nth-child(odd):hover {
    border-top-color: var(--accent-hover);
    box-shadow: 0 0 20px var(--accent-glow);
    transform: scale(1.15);
    animation-duration: 0.5s;
  }

.spinner:nth-child(even):hover {
    border-top-color: var(--accent);
    box-shadow: 0 0 16px var(--accent-glow);
    transform: scale(1.1);
    animation-duration: 0.6s;
  }

.new-session-btn:hover .spinner {
    border-top-color: var(--accent-hover);
    box-shadow: 0 0 8px var(--accent-glow);
  }

@keyframes spin {
    to { transform: rotate(360deg); }
  }

.sidebar-section {
    flex: 1;
    overflow-y: auto;
    padding: 12px;
    transition: background 0.15s;
  }

.sidebar-section:hover {
    background: rgba(var(--accent-rgb), calc(0.005 * var(--glow-k)));
    box-shadow: inset 0 0 16px rgba(var(--accent-rgb), calc(0.02 * var(--glow-k)));
  }

.sidebar-section:nth-child(odd):hover {
    background: rgba(var(--accent-rgb), calc(0.02 * var(--glow-k)));
    box-shadow: inset 0 0 24px rgba(var(--accent-rgb), calc(0.04 * var(--glow-k)));
    border-radius: 8px;
  }

.sidebar-section:nth-child(even):hover {
    background: rgba(255, 255, 255, 0.02);
    box-shadow: inset 0 0 20px rgba(255, 255, 255, 0.03);
    border-radius: 6px;
  }

.sidebar-section:first-child {
    padding-top: 16px;
    transition: all 0.15s;
  }

.sidebar-section:first-child:hover {
    background: rgba(var(--accent-rgb), calc(0.01 * var(--glow-k)));
    box-shadow: inset 0 0 20px rgba(var(--accent-rgb), calc(0.03 * var(--glow-k)));
  }

.sidebar-section:last-child {
    border-bottom: none;
  }

.sidebar-section:last-child:hover {
    background: rgba(var(--accent-rgb), calc(0.01 * var(--glow-k)));
    box-shadow: inset 0 0 20px rgba(var(--accent-rgb), calc(0.03 * var(--glow-k)));
  }

.section-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    margin-bottom: 8px;
    font-family: var(--mono);
    font-size: 11px;
    color: var(--text-faint);
    text-transform: uppercase;
    transition: color 0.15s;
  }

.section-header:hover {
    color: var(--accent);
    text-shadow: 0 0 8px var(--accent-glow);
  }

.section-actions {
    display: flex;
    gap: 4px;
    transition: all 0.15s;
  }

.section-actions:nth-child(odd):hover {
    gap: 12px;
    background: rgba(var(--accent-rgb), calc(0.02 * var(--glow-k)));
    box-shadow: 0 0 16px rgba(var(--accent-rgb), calc(0.04 * var(--glow-k)));
    border-radius: 6px;
  }

.section-actions:nth-child(even):hover {
    gap: 10px;
    background: rgba(255, 255, 255, 0.02);
    box-shadow: 0 0 12px rgba(255, 255, 255, 0.03);
    border-radius: 4px;
  }

.section-actions:hover {
    gap: 6px;
  }

.section-actions button {
    background: none;
    border: none;
    color: var(--text-faint);
    cursor: pointer;
    font-size: 12px;
    padding: 2px 4px;
    opacity: 0.6;
    transition: all 0.15s;
  }

.section-actions button:nth-child(odd):hover {
    opacity: 1;
    color: var(--accent-hover);
    transform: scale(1.3) translateY(-2px);
    filter: drop-shadow(0 0 12px var(--accent-glow));
  }

.section-actions button:nth-child(even):hover {
    opacity: 1;
    color: var(--accent);
    transform: scale(1.2) translateY(-1px);
    filter: drop-shadow(0 0 10px var(--accent-glow));
  }

.section-actions button:hover {
    opacity: 1;
    color: var(--accent);
    transform: scale(1.1);
    filter: drop-shadow(0 0 6px var(--accent-glow));
  }

.sidebar-footer {
    padding: 12px 16px;
    border-top: 1px solid var(--border);
    transition: background 0.15s;
  }

.sidebar-footer:nth-child(odd):hover {
    background: rgba(var(--accent-rgb), calc(0.04 * var(--glow-k)));
    box-shadow: 0 0 24px rgba(var(--accent-rgb), calc(0.08 * var(--glow-k)));
    border-top-color: var(--accent-dim);
    transform: translateY(-2px);
  }

.sidebar-footer:nth-child(even):hover {
    background: rgba(var(--accent-rgb), calc(0.03 * var(--glow-k)));
    box-shadow: 0 0 20px rgba(var(--accent-rgb), calc(0.06 * var(--glow-k)));
    border-top-color: var(--accent-dim);
    transform: translateY(-1px);
  }

.sidebar-footer:hover {
    background: rgba(var(--accent-rgb), calc(0.01 * var(--glow-k)));
    box-shadow: 0 0 16px rgba(var(--accent-rgb), calc(0.03 * var(--glow-k)));
  }

.waveform {
    display: flex;
    align-items: center;
    justify-content: center;
    gap: 2px;
    height: 24px;
    margin-bottom: 12px;
    animation: fade-in 0.3s ease-out;
    transition: all 0.15s;
  }

.waveform:nth-child(odd):hover {
    gap: 8px;
    transform: scale(1.15);
    filter: drop-shadow(0 0 16px var(--accent-glow));
  }

.waveform:nth-child(even):hover {
    gap: 6px;
    transform: scale(1.1);
    filter: drop-shadow(0 0 12px var(--accent-glow));
  }

.waveform:hover {
    gap: 4px;
    transform: scale(1.05);
  }

.wave-bar {
    width: 3px;
    height: 100%;
    background: var(--accent);
    border-radius: 2px;
    animation: wave 1s ease-in-out infinite;
    transform-origin: center;
    transition: all 0.15s;
  }

.wave-bar:nth-child(odd):hover {
    background: var(--accent-hover);
    box-shadow: 0 0 16px var(--accent-glow);
    transform: scaleY(1.3);
  }

.wave-bar:nth-child(even):hover {
    background: var(--accent);
    box-shadow: 0 0 12px var(--accent-glow);
    transform: scaleY(1.2);
  }

.waveform:hover .wave-bar {
    background: var(--accent-hover);
    box-shadow: 0 0 6px var(--accent-glow);
  }

@keyframes wave {
    0%, 100% { transform: scaleY(0.3); opacity: 0.4; }
    50% { transform: scaleY(1); opacity: 1; }
  }

.sidebar-stats {
    display: grid;
    grid-template-columns: repeat(3, 1fr);
    gap: 8px;
    margin-bottom: 12px;
    padding: 10px;
    background: var(--surface-inset);
    border-radius: 8px;
    animation: fade-in 0.3s ease-out 0.2s both;
    transition: all 0.15s;
  }

.sidebar-stats:nth-child(odd):hover {
    background: rgba(var(--accent-rgb), calc(0.08 * var(--glow-k)));
    box-shadow: 0 0 24px rgba(var(--accent-rgb), calc(0.12 * var(--glow-k)));
    transform: scale(1.06);
    border-color: var(--accent-dim);
  }

.sidebar-stats:nth-child(even):hover {
    background: rgba(var(--accent-rgb), calc(0.06 * var(--glow-k)));
    box-shadow: 0 0 20px rgba(var(--accent-rgb), calc(0.1 * var(--glow-k)));
    transform: scale(1.04);
    border-color: var(--accent-dim);
  }

.sidebar-stats:hover {
    background: rgba(var(--accent-rgb), calc(0.02 * var(--glow-k)));
    box-shadow: 0 0 16px rgba(var(--accent-rgb), calc(0.05 * var(--glow-k)));
    transform: scale(1.02);
  }

.stat {
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: 2px;
    transition: all 0.15s;
  }

.stat:nth-child(odd):hover {
    background: rgba(var(--accent-rgb), calc(0.08 * var(--glow-k)));
    box-shadow: 0 0 20px rgba(var(--accent-rgb), calc(0.12 * var(--glow-k)));
    transform: translateY(-3px) scale(1.08);
  }

.stat:nth-child(even):hover {
    background: rgba(255, 255, 255, 0.08);
    box-shadow: 0 0 16px rgba(255, 255, 255, 0.1);
    transform: translateY(-2px) scale(1.06);
  }

.stat:hover {
    transform: translateY(-2px) scale(1.05);
    box-shadow: 0 0 16px rgba(var(--accent-rgb), calc(0.05 * var(--glow-k)));
    border-radius: 4px;
    background: rgba(var(--accent-rgb), calc(0.02 * var(--glow-k)));
  }

.stat:hover .stat-value {
    color: var(--accent);
  }

.stat-value {
    font-family: var(--mono);
    font-size: 14px;
    font-weight: 600;
    color: var(--text);
    transition: all 0.15s;
  }

.stat:hover .stat-value {
    transform: scale(1.1);
    text-shadow: 0 0 8px currentColor;
  }

.stat-label {
    font-family: var(--mono);
    font-size: 9px;
    color: var(--text-faint);
    text-transform: uppercase;
    transition: color 0.15s;
  }

.stat:hover .stat-label {
    color: var(--text-dim);
  }

.status-line {
    font-family: var(--mono);
    font-size: 10px;
    color: var(--text-faint);
    margin-bottom: 8px;
    transition: all 0.15s;
  }

.status-line:nth-child(odd):hover {
    background: rgba(var(--accent-rgb), calc(0.08 * var(--glow-k)));
    box-shadow: 0 0 20px rgba(var(--accent-rgb), calc(0.12 * var(--glow-k)));
    transform: translateX(5px);
    border-left-color: var(--accent);
  }

.status-line:nth-child(even):hover {
    background: rgba(var(--accent-rgb), calc(0.06 * var(--glow-k)));
    box-shadow: 0 0 16px rgba(var(--accent-rgb), calc(0.1 * var(--glow-k)));
    transform: translateX(4px);
    border-left-color: var(--accent-dim);
  }

.status-line:hover {
    color: var(--accent);
    background: rgba(var(--accent-rgb), calc(0.02 * var(--glow-k)));
    box-shadow: 0 0 12px rgba(var(--accent-rgb), calc(0.03 * var(--glow-k)));
    transform: translateX(2px);
    padding: 2px 4px;
    border-radius: 4px;
  }

.status-line .ok {
    color: var(--ok);
    transition: all 0.15s;
  }

.status-line:hover .ok {
    text-shadow: 0 0 8px currentColor;
  }

.status-line .warn {
    color: var(--warn);
    transition: all 0.15s;
  }

.status-line:hover .warn {
    text-shadow: 0 0 8px currentColor;
  }

.settings-btn {
    width: 100%;
    padding: 8px 12px;
    background: none;
    border: none;
    color: var(--text-dim);
    font-family: var(--mono);
    font-size: 12px;
    cursor: pointer;
    text-align: left;
    border-radius: 6px;
    transition: all 0.15s;
  }

.settings-btn:nth-child(odd):hover {
    background: rgba(var(--accent-rgb), calc(0.15 * var(--glow-k)));
    box-shadow: 0 0 24px rgba(var(--accent-rgb), calc(0.2 * var(--glow-k)));
    transform: translateX(5px);
    border-color: var(--accent);
  }

.settings-btn:nth-child(even):hover {
    background: rgba(var(--accent-rgb), calc(0.12 * var(--glow-k)));
    box-shadow: 0 0 20px rgba(var(--accent-rgb), calc(0.18 * var(--glow-k)));
    transform: translateX(4px);
    border-color: var(--accent-dim);
  }

.settings-btn:hover {
    background: rgba(var(--accent-rgb), calc(0.08 * var(--glow-k)));
    color: var(--accent);
    transform: translateX(2px);
    box-shadow: 0 0 12px rgba(var(--accent-rgb), calc(0.1 * var(--glow-k)));
  }

/* Предок `.media-on` висит на <main> в App, поэтому он обёрнут в :global —
   иначе Svelte считает селектор неиспользуемым и вырезает правило целиком.
   Субъект `.sidebar` остаётся скоупнутым хешем этого компонента. */
:global(.media-on) .sidebar {
    background: color-mix(in srgb, var(--bg) var(--panel-glass), transparent);
    backdrop-filter: blur(14px) saturate(1.1);
  }

@keyframes fade-in {
    from { opacity: 0; }
    to { opacity: 1; }
  }

.search-input {
    flex: 1;
    padding: 6px 10px;
    background: var(--bg-elevated);
    border: 1px solid var(--border);
    border-radius: 6px;
    color: var(--text);
    font-family: var(--mono);
    font-size: 12px;
    outline: none;
    transition: all 0.15s;
  }

.search-input:nth-child(odd):hover {
    border-color: var(--accent);
    background: rgba(var(--accent-rgb), calc(0.05 * var(--glow-k)));
    box-shadow: 0 0 16px rgba(var(--accent-rgb), calc(0.15 * var(--glow-k)));
    transform: translateY(-2px);
  }

.search-input:nth-child(even):hover {
    border-color: var(--accent-dim);
    background: rgba(var(--accent-rgb), calc(0.04 * var(--glow-k)));
    box-shadow: 0 0 12px rgba(var(--accent-rgb), calc(0.12 * var(--glow-k)));
    transform: translateY(-1px);
  }

.search-input:hover {
    border-color: var(--accent-dim);
    box-shadow: 0 0 8px rgba(var(--accent-rgb), calc(0.08 * var(--glow-k)));
  }

.search-input:focus {
    border-color: var(--accent);
    box-shadow: 0 0 0 2px rgba(var(--accent-rgb), calc(0.1 * var(--glow-k)));
  }

/* ──────────────────────────────────────────────────────────────
   * Ресайс и сворачивание сайдбара
   * ────────────────────────────────────────────────────────────── */
  .sidebar {
    overflow: hidden;
  }

/* Тот же приём: `main.dsh-layout.collapsed` — предок из App. */
:global(main.dsh-layout.collapsed) .sidebar {
    opacity: 0;
    pointer-events: none;
  }

/* Поповер рабочих областей: пути cwd открытых сессий, как в референсе. */
  .sidebar-section {
    position: relative;
  }

.ws-popover {
    position: absolute;
    top: 30px;
    left: 8px;
    right: 8px;
    z-index: 60;
    padding: 8px;
    background: var(--bg-elevated);
    border: 1px solid var(--border);
    border-radius: 8px;
    box-shadow: 0 12px 32px rgba(0, 0, 0, 0.45);
    display: flex;
    flex-direction: column;
    gap: 6px;
    max-height: 240px;
    overflow-y: auto;
  }

.ws-title {
    font-family: var(--mono);
    font-size: 10px;
    text-transform: uppercase;
    letter-spacing: 0.08em;
    color: var(--text-faint);
  }

.ws-browse-row {
    display: flex;
    align-items: center;
    gap: 8px;
    margin-top: 8px;
  }

.ws-browse {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    padding: 5px 10px;
    border-radius: 7px;
    cursor: pointer;
    border: 1px solid var(--border);
    background: transparent;
    color: var(--text-dim);
    font-size: 11.5px;
    transition: background 0.13s ease, color 0.13s ease, transform 0.13s ease,
      border-color 0.13s ease;
  }

.ws-browse:hover {
    color: var(--text);
    border-color: rgba(var(--accent-rgb), calc(0.5 * var(--glow-k)));
    transform: translateY(-1px);
  }

.ws-browse:active {
    transform: translateY(0) scale(0.98);
  }

.ws-err {
    color: var(--err);
    font-size: 11px;
  }

.ws-empty {
    font-family: var(--mono);
    font-size: 11px;
    color: var(--text-faint);
    padding: 4px 0;
  }

.ws-row {
    display: flex;
    align-items: center;
    gap: 6px;
    color: var(--text-dim);
  }

.ws-path {
    flex: 1;
    min-width: 0;
    font-family: var(--mono);
    font-size: 10px;
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }

.ws-act {
    flex: none;
    display: inline-flex;
    background: none;
    border: none;
    padding: 2px;
    color: var(--text-faint);
    cursor: pointer;
    transition: color 0.15s;
  }

.ws-act:hover {
    color: var(--accent);
  }

/* ── Ревизия 12: сайдбар и шапка как в референсе ─────────────────── */

  .brand-collapse {
    margin-left: auto;
    background: none;
    border: none;
    color: var(--text-faint);
    cursor: pointer;
    padding: 3px;
    border-radius: 5px;
    display: flex;
    align-items: center;
    transition: all 0.15s;
  }

.brand-collapse:hover {
    color: var(--accent);
    background: rgba(var(--accent-rgb), calc(0.1 * var(--glow-k)));
  }

.session-filter {
    padding: 4px 10px 6px;
  }

.group-pop {
    margin: 2px 10px 8px;
    padding: 6px;
    background: var(--bg-panel);
    border: 1px solid var(--border);
    border-radius: 8px;
    box-shadow: 0 8px 24px rgba(0, 0, 0, 0.45);
    z-index: 30;
  }

.gp-cap {
    color: var(--text-faint);
    font-size: 9px;
    font-family: var(--mono);
    text-transform: uppercase;
    letter-spacing: 0.08em;
    padding: 4px 6px 2px;
  }

.gp-row {
    display: flex;
    align-items: center;
    justify-content: space-between;
    width: 100%;
    background: none;
    border: none;
    color: var(--text-dim);
    font-size: 11px;
    font-family: var(--mono);
    padding: 5px 6px;
    border-radius: 5px;
    cursor: pointer;
    transition: all 0.15s;
  }

.gp-row:hover {
    background: rgba(var(--accent-rgb), calc(0.08 * var(--glow-k)));
    color: var(--text);
  }

.gp-row.on {
    color: var(--accent);
  }

.log-note {
    margin-left: auto;
    color: var(--text-faint);
    font-size: 9px;
    font-family: var(--mono);
    max-width: 60%;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
</style>

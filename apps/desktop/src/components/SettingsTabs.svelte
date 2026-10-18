<script lang="ts">
  /**
   * F-10: модальное окно настроек — вкладки general/security/telemetry/
   * providers/memory (+ models, plugins, mcp).
   *
   * Компонент чисто презентационный: всё состояние живёт в App.svelte и
   * приходит пропсами, действия уходят колбэками, а флаги и поля, которые
   * окно правит само, объявлены $bindable (тот же приём, что в Palette и
   * MemoryTab). Ни invoke, ни шины здесь нет — поэтому разметка переехала
   * дословно, без изменения поведения.
   */
  import CommandRules from './CommandRules.svelte'
  import HooksEditor from './HooksEditor.svelte'
  import MemoryTab, { type MemoryItem } from './MemoryTab.svelte'
  import TasksTable from './TasksTable.svelte'
  import Icon, { type IconName } from './Icon.svelte'
  import { type StrKey } from '../lib/strings'
  import { polylinePoints, barHeights } from '../lib/charts'
  import { invoke } from '@tauri-apps/api/core'

  /* ── Уведомления: типы совпадают с бэкендом (serde snake_case). ── */
  type NotifyTrigger = 'turn_complete' | 'turn_error' | 'approval_required'
  type SoundSource = { type: 'builtin'; name: string } | { type: 'custom'; path: string }
  type NotifyAction =
    | { action: 'sound'; source: SoundSource; volume: number }
    | { action: 'toast'; title: string; body: string }
    | { action: 'both'; source: SoundSource; volume: number; title: string; body: string }
  interface NotificationConfig {
    triggers: Record<string, NotifyAction>
    master_volume: number
    enabled: boolean
    suppress_when_focused: boolean
  }

  const BUILTIN_SOUNDS = ['done', 'error', 'chime'] as const
  const TRIGGER_KEYS: NotifyTrigger[] = ['turn_complete', 'turn_error', 'approval_required']
  const TRIGGER_LABELS: Record<NotifyTrigger, string> = {
    turn_complete: 'Turn complete',
    turn_error: 'Turn error',
    approval_required: 'Approval required',
  }

  function defaultAction(): NotifyAction {
    return { action: 'off' as never, source: { type: 'builtin', name: 'done' }, volume: 80, title: '', body: '' }
  }

  let notifyCfg = $state<NotificationConfig>({
    triggers: {},
    master_volume: 80,
    enabled: true,
    suppress_when_focused: false,
  })
  let notifyLoaded = $state(false)
  let notifyStatus = $state<string | null>(null)
  let saveTimer: ReturnType<typeof setTimeout> | null = null

  async function loadNotifyConfig() {
    try {
      notifyCfg = await invoke<NotificationConfig>('get_notification_config')
      notifyLoaded = true
    } catch (e) {
      notifyStatus = `Load failed: ${e}`
    }
  }

  function scheduleSave() {
    if (saveTimer) clearTimeout(saveTimer)
    saveTimer = setTimeout(() => {
      void saveNotifyConfig()
    }, 500)
  }

  async function saveNotifyConfig() {
    notifyStatus = null
    try {
      await invoke('set_notification_config', { config: notifyCfg })
      notifyStatus = 'Saved'
      setTimeout(() => { if (notifyStatus === 'Saved') notifyStatus = null }, 1500)
    } catch (e) {
      notifyStatus = `Save failed: ${e}`
    }
  }

  // Type-safe accessors for NotifyAction union (svelte2tsx can't narrow discriminated unions in templates).
  function actVolume(act: NotifyAction): number { return 'volume' in act ? (act as any).volume : 80 }
  function actSource(act: NotifyAction): SoundSource { return 'source' in act ? (act as any).source : { type: 'builtin', name: 'done' } }
  function actTitle(act: NotifyAction): string { return 'title' in act ? (act as any).title : 'Notification' }
  function actBody(act: NotifyAction): string { return 'body' in act ? (act as any).body : '' }

  async function testTrigger(trigger: NotifyTrigger) {
    try {
      await invoke('test_notification', { trigger })
    } catch (e) {
      notifyStatus = `Test failed: ${e}`
    }
  }

  function getTriggerAction(key: string): NotifyAction {
    return notifyCfg.triggers[key] ?? defaultAction()
  }

  function setTriggerAction(key: string, a: NotifyAction) {
    notifyCfg.triggers[key] = a
    scheduleSave()
  }

  function onNotifyToggle(field: 'enabled' | 'suppress_when_focused', val: boolean) {
    notifyCfg[field] = val
    scheduleSave()
  }

  function onMasterVolume(val: number) {
    notifyCfg.master_volume = val
    scheduleSave()
  }

  $effect(() => {
    if (showSettings && !notifyLoaded) {
      void loadNotifyConfig()
    }
  })

  /* ── Типы, которые нужны и окну, и App: объявлены здесь и экспортируются
     (приём MemoryTab/Icon), чтобы не дублировать объявление в двух файлах. ── */
  export type Appearance = 'dark' | 'light' | 'contrast' | 'system'
  /** Значения совпадают с `ApprovalPolicy` в ядре (serde snake_case). */
  export type PermissionMode = 'always' | 'on_dangerous' | 'never'
  export type UiLang = 'ru' | 'en'
  /* Описание модели: бэкенд отдаёт только id, поэтому способности
     выводим по имени — этого достаточно, чтобы выбрать осознанно. */
  export interface ModelSpec {
    id: string
    ctx: string
    speed: 'fast' | 'balanced' | 'heavy'
    reasoning: boolean
    tools: boolean
    vision: boolean
  }
  export interface ProviderInfo {
    id: string
    label: string
    base_url: string
    flavor: string
    built_in: boolean
    needs_key: boolean
    has_key: boolean
    key_url: string
    models: string[]
  }
  export interface FetchedModel { id: string; label: string }
  export interface ApprovalRow {
    session_id: string
    turn_id: string
    call_id: string
    tool: string
    summary: string
    decision: string
    actor: string
    decided_ms: number
  }
  export interface SeriesPoint { ts_ms: number; value: number }
  export interface TokenDay { day_ms: number; tokens: number; turns: number }
  /* F-11: таблица фоновых задач вынесена в TasksTable.svelte — её можно
     отрендерить в тесте, не поднимая всё окно настроек со сотней пропсов.
     Тип строки объявлен там же (приём MemoryTab/MemoryItem) и переэкспортирован:
     App.svelte по-прежнему берёт TaskRow отсюда, объявление остаётся одним. */
  /* F-11: сама таблица фоновых задач вынесена в TasksTable.svelte — её можно
     отрендерить в тесте, не поднимая всё окно настроек со сотней пропсов. Тип
     строки объявлен ЗДЕСЬ, а не там: App.svelte берёт TaskRow отсюда, а
     re-export типов из instance-скрипта svelte2tsx не поднимает (проверено
     svelte-check — «has no exported member»). TasksTable импортирует его как
     type-only, поэтому объявление остаётся единственным. */
  export interface TaskRow { id: string; kind: string; state: string; attempts: number; next_try_ms: number; last_error: string }
  export interface CmdRules {
    allow: string[]
    deny: string[]
    allow_defaults: string[]
    deny_defaults: string[]
    errors: string[]
    active: number
  }
  export interface HooksDto {
    raw: string
    pre_turn: number
    post_turn: number
    pre_tool: number
    errors: string[]
    timeout_secs: number
    max_per_event: number
  }

  interface Props {
    /* ── окно и вкладки ── */
    showSettings: boolean
    settingsTab: 'general' | 'models' | 'providers' | 'plugins' | 'security' | 'mcp' | 'telemetry' | 'memory'
    t: (key: StrKey) => string

    /* ── общие: внешний вид, студия тем, фон, масштаб ── */
    appearance: Appearance
    fontSize: number
    glowLevel: 'soft' | 'normal' | 'strong'
    uiZoom: number
    uiLang: UiLang
    effectsEnabled: boolean
    bgMode: 'sphere' | 'matrix'
    bgMediaName: string
    bgMediaBusy: boolean
    bgMediaError: string
    customAccent: string
    customTint: string
    themeAccentHex: string
    sidebarWidth: number
    sidebarCollapsed: boolean
    zoomIn: () => void
    zoomOut: () => void
    zoomReset: () => void
    onPickBgMedia: (e: Event) => void
    clearBgMedia: () => Promise<void>
    openConfigFile: () => Promise<void>
    exportDiagnostics: () => Promise<void>
    restartApp: () => Promise<void>

    /* ── безопасность: политика, журнал подтверждений, DPAPI, HTTP API, правила команд, хуки, импорт ── */
    permissionMode: PermissionMode
    permSpecs: { id: PermissionMode; icon: IconName; title: string; desc: string }[]
    permSpec: (id: PermissionMode) => { id: PermissionMode; icon: IconName; title: string; desc: string }
    selectPermission: (mode: PermissionMode) => Promise<void>
    sessPolicy: 'global' | PermissionMode
    applySessPolicy: (p: 'global' | PermissionMode) => Promise<void>
    approvalRows: ApprovalRow[]
    approvalLogLoaded: boolean
    loadApprovalLog: () => Promise<void>
    fmtLogTime: (ms: number) => string
    dpapiKey: string
    dpapiStored: boolean
    dpapiStatus: string | null
    saveDpapiKey: () => Promise<void>
    clearDpapiKey: () => Promise<void>
    httpApi: { enabled: boolean; port: number | null; token: string | null } | null
    copyHttpToken: () => Promise<void>
    cmdRules: CmdRules | null
    cmdRulesBusy: boolean
    cmdRulesStatus: string | null
    saveCmdRules: (allow: string[], deny: string[]) => Promise<void>
    hooksDto: HooksDto | null
    hooksBusy: boolean
    hooksStatus: string | null
    hooksError: string | null
    saveHooks: (raw: string) => Promise<void>
    currentSession: string | null
    dshImportBusy: boolean
    dshImportNote: string
    runDshImport: () => Promise<void>

    /* ── модели ── */
    models: string[]
    modelName: string
    modelSpecs: ModelSpec[]
    currentSpec: ModelSpec
    reloadModels: () => Promise<void>

    /* ── провайдеры ── */
    provState: { active: string; providers: ProviderInfo[] } | null
    provBusy: boolean
    provNote: string
    provFetching: string
    provKeys: Record<string, string>
    customLabel: string
    customBase: string
    customKey: string
    customFlavor: 'openai' | 'anthropic'
    provPreview: { id: string | null; label: string; base_url: string; flavor: string; typedKey: string; models: FetchedModel[] } | null
    loadProviders: () => Promise<void>
    useProvider: (id: string) => Promise<void>
    deleteProvider: (id: string) => Promise<void>
    fetchModels: (p: { id: string | null; label: string; base_url: string; flavor: string }, typedKey: string) => Promise<void>
    savePreview: () => Promise<void>

    /* ── MCP-серверы ── */
    mcpServers: { name: string; command: string; args: string[]; enabled: boolean; state: string; tools: number }[]
    mcpName: string
    mcpCommand: string
    mcpBusy: boolean
    mcpNote: string
    loadMcp: () => Promise<void>
    addMcp: () => Promise<void>
    removeMcp: (name: string) => Promise<void>

    /* ── телеметрия и обновления ── */
    telTokens: TokenDay[]
    telLatency: SeriesPoint[]
    telLagE: SeriesPoint[]
    telLagD: SeriesPoint[]
    telTasks: TaskRow[]
    refreshTelemetry: () => Promise<void>
    cancelTask: (id: string) => Promise<void>
    updateState: 'idle' | 'checking' | 'upToDate' | 'available' | 'downloading' | 'ready' | 'error'
    updateCurrent: string
    updateVersion: string
    updateNotes: string
    updateError: string
    updatePercent: number
    checkUpdate: () => Promise<void>
    installUpdate: () => Promise<void>

    /* ── память ── */
    memoryItems: MemoryItem[]
    memoryScope: 'global' | 'session'
    memoryBusy: boolean
    memoryStatus: string | null
    refreshMemory: () => Promise<void>
    addMemory: (text: string) => Promise<void>
    forgetMemory: (id: number) => Promise<void>
    setMemoryScope: (scope: 'global' | 'session') => void

    /* ── js-плагины ── */
    jsPlugTools: { name: string; description: string; file: string }[]
    jsPlugErrors: string[]
    jsPlugDir: string
    jsPlugBusy: boolean
    loadJsPlugins: () => Promise<void>
    reloadJsPlugins: () => Promise<void>
  }

  /* Пропсы, которые окно меняет само, — $bindable: App видит новое значение
     сразу (тема, размер шрифта, ширины сайдбара, черновики провайдера и MCP).
     Остальное приходит только сверху; provKeys мутируется по ключу — ссылка
     на объект общая, поэтому $bindable там не нужен. */
  let {
    /* окно и вкладки */
    showSettings = $bindable(),
    settingsTab = $bindable(),
    t,

    /* общие: внешний вид, студия тем, фон, масштаб */
    appearance = $bindable(),
    fontSize = $bindable(),
    glowLevel = $bindable(),
    uiZoom,
    uiLang = $bindable(),
    effectsEnabled = $bindable(),
    bgMode = $bindable(),
    bgMediaName,
    bgMediaBusy,
    bgMediaError,
    customAccent = $bindable(),
    customTint = $bindable(),
    themeAccentHex,
    sidebarWidth = $bindable(),
    sidebarCollapsed = $bindable(),
    zoomIn,
    zoomOut,
    zoomReset,
    onPickBgMedia,
    clearBgMedia,
    openConfigFile,
    exportDiagnostics,
    restartApp,

    /* безопасность: политика, журнал подтверждений, DPAPI, HTTP API, правила команд, хуки, импорт */
    permissionMode,
    permSpecs,
    permSpec,
    selectPermission,
    sessPolicy,
    applySessPolicy,
    approvalRows,
    approvalLogLoaded,
    loadApprovalLog,
    fmtLogTime,
    dpapiKey = $bindable(),
    dpapiStored,
    dpapiStatus,
    saveDpapiKey,
    clearDpapiKey,
    httpApi,
    copyHttpToken,
    cmdRules,
    cmdRulesBusy,
    cmdRulesStatus,
    saveCmdRules,
    hooksDto,
    hooksBusy,
    hooksStatus,
    hooksError,
    saveHooks,
    currentSession,
    dshImportBusy,
    dshImportNote,
    runDshImport,

    /* модели */
    models,
    modelName = $bindable(),
    modelSpecs,
    currentSpec,
    reloadModels,

    /* провайдеры */
    provState,
    provBusy,
    provNote,
    provFetching,
    provKeys,
    customLabel = $bindable(),
    customBase = $bindable(),
    customKey = $bindable(),
    customFlavor = $bindable(),
    provPreview = $bindable(),
    loadProviders,
    useProvider,
    deleteProvider,
    fetchModels,
    savePreview,

    /* MCP-серверы */
    mcpServers,
    mcpName = $bindable(),
    mcpCommand = $bindable(),
    mcpBusy,
    mcpNote,
    loadMcp,
    addMcp,
    removeMcp,

    /* телеметрия и обновления */
    telTokens,
    telLatency,
    telLagE,
    telLagD,
    telTasks,
    refreshTelemetry,
    cancelTask,
    updateState,
    updateCurrent,
    updateVersion,
    updateNotes,
    updateError,
    updatePercent,
    checkUpdate,
    installUpdate,

    /* память */
    memoryItems,
    memoryScope,
    memoryBusy,
    memoryStatus,
    refreshMemory,
    addMemory,
    forgetMemory,
    setMemoryScope,

    /* js-плагины */
    jsPlugTools,
    jsPlugErrors,
    jsPlugDir,
    jsPlugBusy,
    loadJsPlugins,
    reloadJsPlugins,
  }: Props = $props()
</script>

{#if showSettings}
  <div class="settings-overlay" onclick={() => (showSettings = false)} onkeydown={(e) => { if (e.key === 'Escape') showSettings = false }} role="presentation">
    <div class="settings-dialog" onclick={(e) => e.stopPropagation()} onkeydown={(e) => { if (e.key === 'Escape') showSettings = false }} role="dialog" aria-label="настройки" tabindex="-1">
      <div class="settings-header">
        <h2>{t('settings')}</h2>
        <div class="settings-actions">
          <button class="settings-action-btn" onclick={openConfigFile}>Open configuration file</button>
          <button class="settings-action-btn" onclick={exportDiagnostics}>Export Diagnostics</button>
          <button class="settings-close" onclick={() => (showSettings = false)} aria-label="закрыть"><Icon name="close" size={14} /></button>
        </div>
      </div>
      <div class="settings-body">
        <nav class="settings-nav">
          <button class="settings-nav-item" class:active={settingsTab === 'general'} onclick={() => (settingsTab = 'general')}><Icon name="gear" size={14} /> General</button>
          <button class="settings-nav-item" class:active={settingsTab === 'models'} onclick={() => (settingsTab = 'models')}><Icon name="cpu" size={14} /> {t('modelTitle')}</button>
          <button class="settings-nav-item" class:active={settingsTab === 'providers'} onclick={() => { settingsTab = 'providers'; void loadProviders() }}><Icon name="sliders" size={14} /> {t('provTab')}</button>
          <button class="settings-nav-item" class:active={settingsTab === 'plugins'} onclick={() => { settingsTab = 'plugins'; void loadJsPlugins() }}><Icon name="plug" size={14} /> Plugins</button>
          <button class="settings-nav-item" class:active={settingsTab === 'security'} onclick={() => (settingsTab = 'security')}><Icon name="shield" size={14} /> {t('securityTab')}</button>
          <button class="settings-nav-item" class:active={settingsTab === 'mcp'} onclick={() => { settingsTab = 'mcp'; void loadMcp() }}><Icon name="external" size={14} /> MCP</button>
          <button class="settings-nav-item" class:active={settingsTab === 'telemetry'} onclick={() => { settingsTab = 'telemetry'; void refreshTelemetry() }}><Icon name="scale" size={14} /> {t('telemetryTab')}</button>
          <button class="settings-nav-item" class:active={settingsTab === 'memory'} onclick={() => { settingsTab = 'memory'; void refreshMemory() }}><Icon name="brain" size={14} /> {t('memoryTab')}</button>
        </nav>
        <div class="settings-content">
          {#if settingsTab === 'general'}
            <div class="settings-section">
              <div class="settings-row">
                <div class="settings-label">
                  <span class="label-title">{t('permTitle')}</span>
                  <span class="label-desc">{permSpec(permissionMode).desc}</span>
                </div>
                <select
                  class="settings-select"
                  value={permissionMode}
                  onchange={(e) => selectPermission(e.currentTarget.value as PermissionMode)}
                >
                  {#each permSpecs as p (p.id)}
                    <option value={p.id}>{p.title}</option>
                  {/each}
                </select>
              </div>
              <div class="settings-row">
                <div class="settings-label">
                  <span class="label-title">{t('language')}</span>
                </div>
                <select class="settings-select" bind:value={uiLang}>
                  <option value="ru">Русский</option>
                  <option value="en">English</option>
                </select>
              </div>
              <div class="settings-row">
                <div class="settings-label">
                  <span class="label-title">{t('appearance')}</span>
                  <span class="label-desc">{t('fontSizeDesc')}</span>
                </div>
                <div class="appearance-options">
                  <button class="appearance-btn" class:active={appearance === 'dark'} onclick={() => (appearance = 'dark')}><Icon name="moon" size={14} /> {t('dark')}</button>
                  <button class="appearance-btn" class:active={appearance === 'light'} onclick={() => (appearance = 'light')}><Icon name="sun" size={14} /> {t('light')}</button>
                  <button class="appearance-btn" class:active={appearance === 'contrast'} onclick={() => (appearance = 'contrast')}><Icon name="contrast" size={14} /> {t('contrast')}</button>
                  <button class="appearance-btn" class:active={appearance === 'system'} onclick={() => (appearance = 'system')}><Icon name="monitor" size={14} /> {t('system')}</button>
                </div>
              </div>
              <div class="settings-row">
                <div class="settings-label">
                  <span class="label-title">{t('glowTitle')}</span>
                  <span class="label-desc">{t('glowDesc')}</span>
                </div>
                <div class="appearance-options">
                  <button class="appearance-btn" class:active={glowLevel === 'soft'} onclick={() => (glowLevel = 'soft')}>{t('glowSoft')}</button>
                  <button class="appearance-btn" class:active={glowLevel === 'normal'} onclick={() => (glowLevel = 'normal')}>{t('glowNormal')}</button>
                  <button class="appearance-btn" class:active={glowLevel === 'strong'} onclick={() => (glowLevel = 'strong')}>{t('glowStrong')}</button>
                </div>
              </div>
              <div class="settings-row">
                <div class="settings-label">
                  <span class="label-title">{t('fontSize')}</span>
                  <span class="label-desc">{t('fontSizeDesc')}</span>
                </div>
                <div class="font-size-control">
                  <button class="step-btn" onclick={() => (fontSize = Math.max(10, fontSize - 1))} aria-label="−">−</button>
                  <input type="number" bind:value={fontSize} min="10" max="24" class="settings-number" />
                  <span>px</span>
                  <button class="step-btn" onclick={() => (fontSize = Math.min(24, fontSize + 1))} aria-label="+">+</button>
                </div>
              </div>
              <div class="settings-row">
                <div class="settings-label">
                  <span class="label-title">{t('zoom')}</span>
                  <span class="label-desc">{t('zoomHotkeys')}</span>
                </div>
                <div class="zoom-control settings-zoom">
                  <button onclick={zoomOut} aria-label="−">−</button>
                  <button class="zoom-value" onclick={zoomReset}>{Math.round(uiZoom * 100)}%</button>
                  <button onclick={zoomIn} aria-label="+">+</button>
                </div>
              </div>
              <div class="settings-row">
                <div class="settings-label">
                  <span class="label-title">{t('sidebarWidth')}</span>
                  <span class="label-desc">Ctrl+B — {t('maximize')}</span>
                </div>
                <div class="font-size-control">
                  <input
                    type="range"
                    min="200"
                    max="560"
                    step="10"
                    bind:value={sidebarWidth}
                    class="settings-range"
                  />
                  <span>{Math.round(sidebarWidth)}px</span>
                  <button class="step-btn text-btn" onclick={() => { sidebarWidth = 260; sidebarCollapsed = false }}>{t('reset')}</button>
                </div>
              </div>
              <div class="settings-row">
                <div class="settings-label">
                  <span class="label-title">Background effects</span>
                  <span class="label-desc">Wireframe sphere, particles, scan lines</span>
                </div>
                <label class="toggle-switch">
                  <input type="checkbox" bind:checked={effectsEnabled} />
                  <span class="toggle-slider"></span>
                </label>
              </div>
              <div class="settings-row">
                <div class="settings-label">
                  <span class="label-title">Background mode</span>
                </div>
                <select bind:value={bgMode} class="settings-select">
                  <option value="sphere">Wireframe sphere</option>
                  <option value="matrix">Matrix rain</option>
                </select>
              </div>
              <div class="settings-row">
                <div class="settings-label">
                  <span class="label-title">{t('dshImportTitle')}</span>
                  <span class="label-desc">{dshImportNote || t('dshImportDesc')}</span>
                </div>
                <button class="appearance-btn" disabled={dshImportBusy} onclick={() => void runDshImport()}>
                  {dshImportBusy ? t('dshImportBusy') : t('dshImportRun')}
                </button>
              </div>
              <div class="settings-row">
                <div class="settings-label">
                  <span class="label-title">{t('studioAccent')}</span>
                  <span class="label-desc">{t('studioAccentDesc')}</span>
                </div>
                <div class="studio-controls">
                  <input
                    type="color"
                    class="color-well"
                    value={customAccent || themeAccentHex}
                    oninput={(e) => (customAccent = (e.target as HTMLInputElement).value)}
                    title={t('studioAccent')}
                    aria-label={t('studioAccent')}
                  />
                  <button class="step-btn text-btn" onclick={() => (customAccent = '')}>{t('reset')}</button>
                </div>
              </div>
              <div class="settings-row">
                <div class="settings-label">
                  <span class="label-title">{t('studioTint')}</span>
                  <span class="label-desc">{t('studioTintDesc')}</span>
                </div>
                <div class="studio-controls">
                  <input
                    type="color"
                    class="color-well"
                    value={customTint || '#7a7a88'}
                    oninput={(e) => (customTint = (e.target as HTMLInputElement).value)}
                    title={t('studioTint')}
                    aria-label={t('studioTint')}
                  />
                  <button class="step-btn text-btn" onclick={() => (customTint = '')}>{t('reset')}</button>
                </div>
              </div>
              <div class="settings-row">
                <div class="settings-label">
                  <span class="label-title">{t('studioMedia')}</span>
                  <span class="label-desc" class:label-err={bgMediaError !== ''}>
                    {bgMediaBusy ? t('studioMediaBusy') : bgMediaError || bgMediaName || t('studioMediaDesc')}
                  </span>
                </div>
                <div class="studio-controls">
                  <label class="step-btn text-btn media-pick">
                    {t('studioMediaPick')}
                    <input
                      type="file"
                      accept=".gif,.mp4,.webm,.png,.jpg,.jpeg,.webp,image/*,video/mp4,video/webm"
                      onchange={onPickBgMedia}
                    />
                  </label>
                  {#if bgMediaName}
                    <button class="step-btn text-btn" onclick={clearBgMedia}>{t('studioMediaClear')}</button>
                  {/if}
                </div>
              </div>
              <div class="settings-row">
                <div class="settings-label">
                  <span class="label-title">{t('updateTitle')}</span>
                  <span class="label-desc" class:label-err={updateState === 'error'}>
                    {#if updateState === 'checking'}
                      {t('updateChecking')}
                    {:else if updateState === 'upToDate'}
                      {updateCurrent ? `${t('updateVersion')} ${updateCurrent} — ` : ''}{t('updateUpToDate')}
                    {:else if updateState === 'available'}
                      {t('updateAvailable')}: {updateVersion}{updateNotes ? ` — ${updateNotes}` : ''}
                    {:else if updateState === 'downloading'}
                      {t('updateDownloading')}… {updatePercent}%
                    {:else if updateState === 'ready'}
                      {t('updateReady')}
                    {:else if updateState === 'error'}
                      {t('updateError')}: {updateError}
                    {:else if updateCurrent}
                      {t('updateVersion')}: {updateCurrent}
                    {:else}
                      {t('updateIdle')}
                    {/if}
                  </span>
                </div>
                <div class="appearance-options">
                  {#if updateState === 'available'}
                    <button class="appearance-btn" onclick={() => void installUpdate()}>{t('updateInstallBtn')}</button>
                  {:else if updateState === 'ready'}
                    <button class="appearance-btn" onclick={() => void restartApp()}>{t('updateRestartBtn')}</button>
                  {:else}
                    <button
                      class="appearance-btn"
                      disabled={updateState === 'checking' || updateState === 'downloading'}
                      onclick={() => void checkUpdate()}
                    >
                      {t('updateCheckBtn')}
                    </button>
                  {/if}
                </div>
              </div>

              <!-- ── Notifications subsection ── -->
              <div class="settings-row">
                <div class="settings-label">
                  <span class="label-title">Notifications</span>
                  <span class="label-desc">{notifyStatus ?? (notifyLoaded ? 'Configure alerts for turn events' : 'Loading...')}</span>
                </div>
                <label class="toggle-switch">
                  <input type="checkbox" checked={notifyCfg.enabled} onchange={(e) => onNotifyToggle('enabled', e.currentTarget.checked)} />
                  <span class="toggle-slider"></span>
                </label>
              </div>
              <div class="settings-row">
                <div class="settings-label">
                  <span class="label-title">Suppress when focused</span>
                  <span class="label-desc">Don't notify while the window is active</span>
                </div>
                <label class="toggle-switch">
                  <input type="checkbox" checked={notifyCfg.suppress_when_focused} onchange={(e) => onNotifyToggle('suppress_when_focused', e.currentTarget.checked)} />
                  <span class="toggle-slider"></span>
                </label>
              </div>
              <div class="settings-row">
                <div class="settings-label">
                  <span class="label-title">Master volume</span>
                  <span class="label-desc">{Math.round(notifyCfg.master_volume)}%</span>
                </div>
                <div class="font-size-control">
                  <input
                    type="range"
                    min="0"
                    max="100"
                    step="1"
                    value={notifyCfg.master_volume}
                    oninput={(e) => onMasterVolume(Number((e.target as HTMLInputElement).value))}
                    class="settings-range"
                  />
                  <span>{Math.round(notifyCfg.master_volume)}%</span>
                </div>
              </div>
              {#each TRIGGER_KEYS as key (key)}
                {@const act = getTriggerAction(key)}
                {@const actionType = (act.action as string) || 'off'}
                <div class="settings-row notify-trigger-row">
                  <div class="settings-label">
                    <span class="label-title">{TRIGGER_LABELS[key]}</span>
                  </div>
                  <div class="notify-controls">
                    <select
                      class="settings-select"
                      value={actionType}
                      onchange={(e) => {
                        const v = e.currentTarget.value
                        if (v === 'off') setTriggerAction(key, { action: 'off' as never, source: { type: 'builtin', name: 'done' }, volume: 80, title: '', body: '' })
                        else if (v === 'sound') setTriggerAction(key, { action: 'sound', source: { type: 'builtin', name: 'done' }, volume: actVolume(act) })
                        else if (v === 'toast') setTriggerAction(key, { action: 'toast', title: actTitle(act), body: actBody(act) })
                        else if (v === 'both') setTriggerAction(key, { action: 'both', source: actSource(act), volume: actVolume(act), title: actTitle(act), body: actBody(act) })
                      }}
                    >
                      <option value="off">Off</option>
                      <option value="sound">Sound</option>
                      <option value="toast">Toast</option>
                      <option value="both">Sound + Toast</option>
                    </select>
                    {#if actionType === 'sound' || actionType === 'both'}
                      <select
                        class="settings-select"
                        value={(actSource(act).type === 'builtin' ? (actSource(act) as any).name : 'done')}
                        onchange={(e) => {
                          const name = e.currentTarget.value
                          const cur = getTriggerAction(key)
                          setTriggerAction(key, { ...cur, source: { type: 'builtin', name } } as NotifyAction)
                        }}
                      >
                        {#each BUILTIN_SOUNDS as s (s)}
                          <option value={s}>{s}</option>
                        {/each}
                      </select>
                      <input
                        type="range"
                        min="0"
                        max="100"
                        step="1"
                        value={actVolume(act)}
                        oninput={(e) => {
                          const vol = Number((e.target as HTMLInputElement).value)
                          const cur = getTriggerAction(key)
                          setTriggerAction(key, { ...cur, volume: vol } as NotifyAction)
                        }}
                        class="settings-range notify-vol-range"
                        title={`Volume: ${actVolume(act)}%`}
                      />
                    {/if}
                    <button class="step-btn text-btn" onclick={() => void testTrigger(key)}>Test</button>
                  </div>
                </div>
              {/each}
            </div>
          {:else if settingsTab === 'security'}
            <div class="settings-section">
              <div class="settings-row">
                <div class="settings-label">
                  <span class="label-title">{t('dpapiTitle')}</span>
                  <span class="label-desc">{t('dpapiDesc')}</span>
                </div>
                <div class="dpapi-controls">
                  <input
                    class="dpapi-input"
                    type="password"
                    bind:value={dpapiKey}
                    placeholder="sk-..."
                    aria-label={t('dpapiTitle')}
                    autocomplete="off"
                  />
                  <button class="appearance-btn" onclick={() => void saveDpapiKey()} disabled={!dpapiKey.trim()}>{t('dpapiSave')}</button>
                  <button class="appearance-btn" onclick={() => void clearDpapiKey()} disabled={!dpapiStored}>{t('dpapiForget')}</button>
                </div>
              </div>
              <div class="settings-row">
                <div class="settings-label">
                  <span class="label-title">{dpapiStored ? t('dpapiStored') : t('dpapiNotStored')}</span>
                </div>
                {#if dpapiStatus}<span class="dpapi-status">{dpapiStatus}</span>{/if}
              </div>
              <div class="settings-row">
                <div class="settings-label">
                  <span class="label-title">{t('httpApiTitle')}</span>
                  <span class="label-desc">{httpApi?.enabled ? `${t('httpApiOn')} 127.0.0.1:${httpApi.port}` : t('httpApiOff')}</span>
                </div>
                {#if httpApi?.token}
                  <button class="appearance-btn" onclick={() => void copyHttpToken()}>{t('httpApiCopy')}</button>
                {/if}
              </div>
              <div class="settings-row">
                <div class="settings-label">
                  <span class="label-title">{t('sessPolicyTitle')}</span>
                  <span class="label-desc">{t('sessPolicyDesc')}</span>
                </div>
                <select
                  class="settings-select"
                  value={sessPolicy}
                  onchange={(e) => void applySessPolicy(e.currentTarget.value as 'global' | PermissionMode)}
                  disabled={!currentSession}
                >
                  <option value="global">{t('policyGlobal')}</option>
                  {#each permSpecs as p (p.id)}
                    <option value={p.id}>{p.title}</option>
                  {/each}
                </select>
              </div>
              {#if cmdRules}
                <div class="settings-row rules-row">
                  <CommandRules
                    title={t('cmdRulesTitle')}
                    desc={t('cmdRulesDesc')}
                    allowLabel={t('cmdAllowLabel')}
                    denyLabel={t('cmdDenyLabel')}
                    allowHint={t('cmdAllowHint')}
                    denyHint={t('cmdDenyHint')}
                    saveLabel={t('cmdRulesSave')}
                    resetLabel={t('cmdRulesReset')}
                    savedLabel={t('cmdRulesBusy')}
                    allow={cmdRules.allow}
                    deny={cmdRules.deny}
                    allowDefaults={cmdRules.allow_defaults}
                    denyDefaults={cmdRules.deny_defaults}
                    busy={cmdRulesBusy}
                    status={cmdRulesStatus}
                    onSave={(a, d) => void saveCmdRules(a, d)}
                  />
                </div>
              {/if}
              {#if hooksDto}
                <div class="settings-row rules-row">
                  <HooksEditor
                    title={t('hooksTitle')}
                    desc={t('hooksDesc')}
                    hint={t('hooksHint')}
                    saveLabel={t('hooksSave')}
                    savedLabel={t('hooksBusy')}
                    raw={hooksDto.raw}
                    counts={{
                      pre_turn: hooksDto.pre_turn,
                      post_turn: hooksDto.post_turn,
                      pre_tool: hooksDto.pre_tool,
                    }}
                    timeoutSecs={hooksDto.timeout_secs}
                    maxPerEvent={hooksDto.max_per_event}
                    busy={hooksBusy}
                    status={hooksStatus}
                    error={hooksError ?? (hooksDto.errors.length > 0 ? hooksDto.errors.join('; ') : null)}
                    onSave={(raw) => void saveHooks(raw)}
                  />
                </div>
              {/if}
              <div class="settings-row">
                <div class="settings-label">
                  <span class="label-title">{t('approvalLogTitle')}</span>
                </div>
                <button class="appearance-btn" onclick={() => void loadApprovalLog()}>{t('approvalLogBtn')}</button>
              </div>
              {#if approvalLogLoaded}
                {#if approvalRows.length === 0}
                  <div class="approval-empty">{t('approvalLogEmpty')}</div>
                {:else}
                  <div class="approval-scroll">
                    <table class="approval-table">
                      <thead>
                        <tr>
                          <th>{t('logColTime')}</th>
                          <th>{t('logColTool')}</th>
                          <th></th>
                          <th>{t('logColDecision')}</th>
                          <th>{t('logColActor')}</th>
                        </tr>
                      </thead>
                      <tbody>
                        {#each approvalRows as r, i (i)}
                          <tr>
                            <td class="log-time">{fmtLogTime(r.decided_ms)}</td>
                            <td>{r.tool}</td>
                            <td class="log-summary" title={r.summary}>{r.summary}</td>
                            <td>
                              <span class="log-decision" class:ok={r.decision === 'approved'}>
                                {r.decision === 'approved' ? t('decisionApproved') : t('decisionDenied')}
                              </span>
                            </td>
                            <!-- F-8: отказ хука — отдельный actor. Показывать
                            его «системой» значило бы стереть разницу между
                            «запретило правило из списка» и «не пустил скрипт
                            владельца» — в аудите это разные решения. -->
                            <td>
                              {#if r.actor === 'user'}
                                {t('actorUser')}
                              {:else if r.actor === 'hook'}
                                {t('actorHook')}
                              {:else}
                                {t('actorSystem')}
                              {/if}
                            </td>
                          </tr>
                        {/each}
                      </tbody>
                    </table>
                  </div>
                {/if}
              {/if}
            </div>
          {:else if settingsTab === 'models'}
            <div class="settings-section">
              <div class="settings-row">
                <div class="settings-label">
                  <span class="label-title">{t('modelTitle')}</span>
                  <span class="label-desc">{currentSpec.ctx} {t('ctx')} · {currentSpec.speed === 'fast' ? t('speedFast') : currentSpec.speed === 'heavy' ? t('speedHeavy') : t('speedBalanced')}</span>
                </div>
                <select id="model-select" bind:value={modelName} class="settings-select">
                  {#each models as m (m)}
                    <option value={m}>{m}</option>
                  {/each}
                </select>
                <button class="appearance-btn" onclick={() => void reloadModels()} title={t('modelRefresh')}>{t('modelRefresh')}</button>
              </div>
              <div class="settings-row">
                <div class="settings-label">
                  <span class="label-title">{t('tempAuto')}</span>
                  <span class="label-desc">{t('tempAutoDesc')}</span>
                </div>
                <span class="auto-badge">{t('auto')}</span>
              </div>
              <div class="model-cards">
                {#each modelSpecs as m (m.id)}
                  <button
                    class="model-card"
                    class:selected={m.id === modelName}
                    onclick={() => (modelName = m.id)}
                  >
                    <span class="model-name">{m.id}</span>
                    {#if m.id === modelName}<span class="model-current">{t('currentModel')}</span>{/if}
                    <span class="model-chips">
                      <span class="chip">{t('ctx')} {m.ctx}</span>
                      <span class="chip" class:fast={m.speed === 'fast'} class:heavy={m.speed === 'heavy'}>
                        {m.speed === 'fast' ? t('speedFast') : m.speed === 'heavy' ? t('speedHeavy') : t('speedBalanced')}
                      </span>
                      {#if m.tools}<span class="chip cap">{t('capTools')}</span>{/if}
                      {#if m.reasoning}<span class="chip cap">{t('capReasoning')}</span>{/if}
                      {#if m.vision}<span class="chip cap">{t('capVision')}</span>{/if}
                    </span>
                  </button>
                {/each}
              </div>
            </div>
          {:else if settingsTab === 'providers'}
            <div class="settings-section">
              <div class="settings-row">
                <div class="settings-label">
                  <span class="label-title">{t('provTitle')}</span>
                  <span class="label-desc">{t('provDesc')}</span>
                </div>
                <button class="appearance-btn" onclick={() => void loadProviders()}>{t('modelRefresh')}</button>
              </div>
              {#if provState}
                {#each provState.providers as p (p.id)}
                  <div class="settings-row">
                    <div class="settings-label">
                      <span class="label-title">
                        {#if p.id === provState.active}<span class="model-current">{t('provActive')}</span>{/if}
                        {p.label}
                        <span class="chip">{p.flavor}</span>
                      </span>
                      <span class="label-desc">{p.base_url} · {p.models.length} {t('provModelsN')} · {p.needs_key ? (p.has_key ? t('provKeySet') : t('provKeyMissing')) : t('provLocalFree')}</span>
                      {#if p.key_url}<span class="label-desc">{t('provGetKey')}: {p.key_url}</span>{/if}
                    </div>
                  </div>
                  <div class="settings-row">
                    <div class="settings-label">
                      {#if p.needs_key && !p.has_key}
                        <input
                          class="settings-number"
                          style="width:200px"
                          type="password"
                          value={provKeys[p.id] ?? ''}
                          oninput={(e) => (provKeys[p.id] = (e.target as HTMLInputElement).value)}
                          placeholder={t('provKeyPh')}
                          aria-label={t('provKeyPh')}
                        />
                      {/if}
                    </div>
                    {#if p.id !== provState.active}
                      <button class="appearance-btn" disabled={provBusy} onclick={() => void useProvider(p.id)}>{t('provUse')}</button>
                    {/if}
                    <button
                      class="appearance-btn"
                      disabled={provFetching !== ''}
                      onclick={() => void fetchModels({ id: p.id, label: p.label, base_url: '', flavor: p.flavor }, provKeys[p.id] ?? '')}
                    >{provFetching === p.id ? t('provFetching') : t('provFetch')}</button>
                    {#if !p.built_in}
                      <button class="appearance-btn" disabled={provBusy} onclick={() => void deleteProvider(p.id)}>{t('provDelete')}</button>
                    {/if}
                  </div>
                {/each}
              {/if}
              {#if provPreview}
                <div class="settings-row">
                  <div class="settings-label">
                    <span class="label-title">{t('provPreviewTitle')} ({provPreview.models.length}) — {provPreview.label}</span>
                    <span class="label-desc">{provPreview.models.slice(0, 200).map((m) => m.label === m.id ? m.id : `${m.label} (${m.id})`).join(', ')}</span>
                  </div>
                </div>
                <div class="settings-row">
                  <div class="settings-label"></div>
                  <button class="appearance-btn" disabled={provBusy} onclick={() => void savePreview()}>{t('provSave')}</button>
                  <button class="appearance-btn" onclick={() => (provPreview = null)}>{t('provCancel')}</button>
                </div>
              {/if}
              <div class="settings-row">
                <div class="settings-label">
                  <span class="label-title">{t('provCustomTitle')}</span>
                </div>
              </div>
              <div class="settings-row">
                <div class="settings-label">
                  <input class="settings-number" style="width:150px" bind:value={customLabel} placeholder={t('provNamePh')} aria-label={t('provNamePh')} />
                  <input class="settings-number" style="width:260px" bind:value={customBase} placeholder={t('provBasePh')} aria-label={t('provBasePh')} />
                  <input class="settings-number" style="width:200px" type="password" bind:value={customKey} placeholder={t('provKeyPh')} aria-label={t('provKeyPh')} />
                  <select class="settings-select" bind:value={customFlavor}>
                    <option value="openai">{t('provFlavorOpenAi')}</option>
                    <option value="anthropic">{t('provFlavorAnthropic')}</option>
                  </select>
                </div>
                <button
                  class="appearance-btn"
                  disabled={provFetching !== '' || !customBase.trim()}
                  onclick={() => void fetchModels({ id: null, label: customLabel.trim() || 'custom', base_url: customBase, flavor: customFlavor }, customKey)}
                >{provFetching === 'custom-new' ? t('provFetching') : t('provFetch')}</button>
              </div>
              {#if provNote}
                <div class="settings-row">
                  <div class="settings-label">
                    <span class="label-desc">{provNote}</span>
                  </div>
                </div>
              {/if}
            </div>
          {:else if settingsTab === 'mcp'}
            <div class="settings-section">
              <div class="settings-row">
                <div class="settings-label">
                  <span class="label-title">{t('mcpTitle')}</span>
                  <span class="label-desc">{t('mcpDesc')}</span>
                </div>
              </div>
              {#each mcpServers as s (s.name)}
                <div class="settings-row">
                  <div class="settings-label">
                    <span class="label-title">{s.name}</span>
                    <span class="label-desc">{s.state} — {s.tools} {t('mcpToolsWord')}</span>
                  </div>
                  <button class="appearance-btn" onclick={() => void removeMcp(s.name)}>{t('mcpRemove')}</button>
                </div>
              {/each}
              <div class="settings-row">
                <div class="settings-label">
                  <input class="settings-number" style="width:110px" bind:value={mcpName} placeholder={t('mcpNamePh')} aria-label={t('mcpNamePh')} />
                  <input class="settings-number" style="width:240px" bind:value={mcpCommand} placeholder={t('mcpCommandPh')} aria-label={t('mcpCommandPh')} />
                </div>
                <button class="appearance-btn" disabled={mcpBusy} onclick={() => void addMcp()}>
                  {mcpBusy ? t('mcpBusy') : t('mcpAdd')}
                </button>
              </div>
              {#if mcpNote}
                <div class="settings-row">
                  <div class="settings-label">
                    <span class="label-desc">{mcpNote}</span>
                  </div>
                </div>
              {/if}
            </div>
          {:else if settingsTab === 'telemetry'}
            <div class="settings-section">
              <div class="settings-row">
                <div class="settings-label">
                  <span class="label-title">{t('telemetryTokens')}</span>
                </div>
                <button class="appearance-btn" onclick={() => void refreshTelemetry()}>{t('telemetryRefresh')}</button>
              </div>
              {#if telTokens.length > 0}
                {@const hs = barHeights(telTokens.map((d) => d.tokens), 56)}
                <svg class="tel-chart" viewBox="0 0 {telTokens.length * 10} 60" preserveAspectRatio="none" role="img" aria-label={t('telemetryTokens')}>
                  {#each telTokens as d, i (d.day_ms)}
                    <rect x={i * 10 + 1} y={58 - hs[i]} width="8" height={Math.max(hs[i], 1)} class="tel-bar" />
                  {/each}
                </svg>
              {:else}
                <div class="approval-empty">{t('telemetryEmpty')}</div>
              {/if}
              <div class="settings-row">
                <div class="settings-label">
                  <span class="label-title">{t('telemetryLatency')}</span>
                </div>
              </div>
              <svg class="tel-chart" viewBox="0 0 300 60" preserveAspectRatio="none" role="img" aria-label={t('telemetryLatency')}>
                <polyline points={polylinePoints(telLatency.map((p) => p.value), 300, 56)} class="tel-line" />
              </svg>
              <div class="settings-row">
                <div class="settings-label">
                  <span class="label-title">{t('telemetryLag')}</span>
                </div>
              </div>
              <svg class="tel-chart" viewBox="0 0 300 60" preserveAspectRatio="none" role="img" aria-label={t('telemetryLag')}>
                <polyline points={polylinePoints(telLagE.map((p) => p.value), 300, 56)} class="tel-line" />
                <polyline points={polylinePoints(telLagD.map((p) => p.value), 300, 56)} class="tel-line tel-line2" />
              </svg>
              <div class="settings-row">
                <div class="settings-label">
                  <span class="label-title">{t('telemetryTasks')}</span>
                </div>
              </div>
              <TasksTable tasks={telTasks} t={t} onCancel={(id) => void cancelTask(id)} />
            </div>
          {:else if settingsTab === 'memory'}
            <div class="settings-section">
              <MemoryTab
                title={t('memoryTitle')}
                desc={t('memoryDesc')}
                globalLabel={t('memoryGlobal')}
                sessionLabel={t('memorySession')}
                globalHint={t('memoryGlobalHint')}
                sessionHint={t('memorySessionHint')}
                placeholder={t('memoryPlaceholder')}
                addLabel={t('memoryAdd')}
                forgetLabel={t('memoryForget')}
                emptyLabel={t('memoryEmpty')}
                items={memoryItems}
                scope={memoryScope}
                busy={memoryBusy}
                status={memoryStatus}
                onScope={setMemoryScope}
                onAdd={(text) => void addMemory(text)}
                onForget={(id) => void forgetMemory(id)}
              />
            </div>
          {:else}
            <div class="settings-section">
              <div class="settings-row">
                <div class="settings-label">
                  <span class="label-title">{t('jsPlugTitle')}</span>
                  <span class="label-desc">{jsPlugDir}</span>
                </div>
                <button class="appearance-btn" disabled={jsPlugBusy} onclick={() => void reloadJsPlugins()}>
                  {jsPlugBusy ? t('jsPlugBusy') : t('jsPlugReload')}
                </button>
              </div>
              {#each jsPlugTools as p (p.name)}
                <div class="settings-row">
                  <div class="settings-label">
                    <span class="label-title">{p.name}</span>
                    <span class="label-desc">{p.description}{p.file ? ` — ${p.file}` : ''}</span>
                  </div>
                </div>
              {/each}
              {#each jsPlugErrors as e, i (`err-${i}`)}
                <div class="settings-row">
                  <div class="settings-label">
                    <span class="label-desc">{e}</span>
                  </div>
                </div>
              {/each}
              {#if !jsPlugBusy && jsPlugTools.length === 0 && jsPlugErrors.length === 0}
                <div class="settings-row">
                  <div class="settings-label">
                    <span class="label-desc">{t('jsPlugNone')}</span>
                  </div>
                </div>
              {/if}
            </div>
          {/if}
        </div>
      </div>
    </div>
  </div>
{/if}

<style>
/* B-7/B-8: вкладка «Безопасность» в настройках */
/* F-3: редактор списков команд занимает всю ширину ряда — две колонки
     regex-строк в половину ширины нечитаемы. */
.rules-row {
    display: block;
    padding-top: 14px;
  }

.dpapi-controls {
    display: flex;
    gap: 8px;
    align-items: center;
    flex-wrap: wrap;
    justify-content: flex-end;
  }

.dpapi-input {
    background: rgba(255, 255, 255, 0.04);
    border: 1px solid rgba(255, 255, 255, 0.1);
    color: inherit;
    border-radius: 8px;
    padding: 6px 10px;
    width: 220px;
    font-size: 12px;
  }

.dpapi-input:focus {
    outline: none;
    border-color: var(--accent);
  }

.dpapi-status {
    font-size: 12px;
    opacity: 0.75;
  }

.approval-empty {
    font-size: 12px;
    opacity: 0.6;
    padding: 4px 2px;
  }

.approval-scroll {
    max-height: 260px;
    overflow-y: auto;
    margin-top: 4px;
  }

.approval-table {
    width: 100%;
    border-collapse: collapse;
    font-size: 12px;
  }

.approval-table th {
    text-align: left;
    opacity: 0.6;
    font-weight: 500;
    padding: 4px 8px;
    border-bottom: 1px solid rgba(255, 255, 255, 0.1);
    position: sticky;
    top: 0;
    background: inherit;
  }

.approval-table td {
    padding: 4px 8px;
    border-bottom: 1px solid rgba(255, 255, 255, 0.05);
    vertical-align: top;
  }

/* E-8: графики телеметрии — компактный SVG без библиотек. */
.tel-chart {
    width: 100%;
    height: 60px;
    margin: 4px 0 10px;
    border-radius: 6px;
    background: rgba(127, 127, 127, 0.07);
  }

.tel-line {
    fill: none;
    stroke: var(--accent, #7aa2f7);
    stroke-width: 1.5;
    vector-effect: non-scaling-stroke;
  }

.tel-line2 {
    stroke: #e06c75;
  }

.tel-bar {
    fill: var(--accent, #7aa2f7);
    opacity: 0.85;
  }

.log-time {
    white-space: nowrap;
    opacity: 0.7;
  }

.log-summary {
    max-width: 340px;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    opacity: 0.85;
  }

.log-decision {
    padding: 1px 8px;
    border-radius: 999px;
    background: rgba(255, 90, 90, 0.14);
    color: #ff8a8a;
    white-space: nowrap;
  }

.log-decision.ok {
    background: rgba(90, 220, 130, 0.12);
    color: #6fdc8c;
  }

@keyframes fade-in {
    from { opacity: 0; }
    to { opacity: 1; }
  }

.settings-overlay {
    position: fixed;
    inset: 0;
    z-index: 600;
    background: var(--surface-overlay);
    display: flex;
    align-items: center;
    justify-content: center;
    backdrop-filter: blur(2px);
    transition: background 0.15s;
  }

.settings-overlay:nth-child(odd):hover {
    background: rgba(0, 0, 0, 0.85);
    backdrop-filter: blur(8px);
  }

.settings-overlay:nth-child(even):hover {
    background: rgba(0, 0, 0, 0.8);
    backdrop-filter: blur(6px);
  }

.settings-overlay:hover {
    background: rgba(0, 0, 0, 0.7);
  }

.settings-dialog {
    width: 780px;
    max-width: 90vw;
    max-height: 80vh;
    background: var(--bg-elevated);
    border: 1px solid var(--border);
    border-radius: 12px;
    overflow: hidden;
    box-shadow: var(--shadow-strong);
    display: flex;
    flex-direction: column;
    animation: dialog-in 0.25s ease-out;
    transition: all 0.15s;
  }

.settings-dialog:hover {
    border-color: var(--accent-dim);
    box-shadow: 0 24px 72px rgba(var(--accent-rgb), calc(0.2 * var(--glow-k)));
    transform: scale(1.01);
  }

@keyframes dialog-in {
    from {
      opacity: 0;
      transform: scale(0.95);
    }
    to {
      opacity: 1;
      transform: scale(1);
    }
  }

.settings-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    padding: 16px 20px;
    border-bottom: 1px solid var(--border);
    transition: background 0.15s;
  }

.settings-header:nth-child(odd):hover {
    background: rgba(var(--accent-rgb), calc(0.01 * var(--glow-k)));
  }

.settings-header:nth-child(even):hover {
    background: rgba(255, 255, 255, 0.01);
  }

.settings-header:hover {
    background: rgba(var(--accent-rgb), calc(0.01 * var(--glow-k)));
    box-shadow: 0 0 16px rgba(var(--accent-rgb), calc(0.03 * var(--glow-k)));
  }

.settings-header h2 {
    font-family: var(--mono);
    font-size: 16px;
    font-weight: 600;
    color: var(--text);
    margin: 0;
    transition: color 0.15s;
  }

.settings-header:hover h2 {
    color: var(--accent);
  }

.settings-actions {
    display: flex;
    gap: 8px;
    align-items: center;
    transition: all 0.15s;
  }

.settings-actions:nth-child(odd):hover {
    gap: 12px;
  }

.settings-actions:nth-child(even):hover {
    gap: 10px;
  }

.settings-actions:hover {
    gap: 10px;
  }

.settings-action-btn {
    padding: 6px 12px;
    background: var(--bg);
    border: 1px solid var(--border);
    border-radius: 6px;
    color: var(--text-dim);
    font-family: var(--mono);
    font-size: 11px;
    cursor: pointer;
    transition: all 0.15s;
  }

.settings-action-btn:nth-child(odd):hover {
    transform: translateY(-2px);
    box-shadow: 0 0 16px rgba(var(--accent-rgb), calc(0.2 * var(--glow-k)));
  }

.settings-action-btn:nth-child(even):hover {
    transform: translateY(-1px);
    box-shadow: 0 0 12px rgba(var(--accent-rgb), calc(0.15 * var(--glow-k)));
  }

.settings-action-btn:hover {
    border-color: var(--accent);
    color: var(--accent);
    background: rgba(var(--accent-rgb), calc(0.05 * var(--glow-k)));
    transform: translateY(-1px);
    box-shadow: 0 0 12px rgba(var(--accent-rgb), calc(0.15 * var(--glow-k)));
  }

.settings-close {
    background: none;
    border: none;
    color: var(--text-faint);
    font-size: 16px;
    cursor: pointer;
    padding: 4px 8px;
    transition: all 0.15s;
  }

.settings-close:nth-child(odd):hover {
    transform: rotate(180deg) scale(1.2);
  }

.settings-close:nth-child(even):hover {
    transform: rotate(90deg) scale(1.1);
  }

.settings-close:hover {
    color: var(--err);
    background: rgba(255, 0, 0, 0.1);
    border-radius: 4px;
    transform: rotate(90deg) scale(1.1);
    box-shadow: 0 0 12px rgba(255, 0, 0, 0.2);
  }

.settings-body {
    display: grid;
    grid-template-columns: 180px 1fr;
    min-height: 0;
    flex: 1;
    transition: background 0.15s;
  }

.settings-body:nth-child(odd):hover {
    background: rgba(var(--accent-rgb), calc(0.005 * var(--glow-k)));
  }

.settings-body:nth-child(even):hover {
    background: rgba(255, 255, 255, 0.005);
  }

.settings-body:hover {
    background: rgba(var(--accent-rgb), calc(0.005 * var(--glow-k)));
    box-shadow: inset 0 0 16px rgba(var(--accent-rgb), calc(0.02 * var(--glow-k)));
  }

.settings-nav {
    padding: 12px;
    border-right: 1px solid var(--border);
    display: flex;
    flex-direction: column;
    gap: 4px;
    transition: background 0.15s;
  }

.settings-nav:nth-child(odd):hover {
    background: rgba(var(--accent-rgb), calc(0.03 * var(--glow-k)));
    box-shadow: inset 0 0 16px rgba(var(--accent-rgb), calc(0.04 * var(--glow-k)));
  }

.settings-nav:nth-child(even):hover {
    background: rgba(255, 255, 255, 0.03);
    box-shadow: inset 0 0 12px rgba(255, 255, 255, 0.03);
  }

.settings-nav:hover {
    background: rgba(var(--accent-rgb), calc(0.01 * var(--glow-k)));
    box-shadow: 0 0 12px rgba(var(--accent-rgb), calc(0.03 * var(--glow-k)));
  }

.settings-nav-item {
    padding: 10px 12px;
    background: none;
    border: none;
    border-radius: 6px;
    color: var(--text-dim);
    font-family: var(--mono);
    font-size: 12px;
    cursor: pointer;
    text-align: left;
    transition: all 0.15s;
  }

.settings-nav-item:nth-child(odd):hover {
    background: rgba(var(--accent-rgb), calc(0.1 * var(--glow-k)));
    transform: translateX(6px);
    box-shadow: 0 0 16px rgba(var(--accent-rgb), calc(0.15 * var(--glow-k)));
  }

.settings-nav-item:nth-child(even):hover {
    background: rgba(255, 255, 255, 0.1);
    transform: translateX(4px);
    box-shadow: 0 0 12px rgba(255, 255, 255, 0.1);
  }

.settings-nav-item:hover {
    background: rgba(255, 255, 255, 0.05);
    color: var(--text);
    transform: translateX(2px);
    box-shadow: 0 0 12px rgba(255, 255, 255, 0.05);
  }

.settings-nav-item.active {
    background: rgba(var(--accent-rgb), calc(0.1 * var(--glow-k)));
    color: var(--accent);
    transition: all 0.15s;
  }

.settings-nav-item.active:hover {
    background: rgba(var(--accent-rgb), calc(0.15 * var(--glow-k)));
    box-shadow: 0 0 16px rgba(var(--accent-rgb), calc(0.1 * var(--glow-k)));
  }

.settings-nav-item.active:nth-child(odd):hover {
    background: rgba(var(--accent-rgb), calc(0.25 * var(--glow-k)));
    box-shadow: 0 0 24px rgba(var(--accent-rgb), calc(0.3 * var(--glow-k)));
    transform: translateX(6px);
  }

.settings-nav-item.active:nth-child(even):hover {
    background: rgba(var(--accent-rgb), calc(0.22 * var(--glow-k)));
    box-shadow: 0 0 20px rgba(var(--accent-rgb), calc(0.28 * var(--glow-k)));
    transform: translateX(5px);
  }

.settings-content {
    padding: 20px;
    overflow-y: auto;
    animation: fade-in 0.2s ease-out;
    transition: all 0.15s;
  }

.settings-content:nth-child(odd):hover {
    background: rgba(var(--accent-rgb), calc(0.02 * var(--glow-k)));
    box-shadow: inset 0 0 24px rgba(var(--accent-rgb), calc(0.04 * var(--glow-k)));
  }

.settings-content:nth-child(even):hover {
    background: rgba(255, 255, 255, 0.02);
    box-shadow: inset 0 0 20px rgba(255, 255, 255, 0.03);
  }

.settings-content:hover {
    background: rgba(var(--accent-rgb), calc(0.005 * var(--glow-k)));
    box-shadow: inset 0 0 16px rgba(var(--accent-rgb), calc(0.02 * var(--glow-k)));
  }

.settings-section {
    display: flex;
    flex-direction: column;
    gap: 20px;
    transition: background 0.15s;
  }

.settings-section:nth-child(odd):hover {
    background: rgba(var(--accent-rgb), calc(0.02 * var(--glow-k)));
    box-shadow: inset 0 0 24px rgba(var(--accent-rgb), calc(0.04 * var(--glow-k)));
    border-radius: 12px;
    padding: 12px;
    margin: -12px;
  }

.settings-section:nth-child(even):hover {
    background: rgba(255, 255, 255, 0.02);
    box-shadow: inset 0 0 20px rgba(255, 255, 255, 0.03);
    border-radius: 10px;
    padding: 10px;
    margin: -10px;
  }

.settings-section:hover {
    background: rgba(var(--accent-rgb), calc(0.005 * var(--glow-k)));
    box-shadow: inset 0 0 16px rgba(var(--accent-rgb), calc(0.02 * var(--glow-k)));
    border-radius: 8px;
    padding: 8px;
    margin: -8px;
  }

.settings-row {
    display: flex;
    align-items: center;
    justify-content: space-between;
    /* Перенос: ряды с широкими контролами (темы, слайдер ширины) не должны
       выползать за край диалога и обрезаться overflow: hidden. */
    flex-wrap: wrap;
    gap: 16px;
    padding-bottom: 16px;
    border-bottom: 1px solid var(--border);
    transition: all 0.15s;
  }

.settings-row:nth-child(odd):hover {
    background: rgba(var(--accent-rgb), calc(0.04 * var(--glow-k)));
    box-shadow: 0 0 16px rgba(var(--accent-rgb), calc(0.08 * var(--glow-k)));
    transform: translateX(4px);
  }

.settings-row:nth-child(even):hover {
    background: rgba(255, 255, 255, 0.04);
    box-shadow: 0 0 12px rgba(255, 255, 255, 0.06);
    transform: translateX(3px);
  }

.settings-row:hover {
    background: rgba(var(--accent-rgb), calc(0.01 * var(--glow-k)));
    padding-left: 8px;
    padding-right: 8px;
    border-radius: 6px;
    box-shadow: 0 0 12px rgba(var(--accent-rgb), calc(0.03 * var(--glow-k)));
  }

.settings-label {
    display: flex;
    flex-direction: column;
    gap: 4px;
    transition: all 0.15s;
  }

.settings-label:nth-child(odd):hover {
    transform: translateX(5px);
    text-shadow: 0 0 8px rgba(255, 255, 255, 0.1);
  }

.settings-label:nth-child(even):hover {
    transform: translateX(4px);
    text-shadow: 0 0 6px rgba(255, 255, 255, 0.08);
  }

.settings-label:hover {
    transform: translateX(2px);
  }

.settings-label:hover .label-title {
    color: var(--accent);
    text-shadow: 0 0 8px var(--accent-glow);
  }

.label-title {
    font-family: var(--mono);
    font-size: 13px;
    font-weight: 500;
    color: var(--text);
    transition: color 0.15s;
  }

.label-title:nth-child(odd):hover {
    text-shadow: 0 0 16px var(--accent-glow);
    transform: scale(1.02);
  }

.label-title:nth-child(even):hover {
    text-shadow: 0 0 12px var(--accent-glow);
    transform: scale(1.01);
  }

.settings-row:hover .label-title {
    color: var(--accent);
    text-shadow: 0 0 8px var(--accent-glow);
  }

.label-desc {
    font-family: var(--mono);
    font-size: 11px;
    color: var(--text-faint);
    transition: all 0.15s;
  }

.label-desc:nth-child(odd):hover {
    transform: translateX(5px);
    text-shadow: 0 0 8px rgba(255, 255, 255, 0.1);
    color: var(--text);
  }

.label-desc:nth-child(even):hover {
    transform: translateX(4px);
    text-shadow: 0 0 6px rgba(255, 255, 255, 0.08);
    color: var(--text-dim);
  }

.settings-row:hover .label-desc {
    color: var(--text-dim);
    transform: translateX(2px);
  }

.settings-select {
    padding: 8px 12px;
    background: var(--bg);
    border: 1px solid var(--border);
    border-radius: 6px;
    color: var(--text);
    font-family: var(--mono);
    font-size: 12px;
    min-width: 150px;
    outline: none;
    transition: all 0.15s;
  }

.settings-select:nth-child(odd):hover {
    border-color: var(--accent);
    background: rgba(var(--accent-rgb), calc(0.05 * var(--glow-k)));
    box-shadow: 0 0 16px rgba(var(--accent-rgb), calc(0.15 * var(--glow-k)));
    transform: translateY(-2px);
  }

.settings-select:nth-child(even):hover {
    border-color: var(--accent-dim);
    background: rgba(var(--accent-rgb), calc(0.04 * var(--glow-k)));
    box-shadow: 0 0 12px rgba(var(--accent-rgb), calc(0.12 * var(--glow-k)));
    transform: translateY(-1px);
  }

.settings-select:hover {
    border-color: var(--accent-dim);
    box-shadow: 0 0 8px rgba(var(--accent-rgb), calc(0.08 * var(--glow-k)));
  }

.settings-select:focus {
    border-color: var(--accent);
    box-shadow: 0 0 0 2px rgba(var(--accent-rgb), calc(0.1 * var(--glow-k)));
  }

.appearance-options {
    display: flex;
    flex-wrap: wrap;
    /* Своим рядом под подписью: четыре кнопки не влезают в колонку значения. */
    flex: 1 1 100%;
    gap: 8px;
    transition: all 0.15s;
  }

.appearance-options:hover {
    gap: 10px;
  }

.appearance-btn {
    padding: 12px 20px;
    background: var(--bg);
    border: 1px solid var(--border);
    border-radius: 8px;
    color: var(--text-dim);
    font-family: var(--mono);
    font-size: 12px;
    cursor: pointer;
    transition: all 0.15s;
  }

.appearance-btn:nth-child(odd):hover {
    border-color: var(--accent);
    background: rgba(var(--accent-rgb), calc(0.1 * var(--glow-k)));
    box-shadow: 0 0 12px rgba(var(--accent-rgb), calc(0.15 * var(--glow-k)));
  }

.appearance-btn:nth-child(even):hover {
    border-color: var(--accent-dim);
    background: rgba(var(--accent-rgb), calc(0.08 * var(--glow-k)));
    box-shadow: 0 0 8px rgba(var(--accent-rgb), calc(0.12 * var(--glow-k)));
  }

.appearance-btn:hover {
    border-color: var(--accent-dim);
    color: var(--text);
    transform: translateY(-1px);
    box-shadow: 0 0 12px rgba(var(--accent-rgb), calc(0.1 * var(--glow-k)));
  }

.appearance-btn.active {
    border-color: var(--accent);
    color: var(--accent);
    background: rgba(var(--accent-rgb), calc(0.05 * var(--glow-k)));
    box-shadow: 0 0 16px rgba(var(--accent-rgb), calc(0.15 * var(--glow-k)));
  }

.appearance-btn.active:nth-child(odd):hover {
    background: rgba(var(--accent-rgb), calc(0.15 * var(--glow-k)));
    box-shadow: 0 0 20px rgba(var(--accent-rgb), calc(0.25 * var(--glow-k)));
  }

.appearance-btn.active:nth-child(even):hover {
    background: rgba(var(--accent-rgb), calc(0.12 * var(--glow-k)));
    box-shadow: 0 0 16px rgba(var(--accent-rgb), calc(0.22 * var(--glow-k)));
  }

.appearance-btn.active:hover {
    background: rgba(var(--accent-rgb), calc(0.1 * var(--glow-k)));
    box-shadow: 0 0 20px rgba(var(--accent-rgb), calc(0.2 * var(--glow-k)));
  }

.font-size-control {
    display: flex;
    align-items: center;
    gap: 8px;
    font-family: var(--mono);
    font-size: 12px;
    color: var(--text-dim);
    transition: color 0.15s;
  }

.font-size-control:nth-child(odd):hover {
    color: var(--accent-hover);
    text-shadow: 0 0 8px var(--accent-glow);
  }

.font-size-control:nth-child(even):hover {
    color: var(--accent);
    text-shadow: 0 0 6px var(--accent-glow);
  }

.font-size-control:hover {
    color: var(--text);
    text-shadow: 0 0 6px rgba(255, 255, 255, 0.08);
  }

.settings-number {
    width: 60px;
    padding: 6px 8px;
    background: var(--bg);
    border: 1px solid var(--border);
    border-radius: 6px;
    color: var(--text);
    font-family: var(--mono);
    font-size: 12px;
    text-align: center;
    outline: none;
    transition: all 0.15s;
  }

.settings-number:nth-child(odd):hover {
    border-color: var(--accent);
    background: rgba(var(--accent-rgb), calc(0.05 * var(--glow-k)));
    box-shadow: 0 0 16px rgba(var(--accent-rgb), calc(0.15 * var(--glow-k)));
    transform: translateY(-2px);
  }

.settings-number:nth-child(even):hover {
    border-color: var(--accent-dim);
    background: rgba(var(--accent-rgb), calc(0.04 * var(--glow-k)));
    box-shadow: 0 0 12px rgba(var(--accent-rgb), calc(0.12 * var(--glow-k)));
    transform: translateY(-1px);
  }

.settings-number:hover {
    border-color: var(--accent-dim);
    box-shadow: 0 0 8px rgba(var(--accent-rgb), calc(0.08 * var(--glow-k)));
  }

.settings-number:focus {
    border-color: var(--accent);
    box-shadow: 0 0 0 2px rgba(var(--accent-rgb), calc(0.1 * var(--glow-k)));
  }

.settings-range {
    width: 200px;
    accent-color: var(--accent);
    cursor: pointer;
    transition: all 0.15s;
  }

.settings-range:hover {
    transform: scaleY(1.2);
  }

.settings-range::-webkit-slider-thumb {
    cursor: grab;
  }

.settings-range::-webkit-slider-thumb:active {
    cursor: grabbing;
  }

.toggle-switch:nth-child(odd):hover {
    transform: scale(1.08);
    filter: drop-shadow(0 0 12px var(--accent-glow));
  }

.toggle-switch:nth-child(even):hover {
    transform: scale(1.06);
    filter: drop-shadow(0 0 10px var(--accent-glow));
  }

.toggle-switch {
    position: relative;
    display: inline-block;
    width: 44px;
    height: 24px;
    transition: all 0.15s;
  }

.toggle-switch:hover {
    transform: scale(1.05);
    filter: drop-shadow(0 0 8px var(--accent-glow));
  }

.toggle-switch input:nth-child(odd):hover {
    opacity: 0.1;
  }

.toggle-switch input:nth-child(even):hover {
    opacity: 0.05;
  }

.toggle-switch input {
    opacity: 0;
    width: 0;
    height: 0;
  }

.toggle-switch input:hover {
    opacity: 0.02;
  }

.toggle-slider {
    position: absolute;
    cursor: pointer;
    inset: 0;
    background: var(--bg);
    border: 1px solid var(--border);
    border-radius: 24px;
    transition: 0.3s cubic-bezier(0.4, 0, 0.2, 1);
  }

.toggle-slider:nth-child(odd):hover {
    background: rgba(var(--accent-rgb), calc(0.1 * var(--glow-k)));
    box-shadow: 0 0 16px rgba(var(--accent-rgb), calc(0.15 * var(--glow-k)));
  }

.toggle-slider:nth-child(even):hover {
    background: rgba(var(--accent-rgb), calc(0.08 * var(--glow-k)));
    box-shadow: 0 0 12px rgba(var(--accent-rgb), calc(0.12 * var(--glow-k)));
  }

.toggle-slider:hover {
    background: rgba(var(--accent-rgb), calc(0.05 * var(--glow-k)));
    box-shadow: 0 0 12px rgba(var(--accent-rgb), calc(0.1 * var(--glow-k)));
  }

.toggle-slider:before:nth-child(odd):hover {
    background: var(--accent-hover);
    box-shadow: 0 0 12px var(--accent-glow);
    transform: translateX(2px);
  }

.toggle-slider:before:nth-child(even):hover {
    background: var(--accent);
    box-shadow: 0 0 10px var(--accent-glow);
    transform: translateX(1px);
  }

.toggle-slider:before {
    position: absolute;
    content: "";
    height: 18px;
    width: 18px;
    left: 2px;
    bottom: 2px;
    background: var(--text-faint);
    border-radius: 50%;
    transition: 0.3s cubic-bezier(0.4, 0, 0.2, 1);
  }

.toggle-slider:before:hover {
    background: var(--accent);
    box-shadow: 0 0 8px var(--accent-glow);
  }

.toggle-switch:hover .toggle-slider:before {
    background: var(--accent);
    box-shadow: 0 0 8px var(--accent-glow);
  }

.toggle-switch input:checked + .toggle-slider {
    background: var(--accent-dim);
    border-color: var(--accent);
    box-shadow: 0 0 12px rgba(var(--accent-rgb), calc(0.2 * var(--glow-k)));
    transition: all 0.15s;
  }

.toggle-switch:hover input:checked + .toggle-slider {
    background: var(--accent);
    box-shadow: 0 0 20px rgba(var(--accent-rgb), calc(0.3 * var(--glow-k)));
  }

.toggle-switch input:checked + .toggle-slider:before {
    transform: translateX(20px);
    background: var(--accent);
    transition: all 0.15s;
  }

.toggle-switch:hover input:checked + .toggle-slider:before {
    background: #fff;
    box-shadow: 0 0 12px var(--accent-glow);
  }

.toggle-switch:hover .toggle-slider {
    border-color: var(--accent);
    box-shadow: 0 0 16px rgba(var(--accent-rgb), calc(0.15 * var(--glow-k)));
  }

.zoom-control {
    display: flex;
    align-items: center;
    gap: 2px;
    padding: 2px;
    background: var(--surface-inset);
    border: 1px solid var(--border);
    border-radius: 6px;
  }

.zoom-control button {
    background: none;
    border: none;
    padding: 2px 8px;
    border-radius: 4px;
    font-family: var(--mono);
    font-size: 11px;
    color: var(--text-dim);
    cursor: pointer;
    transition: all 0.15s;
  }

.zoom-control button:hover {
    color: var(--accent);
    background: rgba(var(--accent-rgb), calc(0.1 * var(--glow-k)));
    box-shadow: 0 0 8px rgba(var(--accent-rgb), calc(0.12 * var(--glow-k)));
  }

.zoom-control .zoom-value {
    min-width: 46px;
    text-align: center;
    color: var(--text);
  }

.model-cards {
    display: flex;
    flex-direction: column;
    gap: 6px;
    padding-top: 12px;
  }

.model-card {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 6px;
    width: 100%;
    padding: 8px 10px;
    background: var(--bg);
    border: 1px solid var(--border);
    border-radius: 8px;
    cursor: pointer;
    text-align: left;
    transition: all 0.15s;
  }

.model-card:hover {
    border-color: var(--accent-dim);
    background: rgba(var(--accent-rgb), calc(0.05 * var(--glow-k)));
    transform: translateX(2px);
    box-shadow: 0 0 12px rgba(var(--accent-rgb), calc(0.1 * var(--glow-k)));
  }

.model-card.selected {
    border-color: var(--accent);
    background: rgba(var(--accent-rgb), calc(0.09 * var(--glow-k)));
    box-shadow: inset 0 0 12px rgba(var(--accent-rgb), calc(0.08 * var(--glow-k)));
  }

.model-name {
    font-family: var(--mono);
    font-size: 12px;
    color: var(--text);
    font-weight: 600;
  }

.model-card.selected .model-name {
    color: var(--accent);
  }

.model-current {
    font-family: var(--mono);
    font-size: 9px;
    text-transform: uppercase;
    letter-spacing: 0.06em;
    color: #fff;
    background: var(--accent);
    border-radius: 8px;
    padding: 1px 6px;
  }

.model-chips {
    display: flex;
    flex-wrap: wrap;
    gap: 4px;
    width: 100%;
  }

.chip {
    font-family: var(--mono);
    font-size: 9px;
    color: var(--text-dim);
    background: var(--surface-inset);
    border: 1px solid var(--border);
    border-radius: 8px;
    padding: 1px 6px;
    white-space: nowrap;
  }

.chip.fast { color: var(--ok); border-color: color-mix(in srgb, var(--ok) 40%, transparent); }

.chip.heavy { color: var(--warn); border-color: color-mix(in srgb, var(--warn) 40%, transparent); }

.chip.cap { color: var(--accent); border-color: var(--accent-dim); }

.auto-badge {
    font-family: var(--mono);
    font-size: 10px;
    text-transform: uppercase;
    letter-spacing: 0.08em;
    color: var(--ok);
    border: 1px solid color-mix(in srgb, var(--ok) 40%, transparent);
    background: color-mix(in srgb, var(--ok) 10%, transparent);
    border-radius: 999px;
    padding: 2px 10px;
  }

/* Мелкие контролы в Settings */
.step-btn {
    width: 26px;
    height: 26px;
    padding: 0;
    background: var(--bg);
    border: 1px solid var(--border);
    border-radius: 6px;
    color: var(--text-dim);
    font-family: var(--mono);
    font-size: 13px;
    cursor: pointer;
    transition: all 0.15s;
  }

.step-btn:hover {
    color: var(--accent);
    border-color: var(--accent-dim);
    box-shadow: 0 0 10px rgba(var(--accent-rgb), calc(0.15 * var(--glow-k)));
  }

.settings-zoom {
    min-width: 150px;
    justify-content: center;
  }

/* Студия тем: круглые палитры и текстовые кнопки рядом с ними. */
.studio-controls {
    display: flex;
    align-items: center;
    gap: 8px;
  }

.color-well {
    width: 34px;
    height: 34px;
    padding: 0;
    border: 2px solid var(--border);
    border-radius: 50%;
    background: none;
    cursor: pointer;
    transition: all 0.15s;
  }

.color-well:hover {
    border-color: var(--accent);
    box-shadow: 0 0 14px rgba(var(--accent-rgb), calc(0.35 * var(--glow-k)));
    transform: scale(1.06);
  }

/* Системную рамку swatch прячем: остаётся чистый круг цвета. */
.color-well::-webkit-color-swatch-wrapper {
    padding: 2px;
  }

.color-well::-webkit-color-swatch {
    border: none;
    border-radius: 50%;
  }

.text-btn {
    width: auto;
    height: 26px;
    padding: 0 10px;
  }

.media-pick {
    display: inline-flex;
    align-items: center;
    cursor: pointer;
  }

.media-pick input {
    display: none;
  }

.label-err {
    color: var(--err);
  }

/* ── Notifications subsection ── */
.notify-trigger-row {
    flex-wrap: wrap;
  }

.notify-controls {
    display: flex;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
  }

.notify-vol-range {
    width: 100px;
  }

</style>

<script lang="ts">
  /**
   * SwagCod — корневой шелл: сайдбар с сессиями, вкладки Chat/Trajectory,
   * зона ввода снизу. Киберпанк-тёмная эстетика, красный акцент.
   * Sidebar с сессиями, Chat/Trajectory tabs, input внизу.
   */
  import { onMount } from 'svelte'
  import { invoke } from '@tauri-apps/api/core'
  import { listen } from '@tauri-apps/api/event'
  import { open as openDialog, save as saveDialog } from '@tauri-apps/plugin-dialog'
  import { getCurrentWindow } from '@tauri-apps/api/window'
  import { bus, type WireEvent } from './lib/bus'
  import { Transcript } from './components'
  import BackgroundFX from './components/BackgroundFX.svelte'
  import Splash from './components/Splash.svelte'
  import SessionList from './components/SessionList.svelte'
  import Terminal from './components/Terminal.svelte'
  import FileTree from './components/FileTree.svelte'
  import ApprovalDialog from './components/ApprovalDialog.svelte'
  import Icon, { type IconName } from './components/Icon.svelte'
  /* Анимированный глаз логотипа: пользовательский gif, чёрный фон вырезается
     blend-mode screen (см. .eye img). */
  import logoGif from './assets/logo.gif'
  import { Transcript as TranscriptModel, type TranscriptItem } from './lib/transcript'
  import { translate, type StrKey } from './lib/strings'
  import { polylinePoints, barHeights, counterDeltas, groupByName } from './lib/charts'

  interface BuildInfo {
    version: string
    rustc: string
    target: string
    debug: boolean
  }

  let info = $state<BuildInfo | null>(null)
  let tauriAvailable = $state(true)
  let connected = $state(false)
  let currentSession = $state<string | null>(null)

  let models = $state<string[]>([])
  let modelName = $state(localStorage.getItem('swagcod-model') || 'fable-ultra-promax')
  let showSettings = $state(false)
  let settingsTab = $state<'general' | 'models' | 'providers' | 'plugins' | 'security' | 'mcp' | 'telemetry'>('general')
  let activeTab = $state<'chat' | 'trajectory' | 'terminal'>('chat')

  /* ────────────────────────────────────────────────────────────────
   * Внешний вид и права. Всё это реально применяется к DOM и
   * переживает перезапуск (localStorage), а не лежит мёртвым контролом.
   * ──────────────────────────────────────────────────────────────── */
  type Appearance = 'dark' | 'light' | 'contrast' | 'system'
  /** Значения совпадают с `ApprovalPolicy` в ядре (serde snake_case). */
  type PermissionMode = 'always' | 'on_dangerous' | 'never'
  type UiLang = 'ru' | 'en'

  function readNum(raw: string | null, min: number, max: number, fallback: number): number {
    const v = parseFloat(raw ?? '')
    if (Number.isNaN(v)) return fallback
    return Math.min(max, Math.max(min, v))
  }

  let appearance = $state<Appearance>(
    (localStorage.getItem('swagcod-appearance') as Appearance) || 'dark'
  )
  let systemLight = $state(false)
  let resolvedAppearance = $derived<'dark' | 'light' | 'contrast'>(
    appearance === 'system' ? (systemLight ? 'light' : 'dark') : appearance
  )
  /** Акцент текущей темы — стартовое значение круглой палитры. */
  let themeAccentHex = $derived(
    resolvedAppearance === 'light' ? '#0e9384' : resolvedAppearance === 'contrast' ? '#9b1830' : '#ff2d44'
  )
  let fontSize = $state(readNum(localStorage.getItem('swagcod-fontsize'), 10, 24, 13))
  let uiZoom = $state(readNum(localStorage.getItem('swagcod-zoom'), 0.6, 2, 1))

  /* Сила свечения: множитель альфы всех акцентных теней (--glow-k в :root).
     Три уровня в настройках; выбор персистится локально. */
  const GLOW_K = { soft: 0.45, normal: 1, strong: 1.9 } as const
  type GlowLevel = keyof typeof GLOW_K
  function readGlow(): GlowLevel {
    const v = localStorage.getItem('swagcod-glow')
    return v === 'soft' || v === 'strong' ? v : 'normal'
  }
  let glowLevel = $state<GlowLevel>(readGlow())
  let sidebarWidth = $state(readNum(localStorage.getItem('swagcod-sidebar-w'), 200, 560, 260))
  let sidebarCollapsed = $state(localStorage.getItem('swagcod-sidebar-collapsed') === '1')
  let resizing = $state(false)
  let permissionMode = $state<PermissionMode>(
    (localStorage.getItem('swagcod-perm') as PermissionMode) || 'on_dangerous'
  )
  let uiLang = $state<UiLang>((localStorage.getItem('swagcod-lang') as UiLang) || 'ru')
  let showModelPicker = $state(false)
  let showPermPicker = $state(false)
  let modelQuery = $state('')


  function t(key: StrKey): string {
    return translate(uiLang, key)
  }

  /* Описание модели: бэкенд отдаёт только id, поэтому способности
     выводим по имени — этого достаточно, чтобы выбрать осознанно. */
  interface ModelSpec {
    id: string
    ctx: string
    speed: 'fast' | 'balanced' | 'heavy'
    reasoning: boolean
    tools: boolean
    vision: boolean
  }

  function describeModel(id: string): ModelSpec {
    const low = id.toLowerCase()
    const has = (...parts: string[]): boolean => parts.some((p) => low.includes(p))
    const fast = has('flash', 'mini', 'lite', 'fast', 'haiku', 'nano')
    const heavy = has('ultra', 'promax', 'max', 'opus', 'pro')
    return {
      id,
      ctx: has('kimi', 'ultra', 'promax') ? '1M' : has('max', 'opus', 'gpt-6') ? '400k' : fast ? '128k' : '256k',
      speed: fast ? 'fast' : heavy ? 'heavy' : 'balanced',
      reasoning: has('gpt-6', 'deepseek', 'glm', 'ultra', 'promax', 'max', 'opus', 'kimi'),
      tools: !has('nano', 'lite'),
      vision: has('gpt-6', 'qwen', 'ultra', 'promax', 'kimi', 'gemini'),
    }
  }

  const modelSpecs = $derived(models.map((m) => describeModel(m)))
  const filteredModels = $derived(
    modelQuery.trim()
      ? modelSpecs.filter((m) => m.id.toLowerCase().includes(modelQuery.trim().toLowerCase()))
      : modelSpecs
  )
  const currentSpec = $derived(describeModel(modelName))

  function selectModel(id: string): void {
    modelName = id
    showModelPicker = false
    modelQuery = ''
  }

  const permSpecs = $derived<{ id: PermissionMode; icon: IconName; title: string; desc: string }[]>([
    { id: 'always', icon: 'shield', title: t('permAlways'), desc: t('permAlwaysDesc') },
    { id: 'on_dangerous', icon: 'scale', title: t('permDangerous'), desc: t('permDangerousDesc') },
    { id: 'never', icon: 'bolt', title: t('permNever'), desc: t('permNeverDesc') },
  ])

  let permStatus = $state<string | null>(null)

  /** Права — не косметика: значение уходит в ядро через set_approval_policy. */
  async function selectPermission(mode: PermissionMode): Promise<void> {
    permissionMode = mode
    showPermPicker = false
    try {
      await invoke('set_approval_policy', { policy: mode })
      permStatus = t('permApplied')
    } catch (err) {
      console.error('set_approval_policy failed:', err)
      permStatus = t('permFailed')
    }
    setTimeout(() => (permStatus = null), 2500)
  }

  function permSpec(id: PermissionMode): { id: PermissionMode; icon: IconName; title: string; desc: string } {
    return permSpecs.find((p) => p.id === id) ?? permSpecs[1]
  }

  function zoomIn(): void {
    uiZoom = Math.min(2, Math.round((uiZoom + 0.1) * 10) / 10)
  }
  function zoomOut(): void {
    uiZoom = Math.max(0.6, Math.round((uiZoom - 0.1) * 10) / 10)
  }
  function zoomReset(): void {
    uiZoom = 1
  }

  /* Перетаскивание правой границы сайдбара — то, чего не хватало. */
  function startSidebarResize(e: MouseEvent): void {
    e.preventDefault()
    resizing = true
    const startX = e.clientX
    const startW = sidebarWidth
    const move = (ev: MouseEvent): void => {
      sidebarWidth = Math.min(560, Math.max(200, startW + (ev.clientX - startX)))
    }
    const up = (): void => {
      resizing = false
      window.removeEventListener('mousemove', move)
      window.removeEventListener('mouseup', up)
      document.body.style.removeProperty('cursor')
      document.body.style.removeProperty('user-select')
    }
    window.addEventListener('mousemove', move)
    window.addEventListener('mouseup', up)
    document.body.style.cursor = 'col-resize'
    document.body.style.userSelect = 'none'
  }

  /* Применение темы/шрифта/масштаба к документу. */
  $effect(() => {
    const root = document.documentElement
    root.dataset.appearance = resolvedAppearance
    root.style.setProperty('--content-font-size', `${fontSize}px`)
    root.style.setProperty('--ui-zoom', String(uiZoom))
    root.style.setProperty('--row-estimate', `${Math.round(fontSize * 5.5)}px`)
  })

  $effect(() => {
    localStorage.setItem('swagcod-appearance', appearance)
    persistPref('appearance', appearance)
  })
  $effect(() => { localStorage.setItem('swagcod-fontsize', String(fontSize)) })
  $effect(() => { localStorage.setItem('swagcod-zoom', String(uiZoom)) })
  $effect(() => {
    document.documentElement.style.setProperty('--glow-k', String(GLOW_K[glowLevel]))
    localStorage.setItem('swagcod-glow', glowLevel)
  })
  $effect(() => { localStorage.setItem('swagcod-sidebar-w', String(Math.round(sidebarWidth))) })
  $effect(() => { localStorage.setItem('swagcod-sidebar-collapsed', sidebarCollapsed ? '1' : '0') })
  $effect(() => {
    localStorage.setItem('swagcod-perm', permissionMode)
    persistPref('perm', permissionMode)
  })
  $effect(() => {
    localStorage.setItem('swagcod-lang', uiLang)
    persistPref('lang', uiLang)
  })
  $effect(() => {
    localStorage.setItem('swagcod-model', modelName)
    persistPref('model', modelName)
  })

  const transcriptsBySession = new Map<string, TranscriptModel>()

  /* B-1: настройки дублируются в store ядра с debounce 500 мс — диск не
     дёргается на каждый чих студии тем, а перезапуск на другой машине
     профиля сможет их прочитать. localStorage остаётся для мгновенного
     применения до старта ядра. */
  const prefTimers = new Map<string, ReturnType<typeof setTimeout>>()
  function persistPref(key: string, value: string): void {
    const old = prefTimers.get(key)
    if (old) clearTimeout(old)
    prefTimers.set(
      key,
      setTimeout(() => {
        prefTimers.delete(key)
        invoke('set_pref', { key, value }).catch(() => {})
      }, 500),
    )
  }
  /** Указатель на транскрипт активной сессии (меняется при переключении). */
  let transcript: TranscriptModel = new TranscriptModel()
  /* Массив элементов НЕ $state: его мутирует модель транскрипции, а UI
     перерисовывается по revision. Иначе прокси Svelte пересоздавался бы на
     каждом батче и виртуализатор сбрасывал кэш высот — список дёргался.
     $state.raw: реактивна замена ссылки (смена сессии), без глубокого прокси. */
  let items = $state.raw<TranscriptItem[]>([])
  let revision = $state(0)
  let seq = $state(0)
  let eventsTotal = $state(0)
  let batchesTotal = $state(0)
  let turnsCount = $state(0)
  /** turn -> session: события стрима не несут session, несёт только turn_started. */
  const turnSession = new Map<string, string>()
  /** E-6: дочерние ходы суб-агентов — их turn_ended не трогает статус сессии. */
  const subTurns = new Set<string>()
  let sessionRefreshTick = $state(0)

  function transcriptFor(sessionId: string): TranscriptModel {
    let tr = transcriptsBySession.get(sessionId)
    if (!tr) {
      tr = new TranscriptModel()
      transcriptsBySession.set(sessionId, tr)
    }
    return tr
  }

  /** Куда отнести событие: своя сессия хода. Фолбэк в активный чат — только
      для ошибок без хода (например, отказ invoke): стрим без известной
      сессии выбрасывается, иначе чужой ход льётся в открытое окно. */
  function sessionOfEvent(ev: WireEvent): string | null {
    const k = ev.kind as { kind: string; data?: { session?: string; turn?: string } }
    const direct = k.data?.session
    if (direct) return direct
    const turn = k.data?.turn
    if (turn && turnSession.has(turn)) return turnSession.get(turn) ?? null
    if (k.kind === 'error') return currentSession
    return null
  }

  /** Переключение сессии: свой транскрипт, своя история с ядра. */
  function switchSession(id: string): void {
    if (id === currentSession) return
    currentSession = id
    chatBirth = false
    transcript = transcriptFor(id)
    items = transcript.items
    revision++
    /* Последняя область следует за пользователем: переключился в сессию
       проекта — «+» и авто-создание пойдут в ту же папку. */
    const cwd = sessionCwds[id]
    if (cwd) rememberCwd(cwd)
    void loadSessionHistory(id)
  }

  async function loadSessionHistory(id: string): Promise<void> {
    const tr = transcriptFor(id)
    if (tr.length > 0) return
    try {
      const evs = await invoke<WireEvent[]>('session_transcript', { sessionId: id })
      if (evs.length === 0) return
      for (const ev of evs) {
        const k = ev.kind as { kind: string; data?: { turn?: string; session?: string; parent?: string } }
        if (k.kind === 'turn_started' && k.data?.turn && k.data?.session) {
          turnSession.set(k.data.turn, k.data.session)
        }
      }
      tr.applyBatch(evs)
      if (currentSession === id) {
        items = tr.items
        revision++
      }
    } catch {
      // История недоступна (старое ядро): сессия просто пустая
    }
  }

  /* Рабочие области: пути cwd открытых сессий, как в референсном клиенте.
     Строка держит id сессии: граница песочницы в ядре — cwd сессии, и
     файловые команды ходят с sessionId, а не с хардкодом пути. */
  let showWorkspaces = $state(false)
  let workspaces = $state<{ cwd: string; id: string }[]>([])
  /* id сессии → её cwd: «+» открывает новую сессию в той же папке, где
     сидит пользователь, а не каждый раз в домашней. */
  let sessionCwds = $state<Record<string, string>>({})

  async function loadWorkspaces(): Promise<void> {
    try {
      const list = await invoke<{ id: string; cwd: string }[]>('list_sessions')
      const map: Record<string, string> = {}
      for (const s of list) map[s.id] = s.cwd
      sessionCwds = map
      const seen = new Set<string>()
      workspaces = list
        .filter((s) => (seen.has(s.cwd) ? false : (seen.add(s.cwd), true)))
        .map((s) => ({ cwd: s.cwd, id: s.id }))
    } catch {
      workspaces = []
    }
  }

  function openWorkspace(path: string, sessionId: string): void {
    invoke('open_in_explorer', { sessionId, path }).catch(() => {})
  }

  /* Провайдеры: каталог пресетов + custom, Fetch models с вопросом
     «сохранять или нет», активный провайдер для ходов. Ключи во фронт
     не прилетают — только has_key; ключ уходит лишь в save/fetch. */
  interface ProviderInfo {
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
  interface FetchedModel { id: string; label: string }
  let provState = $state<{ active: string; providers: ProviderInfo[] } | null>(null)
  let provBusy = $state(false)
  let provNote = $state('')
  let provFetching = $state('')
  let provKeys = $state<Record<string, string>>({})
  let provPreview = $state<{ id: string | null; label: string; base_url: string; flavor: string; typedKey: string; models: FetchedModel[] } | null>(null)
  let customLabel = $state('')
  let customBase = $state('')
  let customKey = $state('')
  let customFlavor = $state<'openai' | 'anthropic'>('openai')

  async function loadProviders(): Promise<void> {
    if (!tauriAvailable) return
    try {
      provState = await invoke<{ active: string; providers: ProviderInfo[] }>('providers_state')
    } catch (err) {
      provNote = `${t('provFailed')}: ${err}`
    }
  }

  /** Список моделей активного провайдера (старт + смена провайдера + refresh). */
  async function reloadModels(): Promise<void> {
    if (!tauriAvailable) return
    try {
      const raw = await invoke<{ data: { id: string }[] }>('list_models')
      models = raw.data.map((m) => m.id)
      if (models.length > 0 && !models.includes(modelName)) {
        modelName = models[0]
      }
    } catch {
      // Не критично
    }
  }

  async function useProvider(id: string): Promise<void> {
    provBusy = true
    try {
      await invoke('set_active_provider', { id })
      await loadProviders()
      await reloadModels()
      provNote = t('provActivated')
    } catch (err) {
      provNote = `${t('provFailed')}: ${err}`
    } finally {
      provBusy = false
    }
  }

  /** Fetch моделей: превью списком + вопрос «сохранять или нет», без тихой записи. */
  async function fetchModels(p: { id: string | null; label: string; base_url: string; flavor: string }, typedKey: string): Promise<void> {
    const pid = p.id ?? 'custom-new'
    provFetching = pid
    provNote = ''
    try {
      const arg: Record<string, unknown> = { baseUrl: p.base_url, flavor: p.flavor }
      if (p.id) arg.id = p.id
      if (typedKey.trim()) arg.key = typedKey
      const list = await invoke<FetchedModel[]>('fetch_provider_models', arg)
      provPreview = { id: p.id, label: p.label, base_url: p.base_url, flavor: p.flavor, typedKey, models: list }
    } catch (err) {
      provNote = `${t('provFailed')}: ${err}`
    } finally {
      provFetching = ''
    }
  }

  async function savePreview(): Promise<void> {
    if (!provPreview) return
    provBusy = true
    try {
      const pv = provPreview
      const arg: Record<string, unknown> = {
        label: pv.label,
        baseUrl: pv.base_url,
        flavor: pv.flavor,
        models: pv.models.map((m) => m.id),
      }
      if (pv.id) arg.id = pv.id
      else {
        if (!customLabel.trim() || !customBase.trim()) return
        arg.label = customLabel
        arg.baseUrl = customBase
      }
      // Ключ кладём в DPAPI только вместе с явным сохранением.
      if (pv.typedKey.trim()) arg.key = pv.typedKey
      const savedId = await invoke<string>('save_provider', arg)
      provPreview = null
      if (!pv.id) {
        customLabel = ''
        customBase = ''
        customKey = ''
      }
      await loadProviders()
      // Сохранили модели активного — сразу в селект.
      const st = provState
      if (st && st.providers.find((x) => x.id === savedId && x.id === st.active)) {
        await reloadModels()
      }
      provNote = t('provSaved')
    } catch (err) {
      provNote = `${t('provFailed')}: ${err}`
    } finally {
      provBusy = false
    }
  }

  async function deleteProvider(id: string): Promise<void> {
    provBusy = true
    try {
      await invoke('delete_provider', { id })
      await loadProviders()
      await reloadModels()
      provNote = t('provDeleted')
    } catch (err) {
      provNote = `${t('provFailed')}: ${err}`
    } finally {
      provBusy = false
    }
  }

  /* E-9: импорт сессий из DSH Desktop (кнопка в настройках General).
     Бэкенд идемпотентен: повторный импорт не плодит дубли. */
  let dshImportBusy = $state(false)
  let dshImportNote = $state('')
  async function runDshImport(): Promise<void> {
    if (dshImportBusy) return
    dshImportBusy = true
    dshImportNote = ''
    try {
      const r = await invoke<{ sessions: number; turns: number; messages: number; skipped: number; resumed: number; errors: string[] }>('import_dsh_sessions')
      if (r.sessions === 0 && r.skipped === 0 && r.resumed === 0) {
        dshImportNote = t('dshImportNone')
      } else {
        dshImportNote = t('dshImportDone')
          .replace('{s}', String(r.sessions))
          .replace('{t}', String(r.turns))
          .replace('{m}', String(r.messages))
        if (r.resumed > 0) dshImportNote += `, ${t('dshImportResumed')}: ${r.resumed}`
        if (r.skipped > 0) dshImportNote += `, ${t('dshImportSkipped')}: ${r.skipped}`
        if (r.errors.length > 0) dshImportNote += `, ${t('dshImportErrors')}: ${r.errors.length}`
      }
      if (r.sessions > 0 || r.resumed > 0) {
        sessionRefreshTick++
        void loadWorkspaces()
      }
    } catch (e) {
      dshImportNote = String(e)
    } finally {
      dshImportBusy = false
    }
  }

  /* E-3: MCP-серверы — реестр в настройках (prefs на бэкенде), живые
     статусы подключений, добавление одной строкой «команда с аргументами». */
  let mcpServers = $state<{ name: string; command: string; args: string[]; enabled: boolean; state: string; tools: number }[]>([])
  let mcpName = $state('')
  let mcpCommand = $state('')
  let mcpBusy = $state(false)
  let mcpNote = $state('')
  async function loadMcp(): Promise<void> {
    try {
      const r = await invoke<{ servers: { name: string; command: string; args: string[]; enabled: boolean; state: string; tools: number }[]; tools: string[] }>('mcp_list')
      mcpServers = r.servers
    } catch (e) {
      mcpServers = []
      mcpNote = String(e)
    }
  }
  async function addMcp(): Promise<void> {
    const name = mcpName.trim()
    const parts = mcpCommand.trim().split(/\s+/).filter(Boolean)
    if (!name || parts.length === 0) {
      mcpNote = t('mcpNeedBoth')
      return
    }
    mcpBusy = true
    mcpNote = ''
    try {
      const r = await invoke<{ name: string; tools: number; state: string }>('mcp_add', {
        name,
        command: parts[0],
        args: parts.slice(1),
      })
      mcpNote = `${r.name}: ${r.state}`
      mcpName = ''
      mcpCommand = ''
      await loadMcp()
    } catch (e) {
      mcpNote = String(e)
    } finally {
      mcpBusy = false
    }
  }
  async function removeMcp(name: string): Promise<void> {
    mcpNote = ''
    try {
      await invoke('mcp_remove', { name })
      await loadMcp()
    } catch (e) {
      mcpNote = String(e)
    }
  }

  /* E-4: JS-плагины sidecar — список инструментов, ошибки загрузки
     каталога plugins/*.js и горячая перезагрузка (полный рестарт sidecar). */
  let jsPlugTools = $state<{ name: string; description: string; file: string }[]>([])
  let jsPlugErrors = $state<string[]>([])
  let jsPlugDir = $state('')
  let jsPlugBusy = $state(false)
  async function loadJsPlugins(): Promise<void> {
    try {
      const r = await invoke<{ running: boolean; dir: string; tools: { name: string; description: string; file: string }[]; errors: string[] }>('js_plugins_list')
      jsPlugTools = r.tools
      jsPlugErrors = r.errors
      jsPlugDir = r.dir
    } catch (e) {
      jsPlugErrors = [String(e)]
    }
  }
  async function reloadJsPlugins(): Promise<void> {
    jsPlugBusy = true
    jsPlugErrors = []
    try {
      const r = await invoke<{ running: boolean; dir: string; tools: { name: string; description: string; file: string }[]; errors: string[] }>('js_plugins_reload')
      jsPlugTools = r.tools
      jsPlugErrors = r.errors
      jsPlugDir = r.dir
    } catch (e) {
      jsPlugErrors = [String(e)]
    } finally {
      jsPlugBusy = false
    }
  }

  /* Диалог подтверждения действия: ядро спрашивает событием
     approval_required, решение уходит командой respond_approval.
     Решение принимает человек здесь, но исполняет его ядро — не UI. */
  let approvalReq = $state<{ call_id: string; tool: string; summary: string } | null>(null)

  async function respondApproval(decision: 'approved' | 'denied'): Promise<void> {
    if (!approvalReq) return
    const callId = approvalReq.call_id
    approvalReq = null
    try {
      await invoke('respond_approval', { callId, decision })
    } catch {
      // ход уже завершился: канал закрыт, решать нечего
    }
  }

  /* ── B-7/B-8: вкладка «Безопасность» — DPAPI-ключ, права сессии, журнал ──
     Всё уже живёт в ядре (save_protected_key, set_session_approval_policy,
     approval_log); UI только показывает и дёргает команды. */
  let dpapiKey = $state('')
  let dpapiStored = $state(false)
  let dpapiStatus = $state<string | null>(null)
  let sessPolicy = $state<'global' | PermissionMode>('global')
  interface ApprovalRow {
    session_id: string
    turn_id: string
    call_id: string
    tool: string
    summary: string
    decision: string
    actor: string
    decided_ms: number
  }
  let approvalRows = $state<ApprovalRow[]>([])
  let approvalLogLoaded = $state(false)
  /* E-7: loopback REST API — порт и токен показываются здесь (та же
     модель доверия, что у DPAPI-ключа): API слушает только 127.0.0.1. */
  let httpApi = $state<{ enabled: boolean; port: number | null; token: string | null } | null>(null)

  async function refreshSecurity(): Promise<void> {
    if (!tauriAvailable) return
    try {
      const blob = await invoke<string | null>('get_pref', { key: 'api_key_dpapi' })
      dpapiStored = !!blob && blob.length > 0
    } catch {
      dpapiStored = false
    }
    try {
      httpApi = await invoke<{ enabled: boolean; port: number | null; token: string | null }>('http_api_status')
    } catch {
      httpApi = null // старое ядро без HTTP API
    }
    try {
      const list = await invoke<{ id: string; approval_policy?: string | null }[]>('list_sessions')
      const cur = list.find((s) => s.id === currentSession)
      sessPolicy = (cur?.approval_policy as PermissionMode | null | undefined) ?? 'global'
    } catch {
      // старое ядро без per-session политики: оставляем «как глобальные»
    }
  }

  async function copyHttpToken(): Promise<void> {
    if (!httpApi?.token) return
    try {
      await navigator.clipboard.writeText(httpApi.token)
      flashStatus(t('httpApiCopied'))
    } catch {
      flashStatus(t('httpApiCopyFail'))
    }
  }

  /* E-8: дашборд телеметрии — токены/день, латентность провайдера,
     лаги шины и отложенный с E-5 список фоновых задач. Данные — из
     таблицы metrics (сэмпл раз в минуту) и агрегации turns. */
  interface MetricPoint { name: string; ts_ms: number; value: number }
  interface SeriesPoint { ts_ms: number; value: number }
  interface TokenDay { day_ms: number; tokens: number; turns: number }
  interface TaskRow { id: string; kind: string; state: string; attempts: number; next_try_ms: number; last_error: string }
  let telTokens = $state<TokenDay[]>([])
  let telLatency = $state<SeriesPoint[]>([])
  let telLagE = $state<SeriesPoint[]>([])
  let telLagD = $state<SeriesPoint[]>([])
  let telTasks = $state<TaskRow[]>([])

  async function refreshTelemetry(): Promise<void> {
    if (!tauriAvailable) return
    const now = Date.now()
    try {
      const [tokens, metrics, tasks] = await Promise.all([
        invoke<TokenDay[]>('tokens_by_day', { sinceMs: now - 30 * 86400000 }),
        invoke<MetricPoint[]>('metrics_history', {
          names: ['provider_latency_ms', 'lagging_events', 'lagging_dropped'],
          sinceMs: now - 86400000,
        }),
        invoke<TaskRow[]>('tasks_list'),
      ])
      telTokens = tokens
      const g = groupByName(metrics)
      telLatency = g.get('provider_latency_ms') ?? []
      // Счётчики лагов накопительные — график строим по минутным дельтам.
      telLagE = counterDeltas(g.get('lagging_events') ?? [])
      telLagD = counterDeltas(g.get('lagging_dropped') ?? [])
      telTasks = tasks
    } catch {
      // старое ядро без телеметрии: графики остаются пустыми
    }
  }

  /* Обновления (rev37): проверка → скачивание → перезапуск. Прогресс
     приходит tauri-событием 'update-progress' из команды update_install;
     подписка живёт только на время скачивания. */
  interface UpdateCheckInfo {
    available: boolean
    version: string | null
    current: string
    notes: string | null
    error: string | null
  }
  let updateState = $state<'idle' | 'checking' | 'upToDate' | 'available' | 'downloading' | 'ready' | 'error'>('idle')
  let updateCurrent = $state('')
  let updateVersion = $state('')
  let updateNotes = $state('')
  let updateError = $state('')
  let updatePercent = $state(0)

  async function checkUpdate(): Promise<void> {
    updateState = 'checking'
    updateError = ''
    try {
      const info = await invoke<UpdateCheckInfo>('update_check')
      updateCurrent = info.current
      if (info.error) {
        updateState = 'error'
        updateError = info.error
      } else if (info.available) {
        updateState = 'available'
        updateVersion = info.version ?? ''
        updateNotes = info.notes ?? ''
      } else {
        updateState = 'upToDate'
        updateVersion = ''
        updateNotes = ''
      }
    } catch (e) {
      updateState = 'error'
      updateError = String(e)
    }
  }

  async function installUpdate(): Promise<void> {
    updateState = 'downloading'
    updatePercent = 0
    let unlistenProgress: (() => void) | undefined
    try {
      unlistenProgress = await listen<{ percent?: number | null }>('update-progress', (e) => {
        const p = e.payload?.percent
        if (typeof p === 'number') updatePercent = p
      })
      updateVersion = await invoke<string>('update_install')
      updateState = 'ready'
    } catch (e) {
      updateState = 'error'
      updateError = String(e)
    } finally {
      if (unlistenProgress) unlistenProgress()
    }
  }

  async function restartApp(): Promise<void> {
    try {
      await invoke('restart_app')
    } catch {
      // приложение уже перезапускается — обрабатывать нечего
    }
  }

  /* F-1: отмена задачи из таблицы телеметрии. Команда task_cancel честная:
     отменяет только queued, выполняющуюся отклоняет текстом ошибки. */
  async function cancelTask(id: string): Promise<void> {
    try {
      await invoke('task_cancel', { id })
      await refreshTelemetry()
    } catch (e) {
      flashStatus(String(e))
    }
  }

  function flashStatus(msg: string): void {
    dpapiStatus = msg
    setTimeout(() => (dpapiStatus = null), 2500)
  }

  async function saveDpapiKey(): Promise<void> {
    if (!dpapiKey.trim()) return
    try {
      await invoke('save_protected_key', { key: dpapiKey })
      dpapiKey = ''
      dpapiStored = true
      flashStatus(t('dpapiSaved'))
    } catch (err) {
      flashStatus(`${t('dpapiFailed')}: ${err}`)
    }
  }

  async function clearDpapiKey(): Promise<void> {
    try {
      await invoke('clear_protected_key')
      dpapiStored = false
      flashStatus(t('dpapiCleared'))
    } catch (err) {
      flashStatus(`${t('dpapiFailed')}: ${err}`)
    }
  }

  async function applySessPolicy(p: 'global' | PermissionMode): Promise<void> {
    sessPolicy = p
    if (!currentSession) return
    try {
      await invoke('set_session_approval_policy', {
        sessionId: currentSession,
        policy: p === 'global' ? null : p,
      })
      flashStatus(t('permApplied'))
    } catch (err) {
      flashStatus(`${t('permFailed')}: ${err}`)
    }
  }

  async function loadApprovalLog(): Promise<void> {
    try {
      approvalRows = await invoke<ApprovalRow[]>('approval_log', { sessionId: null, limit: 100 })
      approvalLogLoaded = true
    } catch {
      approvalRows = []
      approvalLogLoaded = true
    }
  }

  function fmtLogTime(ms: number): string {
    const d = new Date(ms)
    return `${d.toLocaleDateString()} ${d.toLocaleTimeString()}`
  }

  /* B-8: watchdog в UI — живой ход без событий 5 минут показываем как
     «подозрительно тихий», а не вечное «думает». Состояние живёт здесь,
     derived turnQuiet — ниже, рядом с `thinking` (порядок объявлений). */
  let lastLiveEventMs = $state(0)
  let quietTick = $state(0)
  const QUIET_MS = 5 * 60 * 1000
  $effect(() => {
    const id = setInterval(() => (quietTick++), 15000)
    return () => clearInterval(id)
  })
  /* Вкладка безопасности обновляет свои данные при открытии. */
  $effect(() => {
    if (showSettings && settingsTab === 'security') void refreshSecurity()
    if (showSettings && settingsTab === 'telemetry') void refreshTelemetry()
  })

  async function copyWorkspace(path: string): Promise<void> {
    try {
      await navigator.clipboard.writeText(path)
    } catch {
      // буфер недоступен — не критично
    }
  }

  /* Нативный выбор папки (диалог Windows через plugin-dialog): сессия
     стартует в выбранной папке, путь запоминается как последняя область.
     Отказ от выбора — не ошибка: просто ничего не происходит. */
  let browseError = $state('')

  async function browseFolder(): Promise<void> {
    browseError = ''
    try {
      const sel = await openDialog({ directory: true, multiple: false, title: t('browse') })
      const path = typeof sel === 'string' ? sel : ''
      if (!path) return
      rememberCwd(path)
      await createNewSession(path)
      showWorkspaces = false
    } catch (err) {
      browseError = err instanceof Error ? err.message : String(err)
    }
  }

  let searchQuery = $state('')
  let showSearch = $state(false)
  let filteredItems = $derived(
    searchQuery.trim()
      ? items.filter((item) =>
          (item.text || '').toLowerCase().includes(searchQuery.trim().toLowerCase())
        )
      : items
  )

  let showPalette = $state(false)
  let paletteQuery = $state('')
  let paletteIdx = $state(0)

  interface Command {
    label: string
    action: () => void
  }

  const commands: Command[] = [
    { label: 'Новая сессия', action: () => createNewSession() },
    { label: 'Очистить транскрипцию', action: () => { transcriptFor(currentSession ?? '').clear(); items = transcriptFor(currentSession ?? '').items; revision++; eventsTotal = 0; batchesTotal = 0 } },
    { label: 'Экспорт в markdown', action: () => exportTranscript() },
    { label: 'Экспорт диагностики', action: () => exportDiagnostics() },
    { label: 'Выбрать модель (Ctrl+M)', action: () => { showPermPicker = false; showModelPicker = true; modelQuery = '' } },
    { label: 'Права: спрашивать всегда', action: () => { void selectPermission('always') } },
    { label: 'Права: только опасные', action: () => { void selectPermission('on_dangerous') } },
    { label: 'Права: никогда не спрашивать', action: () => { void selectPermission('never') } },
    { label: 'Тема: тёмная', action: () => (appearance = 'dark') },
    { label: 'Тема: светлая', action: () => (appearance = 'light') },
    { label: 'Тема: контрастная', action: () => (appearance = 'contrast') },
    { label: 'Тема: системная', action: () => (appearance = 'system') },
    { label: 'Масштаб: увеличить (Ctrl++)', action: zoomIn },
    { label: 'Масштаб: уменьшить (Ctrl+-)', action: zoomOut },
    { label: 'Масштаб: сбросить (Ctrl+0)', action: zoomReset },
    { label: 'Панель: свернуть/развернуть (Ctrl+B)', action: () => (sidebarCollapsed = !sidebarCollapsed) },
    { label: 'Переключить эффекты', action: () => (effectsEnabled = !effectsEnabled) },
    { label: 'Фон: matrix rain', action: () => (bgMode = 'matrix') },
    { label: 'Фон: wireframe sphere', action: () => (bgMode = 'sphere') },
    { label: 'Симуляция 50 токенов', action: () => simulateStream(50) },
    { label: 'Симуляция 2k токенов', action: () => simulateStream(2000) },
    { label: 'Симуляция 20k токенов', action: () => simulateStream(20000) },
  ]

  const filteredCommands = $derived(
    paletteQuery.trim()
      ? commands.filter((c) => c.label.toLowerCase().includes(paletteQuery.trim().toLowerCase()))
      : commands
  )

  let bookmarks = $state<Set<string>>(
    new Set(JSON.parse(localStorage.getItem('swagcod-bookmarks') || '[]'))
  )

  function toggleBookmark(key: string): void {
    const next = new Set(bookmarks)
    if (next.has(key)) {
      next.delete(key)
    } else {
      next.add(key)
    }
    bookmarks = next
  }

  $effect(() => {
    localStorage.setItem('swagcod-bookmarks', JSON.stringify([...bookmarks]))
  })

  let effectsEnabled = $state(localStorage.getItem('swagcod-fx') !== 'off')
  let bgMode = $state<'sphere' | 'matrix'>(
    (localStorage.getItem('swagcod-bgmode') as 'sphere' | 'matrix') || 'sphere'
  )

  $effect(() => {
    localStorage.setItem('swagcod-fx', effectsEnabled ? 'on' : 'off')
  })
  $effect(() => {
    localStorage.setItem('swagcod-bgmode', bgMode)
  })

  /* ── Студия тем: свой акцент, тонировка и медиафон ─────────────────── */
  let customAccent = $state(localStorage.getItem('swagcod-accent') || '')
  let customTint = $state(localStorage.getItem('swagcod-tint') || '')
  let bgMediaName = $state(localStorage.getItem('swagcod-bgmedia') || '')
  let bgMediaUrl = $state('')
  let bgMediaKind = $state<'video' | 'image'>('image')
  let bgMediaBusy = $state(false)
  /* Ошибку сохранения показываем в настройках, а не глотаем в console.warn:
     иначе «гифка не видно» выглядит как магия. */
  let bgMediaError = $state('')

  /* Цвет и mime живут в lib/studio: чистые функции с тестами, а не инлайн. */
  import { BG_MIME, bgKindFor, hexToRgb, mixHex } from './lib/studio'

  /* Свой акцент перекрывает палитру темы целиком: кнопки, свечения, декор. */
  $effect(() => {
    const root = document.documentElement
    const rgb = customAccent ? hexToRgb(customAccent) : null
    if (rgb) {
      root.style.setProperty('--accent', customAccent)
      root.style.setProperty('--accent-hover', mixHex(customAccent, '#ffffff', 0.24))
      root.style.setProperty('--accent-dim', mixHex(customAccent, '#808080', 0.45))
      root.style.setProperty('--accent-rgb', `${rgb[0]}, ${rgb[1]}, ${rgb[2]}`)
      root.style.setProperty('--accent-glow', `rgba(${rgb[0]}, ${rgb[1]}, ${rgb[2]}, 0.28)`)
    } else {
      for (const p of ['--accent', '--accent-hover', '--accent-dim', '--accent-rgb', '--accent-glow']) {
        root.style.removeProperty(p)
      }
    }
    localStorage.setItem('swagcod-accent', customAccent)
  })

  $effect(() => {
    localStorage.setItem('swagcod-tint', customTint)
  })

  function bgBytesToUrl(name: string, bytes: Uint8Array): void {
    const ext = name.split('.').pop()?.toLowerCase() || ''
    const mime = BG_MIME[ext] || 'application/octet-stream'
    const blob = new Blob([bytes as BlobPart], { type: mime })
    if (bgMediaUrl) URL.revokeObjectURL(bgMediaUrl)
    bgMediaUrl = URL.createObjectURL(blob)
    bgMediaKind = bgKindFor(name)
  }

  async function applyStoredBgMedia(): Promise<void> {
    if (!bgMediaName) return
    try {
      const b64 = await invoke<string>('load_background', { name: bgMediaName })
      const bin = atob(b64)
      const bytes = new Uint8Array(bin.length)
      for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i)
      bgBytesToUrl(bgMediaName, bytes)
    } catch (err) {
      // Не глотаем: пользователь должен видеть, почему фон не вернулся.
      bgMediaError = err instanceof Error ? err.message : String(err)
      bgMediaName = ''
      localStorage.removeItem('swagcod-bgmedia')
    }
  }

  async function onPickBgMedia(e: Event): Promise<void> {
    const input = e.target as HTMLInputElement
    const file = input.files?.[0]
    if (!file) return
    bgMediaBusy = true
    bgMediaError = ''
    try {
      /* Читаем байты ДО сброса input.value: очистка списка файлов может
         отвязать File-объект, и фон молча не ставился. */
      const buf = new Uint8Array(await file.arrayBuffer())
      input.value = ''
      /* Сначала показываем: фон виден сразу из памяти, независимо от того,
         удастся ли положить файл на диск. Раньше инвок шёл первым, и при
         любой ошибке сохранения пользователь не видел вообще ничего. */
      const prev = bgMediaName
      const sessionName = `${Date.now()}-${file.name}`
      bgBytesToUrl(sessionName, buf)
      bgMediaName = sessionName
      try {
        let b64 = ''
        const chunk = 0x8000
        for (let i = 0; i < buf.length; i += chunk) {
          b64 += String.fromCharCode(...buf.subarray(i, i + chunk))
        }
        const name = await invoke<string>('save_background', {
          name: sessionName,
          dataB64: btoa(b64),
        })
        bgMediaName = name
        localStorage.setItem('swagcod-bgmedia', name)
        if (prev && prev !== name) {
          invoke('delete_background', { name: prev }).catch(() => {})
        }
      } catch (err) {
        // Фон работает до перезапуска; честно пишем, что на диск не лёг.
        bgMediaError = err instanceof Error ? err.message : String(err)
        console.warn('фон показан из памяти, но не сохранён', err)
      }
    } catch (err) {
      bgMediaError = err instanceof Error ? err.message : String(err)
      console.warn('не удалось поставить фон', err)
    } finally {
      bgMediaBusy = false
    }
  }

  async function clearBgMedia(): Promise<void> {
    const prev = bgMediaName
    bgMediaName = ''
    localStorage.removeItem('swagcod-bgmedia')
    if (bgMediaUrl) {
      URL.revokeObjectURL(bgMediaUrl)
      bgMediaUrl = ''
    }
    if (prev) {
      try {
        await invoke('delete_background', { name: prev })
      } catch {
        // файл мог уже исчезнуть — не страшно
      }
    }
  }

  let glitchActive = $state(false)
  let lastNotifiedTurn = $state<string | null>(null)

  $effect(() => {
    const lastItem = items[items.length - 1]
    if (
      lastItem &&
      lastItem.kind === 'status' &&
      lastItem.done &&
      lastItem.turn &&
      lastItem.turn !== lastNotifiedTurn &&
      !document.hasFocus()
    ) {
      lastNotifiedTurn = lastItem.turn
      if ('Notification' in window && Notification.permission === 'granted') {
        new Notification('SwagCod', { body: 'ход завершён' })
      }
    }
  })

  /* Занятость — ПО СЕССИЯМ, а не глобально: раньше один идущий ход держал
     «думает» на весь интерфейс, и реплика из второго чата уезжала в очередь
     первого, а потом и отправлялась в первый чат. Живой статус приходит из
     шины (liveStates), sendingSession живёт между invoke и turn_started. */
  let sendingSession = $state<string | null>(null)
  let inputText = $state('')
  /* Скрепка: выбранные в диалоге файлы ждут отправки чипами над полем
     ввода и уходят вместе со следующей репликой (в т.ч. из очереди). */
  let attachments = $state<string[]>([])

  function attachName(p: string): string {
    const parts = p.split(/[\\/]/)
    return parts[parts.length - 1] || p
  }

  async function attachFiles(): Promise<void> {
    if (!tauriAvailable) return
    try {
      const picked = await openDialog({ multiple: true, title: t('attachTitle') })
      if (!picked) return
      const list = Array.isArray(picked) ? picked : [picked]
      for (const p of list) {
        if (!attachments.includes(p)) attachments.push(p)
      }
    } catch (err) {
      console.error('attach failed:', err)
    }
  }

  function removeAttachment(index: number): void {
    attachments.splice(index, 1)
  }

  let creatingSession = $state(false)
  /* Анимация рождения чата: включается кнопкой создания, гаснет с первым
     сообщением. Пассивно на старте приложения не горит. */
  let chatBirth = $state(false)
  /* Сайdbar и шапка как в референсе: фильтр сессий, поповер группировки,
     меню папки сессии, панель Files и окно без системной рамки. */
  let sessionFilterOpen = $state(false)
  let sessionFilter = $state('')
  let groupPop = $state(false)
  let folderMenu = $state(false)
  let dotsMenu = $state(false)
  let filesOpen = $state(false)
  /* Панель Files как в референсе: вкладка «файлы» с деревом плюс вкладки
     открытых файлов; плюс добавляет файл нативным диалогом. */
  interface FileTab {
    id: string
    name: string
    path: string
    text: string
  }

  /** Ответ команды diff_against_head (B-4). */
  interface DiffLine {
    change: 'same' | 'added' | 'removed'
    old_no: number | null
    new_no: number | null
    text: string
  }
  interface DiffView {
    head_exists: boolean
    added: number
    removed: number
    lines: DiffLine[]
  }
  let filesTabs = $state<FileTab[]>([])
  let filesActive = $state<string>('files')
  /* B-4: живое дерево — счётчик растёт от file_changed, {#key} перемонтирует
     FileTree. Поиск по индексу с fuzzy-ранжированием и diff против git HEAD. */
  let treeRefresh = $state(0)
  let filesQuery = $state('')
  let filesResults = $state<string[] | null>(null)
  let filesDiff = $state<DiffView | null>(null)
  let filesDiffFor = $state<string | null>(null)
  let maximized = $state(false)
  let logNote = $state('')

  function readLocal(key: string, def: string): string {
    try {
      return localStorage.getItem(key) ?? def
    } catch {
      return def
    }
  }

  let groupMode = $state<'workspace' | 'list'>(
    readLocal('swagcod-group', 'workspace') === 'list' ? 'list' : 'workspace',
  )
  let orderMode = $state<'manual' | 'updated'>(
    readLocal('swagcod-order', 'updated') === 'manual' ? 'manual' : 'updated',
  )

  function setGroupMode(m: 'workspace' | 'list'): void {
    groupMode = m
    try {
      localStorage.setItem('swagcod-group', m)
    } catch {
      /* приватный режим: настройка живёт до перезапуска */
    }
  }

  function setOrderMode(m: 'manual' | 'updated'): void {
    orderMode = m
    try {
      localStorage.setItem('swagcod-order', m)
    } catch {
      /* приватный режим: настройка живёт до перезапуска */
    }
  }

  /** Папка текущей сессии в проводнике Windows. */
  async function openActiveFolder(): Promise<void> {
    const cwd = currentSession ? sessionCwds[currentSession] : undefined
    if (!cwd || !currentSession) return
    try {
      await invoke('open_in_explorer', { sessionId: currentSession, path: cwd })
    } catch (e) {
      logNote = String(e)
    }
  }

  /** Журнал сессии в файл через нативный диалог сохранения. */
  async function downloadSessionLog(): Promise<void> {
    if (!currentSession) return
    folderMenu = false
    dotsMenu = false
    try {
      const sel = await saveDialog({
        defaultPath: `swagcod-${currentSession.slice(0, 8)}.jsonl`,
        filters: [{ name: 'Журнал SwagCod', extensions: ['jsonl', 'json', 'txt', 'log'] }],
      })
      const path = typeof sel === 'string' ? sel : ''
      if (!path) return
      const saved = await invoke<string>('save_session_log', { sessionId: currentSession, path })
      logNote = saved
    } catch (e) {
      logNote = String(e)
    }
  }

  async function copySessionId(): Promise<void> {
    dotsMenu = false
    if (!currentSession) return
    try {
      await navigator.clipboard.writeText(currentSession)
      logNote = currentSession
    } catch {
      /* клипборд недоступен вне фокуса окна */
    }
  }

  /** Файл из дерева панели Files: открывается вкладкой внутри панели,
      текст читается через песочницу ядра (cwd сессии). */
  async function openFileFromTree(path: string, name: string): Promise<void> {
    if (!currentSession) return
    const existing = filesTabs.find((tb) => tb.path === path)
    if (existing) {
      filesActive = existing.id
      return
    }
    try {
      const text = await invoke<string>('read_file', { sessionId: currentSession, path })
      const tab: FileTab = { id: `ft-${filesTabs.length + 1}-${name}`, name, path, text }
      filesTabs = [...filesTabs, tab]
      filesActive = tab.id
    } catch (e) {
      logNote = String(e)
    }
  }

  /** Плюс на таб-баре: добавить файл вкладкой через нативный диалог. */
  async function addFileTab(): Promise<void> {
    if (!currentSession) return
    try {
      const sel = await openDialog({
        directory: false,
        multiple: false,
        defaultPath: filesRoot || undefined,
      })
      const path = typeof sel === 'string' ? sel : ''
      if (!path) return
      const name = path.split(/[\\/]/).pop() ?? path
      await openFileFromTree(path, name)
    } catch (e) {
      logNote = String(e)
    }
  }

  function closeFileTab(id: string): void {
    filesTabs = filesTabs.filter((tb) => tb.id !== id)
    if (filesActive === id) {
      filesActive = 'files'
      filesDiff = null
      filesDiffFor = null
    }
  }

  /* B-4: watch живой только пока панель файлов открыта — ресурсы не жгутся
     впустую. Смена сессии перезапускает наблюдателя. */
  $effect(() => {
    const sid = currentSession
    const on = filesOpen
    if (!sid || !on) return
    void invoke('start_watch', { sessionId: sid }).catch(() => {})
    return () => {
      void invoke('stop_watch', { sessionId: sid }).catch(() => {})
    }
  })

  /* B-4: fuzzy-поиск по индексу (.gitignore уважается). Debounce 300 мс:
     индекс кэшируется на бэке, но каждый чих всё равно лишний IPC. */
  let searchTimer: ReturnType<typeof setTimeout> | null = null
  $effect(() => {
    const q = filesQuery.trim()
    const sid = currentSession
    if (searchTimer) clearTimeout(searchTimer)
    if (!sid || !q) {
      filesResults = null
      return
    }
    searchTimer = setTimeout(() => {
      void invoke<string[]>('search_files', { sessionId: sid, query: q, limit: 50 })
        .then((r) => {
          filesResults = r
        })
        .catch(() => {
          filesResults = []
        })
    }, 300)
  })

  /** B-4: diff открытого файла против git HEAD. Повторный клик гасит diff. */
  async function toggleFileDiff(tabId: string): Promise<void> {
    const tab = filesTabs.find((tb) => tb.id === tabId)
    if (!tab || !currentSession) return
    if (filesDiffFor === tabId && filesDiff) {
      filesDiff = null
      filesDiffFor = null
      return
    }
    try {
      filesDiff = await invoke<DiffView>('diff_against_head', {
        sessionId: currentSession,
        path: tab.path,
      })
      filesDiffFor = tabId
    } catch (e) {
      logNote = String(e)
      filesDiff = null
      filesDiffFor = null
    }
  }

  /* Окно без системной рамки: свои кнопки свернуть/развернуть/закрыть. */
  const appWindow = getCurrentWindow()

  function winMinimize(): void {
    void appWindow.minimize()
  }

  async function winToggleMax(): Promise<void> {
    try {
      await appWindow.toggleMaximize()
      maximized = await appWindow.isMaximized()
    } catch {
      /* браузерный режим: окна нет */
    }
  }

  function winClose(): void {
    void appWindow.close()
  }
  let streamStartTime = $state<number | null>(null)
  let tokensPerSec = $state<number | null>(null)
  let turnTokens = $state(0)

  /* Живые статусы сессий прямо из событий шины: список сессий не ждёт
     опроса ядра, мозг/галочка/треугольник появляются в тот же кадр. */
  let liveStates = $state<Record<string, 'running' | 'ok' | 'fail'>>({})

  /** Нейронка думает именно в открытом сейчас чате. */
  let thinking = $derived(liveStates[currentSession ?? ''] === 'running')
  /* B-8: «подозрительно тихо» — производная от thinking и метки жизни:
     ход идёт, но событий нет дольше порога. quietTick лишь тикает, чтобы
     derived пересчитывался без внешних событий. */
  const turnQuiet = $derived(
    thinking && lastLiveEventMs > 0 && quietTick >= 0 && Date.now() - lastLiveEventMs > QUIET_MS
  )
  /** Отправка идёт именно в открытый сейчас чат. */
  let sending = $derived(sendingSession !== null && sendingSession === currentSession)
  /* Плашка рождения перечитывается на каждый revision: items мутируется
     моделью напрямую, и без триггера плашка залипала бы над живым чатом. */
  let birthVisible = $derived.by(() => {
    void revision
    return chatBirth && items.length === 0
  })
  /** Папка текущего чата для панели Files. */
  let filesRoot = $derived(currentSession ? (sessionCwds[currentSession] ?? '') : '')

  /* Очередь сообщений — своя у каждой сессии: пока нейронка думает, новые
     реплики не теряются и не льются в чужой разговор. Вложения (скрепка)
     travelling вместе с репликой: что прикрепил, то и уйдёт в ход. */
  interface QueueItem {
    text: string
    attach: string[]
  }
  let queues = $state<Record<string, QueueItem[]>>({})
  let queue = $derived(queues[currentSession ?? ''] ?? [])

  function queueFor(sid: string): QueueItem[] {
    let q = queues[sid]
    if (!q) {
      q = []
      queues[sid] = q
    }
    return q
  }

  function sessionBusy(sid: string | null): boolean {
    if (!sid) return false
    return liveStates[sid] === 'running' || sendingSession === sid
  }

  async function stopTurn(): Promise<void> {
    if (!currentSession) return
    try {
      await invoke('stop_turn', { sessionId: currentSession })
    } catch (err) {
      console.warn('stop_turn failed', err)
    }
    transcriptFor(currentSession).closeAll()
    items = transcriptFor(currentSession).items
    revision++
    if (sendingSession === currentSession) sendingSession = null
  }

  function removeFromQueue(index: number): void {
    const sid = currentSession
    if (!sid) return
    queueFor(sid).splice(index, 1)
  }

  /** Очередь конкретной сессии продолжает её разговор после конца хода. */
  async function drainQueue(sid: string): Promise<void> {
    const q = queues[sid]
    if (!q || q.length === 0 || sessionBusy(sid)) return
    const next = q.shift()
    if (next === undefined) return
    await sendText(next.text, sid, next.attach)
  }

  const bootStart = performance.now()
  let firstFrameMs = $state<number | null>(null)

  let unsubscribeBus: (() => void) | null = null

  function onBusBatch(batch: WireEvent[]): void {
    eventsTotal += batch.length
    batchesTotal++
    if (batch.length > 0) seq = batch[batch.length - 1].seq || seq

    const hasContent = batch.some(
      (e) => e.kind.kind === 'content' || e.kind.kind === 'reasoning'
    )
    if (hasContent && streamStartTime === null) {
      streamStartTime = performance.now()
      turnTokens = 0
    }
    for (const e of batch) {
      if (e.kind.kind === 'content' || e.kind.kind === 'reasoning') {
        turnTokens += Math.ceil((e.kind.data.text || '').length / 4)
      }
    }
    const hasEnd = batch.some((e) => e.kind.kind === 'turn_ended')
    if (hasEnd && streamStartTime !== null) {
      const elapsed = (performance.now() - streamStartTime) / 1000
      tokensPerSec = elapsed > 0 ? Math.round(turnTokens / elapsed) : null
      streamStartTime = null
    }

    // Раскладываем события по сессиям: чужой ход не попадает в активный чат,
    // а дописывается в свою транскрипцию молча.
    let touchedActive = 0
    let hasTurnEvent = false
    const bySession = new Map<string, WireEvent[]>()
    for (const e of batch) {
      if (e.kind.kind === 'turn_started' || e.kind.kind === 'turn_ended' || e.kind.kind === 'approval_required') {
        hasTurnEvent = true
      }
      if (e.kind.kind === 'turn_started') {
        const d = e.kind.data as { turn: string; session: string; parent?: string }
        turnSession.set(d.turn, d.session)
        /* E-6: ветка суб-агента стартует ВНУТРИ родительского хода:
           статус сессии не переключаем (иначе «ok» мигнуло бы посреди хода),
           но карту turn→session регистрируем — стрим ветки routится в чат. */
        if (d.parent) subTurns.add(d.turn)
        else liveStates[d.session] = 'running'
        // B-8: новый ход — часы тишины обнуляются.
        lastLiveEventMs = Date.now()
      }
      if (e.kind.kind === 'turn_ended') {
        const d = e.kind.data as { turn: string; session: string; ok: boolean }
        if (subTurns.has(d.turn)) {
          /* E-6: финиш ветки — закрываем только её строки; статус сессии,
             счётчик ходов и очередь принадлежат родительскому ходу. */
          subTurns.delete(d.turn)
          transcriptFor(d.session).closeTurn(d.turn)
        } else {
          liveStates[d.session] = d.ok ? 'ok' : 'fail'
          /* Строки хода переводим в готовый вид: волна «думает» гаснет. */
          transcriptFor(d.session).closeTurn(d.turn)
          /* Ход кончился — висячий запрос подтверждения не имеет смысла. */
          approvalReq = null
          if (sendingSession === d.session) sendingSession = null
          if (d.session === currentSession) turnsCount++
          /* Ход закончился — очередь ЭТОЙ сессии продолжает её разговор сама. */
          void drainQueue(d.session)
        }
      }
      if (e.kind.kind === 'approval_required') {
        const d = e.kind.data as { turn: string; call_id: string; tool: string; summary: string }
        approvalReq = { call_id: d.call_id, tool: d.tool, summary: d.summary }
      }
      if (e.kind.kind === 'file_changed') {
        /* B-4: файлы сессии изменились — дерево перерисовывается само,
           без опроса. Чужие сессии не трогаем. */
        const d = e.kind.data as { session: string; paths: string[] }
        if (d.session === currentSession) treeRefresh++
      }
      const sid = sessionOfEvent(e)
      if (!sid) continue
      // B-8: любое событие активной сессии — признак жизни для watchdog.
      if (sid === currentSession) lastLiveEventMs = Date.now()
      const arr = bySession.get(sid)
      if (arr) arr.push(e)
      else bySession.set(sid, [e])
    }
    for (const [sid, evs] of bySession) {
      const touched = transcriptFor(sid).applyBatch(evs)
      if (sid === currentSession && touched > 0) touchedActive += touched
    }
    if (touchedActive > 0) {
      items = transcriptFor(currentSession ?? '').items
      revision++
    }
    /* Список сессий обновляем только по ходовым событиям, а не таймером:
       меньше инвоков, меньше мусора, статусы всё равно свежие. */
    if (hasTurnEvent) sessionRefreshTick++
    if (firstFrameMs === null) firstFrameMs = performance.now() - bootStart
  }

  function simulateStream(tokenCount: number): void {
    let s = 1000
    const turnId = `sim-${Date.now()}`
    for (let i = 0; i < tokenCount; i++) {
      bus.ingest({
        seq: ++s,
        ts_ms: Date.now(),
        kind: { kind: 'content', data: { turn: turnId, text: `токен ${i} ` } },
      })
    }
    bus.ingest({
      seq: ++s,
      ts_ms: Date.now(),
      kind: { kind: 'turn_ended', data: { turn: turnId, session: 'sim', ok: true, reason: 'stop' } },
    })
    bus.flush()
  }

  function exportTranscript(): void {
    const md = items
      .map((item) => `**${item.kind}**: ${item.text || ''}`)
      .join('\n\n')
    const blob = new Blob([md], { type: 'text/markdown' })
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = `swagcod-${Date.now()}.md`
    a.click()
    URL.revokeObjectURL(url)
  }

  /** Открывает рабочую директорию текущей сессии в проводнике. */
  async function openConfigFile(): Promise<void> {
    if (workspaces.length === 0) await loadWorkspaces()
    const ws = workspaces.find((w) => w.id === currentSession) ?? workspaces[0]
    if (!ws) return
    try {
      await invoke('open_in_explorer', { sessionId: ws.id, path: ws.cwd })
    } catch (err) {
      console.error('open_in_explorer failed:', err)
    }
  }

  /** Диагностический снимок: сборка, сессия, шина, внешний вид + бекенд (B-8). */
  async function exportDiagnostics(): Promise<void> {
    /* B-8: бекенд-диагностика (Lagging-счётчики, живые ходы с возрастом и
       тишиной, uptime). В браузере без Tauri поля просто не будет. */
    let backend: unknown = null
    if (tauriAvailable) {
      try {
        backend = await invoke('get_diagnostics')
      } catch (err) {
        backend = { error: String(err) }
      }
    }
    const payload = {
      generatedAt: new Date().toISOString(),
      build: info,
      tauriAvailable,
      connected,
      session: currentSession,
      model: modelName,
      modelSpec: currentSpec,
      temperature: 'auto (provider default)',
      permissionMode,
      appearance: { requested: appearance, resolved: resolvedAppearance },
      ui: { fontSize, uiZoom, sidebarWidth, sidebarCollapsed, lang: uiLang, effectsEnabled, bgMode },
      counters: { eventsTotal, batchesTotal, turnsCount, items: items.length, seq },
      kinds: items.reduce<Record<string, number>>((acc, i) => {
        acc[i.kind] = (acc[i.kind] || 0) + 1
        return acc
      }, {}),
      backend,
    }
    const blob = new Blob([JSON.stringify(payload, null, 2)], { type: 'application/json' })
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = `swagcod-diagnostics-${Date.now()}.json`
    a.click()
    URL.revokeObjectURL(url)
  }

  /* cwd новых сессий: папка текущей сессии (пользователь жмёт «+», сидя
     в проекте — получает вторую сессию там же). Сессии нет — пустая
     строка, и ядро подставляет домашний каталог активного пользователя
     Windows (C:\Users\<юзер>): «пустой плюс» всегда означает дом, а не
     забытую в localStorage папку позавчерашней сессии. */
  function preferredCwd(): string {
    const cur = currentSession ? sessionCwds[currentSession] : undefined
    return cur ?? ''
  }

  function rememberCwd(cwd: string): void {
    try {
      localStorage.setItem('swagcod-cwd', cwd)
    } catch {
      // localStorage недоступен — не критично
    }
  }

  async function createNewSession(cwdOverride?: string): Promise<void> {
    creatingSession = true
    /* Защита от событийного аргумента: onclick={onCreate} передаёт клик
       первым параметром, и MouseEvent раньше уезжал в cwd — сессия не
       создавалась, а «второй чат» оказывался первым. */
    const cwd = typeof cwdOverride === 'string' && cwdOverride !== '' ? cwdOverride : undefined
    try {
      const brief = await invoke<{ id: string; cwd: string }>('create_session', {
        cwd: cwd ?? preferredCwd(),
        model: null,
      })
      currentSession = brief.id
      sessionCwds[brief.id] = brief.cwd
      rememberCwd(brief.cwd)
      transcript = transcriptFor(brief.id)
      items = transcript.items
      revision++
      sessionRefreshTick++
      /* Анимация рождения горит только у только что созданного чата и
         гаснет с первым сообщением. На старте приложения её нет. */
      chatBirth = true
      void loadWorkspaces()
    } catch (err) {
      console.error('create_session failed:', err)
    } finally {
      creatingSession = false
    }
  }

  /** Отправка одной реплики в конкретную сессию. Очередь вызывает её же
      после конца хода — и всегда в ту сессию, где реплику написали. */
  async function sendText(text: string, targetSession?: string, attach?: string[]): Promise<void> {
    if (!text) return
    let sid = targetSession ?? currentSession
    if (sid && sessionBusy(sid)) {
      /* Чат занят своим ходом: реплика ждёт в ЕГО очереди, а не в чужой. */
      queueFor(sid).push({ text, attach: attach ?? [] })
      return
    }
    if (sendingSession !== null) return
    try {
      if (!sid) {
        const brief = await invoke<{ id: string; cwd: string }>('create_session', {
          cwd: preferredCwd(),
          model: null,
        })
        sid = brief.id
        currentSession = brief.id
        sessionCwds[brief.id] = brief.cwd
        rememberCwd(brief.cwd)
        transcript = transcriptFor(brief.id)
      }
      sendingSession = sid
      /* Своё сообщение пишем в транскрипт сразу: до ответа модели чат уже
         показывает ход пользователя. */
      transcriptFor(sid).addUser(text)
      if (sid === currentSession) {
        items = transcriptFor(sid).items
        revision++
      }
      await invoke('start_turn', {
        sessionId: sid,
        message: text,
        model: modelName || null,
        // Температура — автоматом от провайдера: None не сериализуется в тело
        // запроса (crates/provider/src/types.rs), провайдер применяет свой дефолт.
        temperature: null,
        // Скрепка: текстовые вложения бекенд подшивает к сообщению сам.
        attachments: attach && attach.length > 0 ? attach : null,
      })
    } catch (err) {
      console.error('start_turn failed:', err)
      bus.ingest({
        seq: 0,
        ts_ms: Date.now(),
        kind: { kind: 'error', data: { turn: null, message: `ошибка: ${err}` } },
      })
      bus.flush()
      if (sendingSession === sid) sendingSession = null
    }
  }

  /** Enter/кнопка: если нейронка занята В ЭТОМ чате — реплика встаёт в его
      очередь, а не уходит вторым параллельным ходом и не в чужой чат. */
  async function sendMessage(): Promise<void> {
    const text = inputText.trim()
    if (!text) return
    inputText = ''
    /* Вложения уходят с этой репликой и список очищается — даже если она
       встанет в очередь: attach travels вместе с QueueItem. */
    const attach = attachments.length > 0 ? [...attachments] : undefined
    attachments = []
    if (sessionBusy(currentSession)) {
      if (currentSession) queueFor(currentSession).push({ text, attach: attach ?? [] })
      return
    }
    await sendText(text, undefined, attach)
  }

  onMount(() => {
    const onKeydown = (e: KeyboardEvent) => {
      if ((e.ctrlKey || e.metaKey) && e.key === 'k') {
        e.preventDefault()
        document.querySelector<HTMLTextAreaElement>('.chat-input')?.focus()
      }
      if ((e.ctrlKey || e.metaKey) && e.key === 'l') {
        e.preventDefault()
        transcript.clear()
        items = transcript.items
        revision++
        eventsTotal = 0
        batchesTotal = 0
      }
      if ((e.ctrlKey || e.metaKey) && e.key === 'f') {
        e.preventDefault()
        showSearch = !showSearch
        if (showSearch) {
          setTimeout(() => document.querySelector<HTMLInputElement>('.search-input')?.focus(), 50)
        }
      }
      if ((e.ctrlKey || e.metaKey) && e.key === 'p') {
        e.preventDefault()
        showPalette = !showPalette
        paletteQuery = ''
        paletteIdx = 0
        if (showPalette) {
          setTimeout(() => document.querySelector<HTMLInputElement>('.palette-input')?.focus(), 50)
        }
      }
      /* Ctrl+B — свернуть/развернуть панель, Ctrl+M — выбор модели,
         Ctrl+± / Ctrl+0 — масштаб интерфейса. */
      if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'b') {
        e.preventDefault()
        sidebarCollapsed = !sidebarCollapsed
      }
      if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'm') {
        e.preventDefault()
        showPermPicker = false
        showModelPicker = !showModelPicker
        modelQuery = ''
        if (showModelPicker) {
          setTimeout(() => document.querySelector<HTMLInputElement>('.model-filter')?.focus(), 50)
        }
      }
      if ((e.ctrlKey || e.metaKey) && (e.key === '+' || e.key === '=')) {
        e.preventDefault()
        zoomIn()
      }
      if ((e.ctrlKey || e.metaKey) && e.key === '-') {
        e.preventDefault()
        zoomOut()
      }
      if ((e.ctrlKey || e.metaKey) && e.key === '0') {
        e.preventDefault()
        zoomReset()
      }
      if (e.key === 'Escape') {
        if (showModelPicker) { showModelPicker = false; modelQuery = '' }
        if (showPermPicker) showPermPicker = false
        if (showSearch) { showSearch = false; searchQuery = '' }
        if (showPalette) { showPalette = false; paletteQuery = '' }
        if (showWorkspaces) showWorkspaces = false
        if (showSettings) showSettings = false
      }
    }
    window.addEventListener('keydown', onKeydown)

    /* Системная тема: следим за prefers-color-scheme, чтобы вариант
       «Системная» действительно реагировал на переключение в ОС. */
    const mq = window.matchMedia('(prefers-color-scheme: light)')
    systemLight = mq.matches
    const onScheme = (ev: MediaQueryListEvent): void => {
      systemLight = ev.matches
    }
    mq.addEventListener('change', onScheme)

    unsubscribeBus = bus.subscribe(onBusBatch)

    bus.start()
      .then(async () => {
        connected = true
        info = await invoke<BuildInfo>('build_info')
        /* Оверрайды из окружения (SWAGCOD_APPEARANCE / SWAGCOD_BG_MODE):
           киоск, скриншоты, CI. Неизвестные значения игнорируем. */
        try {
          const prefs = await invoke<Record<string, string>>('initial_prefs')
          if (prefs.appearance === 'dark' || prefs.appearance === 'light' || prefs.appearance === 'contrast' || prefs.appearance === 'system') {
            appearance = prefs.appearance
          }
          if (prefs.bg_mode === 'sphere' || prefs.bg_mode === 'matrix') {
            bgMode = prefs.bg_mode
          }
        } catch {
          // Не критично: работаем без оверрайдов
        }
        /* Пользовательский медиафон восстанавливаем из хранилища приложения. */
        void applyStoredBgMedia()
        /* Карта сессия→cwd нужна до первого «+»: новая сессия идёт в папку,
           где сидит пользователь, а не в домашнюю. */
        void loadWorkspaces()
        void reloadModels()
        /* Сохранённые права применяем к ядру при старте, иначе UI показывает
           одно, а сессия живёт с ApprovalPolicy::OnDangerous по умолчанию. */
        try {
          await invoke('set_approval_policy', { policy: permissionMode })
        } catch (err) {
          console.warn('set_approval_policy at startup failed', err)
        }
      })
      .catch((err) => {
        tauriAvailable = false
        console.warn('Tauri недоступен', err)
      })

    return () => {
      window.removeEventListener('keydown', onKeydown)
      mq.removeEventListener('change', onScheme)
      unsubscribeBus?.()
      bus.stop()
    }
  })

  if ('Notification' in window && Notification.permission === 'default') {
    Notification.requestPermission()
  }

  const onDrop = (e: DragEvent) => {
    e.preventDefault()
    const files = e.dataTransfer?.files
    if (files) {
      for (const f of files) {
        inputText += (inputText ? '\n' : '') + ((f as File & { path?: string }).path || f.name)
      }
    }
  }
  const onDragOver = (e: DragEvent) => e.preventDefault()
</script>

<Splash duration={2000} />
<BackgroundFX enabled={effectsEnabled} intensity={thinking ? 3 : 1} mode={bgMode} theme={resolvedAppearance} />

<!-- Пользовательский медиафон (gif / mp4 / картинка) лежит под контентом,
     поверх него — эффекты и тонировка. -->
{#if bgMediaUrl}
  {#if bgMediaKind === 'video'}
    <video class="bgmedia" src={bgMediaUrl} autoplay muted loop playsinline aria-hidden="true"></video>
  {:else}
    <img class="bgmedia" src={bgMediaUrl} alt="" aria-hidden="true" />
  {/if}
{/if}

<!-- Тонировка «под вкус и цвет»: мягкий цветовой veil поверх всего,
     blend-mode не даёт убить читаемость текста. -->
{#if customTint}
  <div class="tint-veil" style="background: {customTint}" aria-hidden="true"></div>
{/if}

{#if glitchActive}
  <div class="glitch-overlay" aria-hidden="true"></div>
{/if}

<!-- Подтверждение действия: ядро спрашивает событием approval_required,
     решение уходит командой respond_approval. Пока человек не решил,
     ход стоит: результат тулза в модель не попадает. -->
{#if approvalReq}
  <ApprovalDialog
    summary={approvalReq.summary}
    title={t('approvalTitle')}
    hint={t('approvalHint')}
    approveLabel={t('approve')}
    denyLabel={t('deny')}
    onRespond={respondApproval}
  />
{/if}

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
                            <td>{r.actor === 'user' ? t('actorUser') : t('actorSystem')}</td>
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
              {#if telTasks.length === 0}
                <div class="approval-empty">{t('telemetryEmpty')}</div>
              {:else}
                <div class="approval-scroll">
                  <table class="approval-table">
                    <thead>
                      <tr><th>id</th><th>kind</th><th>{t('taskColState')}</th><th>{t('taskColAttempts')}</th><th>{t('taskColNext')}</th><th>{t('taskColLog')}</th><th></th></tr>
                    </thead>
                    <tbody>
                      {#each telTasks as task (task.id)}
                        <tr>
                          <td>{task.id}</td>
                          <td>{task.kind}</td>
                          <td>{task.state}</td>
                          <td>{task.attempts}</td>
                          <td class="log-time">{task.next_try_ms ? new Date(task.next_try_ms).toLocaleString() : '—'}</td>
                          <td class="tel-err">{task.last_error}</td>
                          <td>
                            {#if task.state === 'queued'}
                              <button class="step-btn text-btn" onclick={() => void cancelTask(task.id)}>{t('taskCancel')}</button>
                            {:else}
                              —
                            {/if}
                          </td>
                        </tr>
                      {/each}
                    </tbody>
                  </table>
                </div>
              {/if}
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

{#if showPalette}
  <div class="palette-overlay" onclick={() => (showPalette = false)} role="presentation">
    <div
      class="palette"
      onclick={(e) => e.stopPropagation()}
      onkeydown={(e) => { if (e.key === 'Escape') showPalette = false }}
      role="dialog"
      aria-label="палитра команд"
      tabindex="-1"
    >
      <input
        type="text"
        class="palette-input"
        placeholder="команда…"
        bind:value={paletteQuery}
        onkeydown={(e) => {
          if (e.key === 'ArrowDown') { e.preventDefault(); paletteIdx = Math.min(paletteIdx + 1, filteredCommands.length - 1) }
          else if (e.key === 'ArrowUp') { e.preventDefault(); paletteIdx = Math.max(paletteIdx - 1, 0) }
          else if (e.key === 'Enter' && filteredCommands[paletteIdx]) {
            filteredCommands[paletteIdx].action()
            showPalette = false
            paletteQuery = ''
          }
        }}
        spellcheck="false"
      />
      <div class="palette-list" role="listbox">
        {#each filteredCommands as cmd, i (cmd.label)}
          <div
            class="palette-item"
            class:selected={i === paletteIdx}
            onclick={() => { cmd.action(); showPalette = false; paletteQuery = '' }}
            onmouseenter={() => (paletteIdx = i)}
            onkeydown={(e) => {
              if (e.key === 'Enter' || e.key === ' ') {
                e.preventDefault()
                cmd.action()
                showPalette = false
                paletteQuery = ''
              }
            }}
            role="option"
            tabindex="0"
            aria-selected={i === paletteIdx}
          >
            {cmd.label}
          </div>
        {/each}
        {#if filteredCommands.length === 0}
          <div class="palette-empty">ничего не найдено</div>
        {/if}
      </div>
    </div>
  </div>
{/if}

<main
  ondrop={onDrop}
  ondragover={onDragOver}
  class="dsh-layout"
  class:collapsed={sidebarCollapsed}
  class:resizing
  class:media-on={bgMediaUrl !== ''}
  style:grid-template-columns={sidebarCollapsed ? '0px 0px 1fr' : `${sidebarWidth}px 5px 1fr`}
>
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

  <!-- Ручка ресайза сайдбара: тянем мышью, двойной клик сбрасывает ширину.
       Всегда в разметке — иначе ломается число колонок grid. -->
  <button
    type="button"
    class="sidebar-resizer"
    class:active={resizing}
    aria-label={t('sidebarWidth')}
    title={t('sidebarWidth')}
    tabindex={sidebarCollapsed ? -1 : 0}
    onmousedown={(e) => { if (!sidebarCollapsed) startSidebarResize(e) }}
    ondblclick={() => (sidebarWidth = 260)}
    onkeydown={(e) => {
      if (e.key === 'ArrowLeft') { e.preventDefault(); sidebarWidth = Math.max(200, sidebarWidth - 16) }
      if (e.key === 'ArrowRight') { e.preventDefault(); sidebarWidth = Math.min(560, sidebarWidth + 16) }
      if (e.key === 'Enter') sidebarWidth = 260
    }}
  ></button>

  <div class="content" class:fx-matrix={effectsEnabled && bgMode === 'matrix'} class:fx-media={bgMediaUrl !== ''}>
    <div
      class="content-header"
      class:files-open={filesOpen}
      role="banner"
      data-tauri-drag-region
      ondblclick={() => void winToggleMax()}
    >
      <div class="session-title">
        {#if sidebarCollapsed}
          <button
            class="header-icon-btn"
            onclick={() => (sidebarCollapsed = false)}
            title={t('restore')}
            aria-label={t('restore')}
          ><Icon name="panel" size={14} /></button>
        {/if}
        <span class="title-text">{currentSession ? `сессия ${currentSession.slice(0, 8)}` : 'новая сессия'}</span>
        <span class="meta">{turnsCount} {t('turns')}</span>
        {#if tokensPerSec !== null}<span class="meta speed">{tokensPerSec} tok/s</span>{/if}
        {#if thinking}
          {#if turnQuiet}
            <span class="meta thinking-badge quiet">● {t('quietTurn')}</span>
          {:else}
            <span class="meta thinking-badge">● {t('thinking')}</span>
          {/if}
        {:else}
          <span class="meta">{t('standard')}</span>
        {/if}
      </div>
      <div class="header-row">
        <div class="tabs">
          <button class="tab" class:active={activeTab === 'chat'} onclick={() => (activeTab = 'chat')} role="tab" aria-selected={activeTab === 'chat'}>{t('chat')}</button>
          <button class="tab" class:active={activeTab === 'trajectory'} onclick={() => (activeTab = 'trajectory')} role="tab" aria-selected={activeTab === 'trajectory'}>{t('trajectory')}</button>
          <!-- B-2: терминал вернулся — вкладка поверх настоящих PTY-событий -->
          <button class="tab" class:active={activeTab === 'terminal'} onclick={() => (activeTab = 'terminal')} role="tab" aria-selected={activeTab === 'terminal'}>{t('terminal')}</button>
        </div>
        <div class="header-tools-right">
          <button
            class="header-icon-btn"
            title={t('searchTranscript')}
            aria-label={t('searchTranscript')}
            aria-pressed={showSearch}
            onclick={() => {
              showSearch = !showSearch
              activeTab = 'chat'
            }}
          ><Icon name="search" size={13} /></button>
          <div class="hmenu-wrap">
            <button
              class="header-icon-btn"
              title={t('sessionFolder')}
              aria-label={t('sessionFolder')}
              aria-expanded={folderMenu}
              onclick={() => {
                folderMenu = !folderMenu
                dotsMenu = false
              }}
            ><Icon name="folder" size={13} /><Icon name="chevron-down" size={9} /></button>
            {#if folderMenu}
              <div class="hmenu" role="menu">
                <button role="menuitem" onclick={() => { folderMenu = false; void openActiveFolder() }}>
                  <Icon name="external" size={12} /> {t('openFolder')}
                </button>
                <button role="menuitem" onclick={() => void downloadSessionLog()}>
                  <Icon name="download" size={12} /> {t('downloadLog')}
                </button>
              </div>
            {/if}
          </div>
          <div class="hmenu-wrap">
            <button
              class="header-icon-btn"
              title={t('more')}
              aria-label={t('more')}
              aria-expanded={dotsMenu}
              onclick={() => {
                dotsMenu = !dotsMenu
                folderMenu = false
              }}
            ><Icon name="dots" size={14} /></button>
            {#if dotsMenu}
              <div class="hmenu" role="menu">
                <button role="menuitem" onclick={() => void downloadSessionLog()}>
                  <Icon name="download" size={12} /> {t('downloadLog')}
                </button>
                <button role="menuitem" onclick={() => void copySessionId()}>
                  <Icon name="clipboard" size={12} /> {t('copyId')}
                </button>
              </div>
            {/if}
          </div>
          <button
            class="header-icon-btn"
            class:on={filesOpen}
            title={t('filesPanel')}
            aria-label={t('filesPanel')}
            aria-pressed={filesOpen}
            onclick={() => (filesOpen = !filesOpen)}
          ><Icon name="panel" size={14} /></button>
          <!-- Своё окно без системной рамки: кнопки управления окном
               нарисованы в стиле интерфейса, как в референсе. -->
          <div class="win-controls">
            <button class="win-btn" title={t('minimize')} aria-label={t('minimize')} onclick={winMinimize}>
              <Icon name="minimize" size={12} />
            </button>
            <button class="win-btn" title={t('maximize')} aria-label={t('maximize')} onclick={() => void winToggleMax()}>
              <Icon name={maximized ? 'restore' : 'maximize'} size={11} />
            </button>
            <button class="win-btn win-close" title={t('closeWin')} aria-label={t('closeWin')} onclick={winClose}>
              <Icon name="close" size={12} />
            </button>
          </div>
        </div>
        <!-- Зум и сворачивание панели уехали в настройки: в шапке они
             дублировали их и съедали место. Горячие клавиши остались:
             Ctrl++ / Ctrl+- / Ctrl+0 / Ctrl+B. -->
      </div>
    </div>

    <div class="content-body">
      <div class="content-main">
    <div class="transcript-area" class:fade={activeTab}>
      {#if activeTab === 'chat'}
        {#if showSearch}
          <div class="search-bar">
            <input
              type="text"
              placeholder="поиск по транскрипции…"
              bind:value={searchQuery}
              class="search-input"
              onkeydown={(e) => { if (e.key === 'Escape') { showSearch = false; searchQuery = '' } }}
            />
            <span class="search-count">{filteredItems.length} найдено</span>
          </div>
        {/if}
        <div class="transcript-wrap">
          {#if birthVisible}
            <!-- Первое сообщение-статус больше не печатается в чат: рождение
                 чата показывает анимация сборки, а первой строкой транскрипта
                 остаётся слово человека. -->
            <div class="chat-birth" aria-label={t('chatCreating')}>
              <div class="birth-lines">
                <span class="birth-line l1"></span>
                <span class="birth-line l2"></span>
                <span class="birth-line l3"></span>
              </div>
              <span class="birth-label">{t('chatCreating')}</span>
            </div>
          {/if}
          <Transcript items={filteredItems} {revision} {bookmarks} onToggleBookmark={toggleBookmark} />
        </div>
      {:else if activeTab === 'trajectory'}
        {@const trajectoryItems = items.filter(i => i.kind === 'tool_call' || i.kind === 'status')}
        {@const groupedByTurn = trajectoryItems.reduce((acc, item) => {
          const turn = item.turn || 'unknown'
          if (!acc[turn]) acc[turn] = []
          acc[turn].push(item)
          return acc
        }, {} as Record<string, typeof trajectoryItems>)}
        <div class="trajectory-wrap">
          <div class="trajectory-header">
            <span>{t('trajectoryTitle')}</span>
            <span class="meta">{turnsCount} {t('turns')}</span>
          </div>
          <div class="trajectory-list">
            {#each Object.entries(groupedByTurn) as [turn, turnItems] (turn)}
              {@const isSub = turn.includes('-sub-')}
              <div class="turn-group" class:turn-sub={isSub}>
                <div class="turn-header">
                  <span class="turn-icon">{isSub ? '↳' : '▶'}</span>
                  <span class="turn-name">{isSub ? `${t('subagentTurn')} ${turn.slice(-6)}` : `${t('turn')} ${turn.slice(0, 8)}`}</span>
                  <span class="turn-count">{turnItems.length} {t('events')}</span>
                </div>
                <div class="turn-items">
                  {#each turnItems as item (item.key)}
                    <div class="trajectory-item">
                      <span class="traj-icon">
                        <Icon name={item.kind === 'tool_call' ? 'wrench' : 'clipboard'} size={12} />
                      </span>
                      <span class="traj-name">{item.tool || item.kind}</span>
                      {#if item.elapsedMs !== undefined}
                        <span class="traj-time">{item.elapsedMs} мс</span>
                      {/if}
                    </div>
                  {/each}
                </div>
              </div>
            {:else}
              <div class="trajectory-empty">{t('noTurns')}</div>
            {/each}
          </div>
        </div>
      {:else}
        <!-- B-2: вкладка терминала: PTY живёт в ядре, компонент рисует
             операции ANSI-парсера из событий pty_output. -->
        <Terminal sessionId={currentSession} lang={uiLang} />
      {/if}
    </div>

    {#if activeTab !== 'terminal'}
    <div class="input-area">
      {#if thinking}
        <div class="stream-progress" aria-hidden="true">
          <div class="progress-bar"></div>
        </div>
      {/if}
      <!-- Клик мимо поповера закрывает его -->
      {#if showModelPicker || showPermPicker}
        <div
          class="popover-backdrop"
          onclick={() => { showModelPicker = false; showPermPicker = false }}
          role="presentation"
        ></div>
      {/if}

      {#if showPermPicker}
        <div class="popover perm-popover" role="dialog" aria-label={t('permTitle')}>
          <div class="popover-head">
            <span class="popover-title">{t('permTitle')}</span>
            <span class="popover-hint">ApprovalPolicy</span>
          </div>
          <div class="perm-list">
            {#each permSpecs as p (p.id)}
              <button
                class="perm-card"
                class:selected={p.id === permissionMode}
                onclick={() => selectPermission(p.id)}
              >
                <span class="perm-icon"><Icon name={p.icon} size={15} /></span>
                <span class="perm-text">
                  <span class="perm-name">{p.title}</span>
                  <span class="perm-desc">{p.desc}</span>
                </span>
                {#if p.id === permissionMode}<span class="perm-check"><Icon name="check" size={13} /></span>{/if}
              </button>
            {/each}
          </div>
        </div>
      {/if}

      {#if showModelPicker}
        <div class="popover model-popover" role="dialog" aria-label={t('modelTitle')}>
          <div class="popover-head">
            <span class="popover-title">{t('modelTitle')}</span>
            <span class="popover-count">{filteredModels.length}</span>
          </div>
          <input
            type="text"
            class="model-filter"
            placeholder={t('searchModels')}
            bind:value={modelQuery}
            spellcheck="false"
            onkeydown={(e) => { if (e.key === 'Escape') { showModelPicker = false; modelQuery = '' } }}
          />
          <div class="model-list" role="listbox">
            {#each filteredModels as m (m.id)}
              <button
                class="model-card"
                class:selected={m.id === modelName}
                role="option"
                aria-selected={m.id === modelName}
                onclick={() => selectModel(m.id)}
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
            {:else}
              <div class="popover-empty">
                {models.length === 0 ? t('noModels') : '—'}
              </div>
            {/each}
          </div>
          <div class="popover-foot">
            <div class="popover-foot-row">
              <span class="temp-auto" title={t('tempAutoDesc')}>{t('tempAuto')}: {t('auto')}</span>
              <button
                class="popover-link"
                onclick={() => { showModelPicker = false; showSettings = true; settingsTab = 'models' }}
              >{t('openModelSettings')} →</button>
            </div>
          </div>
        </div>
      {/if}

      {#if queue.length > 0}
        <div class="queue-strip" aria-label={t('queueTitle')}>
          <span class="queue-title">{t('queueTitle')}:</span>
          {#each queue as q, i (q.text + i)}
            <span class="queue-chip">
              <span class="queue-text" title={q.text}>{q.text}</span>
              <button
                class="queue-drop"
                title={t('studioMediaClear')}
                aria-label={t('studioMediaClear')}
                onclick={() => removeFromQueue(i)}
              ><Icon name="close" size={10} /></button>
            </span>
          {/each}
        </div>
      {/if}

      {#if attachments.length > 0}
        <div class="queue-strip" aria-label={t('attachTitle')}>
          <span class="queue-title"><Icon name="paperclip" size={11} /></span>
          {#each attachments as a, i (a + i)}
            <span class="queue-chip">
              <span class="queue-text" title={a}>{attachName(a)}</span>
              <button
                class="queue-drop"
                title={t('studioMediaClear')}
                aria-label={t('studioMediaClear')}
                onclick={() => removeAttachment(i)}
              ><Icon name="close" size={10} /></button>
            </span>
          {/each}
        </div>
      {/if}

      <div class="input-box">
        <textarea
          class="chat-input"
          placeholder={t('placeholder')}
          bind:value={inputText}
          onkeydown={(e) => {
            if (e.key === 'Enter' && !e.shiftKey) {
              e.preventDefault()
              sendMessage()
            }
          }}
          disabled={sending}
          rows="2"
        ></textarea>
        <div class="input-actions">
          <button class="action-btn" title={t('attachTitle')} aria-label={t('attachTitle')} onclick={() => void attachFiles()}><Icon name="paperclip" size={14} /></button>
          <button
            class="action-btn perm-btn"
            class:danger={permissionMode === 'never'}
            title={t('permTitle')}
            aria-label={t('permTitle')}
            aria-expanded={showPermPicker}
            onclick={() => { showModelPicker = false; showPermPicker = !showPermPicker }}
          ><Icon name={permSpec(permissionMode).icon} size={13} /> {permSpec(permissionMode).title}</button>
          <button
            class="model-selector"
            aria-expanded={showModelPicker}
            title={t('modelTitle')}
            onclick={() => { showPermPicker = false; showModelPicker = !showModelPicker; modelQuery = '' }}
          >{modelName} <Icon name="chevron-down" size={12} /></button>
          {#if thinking || sending}
            <button
              class="send-btn stop"
              onclick={stopTurn}
              title={t('stopTitle')}
              aria-label={t('stopTitle')}
            >
              <Icon name="stop" size={14} />
            </button>
          {:else}
            <button class="send-btn" onclick={sendMessage} disabled={!inputText.trim()} aria-label={t('send')}>
              <Icon name="send" size={15} />
            </button>
          {/if}
        </div>
      </div>
      {#if permStatus}
        <div class="perm-status" class:error={permStatus === t('permFailed')}>{permStatus}</div>
      {/if}
      {#if thinking}
        <div class="thinking-indicator">
          <span class="thinking">{t('thinking')}<span class="dots"></span></span>
        </div>
      {/if}
    </div>
    {/if}
      </div>

      <!-- Панель Files: дерево папки, в которой живёт чат. Открывается
           последней кнопкой шапки, как в референсе. Часть лейаута:
           контент уезжает влево, панель выезжает справа. -->
      {#if filesOpen}
        <aside class="files-panel" aria-label={t('filesPanel')}>
          <!-- Таб-бар как в референсе: вкладка дерева с крестиком, вкладки
               открытых файлов, плюс добавляет файл нативным диалогом. -->
          <div class="files-tabs" role="tablist" aria-label={t('filesPanel')}>
            <button
              class="ftab"
              class:active={filesActive === 'files'}
              role="tab"
              aria-selected={filesActive === 'files'}
              onclick={() => (filesActive = 'files')}
            >
              <Icon name="folder-open" size={12} />
              <span>{t('filesPanel')}</span>
              <span
                class="ftab-x"
                role="button"
                tabindex="0"
                title={t('closeFiles')}
                onclick={(e) => {
                  e.stopPropagation()
                  filesOpen = false
                }}
                onkeydown={(e) => {
                  if (e.key === 'Enter') {
                    e.stopPropagation()
                    filesOpen = false
                  }
                }}
              ><Icon name="close" size={10} /></span>
            </button>
            {#each filesTabs as tb (tb.id)}
              <button
                class="ftab"
                class:active={filesActive === tb.id}
                role="tab"
                aria-selected={filesActive === tb.id}
                title={tb.path}
                onclick={() => (filesActive = tb.id)}
              >
                <span>{tb.name}</span>
                <span
                  class="ftab-x"
                  role="button"
                  tabindex="0"
                  title={t('closeFiles')}
                  onclick={(e) => {
                    e.stopPropagation()
                    closeFileTab(tb.id)
                  }}
                  onkeydown={(e) => {
                    if (e.key === 'Enter') {
                      e.stopPropagation()
                      closeFileTab(tb.id)
                    }
                  }}
                ><Icon name="close" size={10} /></span>
              </button>
            {/each}
            <button class="ftab-add" title={t('tabAdd')} aria-label={t('tabAdd')} onclick={() => void addFileTab()}>
              <Icon name="plus" size={12} />
            </button>
          </div>
          <div class="files-path" title={filesRoot}>{filesRoot}</div>
          {#if filesActive === 'files'}
            <div class="files-search">
              <input
                type="text"
                value={filesQuery}
                oninput={(e) => (filesQuery = e.currentTarget.value)}
                placeholder={t('searchFiles')}
              />
            </div>
            {#if currentSession && filesRoot}
              {#if filesResults !== null}
                <div class="files-results">
                  {#each filesResults as r}
                    <button
                      class="files-result"
                      onclick={() => {
                        void openFileFromTree(r, r.split('/').pop() ?? r)
                        filesQuery = ''
                      }}
                    >{r}</button>
                  {:else}
                    <div class="files-empty">{t('searchNoResults')}</div>
                  {/each}
                </div>
              {:else}
                {#key treeRefresh}
                  <FileTree
                    root={filesRoot}
                    sessionId={currentSession}
                    onFileSelect={(p, n) => void openFileFromTree(p, n)}
                  />
                {/key}
              {/if}
            {:else}
              <div class="files-empty">{t('noSession')}</div>
            {/if}
          {:else}
            {@const tab = filesTabs.find((tb) => tb.id === filesActive)}
            {#if tab}
              <div class="file-tab-tools">
                <button class="diff-btn" onclick={() => void toggleFileDiff(tab.id)} title={t('diffGit')}>
                  Δ git{#if filesDiffFor === tab.id && filesDiff} · {filesDiff.head_exists ? `${filesDiff.added}+ ${filesDiff.removed}−` : t('diffNoHead')}{/if}
                </button>
              </div>
              {#if filesDiffFor === tab.id && filesDiff}
                <div class="diff-body" role="log" aria-live="polite">
                  {#each filesDiff.lines as dl}
                    <div class="diff-line {dl.change}">
                      <span class="diff-no">{dl.old_no ?? ''}</span>
                      <span class="diff-no">{dl.new_no ?? ''}</span>
                      <span class="diff-text">{dl.change === 'added' ? '+ ' : dl.change === 'removed' ? '− ' : '  '}{dl.text}</span>
                    </div>
                  {/each}
                </div>
              {:else}
                <pre class="file-tab-body">{tab.text}</pre>
              {/if}
            {/if}
          {/if}
        </aside>
      {/if}
    </div>
  </div>
</main>

<style>
  main.dsh-layout {
    position: relative;
    z-index: 1;
    display: grid;
    grid-template-columns: 260px 1fr;
    height: 100%;
    /* Прозрачно: иначе layout непрозрачным var(--bg) полностью закрывает
       BackgroundFX (сфера/матрица) и переключатель эффектов ничего не делает. */
    background: transparent;
    transition: all 0.15s;
  }

  main.dsh-layout:nth-child(odd):hover {
    background: rgba(var(--accent-rgb), calc(0.008 * var(--glow-k)));
    box-shadow: inset 0 0 20px rgba(var(--accent-rgb), calc(0.03 * var(--glow-k)));
  }

  main.dsh-layout:nth-child(even):hover {
    background: rgba(255, 255, 255, 0.008);
    box-shadow: inset 0 0 16px rgba(255, 255, 255, 0.02);
  }

  main.dsh-layout:hover {
    background: rgba(var(--accent-rgb), calc(0.005 * var(--glow-k)));
    box-shadow: inset 0 0 16px rgba(var(--accent-rgb), calc(0.02 * var(--glow-k)));
  }

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

  .new-session-btn:hover :global(.icon) {
    transform: rotate(90deg);
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

  .content {
    display: flex;
    flex-direction: column;
    min-height: 0;
    /* Полупрозрачно: фоновая сфера/матрица просвечивают, текст остаётся читаемым. */
    background: color-mix(in srgb, var(--bg) 60%, transparent);
    animation: content-in 0.3s ease-out 0.1s both;
    transition: all 0.15s;
  }

  /* Matrix-дождь плотнее сферы: контент прозрачнее, иначе дождь не читается. */
  .content.fx-matrix,
  .content.fx-media {
    background: color-mix(in srgb, var(--bg) 34%, transparent);
  }

  /* Пользовательский медиафон: кадр на всё окно, как в референсных плеерах —
     слегка размыт и приглушён, панели становятся полупрозрачными, сюжет
     читается сквозь интерфейс. Без blend-mode: на предках есть zoom/filter,
     и blend изолируется — слой просто исчезал. */
  .bgmedia {
    position: fixed;
    inset: 0;
    width: 100%;
    height: 100%;
    object-fit: cover;
    z-index: 0;
    opacity: 0.75;
    filter: blur(2px) brightness(0.62) saturate(1.2);
    pointer-events: none;
  }

  /* Когда медиафон включён, непрозрачных плиток не остаётся: иначе кадр
     виден только в щелях между панелями и выглядит как баг. Панели становятся
     стеклом: сюжет читается сквозь интерфейс, как в референсных плеерах.
     Плотность стекла — переменная темы: светлая не темнеет в тряпку. */
  .media-on .sidebar,
  .media-on .content,
  .media-on .content-header,
  .media-on .input-area,
  .media-on .input-box {
    background: color-mix(in srgb, var(--bg) var(--panel-glass), transparent);
    backdrop-filter: blur(14px) saturate(1.1);
  }

  /* Тонировка поверх всего: soft-light красит панели и текст одинаково
     мягко, читаемость не страдает. */
  .tint-veil {
    position: fixed;
    inset: 0;
    z-index: 999;
    pointer-events: none;
    mix-blend-mode: soft-light;
    opacity: 0.32;
  }

  .content:nth-child(odd):hover {
    background: rgba(var(--accent-rgb), calc(0.008 * var(--glow-k)));
    box-shadow: inset 0 0 20px rgba(var(--accent-rgb), calc(0.03 * var(--glow-k)));
  }

  .content:nth-child(even):hover {
    background: rgba(255, 255, 255, 0.008);
    box-shadow: inset 0 0 16px rgba(255, 255, 255, 0.02);
  }

  .content:hover {
    background: rgba(var(--accent-rgb), calc(0.005 * var(--glow-k)));
    box-shadow: inset 0 0 16px rgba(var(--accent-rgb), calc(0.02 * var(--glow-k)));
  }

  @keyframes content-in {
    from {
      opacity: 0;
    }
    to {
      opacity: 1;
    }
  }

  .content-header {
    padding: 12px 0 12px 20px;
    border-bottom: 1px solid var(--border);
    background: var(--surface-chrome);
    transition: background 0.15s;
    /* Шапка выше панели Files и её меню: поповеры шапки не режутся панелью. */
    position: relative;
    z-index: 70;
  }

  .content-header:nth-child(odd):hover {
    background: rgba(var(--accent-rgb), calc(0.04 * var(--glow-k)));
    box-shadow: 0 0 24px rgba(var(--accent-rgb), calc(0.08 * var(--glow-k)));
    border-bottom-color: var(--accent-dim);
  }

  .content-header:nth-child(even):hover {
    background: rgba(255, 255, 255, 0.04);
    box-shadow: 0 0 20px rgba(255, 255, 255, 0.06);
    border-bottom-color: var(--accent-dim);
  }

  .content-header:hover {
    background: rgba(var(--accent-rgb), calc(0.01 * var(--glow-k)));
    box-shadow: 0 0 16px rgba(var(--accent-rgb), calc(0.03 * var(--glow-k)));
  }

  .session-title {
    display: flex;
    align-items: center;
    gap: 12px;
    margin-bottom: 8px;
    transition: all 0.15s;
  }

  .session-title:nth-child(odd):hover {
    gap: 20px;
    background: rgba(var(--accent-rgb), calc(0.02 * var(--glow-k)));
    box-shadow: 0 0 16px rgba(var(--accent-rgb), calc(0.04 * var(--glow-k)));
    transform: translateY(-2px);
  }

  .session-title:nth-child(even):hover {
    gap: 18px;
    background: rgba(255, 255, 255, 0.02);
    box-shadow: 0 0 12px rgba(255, 255, 255, 0.03);
    transform: translateY(-1px);
  }

  .session-title:hover {
    gap: 14px;
  }

  .title-text {
    font-family: var(--mono);
    font-size: 13px;
    font-weight: 600;
    color: var(--text);
    transition: color 0.15s;
  }

  .title-text:nth-child(odd):hover {
    color: var(--accent-hover);
    text-shadow: 0 0 16px var(--accent-glow);
    transform: scale(1.02);
  }

  .title-text:nth-child(even):hover {
    color: var(--accent);
    text-shadow: 0 0 12px var(--accent-glow);
    transform: scale(1.01);
  }

  .session-title:hover .title-text {
    color: var(--accent);
    text-shadow: 0 0 8px var(--accent-glow);
  }

  .meta {
    font-family: var(--mono);
    font-size: 11px;
    color: var(--text-faint);
    transition: color 0.15s;
  }

  .meta:nth-child(odd):hover {
    color: var(--text);
    text-shadow: 0 0 8px rgba(255, 255, 255, 0.1);
    transform: scale(1.02);
  }

  .meta:nth-child(even):hover {
    color: var(--text-dim);
    text-shadow: 0 0 6px rgba(255, 255, 255, 0.08);
    transform: scale(1.01);
  }

  .session-title:hover .meta {
    color: var(--text-dim);
  }

  .meta.speed {
    color: var(--ok);
    transition: all 0.15s;
  }

  .speed:nth-child(odd):hover {
    text-shadow: 0 0 24px currentColor;
    transform: scale(1.08);
    filter: brightness(1.2);
  }

  .speed:nth-child(even):hover {
    text-shadow: 0 0 20px currentColor;
    transform: scale(1.05);
    filter: brightness(1.1);
  }

  .session-title:hover .meta.speed {
    text-shadow: 0 0 8px currentColor;
  }

  .meta.thinking-badge {
    color: var(--accent);
    animation: breathe 2s ease-in-out infinite;
    transition: all 0.15s;
  }

  .thinking-badge:nth-child(odd):hover {
    text-shadow: 0 0 24px var(--accent-glow);
    transform: scale(1.08);
  }

  .thinking-badge:nth-child(even):hover {
    text-shadow: 0 0 20px var(--accent-glow);
    transform: scale(1.05);
  }

  .session-title:hover .meta.thinking-badge {
    text-shadow: 0 0 8px var(--accent-glow);
  }

  /* B-8: «подозрительно тихо» — янтарный вместо акцентного, без пульса
     «думает»: ход не думает, он молчит, и это надо заметить. */
  .meta.thinking-badge.quiet {
    color: #ffb86b;
    animation: none;
  }

  /* B-7/B-8: вкладка «Безопасность» в настройках */
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

  .tel-err {
    max-width: 240px;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
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

  .tabs {
    display: flex;
    gap: 4px;
    transition: gap 0.15s;
  }

  .tabs:nth-child(odd):hover {
    gap: 12px;
    background: rgba(var(--accent-rgb), calc(0.02 * var(--glow-k)));
    box-shadow: 0 0 16px rgba(var(--accent-rgb), calc(0.04 * var(--glow-k)));
  }

  .tabs:nth-child(even):hover {
    gap: 10px;
    background: rgba(255, 255, 255, 0.02);
    box-shadow: 0 0 12px rgba(255, 255, 255, 0.03);
  }

  .tabs:hover {
    gap: 6px;
  }

  .tab {
    padding: 6px 16px;
    background: none;
    border: none;
    border-bottom: 2px solid transparent;
    color: var(--text-dim);
    font-family: var(--mono);
    font-size: 12px;
    cursor: pointer;
    position: relative;
    transition: all 0.2s;
  }

  .tab:nth-child(odd):hover {
    background: rgba(var(--accent-rgb), calc(0.08 * var(--glow-k)));
    box-shadow: 0 0 16px rgba(var(--accent-rgb), calc(0.12 * var(--glow-k)));
    transform: translateY(-2px);
  }

  .tab:nth-child(even):hover {
    background: rgba(255, 255, 255, 0.08);
    box-shadow: 0 0 12px rgba(255, 255, 255, 0.1);
    transform: translateY(-1px);
  }

  .tab:hover {
    color: var(--accent);
    background: rgba(var(--accent-rgb), calc(0.02 * var(--glow-k)));
    box-shadow: 0 0 12px rgba(var(--accent-rgb), calc(0.03 * var(--glow-k)));
  }

  .tab.active {
    color: var(--accent);
    border-bottom-color: var(--accent);
  }

  .tab.active:hover {
    background: rgba(var(--accent-rgb), calc(0.03 * var(--glow-k)));
    box-shadow: 0 0 16px rgba(var(--accent-rgb), calc(0.05 * var(--glow-k)));
  }

  .tab.active:nth-child(odd):hover {
    background: rgba(var(--accent-rgb), calc(0.15 * var(--glow-k)));
    box-shadow: 0 0 20px rgba(var(--accent-rgb), calc(0.25 * var(--glow-k)));
    transform: translateY(-3px);
  }

  .tab.active:nth-child(even):hover {
    background: rgba(var(--accent-rgb), calc(0.12 * var(--glow-k)));
    box-shadow: 0 0 16px rgba(var(--accent-rgb), calc(0.22 * var(--glow-k)));
    transform: translateY(-2px);
  }

  .tab.active::after {
    content: '';
    position: absolute;
    bottom: -2px;
    left: 0;
    right: 0;
    height: 2px;
    background: var(--accent);
    animation: tab-underline 0.3s ease-out;
  }

  @keyframes tab-underline {
    from {
      transform: scaleX(0);
    }
    to {
      transform: scaleX(1);
    }
  }

  .tab:disabled {
    opacity: 0.4;
    cursor: not-allowed;
  }

  .transcript-area {
    flex: 1;
    display: flex;
    flex-direction: column;
    min-height: 0;
    animation: fade-in 0.2s ease-out;
    transition: background 0.15s;
  }

  .transcript-area:hover {
    background: rgba(255, 255, 255, 0.005);
  }

  /* Рождение чата: скелет-строки собираются и дышат свечением, пока
     транскрипт пуст. Служебное «сессия создана» в чат не печатается. */
  .chat-birth {
    display: flex;
    flex-direction: column;
    align-items: center;
    justify-content: center;
    gap: 14px;
    padding: 48px 24px;
    animation: fade-in 0.25s ease-out;
  }
  .birth-lines {
    display: flex;
    flex-direction: column;
    gap: 8px;
    width: min(420px, 70%);
  }
  .birth-line {
    height: 8px;
    border-radius: 4px;
    background: linear-gradient(
      90deg,
      rgba(var(--accent-rgb), calc(0.05 * var(--glow-k))) 0%,
      rgba(var(--accent-rgb), calc(0.22 * var(--glow-k))) 50%,
      rgba(var(--accent-rgb), calc(0.05 * var(--glow-k))) 100%
    );
    background-size: 220% 100%;
    animation: birth-sheen 1.6s ease-in-out infinite;
  }
  .birth-line.l1 { width: 100%; }
  .birth-line.l2 { width: 78%; animation-delay: 0.18s; }
  .birth-line.l3 { width: 55%; animation-delay: 0.36s; }
  @keyframes birth-sheen {
    0% { background-position: 120% 0; opacity: 0.5; }
    50% { background-position: 0% 0; opacity: 1; }
    100% { background-position: -120% 0; opacity: 0.5; }
  }
  .birth-label {
    font-family: var(--mono);
    font-size: 11px;
    letter-spacing: 0.08em;
    text-transform: uppercase;
    color: var(--text-faint);
    animation: birth-pulse 1.6s ease-in-out infinite;
  }
  @keyframes birth-pulse {
    0%, 100% { opacity: 0.45; }
    50% { opacity: 1; text-shadow: 0 0 10px rgba(var(--accent-rgb), calc(0.4 * var(--glow-k))); }
  }

  @keyframes fade-in {
    from { opacity: 0; }
    to { opacity: 1; }
  }

  .search-bar {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 8px 20px;
    background: rgba(20, 20, 25, 0.9);
    border-bottom: 1px solid var(--border);
    animation: slide-down 0.2s ease-out;
    transition: all 0.15s;
  }

  .search-bar:nth-child(odd):hover {
    background: rgba(var(--accent-rgb), calc(0.08 * var(--glow-k)));
    box-shadow: 0 0 20px rgba(var(--accent-rgb), calc(0.12 * var(--glow-k)));
    border-color: var(--accent-dim);
    transform: translateY(-2px);
  }

  .search-bar:nth-child(even):hover {
    background: rgba(var(--accent-rgb), calc(0.06 * var(--glow-k)));
    box-shadow: 0 0 16px rgba(var(--accent-rgb), calc(0.1 * var(--glow-k)));
    border-color: var(--accent-dim);
    transform: translateY(-1px);
  }

  .search-bar:hover {
    background: rgba(var(--accent-rgb), calc(0.02 * var(--glow-k)));
    box-shadow: 0 0 12px rgba(var(--accent-rgb), calc(0.05 * var(--glow-k)));
  }

  @keyframes slide-down {
    from {
      opacity: 0;
      transform: translateY(-8px);
    }
    to {
      opacity: 1;
      transform: translateY(0);
    }
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

  .search-count {
    font-family: var(--mono);
    font-size: 11px;
    color: var(--text-faint);
    transition: color 0.15s;
  }

  .search-count:nth-child(odd):hover {
    color: var(--accent-hover);
    text-shadow: 0 0 12px var(--accent-glow);
    transform: scale(1.05);
  }

  .search-count:nth-child(even):hover {
    color: var(--accent);
    text-shadow: 0 0 10px var(--accent-glow);
    transform: scale(1.03);
  }

  .search-count:hover {
    color: var(--accent);
  }

  .transcript-wrap {
    flex: 1;
    min-height: 0;
    transition: background 0.15s;
  }

  .transcript-wrap:nth-child(odd):hover {
    background: rgba(var(--accent-rgb), calc(0.005 * var(--glow-k)));
  }

  .transcript-wrap:nth-child(even):hover {
    background: rgba(255, 255, 255, 0.005);
  }

  .transcript-wrap:nth-child(odd):hover {
    background: rgba(var(--accent-rgb), calc(0.02 * var(--glow-k)));
    box-shadow: inset 0 0 24px rgba(var(--accent-rgb), calc(0.04 * var(--glow-k)));
    border-radius: 8px;
  }

  .transcript-wrap:nth-child(even):hover {
    background: rgba(255, 255, 255, 0.02);
    box-shadow: inset 0 0 20px rgba(255, 255, 255, 0.03);
    border-radius: 6px;
  }

  .transcript-wrap:hover {
    background: rgba(255, 255, 255, 0.005);
  }

  .trajectory-wrap {
    flex: 1;
    display: flex;
    flex-direction: column;
    min-height: 0;
    transition: background 0.15s;
  }

  .trajectory-wrap:nth-child(odd):hover {
    background: rgba(var(--accent-rgb), calc(0.02 * var(--glow-k)));
    box-shadow: inset 0 0 24px rgba(var(--accent-rgb), calc(0.04 * var(--glow-k)));
    border-radius: 8px;
  }

  .trajectory-wrap:nth-child(even):hover {
    background: rgba(255, 255, 255, 0.02);
    box-shadow: inset 0 0 20px rgba(255, 255, 255, 0.03);
    border-radius: 6px;
  }

  .trajectory-wrap:hover {
    background: rgba(var(--accent-rgb), calc(0.005 * var(--glow-k)));
    box-shadow: inset 0 0 16px rgba(var(--accent-rgb), calc(0.02 * var(--glow-k)));
  }

  .trajectory-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    padding: 12px 20px;
    border-bottom: 1px solid var(--border);
    font-family: var(--mono);
    font-size: 12px;
    color: var(--text-dim);
    transition: color 0.15s;
  }

  .trajectory-header:nth-child(odd):hover {
    background: rgba(var(--accent-rgb), calc(0.06 * var(--glow-k)));
    box-shadow: 0 0 24px rgba(var(--accent-rgb), calc(0.1 * var(--glow-k)));
    transform: translateY(-2px);
  }

  .trajectory-header:nth-child(even):hover {
    background: rgba(255, 255, 255, 0.06);
    box-shadow: 0 0 20px rgba(255, 255, 255, 0.08);
    transform: translateY(-1px);
  }

  .trajectory-header:hover {
    color: var(--accent);
    background: rgba(var(--accent-rgb), calc(0.01 * var(--glow-k)));
    box-shadow: 0 0 16px rgba(var(--accent-rgb), calc(0.03 * var(--glow-k)));
  }

  .trajectory-list {
    flex: 1;
    overflow-y: auto;
    padding: 12px 20px;
    transition: background 0.15s;
  }

  .trajectory-list:nth-child(odd):hover {
    background: rgba(var(--accent-rgb), calc(0.02 * var(--glow-k)));
    box-shadow: inset 0 0 20px rgba(var(--accent-rgb), calc(0.03 * var(--glow-k)));
    border-radius: 8px;
  }

  .trajectory-list:nth-child(even):hover {
    background: rgba(255, 255, 255, 0.02);
    box-shadow: inset 0 0 16px rgba(255, 255, 255, 0.02);
    border-radius: 6px;
  }

  .trajectory-list:hover {
    background: rgba(255, 255, 255, 0.005);
  }

  .trajectory-item {
    display: flex;
    align-items: center;
    gap: 10px;
    padding: 10px 12px;
    border-bottom: 1px solid var(--border);
    font-family: var(--mono);
    font-size: 12px;
    transition: all 0.15s;
  }

  .trajectory-item:nth-child(odd):hover {
    background: rgba(var(--accent-rgb), calc(0.1 * var(--glow-k)));
    transform: translateX(8px);
    box-shadow: 0 0 24px rgba(var(--accent-rgb), calc(0.18 * var(--glow-k)));
    border-left-color: var(--accent);
  }

  .trajectory-item:nth-child(even):hover {
    background: rgba(255, 255, 255, 0.1);
    transform: translateX(6px);
    box-shadow: 0 0 20px rgba(255, 255, 255, 0.15);
    border-left-color: var(--accent-dim);
  }

  .trajectory-item:hover {
    background: rgba(var(--accent-rgb), calc(0.04 * var(--glow-k)));
    border-left-color: var(--accent);
    transform: translateX(2px);
    box-shadow: 0 0 12px rgba(var(--accent-rgb), calc(0.08 * var(--glow-k)));
  }

  .traj-icon {
    font-size: 14px;
    transition: all 0.15s;
  }

  .traj-icon:nth-child(odd):hover {
    transform: scale(1.6) rotate(10deg);
    filter: drop-shadow(0 0 16px var(--accent-glow));
  }

  .traj-icon:nth-child(even):hover {
    transform: scale(1.5) rotate(-10deg);
    filter: drop-shadow(0 0 12px var(--accent-glow));
  }

  .trajectory-item:hover .traj-icon {
    transform: scale(1.2);
    filter: drop-shadow(0 0 6px var(--accent-glow));
  }

  .traj-name {
    color: var(--text);
    flex: 1;
    transition: color 0.15s;
  }

  .traj-name:nth-child(odd):hover {
    color: var(--accent-hover);
    text-shadow: 0 0 12px var(--accent-glow);
    transform: scale(1.02);
  }

  .traj-name:nth-child(even):hover {
    color: var(--accent);
    text-shadow: 0 0 10px var(--accent-glow);
    transform: scale(1.01);
  }

  .trajectory-item:hover .traj-name {
    color: var(--accent);
    text-shadow: 0 0 6px var(--accent-glow);
  }

  .traj-time {
    color: var(--text-faint);
    font-size: 11px;
    transition: color 0.15s;
  }

  .traj-time:nth-child(odd):hover {
    color: var(--text);
    text-shadow: 0 0 12px rgba(255, 255, 255, 0.15);
    transform: scale(1.05);
  }

  .traj-time:nth-child(even):hover {
    color: var(--text-dim);
    text-shadow: 0 0 10px rgba(255, 255, 255, 0.12);
    transform: scale(1.03);
  }

  .trajectory-item:hover .traj-time {
    color: var(--text-dim);
  }

  .trajectory-empty {
    padding: 40px;
    text-align: center;
    color: var(--text-faint);
    font-family: var(--mono);
    font-size: 12px;
  }

  .turn-group {
    margin-bottom: 16px;
    border: 1px solid var(--border);
    border-radius: 8px;
    overflow: hidden;
    transition: all 0.2s;
  }

  /* E-6: ветка суб-агента — с отступом и чертой слева, читается как
     вложенность под родительским ходом. */
  .turn-sub {
    margin-left: 20px;
    border-left: 2px solid var(--accent-dim);
  }

  .turn-group:nth-child(odd):hover {
    border-color: var(--accent);
    box-shadow: 0 0 32px rgba(var(--accent-rgb), calc(0.25 * var(--glow-k)));
    transform: translateY(-4px) scale(1.01);
    background: rgba(var(--accent-rgb), calc(0.02 * var(--glow-k)));
  }

  .turn-group:nth-child(even):hover {
    border-color: var(--accent-dim);
    box-shadow: 0 0 24px rgba(var(--accent-rgb), calc(0.2 * var(--glow-k)));
    transform: translateY(-3px) scale(1.005);
    background: rgba(255, 255, 255, 0.02);
  }

  .turn-group:hover {
    border-color: var(--accent-dim);
    box-shadow: 0 0 16px rgba(var(--accent-rgb), calc(0.1 * var(--glow-k)));
    transform: translateY(-1px);
  }

  .turn-header {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 10px 12px;
    background: var(--surface-inset);
    border-bottom: 1px solid var(--border);
    font-family: var(--mono);
    font-size: 12px;
    transition: background 0.15s;
  }

  .turn-header:nth-child(odd):hover {
    background: rgba(var(--accent-rgb), calc(0.1 * var(--glow-k)));
    box-shadow: 0 0 24px rgba(var(--accent-rgb), calc(0.15 * var(--glow-k)));
    transform: translateY(-3px);
    border-bottom-color: var(--accent);
  }

  .turn-header:nth-child(even):hover {
    background: rgba(255, 255, 255, 0.1);
    box-shadow: 0 0 20px rgba(255, 255, 255, 0.12);
    transform: translateY(-2px);
    border-bottom-color: var(--accent-dim);
  }

  .turn-header:hover {
    background: rgba(var(--accent-rgb), calc(0.02 * var(--glow-k)));
    box-shadow: 0 0 12px rgba(var(--accent-rgb), calc(0.05 * var(--glow-k)));
  }

  .turn-icon {
    color: var(--accent);
    font-size: 10px;
    transition: all 0.15s;
  }

  .turn-icon:nth-child(odd):hover {
    transform: scale(1.6) rotate(15deg);
    text-shadow: 0 0 16px var(--accent-glow);
    filter: brightness(1.3);
  }

  .turn-icon:nth-child(even):hover {
    transform: scale(1.5) rotate(-15deg);
    text-shadow: 0 0 14px var(--accent-glow);
    filter: brightness(1.2);
  }

  .turn-group:hover .turn-icon {
    transform: scale(1.2);
    text-shadow: 0 0 8px var(--accent-glow);
  }

  .turn-name {
    color: var(--text);
    font-weight: 500;
    flex: 1;
    transition: color 0.15s;
  }

  .turn-group:hover .turn-name {
    color: var(--accent);
    text-shadow: 0 0 6px var(--accent-glow);
  }

  .turn-name:nth-child(odd):hover {
    color: var(--accent-hover);
    text-shadow: 0 0 8px var(--accent-glow);
  }

  .turn-name:nth-child(even):hover {
    color: var(--accent);
    text-shadow: 0 0 6px var(--accent-glow);
  }

  .turn-count {
    color: var(--text-faint);
    font-size: 11px;
    transition: color 0.15s;
  }

  .turn-count:nth-child(odd):hover {
    color: var(--text);
    text-shadow: 0 0 8px rgba(255, 255, 255, 0.1);
  }

  .turn-count:nth-child(even):hover {
    color: var(--text-dim);
    text-shadow: 0 0 6px rgba(255, 255, 255, 0.08);
  }

  .turn-group:hover .turn-count {
    color: var(--text-dim);
    text-shadow: 0 0 6px rgba(255, 255, 255, 0.08);
  }

  .turn-items {
    padding: 4px 0;
    transition: background 0.15s;
  }

  .turn-items:nth-child(odd):hover {
    background: rgba(var(--accent-rgb), calc(0.03 * var(--glow-k)));
    box-shadow: inset 0 0 16px rgba(var(--accent-rgb), calc(0.04 * var(--glow-k)));
  }

  .turn-items:nth-child(even):hover {
    background: rgba(255, 255, 255, 0.03);
    box-shadow: inset 0 0 12px rgba(255, 255, 255, 0.03);
  }

  .turn-items:hover {
    background: rgba(255, 255, 255, 0.01);
  }

  .input-area {
    position: relative;
    padding: 16px 20px;
    background: var(--surface-chrome);
    border-top: 1px solid var(--border);
    transition: background 0.15s;
  }

  .input-area:nth-child(odd):hover {
    background: rgba(var(--accent-rgb), calc(0.04 * var(--glow-k)));
    box-shadow: 0 0 24px rgba(var(--accent-rgb), calc(0.08 * var(--glow-k)));
    border-top-color: var(--accent-dim);
  }

  .input-area:nth-child(even):hover {
    background: rgba(255, 255, 255, 0.04);
    box-shadow: 0 0 20px rgba(255, 255, 255, 0.06);
    border-top-color: var(--accent-dim);
  }

  .input-area:hover {
    background: rgba(var(--accent-rgb), calc(0.01 * var(--glow-k)));
    box-shadow: 0 0 16px rgba(var(--accent-rgb), calc(0.03 * var(--glow-k)));
  }

  .stream-progress {
    height: 2px;
    background: var(--border);
    border-radius: 1px;
    margin-bottom: 8px;
    overflow: hidden;
    animation: fade-in 0.2s ease-out;
    transition: all 0.15s;
  }

  .stream-progress:hover {
    height: 4px;
    box-shadow: 0 0 8px var(--accent-glow);
  }

  .progress-bar {
    height: 100%;
    background: var(--accent);
    animation: progress-pulse 1.5s ease-in-out infinite;
    transition: all 0.15s;
  }

  .stream-progress:hover .progress-bar {
    background: var(--accent-hover);
    box-shadow: 0 0 12px var(--accent-glow);
  }

  @keyframes progress-pulse {
    0% { width: 0%; opacity: 1; }
    50% { width: 100%; opacity: 1; }
    100% { width: 100%; opacity: 0; }
  }

  .input-box {
    background: var(--bg-elevated);
    border: 1px solid var(--border);
    border-radius: 12px;
    overflow: hidden;
    transition: all 0.2s;
  }

  .input-box:hover {
    border-color: var(--accent-dim);
    box-shadow: 0 0 16px rgba(var(--accent-rgb), calc(0.1 * var(--glow-k)));
  }

  .input-box:focus-within {
    border-color: var(--accent-dim);
    box-shadow: 0 0 0 2px rgba(var(--accent-rgb), calc(0.1 * var(--glow-k)));
  }

  .chat-input {
    width: 100%;
    padding: 12px 16px;
    background: transparent;
    border: none;
    color: var(--text);
    font-family: var(--mono);
    font-size: 13px;
    line-height: 1.5;
    resize: none;
    outline: none;
    transition: all 0.15s;
  }

  .chat-input:nth-child(odd):hover {
    background: rgba(var(--accent-rgb), calc(0.03 * var(--glow-k)));
    box-shadow: inset 0 0 16px rgba(var(--accent-rgb), calc(0.04 * var(--glow-k)));
    transform: scale(1.01);
  }

  .chat-input:nth-child(even):hover {
    background: rgba(255, 255, 255, 0.03);
    box-shadow: inset 0 0 12px rgba(255, 255, 255, 0.03);
    transform: scale(1.005);
  }

  .chat-input:hover {
    background: rgba(255, 255, 255, 0.005);
  }

  .chat-input:focus {
    background: rgba(255, 255, 255, 0.01);
  }

  .chat-input::placeholder {
    color: var(--text-faint);
  }

  .input-actions {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 8px 12px;
    border-top: 1px solid var(--border);
    transition: background 0.15s;
  }

  .input-actions:nth-child(odd):hover {
    background: rgba(var(--accent-rgb), calc(0.06 * var(--glow-k)));
    box-shadow: 0 0 20px rgba(var(--accent-rgb), calc(0.1 * var(--glow-k)));
    gap: 12px;
  }

  .input-actions:nth-child(even):hover {
    background: rgba(255, 255, 255, 0.06);
    box-shadow: 0 0 16px rgba(255, 255, 255, 0.08);
    gap: 10px;
  }

  .input-actions:hover {
    background: rgba(255, 255, 255, 0.01);
  }

  .action-btn {
    background: none;
    border: none;
    color: var(--text-faint);
    cursor: pointer;
    font-size: 14px;
    padding: 4px 6px;
    opacity: 0.6;
    transition: all 0.15s;
  }

  .action-btn:hover {
    opacity: 1;
    color: var(--accent);
    transform: scale(1.15);
    filter: drop-shadow(0 0 6px var(--accent-glow));
  }

  .model-selector {
    margin-left: auto;
    background: none;
    border: 1px solid var(--border);
    font-family: var(--mono);
    font-size: 11px;
    color: var(--text-dim);
    cursor: pointer;
    padding: 4px 8px;
    border-radius: 4px;
    transition: all 0.15s;
    white-space: nowrap;
  }

  .model-selector:hover {
    background: rgba(var(--accent-rgb), calc(0.08 * var(--glow-k)));
    color: var(--accent);
    border: 1px solid var(--accent-dim);
    box-shadow: 0 0 8px rgba(var(--accent-rgb), calc(0.1 * var(--glow-k)));
  }

  .send-btn {
    width: 32px;
    height: 32px;
    background: var(--accent);
    border: none;
    border-radius: 50%;
    color: #fff;
    font-size: 14px;
    cursor: pointer;
    display: flex;
    align-items: center;
    justify-content: center;
    transition: all 0.2s;
  }

  .send-btn:hover:not(:disabled) {
    background: var(--accent-hover);
    transform: scale(1.1);
    box-shadow: 0 0 16px var(--accent-glow);
  }

  .send-btn:active:not(:disabled) {
    transform: scale(0.95);
  }

  .send-btn:disabled {
    opacity: 0.4;
    cursor: not-allowed;
  }

  /* Пока нейронка думает, кнопка отправки становится стопом: красный квадрат,
     пульс по краю — видно, что ход живой и его можно оборвать. */
  .send-btn.stop {
    background: color-mix(in srgb, var(--err) 22%, var(--bg));
    color: var(--err);
    border: 1px solid color-mix(in srgb, var(--err) 55%, transparent);
    animation: stop-pulse 1.4s ease-in-out infinite;
  }

  .send-btn.stop:hover {
    background: color-mix(in srgb, var(--err) 38%, var(--bg));
    transform: scale(1.1);
    box-shadow: 0 0 16px color-mix(in srgb, var(--err) 45%, transparent);
  }

  @keyframes stop-pulse {
    0%, 100% { box-shadow: 0 0 0 0 color-mix(in srgb, var(--err) 35%, transparent); }
    50% { box-shadow: 0 0 0 6px color-mix(in srgb, var(--err) 0%, transparent); }
  }

  /* Очередь реплик: чипы над полем ввода, каждый можно вынуть. */
  .queue-strip {
    display: flex;
    align-items: center;
    gap: 6px;
    flex-wrap: wrap;
    margin-bottom: 8px;
  }

  .queue-title {
    font-family: var(--mono);
    font-size: 10px;
    text-transform: uppercase;
    letter-spacing: 0.08em;
    color: var(--text-faint);
  }

  .queue-chip {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    max-width: 220px;
    padding: 3px 8px;
    background: var(--surface-inset);
    border: 1px solid var(--border);
    border-radius: 999px;
    color: var(--text-dim);
  }

  .queue-text {
    font-family: var(--mono);
    font-size: 10px;
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .queue-drop {
    flex: none;
    display: inline-flex;
    background: none;
    border: none;
    padding: 0;
    color: var(--text-faint);
    cursor: pointer;
    transition: color 0.15s;
  }

  .queue-drop:hover {
    color: var(--err);
  }

  .thinking-indicator {
    margin-top: 8px;
    text-align: center;
    animation: fade-in 0.3s ease-out;
    transition: all 0.15s;
  }

  .thinking-indicator:hover {
    transform: scale(1.05);
  }

  .thinking {
    color: var(--accent);
    font-family: var(--mono);
    font-size: 11px;
    animation: breathe 2s ease-in-out infinite;
    transition: all 0.15s;
  }

  .thinking-indicator:hover .thinking {
    text-shadow: 0 0 12px var(--accent-glow);
  }

  .thinking .dots::after {
    content: '';
    animation: dots 1.5s step-end infinite;
    transition: all 0.15s;
  }

  .thinking-indicator:hover .thinking .dots::after {
    content: '…';
  }

  @keyframes breathe {
    0%, 100% { opacity: 1; }
    50% { opacity: 0.6; }
  }

  @keyframes dots {
    0% { content: ''; }
    25% { content: '.'; }
    50% { content: '..'; }
    75% { content: '...'; }
  }

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

  .glitch-overlay {
    position: fixed;
    inset: 0;
    z-index: 1000;
    pointer-events: none;
    background: repeating-linear-gradient(
      0deg,
      transparent,
      transparent 2px,
      rgba(255, 0, 64, 0.05) 2px,
      rgba(255, 0, 64, 0.05) 4px
    );
    animation: screen-glitch 0.5s ease-out;
    transition: opacity 0.15s;
  }

  .glitch-overlay:nth-child(odd):hover {
    opacity: 1;
    filter: blur(2px);
  }

  .glitch-overlay:nth-child(even):hover {
    opacity: 0.9;
    filter: blur(1px);
  }

  .glitch-overlay:hover {
    opacity: 0.8;
  }

  /* Settings dialog в стиле DSH */
  /* Блоки меню живые: каждый интерактивный блок отвечает на курсор —
     hover приподнимает, нажатие прессует, переходы мгновенные. Действует
     на все списки и панели: сессии, вкладки, палитра, настройки, области,
     карточки моделей. */
  :global(.session-item),
  :global(.tab),
  :global(.palette-item),
  :global(.settings-nav-item),
  :global(.ws-row),
  :global(.model-card),
  :global(.sessions-add),
  :global(.section-actions button) {
    transition: background 0.13s ease, color 0.13s ease, transform 0.13s ease,
      box-shadow 0.13s ease, border-color 0.13s ease;
  }
  :global(.session-item:hover),
  :global(.tab:hover),
  :global(.palette-item:hover),
  :global(.settings-nav-item:hover),
  :global(.ws-row:hover),
  :global(.model-card:hover),
  :global(.sessions-add:hover),
  :global(.section-actions button:hover) {
    transform: translateY(-1px);
  }
  :global(.session-item:active),
  :global(.tab:active),
  :global(.palette-item:active),
  :global(.settings-nav-item:active),
  :global(.ws-row:active),
  :global(.model-card:active),
  :global(.sessions-add:active),
  :global(.section-actions button:active) {
    transform: translateY(0) scale(0.985);
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

  .settings-empty {
    font-family: var(--mono);
    font-size: 12px;
    color: var(--text-faint);
    text-align: center;
    padding: 40px;
    transition: color 0.15s;
  }

  .settings-empty:nth-child(odd):hover {
    color: var(--text);
    text-shadow: 0 0 8px rgba(255, 255, 255, 0.1);
  }

  .settings-empty:nth-child(even):hover {
    color: var(--text-dim);
    text-shadow: 0 0 6px rgba(255, 255, 255, 0.08);
  }

  .settings-empty:hover {
    color: var(--text-dim);
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

  @keyframes screen-glitch {
    0% { opacity: 1; transform: translateX(0); }
    20% { transform: translateX(-3px); }
    40% { transform: translateX(3px); }
    100% { opacity: 0; }
  }

  /* ──────────────────────────────────────────────────────────────
   * Ресайс и сворачивание сайдбара
   * ────────────────────────────────────────────────────────────── */
  .sidebar {
    overflow: hidden;
  }

  main.dsh-layout.collapsed .sidebar {
    opacity: 0;
    pointer-events: none;
  }

  main.dsh-layout.resizing {
    cursor: col-resize;
    user-select: none;
  }

  .sidebar-resizer {
    position: relative;
    cursor: col-resize;
    background: transparent;
    border: none;
    border-radius: 0;
    padding: 0;
    transition: background 0.15s, box-shadow 0.15s;
    z-index: 20;
  }

  /* Zona захвата шире видимой полосы — так удобнее тянуть. */
  .sidebar-resizer::after {
    content: '';
    position: absolute;
    top: 0;
    bottom: 0;
    left: -4px;
    right: -4px;
  }

  .sidebar-resizer:hover,
  .sidebar-resizer.active {
    background: var(--accent);
    box-shadow: 0 0 12px var(--accent-glow);
  }

  main.dsh-layout.collapsed .sidebar-resizer {
    opacity: 0;
    pointer-events: none;
  }

  /* ──────────────────────────────────────────────────────────────
   * Шапка: инструменты масштаба и разворота
   * ────────────────────────────────────────────────────────────── */
  .header-row {
    display: flex;
    align-items: center;
    gap: 12px;
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

  .header-icon-btn {
    padding: 4px 9px;
    background: none;
    border: 1px solid var(--border);
    border-radius: 6px;
    color: var(--text-dim);
    font-size: 12px;
    cursor: pointer;
    transition: all 0.15s;
  }

  .header-icon-btn:hover {
    color: var(--accent);
    border-color: var(--accent-dim);
    box-shadow: 0 0 12px rgba(var(--accent-rgb), calc(0.15 * var(--glow-k)));
    transform: translateY(-1px);
  }

  /* ──────────────────────────────────────────────────────────────
   * Поповеры: выбор модели и прав
   * ────────────────────────────────────────────────────────────── */
  .popover-backdrop {
    position: fixed;
    inset: 0;
    z-index: 700;
    background: transparent;
  }

  .popover {
    position: absolute;
    bottom: calc(100% - 4px);
    z-index: 701;
    background: var(--bg-elevated);
    border: 1px solid var(--border-strong);
    border-radius: 10px;
    box-shadow: var(--shadow-strong);
    animation: popover-in 0.16s ease-out;
    display: flex;
    flex-direction: column;
    overflow: hidden;
  }

  .model-popover {
    right: 20px;
    width: 420px;
    max-width: calc(100vw - 40px);
    max-height: 60vh;
  }

  .perm-popover {
    left: 20px;
    width: 360px;
    max-width: calc(100vw - 40px);
  }

  @keyframes popover-in {
    from { opacity: 0; transform: translateY(6px) scale(0.98); }
    to { opacity: 1; transform: translateY(0) scale(1); }
  }

  .popover-head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 8px;
    padding: 10px 12px;
    border-bottom: 1px solid var(--border);
    background: var(--surface-inset);
  }

  .popover-title {
    font-family: var(--mono);
    font-size: 11px;
    text-transform: uppercase;
    letter-spacing: 0.08em;
    color: var(--text-dim);
  }

  .popover-hint,
  .popover-count {
    font-family: var(--mono);
    font-size: 10px;
    color: var(--accent);
    background: rgba(var(--accent-rgb), calc(0.1 * var(--glow-k)));
    border: 1px solid var(--accent-dim);
    border-radius: 10px;
    padding: 1px 8px;
  }

  .model-filter {
    margin: 10px 12px 4px;
    padding: 7px 10px;
    background: var(--bg);
    border: 1px solid var(--border);
    border-radius: 6px;
    color: var(--text);
    font-family: var(--mono);
    font-size: 12px;
    outline: none;
    transition: all 0.15s;
  }

  .model-filter:focus {
    border-color: var(--accent);
    box-shadow: 0 0 12px rgba(var(--accent-rgb), calc(0.15 * var(--glow-k)));
  }

  .model-list {
    overflow-y: auto;
    padding: 6px;
    display: flex;
    flex-direction: column;
    gap: 4px;
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

  .popover-empty {
    padding: 20px 12px;
    text-align: center;
    font-family: var(--mono);
    font-size: 11px;
    color: var(--text-faint);
  }

  .popover-foot {
    display: flex;
    flex-direction: column;
    gap: 6px;
    padding: 10px 12px;
    border-top: 1px solid var(--border);
    background: var(--surface-inset);
  }

  .popover-foot-row {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 10px;
  }

  .temp-auto {
    font-family: var(--mono);
    font-size: 10px;
    color: var(--text-faint);
  }

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

  .popover-link {
    background: none;
    border: none;
    padding: 2px 0;
    text-align: left;
    font-family: var(--mono);
    font-size: 11px;
    color: var(--accent);
    cursor: pointer;
    transition: all 0.15s;
  }

  .popover-link:hover {
    color: var(--accent-hover);
    text-shadow: 0 0 8px var(--accent-glow);
    transform: translateX(2px);
  }

  /* Права */
  .perm-list {
    display: flex;
    flex-direction: column;
    gap: 4px;
    padding: 8px;
  }

  .perm-card {
    display: flex;
    align-items: flex-start;
    gap: 10px;
    width: 100%;
    padding: 9px 10px;
    background: var(--bg);
    border: 1px solid var(--border);
    border-radius: 8px;
    cursor: pointer;
    text-align: left;
    transition: all 0.15s;
  }

  .perm-card:hover {
    border-color: var(--accent-dim);
    background: rgba(var(--accent-rgb), calc(0.05 * var(--glow-k)));
    transform: translateX(2px);
  }

  .perm-card.selected {
    border-color: var(--accent);
    background: rgba(var(--accent-rgb), calc(0.09 * var(--glow-k)));
  }

  .perm-icon {
    font-size: 14px;
    line-height: 1.2;
  }

  .perm-text {
    display: flex;
    flex-direction: column;
    gap: 2px;
    flex: 1;
    min-width: 0;
  }

  .perm-name {
    font-family: var(--mono);
    font-size: 12px;
    color: var(--text);
    font-weight: 600;
  }

  .perm-card.selected .perm-name {
    color: var(--accent);
  }

  .perm-desc {
    font-size: 11px;
    color: var(--text-dim);
    line-height: 1.35;
  }

  .perm-check {
    color: var(--accent);
    font-size: 13px;
  }

  .perm-btn {
    font-family: var(--mono);
    font-size: 11px;
    border: 1px solid var(--border);
    border-radius: 6px;
    padding: 4px 8px;
    opacity: 1;
    color: var(--text-dim);
    white-space: nowrap;
  }

  .perm-btn:hover {
    transform: none;
    border-color: var(--accent-dim);
    color: var(--accent);
  }

  .perm-btn.danger {
    color: var(--err);
    border-color: color-mix(in srgb, var(--err) 45%, transparent);
    background: color-mix(in srgb, var(--err) 10%, transparent);
  }

  .perm-status {
    margin-top: 8px;
    font-family: var(--mono);
    font-size: 11px;
    color: var(--ok);
    text-align: center;
    animation: fade-in 0.2s ease-out;
  }

  .perm-status.error {
    color: var(--err);
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

  .header-tools-right {
    margin-left: auto;
    display: flex;
    align-items: center;
    gap: 2px;
  }

  .hmenu-wrap {
    position: relative;
    display: flex;
  }

  .hmenu {
    position: absolute;
    top: calc(100% + 6px);
    right: 0;
    min-width: 210px;
    background: var(--bg-panel);
    border: 1px solid var(--border);
    border-radius: 8px;
    padding: 4px;
    box-shadow: 0 10px 30px rgba(0, 0, 0, 0.5);
    z-index: 60;
    display: flex;
    flex-direction: column;
  }

  .hmenu button {
    display: flex;
    align-items: center;
    gap: 8px;
    background: none;
    border: none;
    color: var(--text-dim);
    font-size: 11px;
    font-family: var(--mono);
    padding: 7px 8px;
    border-radius: 5px;
    cursor: pointer;
    text-align: left;
    transition: all 0.15s;
  }

  .hmenu button:hover {
    background: rgba(var(--accent-rgb), calc(0.08 * var(--glow-k)));
    color: var(--text);
  }

  /* Свои кнопки окна: системную рамку выключили в tauri.conf.json.
     Кнопки прижаты к правому верхнему углу, тулзы шапки — чуть правее. */
  .win-controls {
    display: flex;
    align-items: center;
    margin-left: 6px;
    border-left: 1px solid var(--border);
    padding-left: 4px;
    gap: 0;
  }

  .win-btn {
    background: none;
    border: none;
    color: var(--text-dim);
    cursor: pointer;
    padding: 7px 12px;
    border-radius: 0;
    display: flex;
    align-items: center;
    transition: all 0.12s;
  }

  .win-btn:hover {
    background: rgba(255, 255, 255, 0.06);
    color: var(--text);
  }

  .win-close:hover {
    background: var(--err);
    color: #fff;
  }

  /* Панель Files — часть лейаута: контент плавно уезжает влево, панель
     выезжает справа, ничего не накладывается и не обрезается. */
  .content-body {
    flex: 1;
    display: flex;
    min-height: 0;
  }

  .content-main {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
  }

  .files-panel {
    /* Широкая, как в референсе: почти половина окна, но с пределами. */
    width: clamp(360px, 44%, 720px);
    flex-shrink: 0;
    background: var(--bg-panel);
    border-left: 1px solid var(--border);
    box-shadow: inset 1px 0 0 rgba(var(--accent-rgb), calc(0.22 * var(--glow-k))),
      -16px 0 40px rgba(0, 0, 0, 0.35);
    display: flex;
    flex-direction: column;
    min-height: 0;
    animation: files-in 0.18s ease-out;
  }

  @keyframes files-in {
    from {
      transform: translateX(28px);
      opacity: 0;
    }
    to {
      transform: translateX(0);
      opacity: 1;
    }
  }

  /* Под медиафоном панель Files стеклянная, как остальные поверхности. */
  .media-on .files-panel {
    background: rgba(14, 14, 18, var(--panel-glass));
    backdrop-filter: blur(14px);
  }

  /* Таб-бар панели: чипы вкладок как в референсе. */
  .files-tabs {
    display: flex;
    align-items: flex-end;
    gap: 4px;
    padding: 6px 8px 0;
    border-bottom: 1px solid var(--border);
    background: var(--surface-inset);
    overflow-x: auto;
    overflow-y: hidden;
  }

  .ftab {
    display: flex;
    align-items: center;
    gap: 6px;
    max-width: 180px;
    padding: 6px 8px;
    background: transparent;
    border: 1px solid transparent;
    border-bottom: none;
    border-radius: 7px 7px 0 0;
    color: var(--text-faint);
    font-family: var(--mono);
    font-size: 10px;
    cursor: pointer;
    transition: all 0.15s;
  }

  .ftab span:not(.ftab-x) {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .ftab:hover {
    color: var(--text-dim);
    background: rgba(255, 255, 255, 0.03);
  }

  .ftab.active {
    background: var(--bg-panel);
    border-color: var(--border);
    color: var(--accent);
    box-shadow: 0 -4px 14px rgba(var(--accent-rgb), calc(0.12 * var(--glow-k)));
  }

  .ftab-x {
    display: flex;
    align-items: center;
    color: var(--text-faint);
    border-radius: 4px;
    padding: 1px;
    transition: all 0.12s;
  }

  .ftab-x:hover {
    color: var(--err);
    background: rgba(var(--err-rgb), 0.12);
  }

  .ftab-add {
    display: flex;
    align-items: center;
    justify-content: center;
    background: none;
    border: none;
    color: var(--text-faint);
    cursor: pointer;
    padding: 6px;
    margin-bottom: 3px;
    border-radius: 5px;
    transition: all 0.15s;
  }

  .ftab-add:hover {
    color: var(--accent);
    background: rgba(var(--accent-rgb), calc(0.1 * var(--glow-k)));
  }

  .files-path {
    min-width: 0;
    font-size: 10px;
    font-family: var(--mono);
    color: var(--text-faint);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    padding: 6px 12px;
    border-bottom: 1px solid var(--border);
  }

  .files-empty {
    padding: 16px 12px;
    color: var(--text-faint);
    font-size: 11px;
    font-family: var(--mono);
  }

  .file-tab-body {
    margin: 0;
    padding: 12px;
    overflow: auto;
    flex: 1;
    min-height: 0;
    font-family: var(--mono);
    font-size: 11px;
    line-height: 1.55;
    color: var(--text-dim);
    white-space: pre-wrap;
    word-break: break-word;
  }

  /* B-4: поиск по индексу, результаты, diff против git HEAD. */
  .files-search {
    padding: 6px 8px;
    border-bottom: 1px solid var(--border);
  }

  .files-search input {
    width: 100%;
    background: rgba(255, 255, 255, 0.04);
    border: 1px solid var(--border);
    border-radius: 6px;
    color: var(--text);
    font-family: var(--mono);
    font-size: 11px;
    padding: 5px 8px;
    outline: none;
  }

  .files-search input:focus {
    border-color: var(--accent-dim);
  }

  .files-results {
    overflow: auto;
    flex: 1;
    min-height: 0;
    padding: 4px 0;
  }

  .files-result {
    display: block;
    width: 100%;
    text-align: left;
    background: none;
    border: none;
    color: var(--text-dim);
    font-family: var(--mono);
    font-size: 11px;
    padding: 5px 12px;
    cursor: pointer;
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .files-result:hover {
    background: rgba(var(--accent-rgb), 0.08);
    color: var(--text);
  }

  .file-tab-tools {
    display: flex;
    gap: 6px;
    padding: 6px 12px;
    border-bottom: 1px solid var(--border);
  }

  .diff-btn {
    background: rgba(255, 255, 255, 0.04);
    border: 1px solid var(--border);
    border-radius: 6px;
    color: var(--text-dim);
    font-family: var(--mono);
    font-size: 10px;
    padding: 3px 8px;
    cursor: pointer;
  }

  .diff-btn:hover {
    border-color: var(--accent-dim);
    color: var(--text);
  }

  .diff-body {
    overflow: auto;
    flex: 1;
    min-height: 0;
    font-family: var(--mono);
    font-size: 11px;
    line-height: 1.5;
    padding: 8px 0;
  }

  .diff-line {
    display: flex;
    gap: 8px;
    padding: 0 12px;
    white-space: pre-wrap;
    word-break: break-word;
  }

  .diff-line.added {
    background: rgba(80, 200, 120, 0.1);
    color: #7ee2a8;
  }

  .diff-line.removed {
    background: rgba(255, 100, 100, 0.08);
    color: #ff9d9d;
  }

  .diff-line.same {
    color: var(--text-faint);
  }

  .diff-no {
    flex: 0 0 34px;
    text-align: right;
    color: var(--text-faint);
    opacity: 0.6;
    user-select: none;
  }

  .diff-text {
    flex: 1;
    min-width: 0;
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

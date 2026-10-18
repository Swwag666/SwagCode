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
  import Sidebar from './components/Sidebar.svelte'
  import Terminal from './components/Terminal.svelte'
  import FileTree from './components/FileTree.svelte'
  import ApprovalDialog from './components/ApprovalDialog.svelte'
  import RevertDialog from './components/RevertDialog.svelte'
  import Palette from './components/Palette.svelte'
  /* F-10: окно настроек — компонент; общие с ним типы объявлены там и
     импортируются сюда (приём MemoryTab/Icon), чтобы не дублировать их. */
  import SettingsTabs, {
    type Appearance,
    type PermissionMode,
    type UiLang,
    type ModelSpec,
    type ProviderInfo,
    type FetchedModel,
    type ApprovalRow,
    type SeriesPoint,
    type TokenDay,
    type TaskRow,
    type CmdRules,
    type HooksDto,
  } from './components/SettingsTabs.svelte'
  import Icon, { type IconName } from './components/Icon.svelte'
  import { type MemoryItem } from './components/MemoryTab.svelte'
  /* F-10: полнотекстовый поиск по истории. Панель — components/HistorySearch.svelte,
     разбор сниппета — lib/search.ts; здесь только состояние и вызов команды. */
  import HistorySearch, { type MessageHitDto } from './components/HistorySearch.svelte'
  /* Анимированный глаз логотипа: пользовательский gif, чёрный фон вырезается
     blend-mode screen (см. .eye img). */
  import { Transcript as TranscriptModel, type TranscriptItem } from './lib/transcript'
  import { translate, type StrKey } from './lib/strings'
  import { type PaletteCommand } from './lib/palette'
  import { counterDeltas, groupByName } from './lib/charts'

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
  let settingsTab = $state<'general' | 'models' | 'providers' | 'plugins' | 'security' | 'mcp' | 'telemetry' | 'memory'>('general')
  let activeTab = $state<'chat' | 'trajectory' | 'terminal'>('chat')

  /* ────────────────────────────────────────────────────────────────
   * Внешний вид и права. Всё это реально применяется к DOM и
   * переживает перезапуск (localStorage), а не лежит мёртвым контролом.
   * ──────────────────────────────────────────────────────────────── */

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
  /** Показ мыслей модели: 'live' — размышления раскрыты, пока стримятся
      (действия и текст ответа видны вместе с ходом рассуждения), 'always' —
      плюс раскрыты завершённые строки мыслей прошедших ходов. */
  let showThoughts = $state<'live' | 'always'>(
    (localStorage.getItem('swagcod-thoughts') as 'live' | 'always') || 'live'
  )
  /* Per-session модель: если включено, каждая сессия помнит свою модель.
     Если выключено - всегда используется глобальная modelName. */
  let perSessionModels = $state<boolean>(
    localStorage.getItem('swagcod-per-session-models') !== 'false'
  )
  /* Per-session модель: каждая сессия помнит свою модель, не переключаешься
     глобально. Пустая строка = использовать глобальный дефолт из prefs.
     Объявлена до effectiveModel: derived читает её при первом же кадре. */
  let sessionModels = $state<Record<string, string>>({})
  let showModelPicker = $state(false)
  let showPermPicker = $state(false)
  let modelQuery = $state('')


  function t(key: StrKey): string {
    return translate(uiLang, key)
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

  /* Эффективная модель текущей сессии: per-session приоритетнее глобальной.
     Пустая строка в sessionModels = использовать глобальный дефолт. */
  const effectiveModel = $derived(
    perSessionModels && currentSession && sessionModels[currentSession]
      ? sessionModels[currentSession]
      : modelName
  )

  function selectModel(id: string): void {
    if (perSessionModels && currentSession) {
      // Per-session выбор: запоминаем для этой сессии, не трогаем глобальную.
      sessionModels[currentSession] = id
    } else {
      modelName = id
    }
    showModelPicker = false
    modelQuery = ''
  }

  /* Сброс per-session модели на глобальный дефолт. */
  function resetSessionModel(): void {
    if (currentSession) {
      delete sessionModels[currentSession]
    }
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
    localStorage.setItem('swagcod-thoughts', showThoughts)
    persistPref('show_thoughts', showThoughts)
  })
  $effect(() => {
    localStorage.setItem('swagcod-per-session-models', perSessionModels ? 'true' : 'false')
  })
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
  /* Сигнал «чат открылся заново»: Transcript по нему принудительно прижимает
     список к низу. Без него scrollTop переживает переключение сессии, и новый
     чат открывается с дырой вместо истории. */
  let scrollNonce = $state(0)
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
    scrollNonce++
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
        scrollNonce++
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
      const list = await invoke<{ id: string; cwd: string; model?: string }[]>('list_sessions')
      const cwds: Record<string, string> = {}
      const models: Record<string, string> = {}
      for (const s of list) {
        cwds[s.id] = s.cwd
        if (s.model) models[s.id] = s.model
      }
      sessionCwds = cwds
      sessionModels = models
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
  let approvalReq = $state<{ call_id: string; tool: string; summary: string; preview?: string } | null>(null)

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
    // F-3: списки команд обновляем вместе с остальной безопасностью.
    void loadCmdRules()
    // F-8: хуки — там же, это тоже граница разрешённого.
    void loadHooks()
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

  /* ── F-3: allow/deny-списки команд ────────────────────────────────────────
     Движок, дефолты и порядок (deny > allow > политика) живут в ядре
     (core::rules); prefs-ключи cmd_allow / cmd_deny читаются на каждый ход,
     поэтому сохранение действует сразу. UI только показывает строки. */
  let cmdRules = $state<CmdRules | null>(null)
  let cmdRulesBusy = $state(false)
  let cmdRulesStatus = $state<string | null>(null)
  let cmdRulesTimer: ReturnType<typeof setTimeout> | null = null

  async function loadCmdRules(): Promise<void> {
    if (!tauriAvailable) return
    try {
      cmdRules = await invoke<CmdRules>('cmd_rules_load')
    } catch {
      cmdRules = null // ядро без F-3: секцию не показываем
    }
  }

  async function saveCmdRules(allow: string[], deny: string[]): Promise<void> {
    cmdRulesBusy = true
    if (cmdRulesTimer) clearTimeout(cmdRulesTimer)
    try {
      cmdRules = await invoke<CmdRules>('cmd_rules_save', { allow, deny })
      cmdRulesStatus =
        cmdRules.active > 0
          ? t('cmdRulesSaved').replace('{n}', String(cmdRules.active))
          : t('cmdRulesEmpty')
    } catch (e) {
      // Битый regex ядро отклоняет целиком: список не сохранён, текст остался.
      cmdRulesStatus = String(e)
    } finally {
      cmdRulesBusy = false
      cmdRulesTimer = setTimeout(() => (cmdRulesStatus = null), 5000)
    }
  }

  /* ── F-8: хуки (pre/post turn, pre_tool) ──────────────────────────────────
     Движок — core::hooks, исполнение — app::hooks. Конфигурация читается на
     каждый ход, поэтому сохранение действует без перезапуска. Редактор
     показывает JSON как есть: три события с разными формами в плоские поля
     не раскладываются без потери смысла. */
  let hooksDto = $state<HooksDto | null>(null)
  let hooksBusy = $state(false)
  let hooksStatus = $state<string | null>(null)
  let hooksError = $state<string | null>(null)
  let hooksTimer: ReturnType<typeof setTimeout> | null = null

  async function loadHooks(): Promise<void> {
    if (!tauriAvailable) return
    try {
      hooksDto = await invoke<HooksDto>('hooks_load')
    } catch {
      hooksDto = null // ядро без F-8: секцию не показываем
    }
  }

  async function saveHooks(raw: string): Promise<void> {
    hooksBusy = true
    if (hooksTimer) clearTimeout(hooksTimer)
    try {
      hooksDto = await invoke<HooksDto>('hooks_save', { raw })
      hooksError = null
      const total = hooksDto.pre_turn + hooksDto.post_turn + hooksDto.pre_tool
      hooksStatus = total > 0 ? t('hooksSaved').replace('{n}', String(total)) : t('hooksEmpty')
    } catch (e) {
      /* Ядро отклоняет битую конфигурацию целиком: текст остался в редакторе,
      сохранённая конфигурация не тронута. */
      hooksError = String(e)
      hooksStatus = null
    } finally {
      hooksBusy = false
      hooksTimer = setTimeout(() => (hooksStatus = null), 5000)
    }
  }

  /* ── F-5: git-чекпоинты ходов ───────────────────────────────────────────
     Ядро снимает worktree перед ходом и пишет sha в turns.checkpoint_sha.
     Здесь — карта «ход → короткий sha» для кнопки отката в траектории:
     кнопка рисуется только там, где снимок действительно есть. */
  interface TurnCheckpoint {
    turn_id: string
    sha: string
    short_sha: string
  }
  let checkpoints = $state<Record<string, string>>({})
  let revertTarget = $state<{ turnId: string; sha: string } | null>(null)
  let revertBusy = $state(false)

  async function refreshCheckpoints(): Promise<void> {
    if (!tauriAvailable || !currentSession) {
      checkpoints = {}
      return
    }
    try {
      const rows = await invoke<TurnCheckpoint[]>('turn_checkpoints', {
        sessionId: currentSession,
      })
      const map: Record<string, string> = {}
      for (const r of rows) map[r.turn_id] = r.short_sha || r.sha.slice(0, 10)
      checkpoints = map
    } catch {
      // Ядро без F-5 или сессия ещё не загружена: кнопок нет, ходы не ломаем.
      checkpoints = {}
    }
  }

  async function confirmRevert(): Promise<void> {
    const target = revertTarget
    if (!target || !currentSession || revertBusy) return
    revertBusy = true
    try {
      const msg = await invoke<string>('turn_revert', {
        sessionId: currentSession,
        turnId: target.turnId,
      })
      flashStatus(t('revertDone').replace('{msg}', msg))
      revertTarget = null
    } catch (e) {
      // Отказ ядра (ход идёт, снимка нет, git сломался) — человеку текстом.
      flashStatus(t('revertFailed').replace('{msg}', String(e)))
    } finally {
      revertBusy = false
    }
  }

  /* Вкладка траектории показывает свежие снимки: при открытии и после
     каждого завершившегося хода. */
  $effect(() => {
    if (activeTab === 'trajectory') void refreshCheckpoints()
  })

  /* ── F-7: память приложения ──────────────────────────────────────────────
     Та же таблица `memory`, куда пишет агент инструментами memory_*: человек
     и модель видят один список. Глобальные факты вшиваются в системный промпт
     каждого хода, заметки сессии — только свои. */
  let memoryItems = $state<MemoryItem[]>([])
  let memoryScope = $state<'global' | 'session' | 'folder'>('global')
  let memoryBusy = $state(false)
  let memoryStatus = $state<string | null>(null)
  /* Заметки сессии читаются для ВЫБРАННОЙ сессии, а не только для активной:
     владелец видит сессии в сайдбаре и вправе править память любой из них, не
     переключая активный чат (переключение сбросило бы ход и транскрипцию).
     null — выбор не сделан, значит активная сессия. */
  let memorySessions = $state<{ id: string; title: string }[]>([])
  let memorySessionId = $state<string | null>(null)
  let memoryImporting = $state(false)
  let memoryExporting = $state(false)
  /* rev56: третья область — факты рабочей папки. Папка по умолчанию та же, что
     у правил папки на вкладке Security (cwd активной сессии), но выбрать можно
     любую: владелец правит факты проекта, который сейчас не открыт. Выражение
     повторяет `filesRoot`, а не ссылается на него — тот объявлен ниже по файлу. */
  let memoryDirId = $state<string | null>(null)
  const memoryCwd = $derived(currentSession ? (sessionCwds[currentSession] ?? '') : '')
  const memoryDir = $derived(memoryDirId ?? (memoryCwd || null))

  const memorySession = $derived(memorySessionId ?? currentSession)

  /* Владелец строки обязан соответствовать области, и проверяет это ядро
     (`memory_check_owner`), а не фронтенд: global с подставленным sessionId
     теперь ошибка, а не «сохраним как глобальный». Поэтому аргументы
     передаются только для своей области. */
  const memoryOwnerArgs = $derived({
    sessionId: memoryScope === 'session' ? memorySession : null,
    dir: memoryScope === 'folder' ? memoryDir : null,
  })

  async function refreshMemory(): Promise<void> {
    if (!tauriAvailable) return
    /* Факты папки без папки не выбираются. Ядро ответило бы «область folder
       требует рабочую папку», и показывать это как «Не получилось» значило бы
       пугать человека тем, что он просто ещё не выбрал папку. */
    if (memoryScope === 'folder' && !memoryDir) {
      memoryItems = []
      return
    }
    memoryBusy = true
    try {
      memoryItems = await invoke<MemoryItem[]>('memory_list', {
        scope: memoryScope,
        sessionId: memoryOwnerArgs.sessionId,
        dir: memoryOwnerArgs.dir,
        limit: 200,
      })
    } catch (e) {
      memoryItems = []
      memoryStatus = t('memoryFailed').replace('{msg}', String(e))
    } finally {
      memoryBusy = false
    }
  }

  /** Список сессий для селектора памяти. Тот же `list_sessions`, что кормит
      сайдбар (SessionBrief: title никогда не пуст — ядро подставляет cwd),
      порядок «свежие сверху» как у сайдбара в режиме по обновлению. */
  async function refreshMemorySessions(): Promise<void> {
    if (!tauriAvailable) return
    try {
      const rows = await invoke<{ id: string; title: string; last_activity_ms: number }[]>(
        'list_sessions'
      )
      memorySessions = rows
        .slice()
        .sort((a, b) => b.last_activity_ms - a.last_activity_ms)
        .map((s) => ({ id: s.id, title: s.title }))
    } catch {
      memorySessions = []
    }
  }

  function pickMemorySession(id: string): void {
    if (memorySessionId === id) return
    memorySessionId = id
    memoryStatus = null
    void refreshMemory()
  }

  /* Импорт промпта из txt/md. Путь выбирает нативный диалог, а читает ядро
     командой `read_prompt_file`: существующий `read_file` заперт песочницей в
     cwd сессии, а промпт владельца может лежать где угодно. Возврат null —
     отмена диалога, а не ошибка: молча ничего не делаем. */
  async function importPromptFile(): Promise<string | null> {
    if (!tauriAvailable) return null
    memoryImporting = true
    try {
      const sel = await openDialog({
        multiple: false,
        title: t('memoryImportTitle'),
        filters: [{ name: 'Text / Markdown', extensions: ['txt', 'md', 'markdown', 'text'] }],
      })
      const path = typeof sel === 'string' ? sel : ''
      if (!path) return null
      const text = await invoke<string>('read_prompt_file', { path })
      memoryStatus = null
      return text
    } catch (e) {
      memoryStatus = t('memoryImportFailed').replace('{msg}', String(e))
      return null
    } finally {
      memoryImporting = false
    }
  }

  async function addMemory(text: string): Promise<void> {
    if (!tauriAvailable || memoryBusy) return
    memoryBusy = true
    try {
      await invoke<number>('memory_add', {
        scope: memoryScope,
        sessionId: memoryOwnerArgs.sessionId,
        dir: memoryOwnerArgs.dir,
        text,
      })
      memoryStatus = t('memoryAdded')
      await refreshMemory()
    } catch (e) {
      // Отказ ядра (пусто, слишком длинно, неизвестная область) — текстом.
      memoryStatus = t('memoryFailed').replace('{msg}', String(e))
    } finally {
      memoryBusy = false
    }
  }

  /** rev56: выбрать рабочую папку для фактов области folder. Нативный диалог,
      тот же `openDialog({ directory: true })`, что у правил папки и рабочих
      областей. Отмена (null) текущий выбор не трогает. */
  async function pickMemoryDir(): Promise<void> {
    if (!tauriAvailable) return
    try {
      const sel = await openDialog({
        multiple: false,
        directory: true,
        title: t('memoryPickFolderHint'),
      })
      const path = typeof sel === 'string' ? sel : ''
      if (!path) return
      memoryDirId = path
      memoryStatus = null
      await refreshMemory()
    } catch (e) {
      memoryStatus = t('memoryFailed').replace('{msg}', String(e))
    }
  }

  /** rev56: экспорт факта в txt/md. Путь выбирает нативный диалог сохранения,
      пишет ядро командой `write_prompt_file`. `overwrite: true` передаём
      осознанно: диалог уже спросил человека про замену, а флаг в команде
      существует, чтобы никакой ДРУГОЙ вызывающий не затёр файл молча.
      Возвращает текст статуса (или null при отмене диалога): правилам папки
      тот же экспорт нужен со своим статусом, и читать чужой `memoryStatus`
      они не должны — там могло остаться вчерашнее «Сохранено». */
  async function exportMemoryFile(text: string): Promise<string | null> {
    if (!tauriAvailable || memoryExporting) return null
    memoryExporting = true
    try {
      const path = await saveDialog({
        title: t('memoryExportTitle'),
        defaultPath: 'prompt.md',
        filters: [{ name: 'Text / Markdown', extensions: ['md', 'txt', 'markdown', 'text'] }],
      })
      if (!path) return null
      const n = await invoke<number>('write_prompt_file', { path, text, overwrite: true })
      const msg = t('memoryExportDone').replace('{path}', path).replace('{n}', String(n))
      memoryStatus = msg
      return msg
    } catch (e) {
      const msg = t('memoryExportFailed').replace('{msg}', String(e))
      memoryStatus = msg
      return msg
    } finally {
      memoryExporting = false
    }
  }

  async function forgetMemory(id: number): Promise<void> {
    if (!tauriAvailable || memoryBusy) return
    memoryBusy = true
    try {
      await invoke<boolean>('memory_forget', { id })
      memoryStatus = t('memoryRemoved')
      await refreshMemory()
    } catch (e) {
      memoryStatus = t('memoryFailed').replace('{msg}', String(e))
    } finally {
      memoryBusy = false
    }
  }

  /* Смена области перечитывает список: показывать глобальные факты под
     подписью «заметки сессии» значило бы врать человеку. */
  function setMemoryScope(scope: 'global' | 'session' | 'folder'): void {
    if (memoryScope === scope) return
    memoryScope = scope
    memoryStatus = null
    void refreshMemory()
  }

  $effect(() => {
    if (showSettings && settingsTab === 'memory') {
      /* Чтение выбранного владельца регистрируется как зависимость: смена
         сессии в селекторе или папки в диалоге перечитывает список, а не
         оставляет строки прежней области. `memoryCwd` читаем отдельно — иначе
         смена активной сессии при невыбранной папке не обновила бы факты. */
      const sid = memorySession
      const dir = memoryDir
      const cwd = memoryCwd
      void sid
      void dir
      void cwd
      void refreshMemory()
      void refreshMemorySessions()
    }
  })

  $effect(() => {
    /* Диалог настроек закрыли — выбор сбрасывается: в следующий раз память
       открывается на активной сессии и её рабочей папке, а не на вчерашнем
       выборе, который молча показывал бы чужие строки. */
    if (!showSettings) {
      if (memorySessionId !== null) memorySessionId = null
      if (memoryDirId !== null) memoryDirId = null
    }
  })

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

  /* F-10: поиск по истории всех сессий (FTS5 в ядре). Имена отдельные от
     showSearch/searchQuery выше: те фильтруют текущую транскрипцию на месте, а эти
     ходят в ядро по всей базе. Поколение запроса защищает от гонки: ответ на
     устаревший запрос не должен перекрывать свежий. */
  const HISTORY_SEARCH_LIMIT = 50
  let showHistorySearch = $state(false)
  let historyHits = $state<MessageHitDto[]>([])
  let historyError = $state('')
  let historyBusy = $state(false)
  let historyGen = 0

  async function runHistorySearch(query: string): Promise<void> {
    const gen = ++historyGen
    const q = query.trim()
    if (!q) {
      historyHits = []
      historyError = ''
      historyBusy = false
      return
    }
    historyBusy = true
    try {
      const hits = await invoke<MessageHitDto[]>('search_messages', {
        query: q,
        limit: HISTORY_SEARCH_LIMIT,
      })
      if (gen !== historyGen) return
      historyHits = hits
      historyError = ''
    } catch (err) {
      if (gen !== historyGen) return
      historyHits = []
      historyError = String(err)
    } finally {
      if (gen === historyGen) historyBusy = false
    }
  }

  /* Находка открывает свою сессию. К самому сообщению не перематываем: в транскрипции
     нет якоря по ord, а id из F-9 — на ход, а не на строку базы. */
  function openHistoryHit(hit: MessageHitDto): void {
    historyError = ''
    if (hit.session_id !== currentSession) switchSession(hit.session_id)
  }

  let showPalette = $state(false)

  /* F-2: палитра уровня DSH-референса — slash-имя, группа, описание.
     Имена команд языконейтральны, описания локализованы.
     F-10: сам компонент — components/Palette.svelte, скоринг и фильтрация —
     lib/palette.ts; здесь остаются только команды, потому что их действия
     замкнуты на состояние приложения. */
  const commands: PaletteCommand[] = [
    { slash: '/new', group: 'palGroupSession', desc: 'cmdNewDesc', action: () => createNewSession() },
    { slash: '/clear', group: 'palGroupSession', desc: 'cmdClearDesc', action: () => { transcriptFor(currentSession ?? '').clear(); items = transcriptFor(currentSession ?? '').items; revision++; eventsTotal = 0; batchesTotal = 0 } },
    { slash: '/compact', group: 'palGroupSession', desc: 'cmdCompactDesc', action: () => void compactNow() },
    { slash: '/search', group: 'palGroupSession', desc: 'cmdSearchDesc', action: () => { showHistorySearch = true } },
    { slash: '/export', group: 'palGroupExport', desc: 'cmdExportDesc', action: () => exportTranscript() },
    { slash: '/diag', group: 'palGroupExport', desc: 'cmdDiagDesc', action: () => exportDiagnostics() },
    { slash: '/model', group: 'palGroupAgent', desc: 'cmdModelDesc', action: () => { showPermPicker = false; showModelPicker = true; modelQuery = '' } },
    { slash: '/perm:always', group: 'palGroupAgent', desc: 'cmdPermAlwaysDesc', action: () => { void selectPermission('always') } },
    { slash: '/perm:dangerous', group: 'palGroupAgent', desc: 'cmdPermDangerousDesc', action: () => { void selectPermission('on_dangerous') } },
    { slash: '/perm:never', group: 'palGroupAgent', desc: 'cmdPermNeverDesc', action: () => { void selectPermission('never') } },
    { slash: '/update', group: 'palGroupAgent', desc: 'cmdUpdateDesc', action: () => { showSettings = true; settingsTab = 'general'; void checkUpdate() } },
    { slash: '/tasks', group: 'palGroupAgent', desc: 'cmdTasksDesc', action: () => { showSettings = true; settingsTab = 'telemetry'; void refreshTelemetry() } },
    /* F-7: память приложения — та же таблица, куда пишет агент через
       memory_add. Палитра объявлена в плане F-2, содержимое появилось здесь. */
    { slash: '/memory', group: 'palGroupAgent', desc: 'memoryCmdDesc', action: () => { showSettings = true; settingsTab = 'memory'; void refreshMemory() } },
    { slash: '/theme:dark', group: 'palGroupView', desc: 'cmdThemeDarkDesc', action: () => (appearance = 'dark') },
    { slash: '/theme:light', group: 'palGroupView', desc: 'cmdThemeLightDesc', action: () => (appearance = 'light') },
    { slash: '/theme:contrast', group: 'palGroupView', desc: 'cmdThemeContrastDesc', action: () => (appearance = 'contrast') },
    { slash: '/theme:system', group: 'palGroupView', desc: 'cmdThemeSystemDesc', action: () => (appearance = 'system') },
    { slash: '/zoom:in', group: 'palGroupView', desc: 'cmdZoomInDesc', action: zoomIn },
    { slash: '/zoom:out', group: 'palGroupView', desc: 'cmdZoomOutDesc', action: zoomOut },
    { slash: '/zoom:reset', group: 'palGroupView', desc: 'cmdZoomResetDesc', action: zoomReset },
    { slash: '/sidebar', group: 'palGroupView', desc: 'cmdSidebarDesc', action: () => (sidebarCollapsed = !sidebarCollapsed) },
    { slash: '/fx', group: 'palGroupView', desc: 'cmdFxDesc', action: () => (effectsEnabled = !effectsEnabled) },
    { slash: '/bg:matrix', group: 'palGroupView', desc: 'cmdBgMatrixDesc', action: () => (bgMode = 'matrix') },
    { slash: '/bg:sphere', group: 'palGroupView', desc: 'cmdBgSphereDesc', action: () => (bgMode = 'sphere') },
    { slash: '/sim:50', group: 'palGroupDev', desc: 'cmdSim50Desc', action: () => simulateStream(50) },
    { slash: '/sim:2k', group: 'palGroupDev', desc: 'cmdSim2kDesc', action: () => simulateStream(2000) },
    { slash: '/sim:20k', group: 'palGroupDev', desc: 'cmdSim20kDesc', action: () => simulateStream(20000) },
  ]

  /* F-2: /compact — ручная компакция (B-3 по требованию). Честно: 0 —
     сжимать нечего, ошибка провайдера — во flash-статус. */
  async function compactNow(): Promise<void> {
    if (!currentSession) return
    try {
      const folded = await invoke<number>('session_compact', { sessionId: currentSession })
      flashStatus(folded > 0 ? `${t('cmdCompactDone')}: ${folded}` : t('cmdCompactNothing'))
    } catch (e) {
      flashStatus(String(e))
    }
  }

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
    const sid = currentSession
    try {
      await invoke('stop_turn', { sessionId: sid })
    } catch (err) {
      /* «нет живого хода у сессии» — ход уже умер или handle потерян:
         для UI это всё равно остановка. */
      console.warn('stop_turn failed', err)
    }
    /* Состояние сбрасываем ВСЕГДА, даже если бэк не нашёл handle: иначе
       liveStates остаётся 'running' навсегда и красный квадрат выглядит
       мёртвым — клик есть, реакции нет. */
    liveStates[sid] = 'fail'
    transcriptFor(sid).closeAll()
    items = transcriptFor(sid).items
    revision++
    if (sendingSession === sid) sendingSession = null
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
          /* F-5: ход записан — его снимок стал доступен для отката. */
          if (d.session === currentSession) void refreshCheckpoints()
          /* Ход закончился — очередь ЭТОЙ сессии продолжает её разговор сама. */
          void drainQueue(d.session)
        }
      }
      if (e.kind.kind === 'approval_required') {
        const d = e.kind.data as {
          turn: string
          call_id: string
          tool: string
          summary: string
          preview?: string
        }
        /* F-6: preview (unified-diff будущей правки) показываем вместе со
           сводкой — решение по тексту «write {"path":…}» принимается вслепую. */
        approvalReq = {
          call_id: d.call_id,
          tool: d.tool,
          summary: d.summary,
          preview: d.preview,
        }
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
      // Spread-копия массива: Svelte 5 не видит мутацию свойств элементов
      // внутри того же массива (ex.text += ...). Новая ссылка триггерит
      // перерисовку всех строк, включая live-reasoning.
      items = [...transcriptFor(currentSession ?? '').items]
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
        // Per-session модель приоритетнее глобальной (если включено): каждая
        // сессия помнит свою модель, не нужно переключаться туда-сюда.
        model: (perSessionModels && sessionModels[sid]) ? sessionModels[sid] : (modelName || null),
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
      /* F-10: сброс запроса и фокус в поле — забота Palette.svelte, поэтому
         здесь остаётся только переключение видимости. */
      if ((e.ctrlKey || e.metaKey) && e.key === 'p') {
        e.preventDefault()
        showPalette = !showPalette
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
        if (showHistorySearch) showHistorySearch = false
        if (showPalette) showPalette = false
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
    preview={approvalReq.preview}
    diffHiddenLabel={t('diffHidden')}
    onRespond={respondApproval}
  />
{/if}

<!-- F-5: подтверждение отката хода к git-снимку. Откат переписывает файлы
     рабочей директории, поэтому без диалога не обходится. -->
{#if revertTarget}
  <RevertDialog
    title={t('revertTitle')}
    turnLabel={`${t('turn')} ${revertTarget.turnId.slice(0, 8)}`}
    sha={revertTarget.sha}
    warning={t('revertWarning')}
    confirmLabel={t('revertConfirm')}
    cancelLabel={t('revertCancel')}
    busyLabel={t('revertBusy')}
    busy={revertBusy}
    onConfirm={() => void confirmRevert()}
    onCancel={() => (revertTarget = null)}
  />
{/if}

<!-- F-10: настройки — компонент; App остаётся оркестратором состояния и
     передаёт его вниз пропсами (тот же приём, что в Palette). -->
<SettingsTabs
  /* окно и вкладки */
  bind:showSettings={showSettings}
  bind:settingsTab={settingsTab}
  {t}

  /* общие: внешний вид, студия тем, фон, масштаб */
  bind:appearance={appearance}
  bind:fontSize={fontSize}
  bind:glowLevel={glowLevel}
  bind:showThoughts={showThoughts}
  bind:perSessionModels={perSessionModels}
  {uiZoom}
  bind:uiLang={uiLang}
  bind:effectsEnabled={effectsEnabled}
  bind:bgMode={bgMode}
  {bgMediaName}
  {bgMediaBusy}
  {bgMediaError}
  bind:customAccent={customAccent}
  bind:customTint={customTint}
  {themeAccentHex}
  bind:sidebarWidth={sidebarWidth}
  bind:sidebarCollapsed={sidebarCollapsed}
  {zoomIn}
  {zoomOut}
  {zoomReset}
  {onPickBgMedia}
  {clearBgMedia}
  {openConfigFile}
  {exportDiagnostics}
  {restartApp}

  /* безопасность: политика, журнал подтверждений, DPAPI, HTTP API, правила команд, хуки, импорт */
  {permissionMode}
  {permSpecs}
  {permSpec}
  {selectPermission}
  {sessPolicy}
  {applySessPolicy}
  {approvalRows}
  {approvalLogLoaded}
  {loadApprovalLog}
  {fmtLogTime}
  bind:dpapiKey={dpapiKey}
  {dpapiStored}
  {dpapiStatus}
  {saveDpapiKey}
  {clearDpapiKey}
  {httpApi}
  {copyHttpToken}
  {cmdRules}
  {cmdRulesBusy}
  {cmdRulesStatus}
  {saveCmdRules}
  {hooksDto}
  {hooksBusy}
  {hooksStatus}
  {hooksError}
  {saveHooks}
  {currentSession}
  /* cwd текущей сессии: тот же filesRoot, что и у панели Files, — правилами
     папки правят для той директории, где реально идёт ход. */
  sessionCwd={filesRoot || null}
  {dshImportBusy}
  {dshImportNote}
  {runDshImport}

  /* модели */
  {models}
  bind:modelName={modelName}
  {modelSpecs}
  {currentSpec}
  {reloadModels}

  /* провайдеры */
  {provState}
  {provBusy}
  {provNote}
  {provFetching}
  {provKeys}
  bind:customLabel={customLabel}
  bind:customBase={customBase}
  bind:customKey={customKey}
  bind:customFlavor={customFlavor}
  bind:provPreview={provPreview}
  {loadProviders}
  {useProvider}
  {deleteProvider}
  {fetchModels}
  {savePreview}

  /* MCP-серверы */
  {mcpServers}
  bind:mcpName={mcpName}
  bind:mcpCommand={mcpCommand}
  {mcpBusy}
  {mcpNote}
  {loadMcp}
  {addMcp}
  {removeMcp}

  /* телеметрия и обновления */
  {telTokens}
  {telLatency}
  {telLagE}
  {telLagD}
  {telTasks}
  {refreshTelemetry}
  {cancelTask}
  {updateState}
  {updateCurrent}
  {updateVersion}
  {updateNotes}
  {updateError}
  {updatePercent}
  {checkUpdate}
  {installUpdate}

  /* память */
  {memoryItems}
  {memoryScope}
  {memoryBusy}
  {memoryStatus}
  {refreshMemory}
  {addMemory}
  {forgetMemory}
  {setMemoryScope}
  /* выбор сессии и импорт промпта из txt/md */
  {memorySessions}
  {memorySession}
  {memoryImporting}
  {pickMemorySession}
  {importPromptFile}
  /* rev56: факты рабочей папки и экспорт промпта в txt/md */
  {memoryDir}
  {pickMemoryDir}
  {exportMemoryFile}
  {memoryExporting}

  /* js-плагины */
  {jsPlugTools}
  {jsPlugErrors}
  {jsPlugDir}
  {jsPlugBusy}
  {loadJsPlugins}
  {reloadJsPlugins}
/>

<!-- F-10: палитра команд — компонент; здесь остаётся только «открыта ли» и
     список команд, чьи действия замкнуты на состояние приложения. -->
<Palette bind:open={showPalette} {commands} {t} />

<!-- F-10: результаты поиска по истории. Запрос уходит в ядро с задержкой из компонента,
     здесь — состояние и счётчик поколений. -->
<HistorySearch
  bind:open={showHistorySearch}
  hits={historyHits}
  error={historyError}
  busy={historyBusy}
  limit={HISTORY_SEARCH_LIMIT}
  {t}
  onquery={(q) => void runHistorySearch(q)}
  onpick={openHistoryHit}
/>

<main
  ondrop={onDrop}
  ondragover={onDragOver}
  class="dsh-layout"
  class:collapsed={sidebarCollapsed}
  class:resizing
  class:media-on={bgMediaUrl !== ''}
  style:grid-template-columns={sidebarCollapsed ? '0px 0px 1fr' : `${sidebarWidth}px 5px 1fr`}
>
  <!-- F-10: левая панель — компонент; состояние осталось в App и приходит
       пропсами (тот же приём, что в Palette и SettingsTabs). -->
  <Sidebar
    /* шапка: бренд, сворачивание, новая сессия */
    {t}
    bind:sidebarCollapsed={sidebarCollapsed}
    {creatingSession}
    {createNewSession}

    /* список сессий: фильтр, группировка, порядок */
    bind:sessionFilterOpen={sessionFilterOpen}
    bind:sessionFilter={sessionFilter}
    bind:groupPop={groupPop}
    {groupMode}
    {orderMode}
    {setGroupMode}
    {setOrderMode}
    {currentSession}
    {sessionRefreshTick}
    {liveStates}
    {switchSession}

    /* рабочие области */
    bind:showWorkspaces={showWorkspaces}
    {workspaces}
    {loadWorkspaces}
    {openWorkspace}
    {copyWorkspace}
    {browseFolder}
    {browseError}
    {openActiveFolder}

    /* нижняя статусная строка и вход в настройки */
    {thinking}
    {items}
    {turnsCount}
    {eventsTotal}
    {connected}
    {tauriAvailable}
    {logNote}
    bind:showSettings={showSettings}
  />

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
          <Transcript items={filteredItems} {revision} {bookmarks} showThoughts={showThoughts} scrollNonce={scrollNonce} onToggleBookmark={toggleBookmark} />
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
                  {#if !isSub && checkpoints[turn]}
                    <!-- F-5: снимок есть — значит ход можно откатить. -->
                    <button
                      class="turn-revert"
                      title={t('revertButton')}
                      onclick={() => (revertTarget = { turnId: turn, sha: checkpoints[turn] })}
                    >
                      <Icon name="restore" size={12} />
                      <span>{t('revertButton')}</span>
                    </button>
                  {/if}
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
                class:selected={m.id === effectiveModel}
                role="option"
                aria-selected={m.id === effectiveModel}
                onclick={() => selectModel(m.id)}
              >
                <span class="model-name">{m.id}</span>
                {#if m.id === effectiveModel}<span class="model-current">{t('currentModel')}</span>{/if}
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
              {#if perSessionModels && currentSession && sessionModels[currentSession]}
                <button
                  class="popover-link"
                  onclick={resetSessionModel}
                  title={t('resetSessionModel')}
                >{t('resetToGlobal')} ({modelName})</button>
              {/if}
              <button
                class="popover-link"
                onclick={() => { showModelPicker = false; showSettings = true; settingsTab = 'models' }}
              >{t('openModelSettings')} →</button>
            </div>
            {#if perSessionModels && currentSession && sessionModels[currentSession]}
              <div class="popover-hint">{t('sessionModelHint')}</div>
            {/if}
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
            // F-2: «/» в пустом вводе открывает палитру команд, как в DSH.
            // F-10: фокус и сброс запроса — внутри Palette.svelte.
            if (e.key === '/' && inputText === '') {
              e.preventDefault()
              showPalette = true
              return
            }
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
            class:session-override={perSessionModels && currentSession && sessionModels[currentSession]}
            aria-expanded={showModelPicker}
            title={perSessionModels && currentSession && sessionModels[currentSession] ? t('sessionModelHint') : t('modelTitle')}
            onclick={() => { showPermPicker = false; showModelPicker = !showModelPicker; modelQuery = '' }}
          >{effectiveModel} <Icon name="chevron-down" size={12} /></button>
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

  .new-session-btn:hover :global(.icon) {
    transform: rotate(90deg);
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

  /* F-5: откат хода к git-снимку. Кнопка в строке хода, правый край. */
  .turn-revert {
    margin-left: auto;
    display: inline-flex;
    align-items: center;
    gap: 5px;
    padding: 2px 9px;
    border-radius: 7px;
    border: 1px solid var(--border);
    background: transparent;
    color: var(--text-dim);
    font-family: var(--mono);
    font-size: 11px;
    cursor: pointer;
    transition: color 0.15s, border-color 0.15s, box-shadow 0.15s;
  }

  .turn-revert:hover {
    color: var(--err);
    border-color: rgba(var(--err-rgb), 0.55);
    box-shadow: 0 0 14px rgba(var(--err-rgb), 0.18);
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

  /* Per-session модель: визуальный индикатор что сессия использует свою,
     а не глобальную модель. Тонкая подсветка границы. */
  .model-selector.session-override {
    border-color: var(--accent);
    color: var(--accent);
    box-shadow: inset 0 0 0 1px rgba(var(--accent-rgb), 0.15);
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























  @keyframes screen-glitch {
    0% { opacity: 1; transform: translateX(0); }
    20% { transform: translateX(-3px); }
    40% { transform: translateX(3px); }
    100% { opacity: 0; }
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

</style>

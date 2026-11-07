<script lang="ts">
  /**
   * Ночной прогон: экран запуска (UX-02-002, ревизия 86 — решение владельца:
   * «экран/форма настроек»).
   *
   * Форма с целью, лимитом раундов и бюджетом; старт дёргает night_run
   * ядра (часы могут идти — обещание invoke живёт всё это время и не
   * блокирует интерфейс); прогресс читается из живых событий сессии
   * (счётчик ходов и статус «думает» уже работают в шапке); «стоп»
   * останавливает текущий раунд — ядро прогон завершает честно.
   * Утренний отчёт приходит и строкой в чат сессии, и тостом Action
   * Center (backend), здесь он только дублируется для чтения не выходя
   * из экрана.
   */
  interface Props {
    sessionId: string | null
    sessionLabel: string
    roundsRunning: number
    busy: boolean
    runNight: (objective: string, maxRounds: number, budgetUsd: number) => Promise<string>
    stopNight: () => Promise<void>
    onclose: () => void
  }

  let {
    sessionId,
    sessionLabel,
    roundsRunning,
    busy,
    runNight,
    stopNight,
    onclose,
  }: Props = $props()

  let objective = $state('')
  let maxRounds = $state(20)
  let budget = $state(2)
  let running = $state(false)
  let report = $state<string | null>(null)
  let error = $state('')

  const canStart = $derived(
    sessionId !== null && !running && !busy && objective.trim().length > 0 && maxRounds >= 1 && budget > 0
  )

  async function start(): Promise<void> {
    if (!sessionId || !canStart) return
    running = true
    report = null
    error = ''
    try {
      const text = await runNight(objective.trim(), Math.floor(maxRounds), budget)
      report = text
    } catch (err) {
      error = `${err}`
    } finally {
      running = false
    }
  }

  function onKey(e: KeyboardEvent): void {
    if (e.key === 'Escape' && !running) onclose()
  }
</script>

<svelte:window onkeydown={onKey} />

<div class="night-overlay" role="presentation">
  <div class="night" role="dialog" aria-label="ночной прогон">
    <div class="night-head">
      <span class="night-title">ночной прогон</span>
      <span class="night-sub">{sessionLabel}</span>
      <button class="night-close" onclick={() => { if (!running) onclose() }} disabled={running} aria-label="закрыть">✕</button>
    </div>

    <div class="night-body">
      {#if !sessionId}
        <p class="night-warn">прогон привязан к сессии: открой или создай чат с нужной папкой, потом возвращайся</p>
      {:else}
        <label class="night-field">
          <span class="night-label">цель — что должно быть сделано к утру</span>
          <textarea
            class="night-input night-objective"
            bind:value={objective}
            rows="3"
            placeholder="например: доведи тесты до зелёных и почини warnings clippy"
            disabled={running}
          ></textarea>
        </label>
        <div class="night-grid">
          <label class="night-field">
            <span class="night-label">максимум раундов</span>
            <input class="night-input" type="number" min="1" max="500" step="1" bind:value={maxRounds} disabled={running} />
          </label>
          <label class="night-field">
            <span class="night-label">бюджет, $</span>
            <input class="night-input" type="number" min="0.1" max="1000" step="0.1" bind:value={budget} disabled={running} />
          </label>
        </div>

        {#if running}
          <div class="night-running">
            <span class="night-pulse">●</span>
            <span>идёт прогон — раундов завершено: {roundsRunning}</span>
            <button class="night-btn night-stop" onclick={() => void stopNight()}>остановить текущий раунд</button>
          </div>
          <p class="night-hint">после остановки раунда ядро завершает прогон и пишет честный отчёт; окно можно свернуть — тост утром разбудит</p>
        {:else if report}
          <div class="night-report">
            <span class="night-label">отчёт</span>
            <pre class="night-report-text">{report}</pre>
            <div class="night-actions">
              <button class="night-btn" onclick={() => onclose()}>закрыть</button>
              <button class="night-btn" onclick={() => void start()} disabled={!canStart}>повторить прогон</button>
            </div>
          </div>
        {:else}
          <div class="night-actions">
            <button class="night-btn night-start" onclick={() => void start()} disabled={!canStart}>запустить прогон</button>
          </div>
        {/if}

        {#if error}
          <p class="night-error">{error}</p>
        {/if}
      {/if}
    </div>
  </div>
</div>

<style>
  .night-overlay {
    position: fixed;
    inset: 0;
    background: rgb(0 0 0 / 0.55);
    display: flex;
    align-items: center;
    justify-content: center;
    z-index: 90;
  }

  .night {
    width: min(560px, 92vw);
    max-height: 86vh;
    overflow-y: auto;
    background: var(--bg-panel, #14161c);
    border: 1px solid var(--hairline, #2a2e38);
    border-radius: 10px;
    box-shadow: 0 18px 60px rgb(0 0 0 / 0.5);
    padding: 18px 22px 22px;
  }

  .night-head {
    display: flex;
    align-items: baseline;
    gap: 10px;
    margin-bottom: 14px;
  }

  .night-title {
    font-weight: 600;
    letter-spacing: 0.02em;
  }

  .night-sub {
    color: var(--text-dim, #8b93a3);
    font-size: 12px;
    flex: 1;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .night-close {
    background: none;
    border: none;
    color: var(--text-dim, #8b93a3);
    cursor: pointer;
    font-size: 14px;
    padding: 2px 6px;
  }

  .night-close:disabled {
    opacity: 0.4;
    cursor: default;
  }

  .night-field {
    display: flex;
    flex-direction: column;
    gap: 4px;
    margin-bottom: 12px;
  }

  .night-label {
    font-size: 12px;
    color: var(--text-dim, #8b93a3);
  }

  .night-input {
    background: var(--bg-input, #0f1116);
    border: 1px solid var(--hairline, #2a2e38);
    border-radius: 6px;
    color: var(--text, #e7eaf0);
    padding: 8px 10px;
    font-size: 13px;
    font-family: inherit;
    outline: none;
  }

  .night-input:focus {
    border-color: var(--accent, #7aa2f7);
  }

  .night-objective {
    resize: vertical;
    min-height: 64px;
  }

  .night-grid {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: 12px;
  }

  .night-actions {
    display: flex;
    gap: 10px;
    margin-top: 8px;
  }

  .night-btn {
    background: var(--bg-input, #0f1116);
    border: 1px solid var(--hairline, #2a2e38);
    border-radius: 6px;
    color: var(--text, #e7eaf0);
    cursor: pointer;
    font-size: 13px;
    padding: 8px 14px;
  }

  .night-btn:hover:not(:disabled) {
    border-color: var(--accent, #7aa2f7);
  }

  .night-btn:disabled {
    opacity: 0.45;
    cursor: default;
  }

  .night-start {
    border-color: var(--accent, #7aa2f7);
    color: var(--accent, #7aa2f7);
  }

  .night-stop {
    border-color: var(--err, #e06c75);
    color: var(--err, #e06c75);
  }

  .night-running {
    display: flex;
    align-items: center;
    gap: 10px;
    margin: 6px 0;
    font-size: 13px;
  }

  .night-pulse {
    color: var(--accent, #7aa2f7);
    animation: night-blink 1.2s steps(2, jump-none) infinite;
  }

  @keyframes night-blink {
    50% { opacity: 0.25; }
  }

  .night-hint {
    font-size: 12px;
    color: var(--text-dim, #8b93a3);
    margin: 8px 0 0;
  }

  .night-warn {
    color: var(--err, #e06c75);
    font-size: 13px;
  }

  .night-report {
    display: flex;
    flex-direction: column;
    gap: 6px;
  }

  .night-report-text {
    white-space: pre-wrap;
    font-family: var(--mono, monospace);
    font-size: 12px;
    line-height: 1.55;
    color: var(--text, #e7eaf0);
    background: var(--bg-input, #0f1116);
    border: 1px solid var(--hairline, #2a2e38);
    border-radius: 6px;
    padding: 10px 12px;
    margin: 0;
    max-height: 40vh;
    overflow-y: auto;
  }

  .night-error {
    color: var(--err, #e06c75);
    font-size: 12px;
    margin: 10px 0 0;
  }
</style>

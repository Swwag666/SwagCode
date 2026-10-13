<script lang="ts">
  /* F-11: таблица фоновых задач во вкладке «Телеметрия».

     Жила внутри SettingsTabs.svelte. Вынесена не ради красоты, а ради
     тестируемости: у SettingsTabs больше сотни пропсов, и рендерить всё окно в
     jsdom, чтобы нажать одну кнопку отмены, непрактично. Здесь только разметка
     и CSS — ни состояния, ни invoke: список приходит сверху, отмена уходит
     колбэком (тот же приём, что в Palette, Sidebar и MemoryTab). Пустой список
     компонент показывает сам, как MemoryTab: родителю не нужна вторая ветка. */
  /* Тип строки объявлен в SettingsTabs.svelte и переиспользуется здесь: App
     берёт TaskRow оттуда, и второе объявление разъехалось бы с первым.
     Направление импорта (ребёнок ← родитель) выглядит наоборот, но импорт
     type-only и из JS стирается целиком, поэтому цикла в рантайме нет.
     Переэкспортировать его отсюда нельзя: svelte2tsx не поднимает
     re-export типов из instance-скрипта, проверено svelte-check. */
  import { type StrKey } from '../lib/strings'
  import { type TaskRow } from './SettingsTabs.svelte'

  interface Props {
    tasks: TaskRow[]
    t: (key: StrKey) => string
    /** Отмена queued-задачи. Обещание родителю возвращать можно, компонент его не ждёт. */
    onCancel: (id: string) => void
  }

  let { tasks, t, onCancel }: Props = $props()
</script>

{#if tasks.length === 0}
  <div class="approval-empty">{t('telemetryEmpty')}</div>
{:else}
  <div class="approval-scroll">
    <table class="approval-table">
      <thead>
        <tr><th>id</th><th>kind</th><th>{t('taskColState')}</th><th>{t('taskColAttempts')}</th><th>{t('taskColNext')}</th><th>{t('taskColLog')}</th><th></th></tr>
      </thead>
      <tbody>
        {#each tasks as task (task.id)}
          <tr>
            <td>{task.id}</td>
            <td>{task.kind}</td>
            <td>{task.state}</td>
            <td>{task.attempts}</td>
            <td class="log-time">{task.next_try_ms ? new Date(task.next_try_ms).toLocaleString() : '—'}</td>
            <td class="tel-err">{task.last_error}</td>
            <td>
              <!-- Отменить можно только то, что ещё не началось: у запущенной
                   или мёртвой задачи отмена ничего не значит. -->
              {#if task.state === 'queued'}
                <button class="step-btn text-btn" onclick={() => onCancel(task.id)}>{t('taskCancel')}</button>
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

<style>
  /* Правила скопированы из SettingsTabs.svelte дословно: те же классы там нужны
     таблице подтверждений (approval-*) и кнопкам-шагам (step-btn/text-btn), а
     Svelte скоупит CSS по компонентам — общего файла стилей в проекте нет.
     Расхождение копий является известной платой за перенос (в rev47 так же
     продублированы .model-card и .chip). Исключение — .tel-err: им пользовалась
     только эта таблица, поэтому правило переехало, а не скопировано. */
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
  .text-btn {
    width: auto;
    height: 26px;
    padding: 0 10px;
  }
</style>

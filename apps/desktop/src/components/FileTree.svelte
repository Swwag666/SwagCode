<script lang="ts">
  /**
   * Файловое дерево проекта (Этап 2).
   *
   * Показывает структуру рабочей директории сессии. Ленивая загрузка:
   * директории раскрываются по клику, содержимое запрашивается у ядра.
   * Иконки: папка/файл по расширению.
   */
  import { invoke } from '@tauri-apps/api/core'

  interface FileEntry {
    name: string
    path: string
    is_dir: boolean
    children?: FileEntry[]
    expanded?: boolean
  }

  interface Props {
    root: string
    onFileSelect?: (path: string, name: string) => void
  }

  let { root, onFileSelect }: Props = $props()

  let entries = $state<FileEntry[]>([])
  let loading = $state(false)
  let error = $state<string | null>(null)

  /** Контекстное меню. */
  let contextMenu = $state<{ x: number; y: number; entry: FileEntry } | null>(null)

  function onContextMenu(e: MouseEvent, entry: FileEntry): void {
    e.preventDefault()
    contextMenu = { x: e.clientX, y: e.clientY, entry }
  }

  function closeContextMenu(): void {
    contextMenu = null
  }

  function copyPath(): void {
    if (contextMenu) {
      navigator.clipboard.writeText(contextMenu.entry.path)
    }
    closeContextMenu()
  }

  function openInExplorer(): void {
    if (contextMenu) {
      // C-1 фикс: путь отдельным аргументом, без склейки строк
      invoke('open_in_explorer', { path: contextMenu.entry.path }).catch(() => {})
    }
    closeContextMenu()
  }

  /** Цвета иконок по расширению. */
  const extColors: Record<string, string> = {
    '.rs': '#dea584',
    '.ts': '#3178c6',
    '.js': '#f7df1e',
    '.svelte': '#ff3e00',
    '.json': '#cbcb41',
    '.toml': '#9c4221',
    '.md': '#519aba',
    '.css': '#563d7c',
    '.html': '#e34c26',
    '.yaml': '#cb171e',
    '.yml': '#cb171e',
    '.lock': '#8b949e',
    '.env': '#8b949e',
  }

  function extColor(name: string): string {
    const dot = name.lastIndexOf('.')
    if (dot === -1) return 'var(--text-dim)'
    const ext = name.slice(dot).toLowerCase()
    return extColors[ext] || 'var(--text-dim)'
  }

  function icon(entry: FileEntry): string {
    if (entry.is_dir) return entry.expanded ? '▾' : '▸'
    return '·'
  }

  async function loadDir(path: string): Promise<FileEntry[]> {
    try {
      const result = await invoke<FileEntry[]>('list_dir', { path })
      return result
    } catch (err) {
      console.warn('list_dir failed:', err)
      return []
    }
  }

  async function toggle(entry: FileEntry): Promise<void> {
    if (entry.is_dir) {
      entry.expanded = !entry.expanded
      if (entry.expanded && !entry.children) {
        entry.children = await loadDir(entry.path)
      }
      // Триггерим реактивность
      entries = [...entries]
    } else {
      onFileSelect?.(entry.path, entry.name)
    }
  }

  async function loadRoot(): Promise<void> {
    loading = true
    error = null
    try {
      entries = await loadDir(root)
    } catch (err) {
      error = String(err)
    } finally {
      loading = false
    }
  }

  // Загружаем при монтировании
  $effect(() => {
    if (root) loadRoot()
  })
</script>

<div class="file-tree">
  <div class="tree-header">
    <span class="tree-title">файлы</span>
    <span class="tree-root">{root.split(/[/\\]/).pop()}</span>
  </div>
  {#if loading}
    <div class="tree-loading">загрузка…</div>
  {:else if error}
    <div class="tree-error">{error}</div>
  {:else}
    <div class="tree-entries">
      {#each entries as entry (entry.path)}
        <div
          class="tree-entry"
          class:dir={entry.is_dir}
          onclick={() => toggle(entry)}
          oncontextmenu={(e) => onContextMenu(e, entry)}
          onkeydown={(e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); toggle(entry) } }}
          role="treeitem"
          tabindex="0"
          aria-expanded={entry.is_dir ? entry.expanded : undefined}
          aria-selected="false"
        >
          <span class="tree-icon" style="color: {entry.is_dir ? 'var(--accent)' : extColor(entry.name)}">
            {icon(entry)}
          </span>
          <span class="tree-name">{entry.name}</span>
        </div>
        {#if entry.is_dir && entry.expanded && entry.children}
          <div class="tree-children" role="group">
            {#each entry.children as child (child.path)}
              <div
                class="tree-entry child"
                class:dir={child.is_dir}
                onclick={() => toggle(child)}
                onkeydown={(e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); toggle(child) } }}
                role="treeitem"
                tabindex="0"
                aria-selected="false"
              >
                <span class="tree-icon" style="color: {child.is_dir ? 'var(--accent)' : extColor(child.name)}">
                  {icon(child)}
                </span>
                <span class="tree-name">{child.name}</span>
              </div>
            {/each}
          </div>
        {/if}
      {/each}
    </div>
  {/if}
</div>

{#if contextMenu}
  <div
    class="context-menu"
    style="left: {contextMenu.x}px; top: {contextMenu.y}px"
    onclick={closeContextMenu}
    onkeydown={(e) => { if (e.key === 'Escape') closeContextMenu() }}
    role="menu"
    tabindex="-1"
  >
    <button onclick={copyPath} role="menuitem">копировать путь</button>
    <button onclick={openInExplorer} role="menuitem">в проводнике</button>
  </div>
{/if}

<style>
  .file-tree {
    display: flex;
    flex-direction: column;
    height: 100%;
    overflow: hidden;
    background: rgba(20, 20, 25, 0.95);
    border-right: 1px solid var(--border);
    font-family: var(--mono);
    font-size: 12px;
  }

  .tree-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    padding: 8px 12px;
    border-bottom: 1px solid var(--border);
  }

  .tree-title {
    color: var(--text-dim);
    text-transform: uppercase;
    letter-spacing: 0.08em;
    font-size: 10px;
  }

  .tree-root {
    color: var(--accent);
    font-size: 11px;
  }

  .tree-loading,
  .tree-error {
    padding: 12px;
    color: var(--text-dim);
    font-size: 11px;
  }

  .tree-error {
    color: var(--err);
  }

  .tree-entries {
    flex: 1;
    overflow-y: auto;
    padding: 4px 0;
  }

  .tree-entry {
    display: flex;
    align-items: center;
    gap: 6px;
    padding: 3px 12px;
    cursor: pointer;
    transition: background 0.1s;
  }

  .tree-entry:hover {
    background: var(--bg-hover);
  }

  .tree-entry.child {
    padding-left: 24px;
  }

  .tree-icon {
    width: 12px;
    text-align: center;
    flex-shrink: 0;
  }

  .tree-name {
    color: var(--text);
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .tree-entry.dir .tree-name {
    color: var(--text);
    font-weight: 500;
  }

  .context-menu {
    position: fixed;
    z-index: 1000;
    background: var(--bg-elevated);
    border: 1px solid var(--border);
    border-radius: 6px;
    padding: 4px;
    box-shadow: 0 4px 16px rgba(0, 0, 0, 0.4);
    min-width: 160px;
  }

  .context-menu button {
    display: block;
    width: 100%;
    text-align: left;
    padding: 6px 10px;
    border: none;
    background: none;
    color: var(--text);
    font-family: var(--mono);
    font-size: 11px;
    cursor: pointer;
    border-radius: 4px;
  }

  .context-menu button:hover {
    background: var(--bg-hover);
    color: var(--accent);
  }
</style>

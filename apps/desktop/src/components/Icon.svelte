<script lang="ts">
  /**
   * Единый набор иконок интерфейса.
   *
   * Штриховые SVG 24×24 на currentColor: наследуют цвет и свечение окружающих
   * правил, масштабируются без артефактов и не тянут шрифтовые глифы.
   * Эмодзи в интерфейсе не используются вовсе.
   */
  export type IconName =
    | 'shield'
    | 'scale'
    | 'bolt'
    | 'close'
    | 'gear'
    | 'cpu'
    | 'plug'
    | 'moon'
    | 'sun'
    | 'contrast'
    | 'monitor'
    | 'search'
    | 'menu'
    | 'wrench'
    | 'clipboard'
    | 'check'
    | 'paperclip'
    | 'star'
    | 'star-o'
    | 'expand'
    | 'collapse'
    | 'chevron-down'
    | 'send'
    | 'panel'
    | 'plus'
    | 'alert'
    | 'brain'
    | 'folder'
    | 'folder-open'
    | 'sliders'
    | 'external'
    | 'dots'
    | 'download'
    | 'minimize'
    | 'maximize'
    | 'restore'
    | 'stop'

  interface Props {
    name: IconName
    size?: number
    strokeWidth?: number
  }

  let { name, size = 16, strokeWidth = 1.7 }: Props = $props()

  /** Внутренняя разметка каждой иконки: только path/circle/rect, без fill по умолчанию. */
  const SHAPES: Record<IconName, string> = {
    shield: '<path d="M12 3l7 2.8v5.4c0 4.4-2.9 7.9-7 9.8-4.1-1.9-7-5.4-7-9.8V5.8z"/>',
    scale:
      '<path d="M12 4.5v15M8.5 19.5h7M4.5 7.5h15"/>' +
      '<path d="M6.5 7.5l-2.8 5.4a3 3 0 0 0 5.6 0z"/>' +
      '<path d="M17.5 7.5l-2.8 5.4a3 3 0 0 0 5.6 0z"/>',
    bolt: '<path d="M13.2 3L5.5 13h5.2l-1 8 7.8-10h-5.2z"/>',
    close: '<path d="M6.5 6.5l11 11M17.5 6.5l-11 11"/>',
    gear:
      '<circle cx="12" cy="12" r="3.1"/>' +
      '<path d="M12 2.9v2.3M12 18.8v2.3M5.6 5.6l1.6 1.6M16.8 16.8l1.6 1.6M2.9 12h2.3M18.8 12h2.3M5.6 18.4l1.6-1.6M16.8 7.2l1.6-1.6"/>',
    cpu:
      '<rect x="7" y="7" width="10" height="10" rx="2"/>' +
      '<rect x="10.4" y="10.4" width="3.2" height="3.2"/>' +
      '<path d="M9.5 2.8v2.4M14.5 2.8v2.4M9.5 18.8v2.4M14.5 18.8v2.4M2.8 9.5h2.4M2.8 14.5h2.4M18.8 9.5h2.4M18.8 14.5h2.4"/>',
    plug:
      '<path d="M9 3.2v4.6M15 3.2v4.6"/>' +
      '<path d="M6.5 7.8h11v3.2a5.5 5.5 0 0 1-11 0z"/>' +
      '<path d="M12 16.5v4.3"/>',
    moon: '<path d="M20 14.4A8.4 8.4 0 1 1 9.6 4a6.9 6.9 0 0 0 10.4 10.4z"/>',
    sun:
      '<circle cx="12" cy="12" r="3.9"/>' +
      '<path d="M12 2.6v2.3M12 19.1v2.3M2.6 12h2.3M19.1 12h2.3M5 5l1.6 1.6M17.4 17.4L19 19M5 19l1.6-1.6M17.4 6.6L19 5"/>',
    contrast:
      '<circle cx="12" cy="12" r="8.4"/>' +
      '<path d="M12 3.6v16.8a8.4 8.4 0 0 0 0-16.8z" fill="currentColor" stroke="none"/>',
    monitor: '<rect x="3.2" y="4.6" width="17.6" height="11.8" rx="2"/><path d="M9 20h6M12 16.4V20"/>',
    search: '<circle cx="10.6" cy="10.6" r="5.9"/><path d="M15 15l5.4 5.4"/>',
    menu: '<path d="M4.5 7h15M4.5 12h15M4.5 17h15"/>',
    wrench:
      '<path d="M14.6 6.2a4.4 4.4 0 0 0-5.8 5.8L4 16.8V20h3.2l4.8-4.8a4.4 4.4 0 0 0 5.8-5.8L14.6 12.6 11.4 9.4z"/>',
    clipboard:
      '<rect x="6" y="4.4" width="12" height="16.2" rx="2"/>' +
      '<path d="M9.2 4.4V3h5.6v1.4M9.2 11h5.6M9.2 15h3.6"/>',
    check: '<path d="M5.2 12.6l4.4 4.4L18.8 7.6"/>',
    paperclip:
      '<path d="M19.8 11.4l-7.9 7.9a4.9 4.9 0 0 1-6.9-6.9l8.4-8.4a3.3 3.3 0 0 1 4.7 4.7l-8.4 8.4a1.7 1.7 0 0 1-2.4-2.4l7.4-7.4"/>',
    star: '<path d="M12 3.8l2.5 5.1 5.6.8-4 4 .9 5.6-5-2.7-5 2.7.9-5.6-4-4 5.6-.8z" fill="currentColor" stroke="none"/>',
    'star-o': '<path d="M12 3.8l2.5 5.1 5.6.8-4 4 .9 5.6-5-2.7-5 2.7.9-5.6-4-4 5.6-.8z"/>',
    expand: '<path d="M9 4.2H4.2V9M15 4.2h4.8V9M9 19.8H4.2V15M15 19.8h4.8V15"/>',
    collapse: '<path d="M4.2 9H9V4.2M19.8 9H15V4.2M4.2 15H9v4.8M19.8 15H15v4.8"/>',
    'chevron-down': '<path d="M6.4 9.6l5.6 5.6 5.6-5.6"/>',
    send: '<path d="M12 19V5.8M6.6 11.2L12 5.8l5.4 5.4"/>',
    panel: '<rect x="3.4" y="4.6" width="17.2" height="14.8" rx="2"/><path d="M9.6 4.6v14.8"/>',
    plus: '<path d="M12 5.2v13.6M5.2 12h13.6"/>',
    folder:
      '<path d="M3.4 6.4h6l1.8 2.4h9.4v10.8H3.4z"/>',
    'folder-open':
      '<path d="M3.4 6.4h6l1.8 2.4h7.4v2"/>' +
      '<path d="M3.4 19.6l2.6-8h15l-2.6 8z"/>',
    sliders:
      '<path d="M4 7.2h9M17 7.2h3M4 16.8h3M11 16.8h9"/>' +
      '<circle cx="15" cy="7.2" r="2.1"/>' +
      '<circle cx="9" cy="16.8" r="2.1"/>',
    external:
      '<path d="M13.6 4.4h6v6"/>' +
      '<path d="M19.6 4.4L11 13"/>' +
      '<path d="M18 14.2v5.4H4.4V6h5.4"/>',
    dots:
      '<circle cx="5.4" cy="12" r="1.5" fill="currentColor" stroke="none"/>' +
      '<circle cx="12" cy="12" r="1.5" fill="currentColor" stroke="none"/>' +
      '<circle cx="18.6" cy="12" r="1.5" fill="currentColor" stroke="none"/>',
    download:
      '<path d="M12 4.2v10.4M7.4 10.2l4.6 4.6 4.6-4.6"/>' +
      '<path d="M4.6 19.4h14.8"/>',
    minimize: '<path d="M5 12h14"/>',
    maximize: '<rect x="5.4" y="5.4" width="13.2" height="13.2" rx="1.6"/>',
    restore:
      '<rect x="4.6" y="7.4" width="12" height="12" rx="1.6"/>' +
      '<path d="M8.2 7.4v-2.8h11.2v11.2h-2.8"/>',
    stop: '<rect x="6.8" y="6.8" width="10.4" height="10.4" rx="1.6" fill="currentColor" stroke="none"/>',
    alert:
      '<path d="M12 3.6L22 20.4H2z"/>' +
      '<path d="M12 9.8v4.6"/>' +
      '<circle cx="12" cy="17.3" r="1" fill="currentColor" stroke="none"/>',
    /* Нейросеть: узлы пульсируют через .neuron (анимация ниже),
       поэтому «мозг» живой без единого эмодзи и без JS. */
    brain:
      '<path d="M6 7.5l5.5 4M16.5 6l-5 5.5M9 17.5l2.5-6M17.5 15.5l-6-4"/>' +
      '<circle class="neuron n1" cx="6" cy="7.5" r="2"/>' +
      '<circle class="neuron n2" cx="16.5" cy="6" r="2"/>' +
      '<circle class="neuron n3" cx="9" cy="17.5" r="2"/>' +
      '<circle class="neuron n4" cx="17.5" cy="15.5" r="2"/>' +
      '<circle class="neuron n5" cx="11.5" cy="11.5" r="1.7" fill="currentColor" stroke="none"/>',
  }
</script>

<svg
  viewBox="0 0 24 24"
  width={size}
  height={size}
  fill="none"
  stroke="currentColor"
  stroke-width={strokeWidth}
  stroke-linecap="round"
  stroke-linejoin="round"
  aria-hidden="true"
  class="icon"
>
  {@html SHAPES[name]}
</svg>

<style>
  .icon {
    display: inline-block;
    vertical-align: -0.18em;
    flex: none;
  }

  /* Узлы «мозга»: разметка приходит через {@html}, поэтому селектор global.
     Пульс идёт по кругу задержками — выглядит как бегущие нейроны. */
  .icon :global(.neuron) {
    animation: neuron-pulse 1.8s ease-in-out infinite;
  }
  .icon :global(.n2) {
    animation-delay: 0.35s;
  }
  .icon :global(.n3) {
    animation-delay: 0.7s;
  }
  .icon :global(.n4) {
    animation-delay: 1.05s;
  }
  .icon :global(.n5) {
    animation-delay: 1.4s;
  }

  @keyframes neuron-pulse {
    0%,
    100% {
      opacity: 0.3;
    }
    50% {
      opacity: 1;
    }
  }
</style>

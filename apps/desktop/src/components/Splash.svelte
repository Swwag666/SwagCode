<script lang="ts">
  /**
   * Splash-экран: анимированный глаз с glitch-эффектом (D-014).
   *
   * Референсы: хроматическая аберрация (Tokyo Ghoul), scan lines,
   * красный glow, чёрный фон. Чёрный фон гифки вырезается blend-mode screen.
   */
  import { onMount } from 'svelte'
  import logoGif from '../assets/logo.gif'

  interface Props {
    duration?: number
  }

  let { duration = 2200 }: Props = $props()

  let visible = $state(true)
  let fading = $state(false)
  let loadText = $state('')

  const loadMessages = [
    'инициализация ядра…',
    'загрузка провайдера…',
    'подключение шины…',
    'калибровка интерфейса…',
    'готово.',
  ]

  onMount(() => {
    // Печатаем сообщения загрузки
    let msgIdx = 0
    const typeInterval = setInterval(() => {
      if (msgIdx < loadMessages.length) {
        loadText = loadMessages[msgIdx]
        msgIdx++
      } else {
        clearInterval(typeInterval)
      }
    }, 400)

    const t1 = setTimeout(() => {
      fading = true
    }, duration)
    const t2 = setTimeout(() => {
      visible = false
    }, duration + 500)
    return () => {
      clearInterval(typeInterval)
      clearTimeout(t1)
      clearTimeout(t2)
    }
  })
</script>

{#if visible}
  <div class="splash" class:fading>
    <div class="glitch-container">
      <!-- Чёрный фон кадра вырезаем luminance-маской: blend-mode screen
           ломается от filter/zoom на предках, маска — нет. -->
      <span
        class="skull-img"
        style="-webkit-mask-image: url('{logoGif}'); mask-image: url('{logoGif}'); -webkit-mask-mode: luminance; mask-mode: luminance; -webkit-mask-size: contain; mask-size: contain; -webkit-mask-repeat: no-repeat; mask-repeat: no-repeat;"
      ></span>
      <!-- Glitch-слои: та же маска со сдвигом каналов -->
      <span
        class="skull-img glitch-r"
        style="-webkit-mask-image: url('{logoGif}'); mask-image: url('{logoGif}'); -webkit-mask-mode: luminance; mask-mode: luminance; -webkit-mask-size: contain; mask-size: contain; -webkit-mask-repeat: no-repeat; mask-repeat: no-repeat;"
      ></span>
      <span
        class="skull-img glitch-b"
        style="-webkit-mask-image: url('{logoGif}'); mask-image: url('{logoGif}'); -webkit-mask-mode: luminance; mask-mode: luminance; -webkit-mask-size: contain; mask-size: contain; -webkit-mask-repeat: no-repeat; mask-repeat: no-repeat;"
      ></span>
    </div>
    <div class="title glitch-text">
      <span class="diamond">◆</span>
      <span class="name">SwagCod</span>
    </div>
    <div class="subtitle">быстро · стабильно · без компромиссов</div>
    <div class="progress-bar">
      <div class="progress-fill"></div>
    </div>
    <div class="load-text">{loadText}</div>
    <div class="scanlines"></div>
  </div>
{/if}

<style>
  .splash {
    position: fixed;
    inset: 0;
    z-index: 1000;
    display: flex;
    flex-direction: column;
    align-items: center;
    justify-content: center;
    background: var(--bg);
    transition: opacity 0.5s ease-out;
    overflow: hidden;
  }

  .splash.fading {
    opacity: 0;
    pointer-events: none;
  }

  .glitch-container {
    position: relative;
    margin-bottom: 24px;
  }

  .skull-img {
    display: block;
    width: 150px;
    height: 120px;
    background: var(--accent);
    user-select: none;
    position: relative;
  }

  .glitch-r {
    position: absolute;
    top: 0;
    left: 0;
    background: #ff0040;
    opacity: 0.7;
    animation: glitch-shift-r 2s infinite;
    clip-path: inset(0 0 60% 0);
  }

  .glitch-b {
    position: absolute;
    top: 0;
    left: 0;
    background: #00ffff;
    opacity: 0.5;
    animation: glitch-shift-b 2s infinite;
    clip-path: inset(40% 0 0 0);
  }

  @keyframes glitch-shift-r {
    0%, 100% { transform: translate(0, 0); }
    10% { transform: translate(-2px, 1px); }
    20% { transform: translate(2px, -1px); }
    30% { transform: translate(0, 0); }
    50% { transform: translate(-1px, 2px); }
    70% { transform: translate(1px, -2px); }
    90% { transform: translate(0, 0); }
  }

  @keyframes glitch-shift-b {
    0%, 100% { transform: translate(0, 0); }
    15% { transform: translate(2px, -1px); }
    25% { transform: translate(-2px, 1px); }
    40% { transform: translate(0, 0); }
    55% { transform: translate(1px, -2px); }
    75% { transform: translate(-1px, 2px); }
    95% { transform: translate(0, 0); }
  }

  .title {
    display: flex;
    align-items: baseline;
    gap: 10px;
    margin-bottom: 8px;
    position: relative;
  }

  .glitch-text {
    animation: text-glitch 3s infinite;
  }

  @keyframes text-glitch {
    0%, 100% { text-shadow: none; }
    5% { text-shadow: -2px 0 #ff0040, 2px 0 #00ffff; }
    10% { text-shadow: none; }
    45% { text-shadow: none; }
    50% { text-shadow: -1px 0 #ff0040, 1px 0 #00ffff; }
    55% { text-shadow: none; }
  }

  .diamond {
    color: var(--accent);
    font-size: 28px;
  }

  .name {
    font-family: var(--mono);
    font-size: 32px;
    font-weight: 700;
    letter-spacing: 0.05em;
    color: var(--text);
  }

  .subtitle {
    font-family: var(--mono);
    font-size: 12px;
    color: var(--text-dim);
    letter-spacing: 0.15em;
    text-transform: uppercase;
    margin-bottom: 24px;
  }

  .progress-bar {
    width: 200px;
    height: 2px;
    background: var(--border);
    border-radius: 1px;
    overflow: hidden;
  }

  .progress-fill {
    height: 100%;
    background: var(--accent);
    border-radius: 1px;
    animation: progress 2s ease-in-out forwards;
    box-shadow: 0 0 8px var(--accent-glow);
  }

  @keyframes progress {
    from { width: 0%; }
    to { width: 100%; }
  }

  .load-text {
    margin-top: 12px;
    font-family: var(--mono);
    font-size: 10px;
    color: var(--text-faint);
    letter-spacing: 0.05em;
    min-height: 14px;
  }

  .scanlines {
    position: absolute;
    inset: 0;
    pointer-events: none;
    background: repeating-linear-gradient(
      0deg,
      transparent,
      transparent 2px,
      rgba(0, 0, 0, 0.08) 2px,
      rgba(0, 0, 0, 0.08) 3px
    );
  }
</style>
